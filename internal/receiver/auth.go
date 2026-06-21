package receiver

import (
	"crypto/subtle"
	"net"
	"strings"
)

// Authentication (F1): when --auth-token is set, every OTLP request from a
// non-loopback peer must present the matching bearer token, mirroring the api
// package's auth middleware. Loopback peers (the local Claude Code process) are
// exempt so the zero-config local flow keeps working. When the token is empty
// auth is disabled entirely (the bind policy in config.Validate already requires
// either loopback, --unsafe, or --auth-token, so an empty token means the
// operator chose loopback-only or --unsafe).

// tokenEqual compares a presented token against the configured one in constant
// time to avoid leaking length/contents via timing.
func tokenEqual(presented, want string) bool {
	return subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" value.
// The scheme match is case-insensitive per RFC 7235; the token itself is
// returned verbatim. An empty/garbage header yields "".
func bearerToken(authHeader string) string {
	authHeader = strings.TrimSpace(authHeader)
	const prefix = "bearer "
	if len(authHeader) < len(prefix) || !strings.EqualFold(authHeader[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(authHeader[len(prefix):])
}

// peerIsLoopback reports whether addr (an IP[:port] or bare IP literal, as found
// in http.Request.RemoteAddr or a gRPC peer.Addr) is on the loopback interface.
// An unparseable address is treated as non-loopback (fail closed: require auth).
func peerIsLoopback(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
