package api

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/openwong2kim/wlog/internal/config"
	"github.com/openwong2kim/wlog/internal/ingest"
	"github.com/openwong2kim/wlog/internal/web"
)

// Server is the UI-facing HTTP API. It holds the read DB pool, the live bus,
// a counters accessor for /api/health, and metadata (config, db path). It does
// not own the http.Server lifecycle — Batch G wraps Handler() in an
// http.Server — but a convenience Start/Shutdown is provided for standalone use.
type Server struct {
	cfg      *config.Config
	db       *sql.DB
	bus      *ingest.Bus
	counters func() ingest.Counters
	dbPath   string

	started time.Time
	static  http.Handler // web UI fallback for non-/api paths

	// httpSrv is lazily created by Start; it is nil when the server is only used
	// via Handler() (the Batch G wrapping path).
	httpSrv *http.Server
}

// New constructs a Server. db must be the store read pool (never the writer).
// counters returns a point-in-time pipeline+bus counter snapshot for
// /api/health. dbPath is the on-disk database path ("" / ":memory:" for memory).
func New(cfg *config.Config, db *sql.DB, bus *ingest.Bus, counters func() ingest.Counters, dbPath string) *Server {
	if counters == nil {
		counters = func() ingest.Counters { return ingest.Counters{} }
	}
	return &Server{
		cfg:      cfg,
		db:       db,
		bus:      bus,
		counters: counters,
		dbPath:   dbPath,
		started:  time.Now(),
		static:   web.Handler(),
	}
}

// Handler returns the root http.Handler: it routes /api/* to the JSON/SSE API
// and falls back to the embedded web UI for everything else. The full handler
// (API + static) is wrapped by the auth gate (loopback-exempt bearer/token
// enforcement when cfg.AuthToken is set) and then CORS (localhost origins only,
// applied to /api/* responses).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Static UI fallback (and SPA hash routing) for non-API paths.
	mux.Handle("/", s.static)

	// API routes. Go 1.22+ pattern routing with method + wildcard segments.
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleSession)
	mux.HandleFunc("GET /api/sessions/{id}/timeline", s.handleTimeline)
	mux.HandleFunc("GET /api/cost", s.handleCost)
	mux.HandleFunc("GET /api/tools", s.handleTools)
	mux.HandleFunc("GET /api/now", s.handleNow)
	mux.HandleFunc("GET /api/setup", s.handleSetup)
	mux.HandleFunc("GET /api/export", s.handleExport)

	// Order: CORS sets/clears headers and answers OPTIONS preflight (which must
	// not require auth), then the auth gate guards the real request, then the mux.
	return s.withCORS(s.withAuth(mux))
}

// withAuth enforces the configured --auth-token on non-loopback requests. When
// cfg.AuthToken is empty the middleware is a pass-through. When set, requests
// from a loopback peer (RemoteAddr) are exempt — the locally embedded UI works
// without a token — while every non-loopback request must present a matching
// token via the Authorization: Bearer <token> header OR a ?token=<token> query
// parameter (the latter for EventSource/SSE, which cannot set headers). A
// missing or mismatched token yields 401. It wraps both /api/* and static
// serving (the next handler is the whole mux).
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg == nil || s.cfg.AuthToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		if isLoopbackAddr(r.RemoteAddr) {
			next.ServeHTTP(w, r)
			return
		}
		if !authTokenMatches(r, s.cfg.AuthToken) {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "missing or invalid auth token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authTokenMatches reports whether the request carries the expected token in
// either the Authorization: Bearer header or the ?token= query parameter.
// Comparison is constant-time to avoid leaking the token via timing.
func authTokenMatches(r *http.Request, token string) bool {
	if h := r.Header.Get("Authorization"); h != "" {
		if rest, ok := strings.CutPrefix(h, "Bearer "); ok {
			if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(rest)), []byte(token)) == 1 {
				return true
			}
		}
	}
	if q := r.URL.Query().Get("token"); q != "" {
		if subtle.ConstantTimeCompare([]byte(q), []byte(token)) == 1 {
			return true
		}
	}
	return false
}

// isLoopbackAddr reports whether a RemoteAddr ("host:port" or bare host) refers
// to the loopback interface. A non-IP host (e.g. "localhost", or empty) is
// treated as NON-loopback so it does not bypass auth — RemoteAddr is always an
// IP:port set by net/http, so this only guards against malformed/test inputs.
func isLoopbackAddr(remoteAddr string) bool {
	if remoteAddr == "" {
		return false
	}
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// withCORS wraps the mux to add CORS headers for localhost origins on /api/*
// requests and to answer preflight OPTIONS. Non-localhost origins get no CORS
// headers (the browser then blocks the cross-origin read), matching the
// loopback-only security posture (PLAN §17).
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			origin := r.Header.Get("Origin")
			if origin != "" && isLocalhostOrigin(origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Last-Event-ID")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// isLocalhostOrigin reports whether origin's host is loopback (localhost,
// 127.0.0.0/8, or ::1), regardless of port. A malformed origin is rejected.
func isLocalhostOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// Start binds an http.Server on the configured listen address + UI port and
// serves Handler() until ctx is cancelled. It is a convenience for standalone
// use; the production wiring (Batch G) calls Handler() and owns its own server.
func (s *Server) Start(ctx context.Context, addr string) error {
	s.httpSrv = &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errCh := make(chan error, 1)
	go func() {
		err := s.httpSrv.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// Shutdown gracefully stops the http.Server started by Start. It is a no-op when
// Start was never called (the Batch G wrapping path owns shutdown).
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}
	return s.httpSrv.Shutdown(ctx)
}
