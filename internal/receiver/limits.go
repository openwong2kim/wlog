// Package receiver terminates OTLP over gRPC (:4317) and HTTP (:4318),
// applies wire-level limits and defenses, and hands each request to the ingest
// pipeline with retryable backpressure (PLAN §5, §17, REVIEWS B·D).
//
// Limits & defenses (limits.go):
//   - --max-recv-bytes bounds the raw request body (gRPC via maxRecvMsgSize, HTTP
//     via http.MaxBytesReader on the compressed bytes plus an up-front
//     Content-Length check, so a small gzip with a large trailing payload cannot
//     slip past the cap before decompression).
//   - gzip is transparently decompressed; the decompressed size is capped at
//     decompressLimit (32 MiB) to defeat decompression bombs — exceeding it is a
//     413 (HTTP) / rejected request (gRPC handles size at the codec layer).
//   - On a loopback bind the HTTP Host header must itself be loopback (or empty),
//     rejecting cross-origin DNS-rebinding attempts. Non-loopback binds are
//     gated upstream by config.Validate (--unsafe/--auth-token), so here we only
//     enforce the Host check for the loopback case.
package receiver

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// decompressLimit caps the number of bytes read out of a gzip stream (after
// decompression) to defeat decompression bombs (PLAN §5/§17: 32 MiB).
const decompressLimit = 32 * 1024 * 1024

// errTooLarge signals that a body (compressed or decompressed) exceeded a limit.
var errTooLarge = errors.New("receiver: payload too large")

// readBody reads the HTTP request body honoring Content-Encoding: gzip and the
// two size limits. maxRecvBytes bounds the raw (possibly compressed) body;
// decompressLimit bounds the decompressed size. It returns errTooLarge when
// either limit is exceeded so the caller can map it to 413.
//
// The raw cap is enforced on the *compressed* bytes via http.MaxBytesReader so a
// small gzip header followed by a large trailing payload cannot slip past
// --max-recv-bytes by only being detected after decompression (C9). A declared
// Content-Length over the cap is rejected up front without reading the body.
func readBody(w http.ResponseWriter, r *http.Request, maxRecvBytes int64) ([]byte, error) {
	if maxRecvBytes <= 0 {
		maxRecvBytes = 16 * 1024 * 1024
	}
	// Fast reject: an honestly-declared Content-Length over the cap fails before
	// any body is read. A spoofed/absent length is still bounded by MaxBytesReader.
	if r.ContentLength > maxRecvBytes {
		return nil, errTooLarge
	}
	// Cap the raw (possibly compressed) stream. MaxBytesReader stops the read at
	// maxRecvBytes+1 bytes and surfaces *http.MaxBytesError, which we normalize to
	// errTooLarge below. This bounds the compressed input itself, not just the
	// decompressed output.
	raw := http.MaxBytesReader(w, r.Body, maxRecvBytes+1)

	enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	if enc == "gzip" {
		gz, err := gzip.NewReader(raw)
		if err != nil {
			return nil, mapMaxBytes(err)
		}
		defer gz.Close()
		// Read the decompressed stream, capped at decompressLimit+1. A raw-stream
		// over-read trips MaxBytesReader and surfaces here as errTooLarge.
		out, err := readCapped(gz, decompressLimit)
		if err != nil {
			return nil, mapMaxBytes(err)
		}
		return out, nil
	}

	out, err := readCapped(raw, maxRecvBytes)
	if err != nil {
		return nil, mapMaxBytes(err)
	}
	return out, nil
}

// mapMaxBytes normalizes an *http.MaxBytesError (raw cap exceeded) to errTooLarge
// so the HTTP handler maps it to 413. Other errors pass through unchanged.
func mapMaxBytes(err error) error {
	if err == nil {
		return nil
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return errTooLarge
	}
	return err
}

// readCapped reads up to limit bytes; reading even one more byte returns
// errTooLarge. It reads limit+1 from r (r is expected to already be bounded one
// past the limit for the raw case) to distinguish "exactly limit" from "over".
func readCapped(r io.Reader, limit int64) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("receiver: read body: %w", err)
	}
	if int64(len(buf)) > limit {
		return nil, errTooLarge
	}
	return buf, nil
}

// checkHost enforces the DNS-rebinding defense: when the server is bound to a
// loopback address, the request's Host header must also resolve to loopback (or
// be empty / a bare loopback name). It returns false when the Host is a
// non-loopback name, which the caller maps to 403.
//
// bindLoopback reports whether the listener is bound to a loopback address
// (computed once from config at server construction).
func checkHost(host string, bindLoopback bool) bool {
	if !bindLoopback {
		// Non-loopback binds are explicitly opt-in (config.Validate) and may be
		// reached by any hostname; do not second-guess the operator here.
		return true
	}
	if host == "" {
		return true
	}
	// Strip any port.
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.TrimSpace(h)
	if h == "" || h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	// A non-loopback hostname pointed at our loopback listener is the rebinding
	// case we reject.
	return false
}
