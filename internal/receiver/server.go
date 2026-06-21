package receiver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/openwong2kim/wlog/internal/config"
	"github.com/openwong2kim/wlog/internal/ingest"
	"google.golang.org/grpc"
)

// Server terminates OTLP/gRPC and OTLP/HTTP, sharing a single ingest pipeline.
// Both listeners bind to cfg.Listen on their configured ports. The pipeline's
// lifecycle (Start/Shutdown drain) is owned by the app, which orchestrates the
// graceful-shutdown sequence (listeners stop first, then ingest drains).
type Server struct {
	cfg *config.Config
	p   *ingest.Pipeline

	grpcSrv  *grpc.Server
	httpSrv  *http.Server
	bindLoop bool

	mu        sync.Mutex
	grpcLn    net.Listener
	httpLn    net.Listener
	startedAt time.Time
}

// New constructs a Server from config and the pipeline. It does not bind any
// port until Start is called.
func New(cfg *config.Config, p *ingest.Pipeline) *Server {
	bindLoop := isLoopbackHost(cfg.Listen)
	s := &Server{
		cfg:      cfg,
		p:        p,
		bindLoop: bindLoop,
	}
	s.grpcSrv = registerGRPC(p, cfg.MaxRecvBytes, cfg.AuthToken)
	s.httpSrv = &http.Server{
		Handler:           newHTTPHandler(p, cfg.MaxRecvBytes, bindLoop, cfg.AuthToken),
		ReadHeaderTimeout: 10 * time.Second,
		// MaxHeaderBytes guards against oversized header floods; bodies are bounded
		// in readBody by --max-recv-bytes.
		MaxHeaderBytes: 1 << 20,
	}
	return s
}

// Start binds both listeners and serves in the background. It returns nil once
// both listeners are bound and serving; a bind error (e.g. a port conflict) is
// returned synchronously with a DESIGN §6 hint. The two Serve loops run in
// goroutines and exit on Shutdown (returning ErrServerClosed, which is ignored).
func (s *Server) Start(ctx context.Context) error {
	lc := net.ListenConfig{}

	grpcAddr := net.JoinHostPort(s.cfg.Listen, fmt.Sprintf("%d", s.cfg.OTLPGRPCPort))
	grpcLn, err := lc.Listen(ctx, "tcp", grpcAddr)
	if err != nil {
		return portErr("OTLP/gRPC", grpcAddr, s.cfg.OTLPGRPCPort, err)
	}

	httpAddr := net.JoinHostPort(s.cfg.Listen, fmt.Sprintf("%d", s.cfg.OTLPHTTPPort))
	httpLn, err := lc.Listen(ctx, "tcp", httpAddr)
	if err != nil {
		_ = grpcLn.Close()
		return portErr("OTLP/HTTP", httpAddr, s.cfg.OTLPHTTPPort, err)
	}

	s.mu.Lock()
	s.grpcLn = grpcLn
	s.httpLn = httpLn
	s.startedAt = time.Now()
	s.mu.Unlock()

	go func() {
		// Serve returns ErrServerClosed on graceful stop; ignore it.
		_ = s.grpcSrv.Serve(grpcLn)
	}()
	go func() {
		if err := s.httpSrv.Serve(httpLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// A non-graceful HTTP serve error is logged by the app via health; we
			// have no logger here. The listener is already closed in this case.
			_ = err
		}
	}()

	return nil
}

// GRPCAddr / HTTPAddr return the actual bound addresses (useful when a port was
// chosen as 0). Empty until Start succeeds.
func (s *Server) GRPCAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.grpcLn == nil {
		return ""
	}
	return s.grpcLn.Addr().String()
}

func (s *Server) HTTPAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.httpLn == nil {
		return ""
	}
	return s.httpLn.Addr().String()
}

// Shutdown gracefully stops both listeners so no new requests are accepted. It
// does NOT drain the ingest pipeline — that is the app's responsibility after
// the listeners stop (so in-flight requests are committed). It honours the
// context deadline for the HTTP graceful stop and falls back to a hard stop.
func (s *Server) Shutdown(ctx context.Context) error {
	var firstErr error

	// Stop HTTP gracefully (waits for in-flight handlers up to the deadline).
	if err := s.httpSrv.Shutdown(ctx); err != nil {
		firstErr = err
	}

	// gRPC: prefer graceful stop bounded by the context, else hard stop.
	stopped := make(chan struct{})
	go func() {
		s.grpcSrv.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-ctx.Done():
		s.grpcSrv.Stop() // force-close active conns past the deadline
		<-stopped
		if firstErr == nil {
			firstErr = ctx.Err()
		}
	}
	return firstErr
}

// portErr formats a bind failure with an actionable DESIGN §6 hint. The single
// "wlog: error:" prefix is added once by main (F4), so this returns a plain
// message.
func portErr(name, addr string, port int, err error) error {
	return fmt.Errorf("cannot bind %s on %s: %w\n  hint: another process may be using port %d; pass a different --otlp-grpc-port / --otlp-http-port or stop the conflicting process", name, addr, err, port)
}

// isLoopbackHost reports whether host (an IP literal or "localhost") is loopback.
func isLoopbackHost(host string) bool {
	if host == "localhost" || host == "" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
