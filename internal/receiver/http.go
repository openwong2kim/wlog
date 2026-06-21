package receiver

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/openwong2kim/wlog/internal/ingest"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

// contentTypeProto / contentTypeJSON are the two OTLP/HTTP encodings we accept.
const (
	contentTypeProto = "application/x-protobuf"
	contentTypeJSON  = "application/json"
)

// retryAfterSeconds is the Retry-After header value (seconds) on HTTP
// backpressure (HTTP cannot carry sub-second precision portably).
const retryAfterSeconds = "1"

// httpHandler builds the OTLP/HTTP mux: POST /v1/{metrics,logs,traces}. It is
// safe for concurrent use (it only reads immutable config and calls the
// concurrency-safe pipeline).
type httpHandler struct {
	p            submitter
	maxRecvBytes int64
	bindLoopback bool
	authToken    string // when non-empty, required for non-loopback peers (F1)
}

func newHTTPHandler(p submitter, maxRecvBytes int64, bindLoopback bool, authToken string) http.Handler {
	h := &httpHandler{p: p, maxRecvBytes: maxRecvBytes, bindLoopback: bindLoopback, authToken: authToken}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/metrics", h.handleMetrics)
	mux.HandleFunc("/v1/logs", h.handleLogs)
	mux.HandleFunc("/v1/traces", h.handleTraces)
	return mux
}

func (h *httpHandler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	msg := &collectormetricspb.ExportMetricsServiceRequest{}
	if !h.decode(w, r, msg) {
		return
	}
	if !h.submit(w, ingest.KindMetrics, msg) {
		return
	}
	h.writeResponse(w, r, &collectormetricspb.ExportMetricsServiceResponse{})
}

func (h *httpHandler) handleLogs(w http.ResponseWriter, r *http.Request) {
	msg := &collectorlogspb.ExportLogsServiceRequest{}
	if !h.decode(w, r, msg) {
		return
	}
	if !h.submit(w, ingest.KindLogs, msg) {
		return
	}
	h.writeResponse(w, r, &collectorlogspb.ExportLogsServiceResponse{})
}

func (h *httpHandler) handleTraces(w http.ResponseWriter, r *http.Request) {
	msg := &collectortracepb.ExportTraceServiceRequest{}
	if !h.decode(w, r, msg) {
		return
	}
	if !h.submit(w, ingest.KindTraces, msg) {
		return
	}
	h.writeResponse(w, r, &collectortracepb.ExportTraceServiceResponse{})
}

// decode validates method/Host, reads+decompresses the body honoring the size
// limits, and unmarshals it (proto or protojson) into msg. On any failure it
// writes the appropriate status and returns false.
func (h *httpHandler) decode(w http.ResponseWriter, r *http.Request, msg proto.Message) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if !checkHost(r.Host, h.bindLoopback) {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return false
	}
	if !h.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}

	body, err := readBody(w, r, h.maxRecvBytes)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return false
		}
		// A malformed gzip stream or read error is a harmless client problem.
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}

	ct := contentType(r.Header.Get("Content-Type"))
	switch ct {
	case contentTypeJSON:
		if err := protojson.Unmarshal(body, msg); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return false
		}
	default: // default to protobuf for x-protobuf and unspecified
		if err := proto.Unmarshal(body, msg); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return false
		}
	}
	return true
}

// authorized reports whether the request may proceed under the F1 auth policy:
// when an auth token is configured, a non-loopback peer must present the
// matching "Authorization: Bearer <token>" header. Loopback peers and the
// no-token (loopback-only / --unsafe) configuration are always allowed.
func (h *httpHandler) authorized(r *http.Request) bool {
	if h.authToken == "" {
		return true
	}
	if peerIsLoopback(r.RemoteAddr) {
		return true
	}
	return tokenEqual(bearerToken(r.Header.Get("Authorization")), h.authToken)
}

// submit enqueues the request, mapping a full queue to 503 + Retry-After
// (retryable, REVIEWS B). Returns false when a response has been written.
func (h *httpHandler) submit(w http.ResponseWriter, kind ingest.RequestKind, msg any) bool {
	err := h.p.Submit(kind, msg)
	switch {
	case err == nil:
		return true
	case errors.Is(err, ingest.ErrBusy):
		w.Header().Set("Retry-After", retryAfterSeconds)
		http.Error(w, "ingest queue full; retry", http.StatusServiceUnavailable)
		return false
	default:
		w.Header().Set("Retry-After", retryAfterSeconds)
		http.Error(w, "ingest unavailable", http.StatusServiceUnavailable)
		return false
	}
}

// writeResponse encodes the (empty success) OTLP response in the same encoding
// the client requested (proto by default, protojson when Content-Type was JSON).
func (h *httpHandler) writeResponse(w http.ResponseWriter, r *http.Request, msg proto.Message) {
	if contentType(r.Header.Get("Content-Type")) == contentTypeJSON {
		out, err := protojson.Marshal(msg)
		if err != nil {
			http.Error(w, "internal", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", contentTypeJSON)
		_, _ = w.Write(out)
		return
	}
	out, err := proto.Marshal(msg)
	if err != nil {
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentTypeProto)
	_, _ = w.Write(out)
}

// contentType normalizes a Content-Type header to its bare media type
// (lower-cased, parameters stripped). An empty/unknown value falls through to
// the protobuf default at the call site.
func contentType(v string) string {
	v = strings.TrimSpace(strings.ToLower(v))
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

// parseRetryAfter is a tiny helper kept for symmetry/testing of the header value.
func parseRetryAfter(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
