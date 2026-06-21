package receiver

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckHost(t *testing.T) {
	cases := []struct {
		host     string
		loopback bool
		want     bool
	}{
		{"127.0.0.1:4318", true, true},
		{"127.0.0.1", true, true},
		{"localhost:4318", true, true},
		{"localhost", true, true},
		{"[::1]:4318", true, true},
		{"", true, true},
		{"evil.example.com", true, false},
		{"evil.example.com:80", true, false},
		{"10.0.0.5:4318", true, false},
		// Non-loopback bind: any Host is allowed (operator opted in).
		{"evil.example.com", false, true},
		{"10.0.0.5", false, true},
	}
	for _, c := range cases {
		if got := checkHost(c.host, c.loopback); got != c.want {
			t.Errorf("checkHost(%q, loopback=%v) = %v, want %v", c.host, c.loopback, got, c.want)
		}
	}
}

func TestReadBodyPlain(t *testing.T) {
	data := []byte("hello world")
	r := newReq(data, "")
	out, err := readBody(httptest.NewRecorder(), r, 1024)
	if err != nil {
		t.Fatalf("readBody: %v", err)
	}
	if !bytes.Equal(out, data) {
		t.Fatalf("readBody = %q, want %q", out, data)
	}
}

func TestReadBodyGzip(t *testing.T) {
	data := bytes.Repeat([]byte("abc"), 1000)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(data)
	_ = gz.Close()

	r := newReq(buf.Bytes(), "gzip")
	out, err := readBody(httptest.NewRecorder(), r, 1024*1024)
	if err != nil {
		t.Fatalf("readBody gzip: %v", err)
	}
	if !bytes.Equal(out, data) {
		t.Fatalf("decompressed mismatch (len %d vs %d)", len(out), len(data))
	}
}

func TestReadBodyMaxRecvExceeded(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 100)
	r := newReq(data, "")
	_, err := readBody(httptest.NewRecorder(), r, 10)
	if !errors.Is(err, errTooLarge) {
		t.Fatalf("expected errTooLarge, got %v", err)
	}
}

// TestReadBodyContentLengthRejected verifies that an honestly-declared
// Content-Length over the cap is rejected before the body is read (C9).
func TestReadBodyContentLengthRejected(t *testing.T) {
	r := newReq(bytes.Repeat([]byte("x"), 100), "")
	r.ContentLength = 100
	_, err := readBody(httptest.NewRecorder(), r, 10)
	if !errors.Is(err, errTooLarge) {
		t.Fatalf("expected errTooLarge for over-cap Content-Length, got %v", err)
	}
}

// TestReadBodyGzipRawCapEnforced is the C9 regression: a tiny gzip header
// followed by a large *compressed* trailing payload must trip the raw-bytes cap
// (via MaxBytesReader) even though each individual decompressed read stays under
// decompressLimit. Before the fix only the decompressed size was bounded, so a
// large raw body could slip past --max-recv-bytes.
func TestReadBodyGzipRawCapEnforced(t *testing.T) {
	// Build a gzip stream whose *compressed* size exceeds the raw cap. Random-ish
	// incompressible data keeps the compressed size close to the input size.
	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte(i*31 + 7)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(payload)
	_ = gz.Close()

	rawLen := int64(buf.Len())
	if rawLen <= 64 {
		t.Skip("compressed payload unexpectedly tiny")
	}
	// Cap below the compressed size: the raw stream must be refused.
	r := newReq(buf.Bytes(), "gzip")
	r.ContentLength = -1 // force the body-read path, not the up-front length check
	_, err := readBody(httptest.NewRecorder(), r, 64)
	if !errors.Is(err, errTooLarge) {
		t.Fatalf("expected errTooLarge from raw gzip cap, got %v", err)
	}
}

func TestReadBodyGzipBombCapped(t *testing.T) {
	// Compress a body whose decompressed size exceeds decompressLimit. We make a
	// highly-compressible payload just over the limit so the gzip stream itself
	// stays under maxRecvBytes but the decompressed size trips the cap.
	huge := bytes.Repeat([]byte{0}, decompressLimit+1024)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(huge)
	_ = gz.Close()

	if int64(buf.Len()) >= decompressLimit {
		t.Skip("compressed bomb unexpectedly large for this test host")
	}
	r := newReq(buf.Bytes(), "gzip")
	_, err := readBody(httptest.NewRecorder(), r, int64(buf.Len())+1024) // raw fits; decompressed must not
	if !errors.Is(err, errTooLarge) {
		t.Fatalf("expected errTooLarge from decompression cap, got %v", err)
	}
}

func TestReadBodyBadGzip(t *testing.T) {
	r := newReq([]byte("not gzip data at all"), "gzip")
	_, err := readBody(httptest.NewRecorder(), r, 1024)
	if err == nil {
		t.Fatalf("expected error for malformed gzip")
	}
	if errors.Is(err, errTooLarge) {
		t.Fatalf("malformed gzip should not be errTooLarge")
	}
}

// newReq builds a minimal *http.Request with the given body and optional
// Content-Encoding for exercising readBody directly.
func newReq(body []byte, encoding string) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1/v1/metrics", io.NopCloser(bytes.NewReader(body)))
	if encoding != "" {
		r.Header.Set("Content-Encoding", encoding)
	}
	return r
}

func TestReadBodyExactLimit(t *testing.T) {
	data := []byte(strings.Repeat("y", 16))
	r := newReq(data, "")
	out, err := readBody(httptest.NewRecorder(), r, 16)
	if err != nil {
		t.Fatalf("exact-limit body should be accepted: %v", err)
	}
	if len(out) != 16 {
		t.Fatalf("got %d bytes, want 16", len(out))
	}
}
