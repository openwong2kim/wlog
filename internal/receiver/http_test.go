package receiver

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

func newHTTPTest(sub submitter, maxRecv int64, bindLoopback bool) *httptest.Server {
	return httptest.NewServer(newHTTPHandler(sub, maxRecv, bindLoopback, ""))
}

// newHTTPTestAuth is like newHTTPTest but with an auth token configured (F1).
func newHTTPTestAuth(sub submitter, maxRecv int64, bindLoopback bool, token string) *httptest.Server {
	return httptest.NewServer(newHTTPHandler(sub, maxRecv, bindLoopback, token))
}

func TestHTTPProtoRoundTrip(t *testing.T) {
	sub := &fakeSubmitter{}
	ts := newHTTPTest(sub, 16*1024*1024, true)
	defer ts.Close()

	body, err := proto.Marshal(sampleMetricsReq())
	if err != nil {
		t.Fatal(err)
	}
	resp := post(t, ts.URL+"/v1/metrics", contentTypeProto, "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	got := &collectormetricspb.ExportMetricsServiceResponse{}
	if err := proto.Unmarshal(out, got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if sub.metrics.Load() != 1 {
		t.Fatalf("metrics not submitted: %d", sub.metrics.Load())
	}
}

func TestHTTPJSONRoundTrip(t *testing.T) {
	sub := &fakeSubmitter{}
	ts := newHTTPTest(sub, 16*1024*1024, true)
	defer ts.Close()

	body, err := protojson.Marshal(sampleLogsReq())
	if err != nil {
		t.Fatal(err)
	}
	resp := post(t, ts.URL+"/v1/logs", contentTypeJSON, "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	got := &collectorlogspb.ExportLogsServiceResponse{}
	if err := protojson.Unmarshal(out, got); err != nil {
		t.Fatalf("unmarshal json response: %v", err)
	}
	if sub.logs.Load() != 1 {
		t.Fatalf("logs not submitted: %d", sub.logs.Load())
	}
}

func TestHTTPTracesProto(t *testing.T) {
	sub := &fakeSubmitter{}
	ts := newHTTPTest(sub, 16*1024*1024, true)
	defer ts.Close()

	body, _ := proto.Marshal(sampleTracesReq())
	resp := post(t, ts.URL+"/v1/traces", contentTypeProto, "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	got := &collectortracepb.ExportTraceServiceResponse{}
	if err := proto.Unmarshal(out, got); err != nil {
		t.Fatalf("unmarshal trace response: %v", err)
	}
	if sub.traces.Load() != 1 {
		t.Fatalf("traces not submitted: %d", sub.traces.Load())
	}
}

func TestHTTPGzipDecompress(t *testing.T) {
	sub := &fakeSubmitter{}
	ts := newHTTPTest(sub, 16*1024*1024, true)
	defer ts.Close()

	raw, _ := proto.Marshal(sampleMetricsReq())
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(raw)
	_ = gz.Close()

	resp := post(t, ts.URL+"/v1/metrics", contentTypeProto, "gzip", buf.Bytes())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gzip status = %d, want 200", resp.StatusCode)
	}
	if sub.metrics.Load() != 1 {
		t.Fatalf("gzip metrics not submitted: %d", sub.metrics.Load())
	}
}

func TestHTTPBackpressure503(t *testing.T) {
	sub := &fakeSubmitter{}
	sub.busy.Store(true)
	ts := newHTTPTest(sub, 16*1024*1024, true)
	defer ts.Close()

	body, _ := proto.Marshal(sampleMetricsReq())
	resp := post(t, ts.URL+"/v1/metrics", contentTypeProto, "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatalf("expected Retry-After header on 503")
	}
}

func TestHTTPMaxRecvBytesExceeded(t *testing.T) {
	sub := &fakeSubmitter{}
	// Tiny limit so a normal proto body exceeds it.
	ts := newHTTPTest(sub, 8, true)
	defer ts.Close()

	body, _ := proto.Marshal(sampleMetricsReq())
	if len(body) <= 8 {
		t.Skip("sample body unexpectedly tiny")
	}
	resp := post(t, ts.URL+"/v1/metrics", contentTypeProto, "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if sub.metrics.Load() != 0 {
		t.Fatalf("over-limit body must not be submitted")
	}
}

func TestHTTPHostCheckRejectsRebind(t *testing.T) {
	sub := &fakeSubmitter{}
	ts := newHTTPTest(sub, 16*1024*1024, true /* loopback bind */)
	defer ts.Close()

	body, _ := proto.Marshal(sampleMetricsReq())
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/metrics", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentTypeProto)
	// Spoof a non-loopback Host header (DNS rebinding attempt).
	req.Host = "evil.example.com"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for non-loopback Host", resp.StatusCode)
	}
	if sub.metrics.Load() != 0 {
		t.Fatalf("rebinding request must not be submitted")
	}
}

func TestHTTPHostCheckAllowsLoopback(t *testing.T) {
	sub := &fakeSubmitter{}
	ts := newHTTPTest(sub, 16*1024*1024, true)
	defer ts.Close()

	body, _ := proto.Marshal(sampleMetricsReq())
	// httptest binds 127.0.0.1; default Host is 127.0.0.1:port → loopback, allowed.
	resp := post(t, ts.URL+"/v1/metrics", contentTypeProto, "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("loopback Host rejected: %d", resp.StatusCode)
	}
}

func TestHTTPBadBody400(t *testing.T) {
	sub := &fakeSubmitter{}
	ts := newHTTPTest(sub, 16*1024*1024, true)
	defer ts.Close()

	resp := post(t, ts.URL+"/v1/metrics", contentTypeProto, "", []byte("not-a-protobuf-\xff\xfe"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for malformed body", resp.StatusCode)
	}
	if sub.metrics.Load() != 0 {
		t.Fatalf("malformed body must not be submitted")
	}
}

func TestHTTPMethodNotAllowed(t *testing.T) {
	sub := &fakeSubmitter{}
	ts := newHTTPTest(sub, 16*1024*1024, true)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", resp.StatusCode)
	}
}

// TestHTTPAuthorized covers the F1 bearer-token policy on the HTTP path. The
// authorized() decision is exercised directly so we can vary RemoteAddr (httptest
// always binds loopback, which would otherwise mask the non-loopback path).
func TestHTTPAuthorized(t *testing.T) {
	h := &httpHandler{authToken: "s3cr3t"}

	mkReq := func(remote, authHeader string) *http.Request {
		r, _ := http.NewRequest(http.MethodPost, "http://example/v1/metrics", nil)
		r.RemoteAddr = remote
		if authHeader != "" {
			r.Header.Set("Authorization", authHeader)
		}
		return r
	}

	cases := []struct {
		name   string
		remote string
		auth   string
		want   bool
	}{
		{"loopback exempt no token", "127.0.0.1:5000", "", true},
		{"loopback exempt ipv6", "[::1]:5000", "", true},
		{"non-loopback valid token", "10.0.0.5:5000", "Bearer s3cr3t", true},
		{"non-loopback valid token case-insensitive scheme", "10.0.0.5:5000", "bearer s3cr3t", true},
		{"non-loopback wrong token", "10.0.0.5:5000", "Bearer nope", false},
		{"non-loopback missing token", "10.0.0.5:5000", "", false},
		{"non-loopback malformed header", "10.0.0.5:5000", "s3cr3t", false},
		{"unresolvable peer requires token", "garbage", "", false},
	}
	for _, c := range cases {
		if got := h.authorized(mkReq(c.remote, c.auth)); got != c.want {
			t.Errorf("%s: authorized = %v, want %v", c.name, got, c.want)
		}
	}

	// Empty token disables auth entirely (loopback-only / --unsafe posture).
	hOpen := &httpHandler{authToken: ""}
	if !hOpen.authorized(mkReq("10.0.0.5:5000", "")) {
		t.Error("empty token should allow any peer")
	}
}

// TestHTTPAuthRejectsUnauthenticated drives a full request through a server with
// auth enabled but a loopback peer (httptest): with no token it still passes
// because the local peer is exempt, proving the loopback bypass end-to-end.
func TestHTTPAuthLoopbackBypass(t *testing.T) {
	sub := &fakeSubmitter{}
	ts := newHTTPTestAuth(sub, 16*1024*1024, true, "s3cr3t")
	defer ts.Close()

	body, _ := proto.Marshal(sampleMetricsReq())
	resp := post(t, ts.URL+"/v1/metrics", contentTypeProto, "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("loopback peer with auth enabled should pass: status %d", resp.StatusCode)
	}
	if sub.metrics.Load() != 1 {
		t.Fatalf("loopback request not submitted: %d", sub.metrics.Load())
	}
}

func TestContentTypeNormalize(t *testing.T) {
	cases := map[string]string{
		"application/json":              contentTypeJSON,
		"application/json; charset=utf8": contentTypeJSON,
		"APPLICATION/X-PROTOBUF":        contentTypeProto,
		"":                              "",
	}
	for in, want := range cases {
		if got := contentType(in); got != want {
			t.Errorf("contentType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	if n, ok := parseRetryAfter(" 3 "); !ok || n != 3 {
		t.Fatalf("parseRetryAfter(3) = %d,%v", n, ok)
	}
	if _, ok := parseRetryAfter("x"); ok {
		t.Fatalf("parseRetryAfter(x) should fail")
	}
}

// post is a small helper that performs a POST with the given content-type and
// optional content-encoding and verifies that the assertion uses _ = ingest to
// keep the import meaningful.
func post(t *testing.T, url, contentType, encoding string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
