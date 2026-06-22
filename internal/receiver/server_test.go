package receiver

import (
	"context"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openwong2kim/wlog/internal/config"
	"github.com/openwong2kim/wlog/internal/ingest"
	"github.com/openwong2kim/wlog/internal/model"
	"github.com/openwong2kim/wlog/internal/otel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

// countingWriter records how many batches it wrote.
type countingWriter struct {
	n atomic.Int64
}

func (c *countingWriter) WriteBatch(_ context.Context, _ model.Batch) error {
	c.n.Add(1)
	return nil
}

// freePorts returns two free loopback TCP ports for the gRPC and HTTP listeners.
func freePorts(t *testing.T) (int, int) {
	t.Helper()
	a := freePort(t)
	b := freePort(t)
	for b == a {
		b = freePort(t)
	}
	return a, b
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// portOf extracts the numeric port from a host:port address string.
func portOf(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}

func startServer(t *testing.T, w model.Writer) (*Server, *ingest.Pipeline) {
	t.Helper()
	grpcPort, httpPort := freePorts(t)
	cfg := config.Default()
	cfg.OTLPGRPCPort = grpcPort
	cfg.OTLPHTTPPort = httpPort
	cfg.Listen = "127.0.0.1"

	bus := ingest.NewBus(64)
	p := ingest.New(otel.NewParser(otel.Options{}), w, bus, 1024)
	p.Start(context.Background())

	srv := New(cfg, p)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("server start: %v", err)
	}
	return srv, p
}

func dialServer(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	// grpc.NewClient connects lazily; the Export RPCs that follow establish the
	// connection (the server is already listening before we dial here).
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	return conn
}

func TestServerStartShutdownRoundTrip(t *testing.T) {
	w := &countingWriter{}
	srv, p := startServer(t, w)
	defer func() {
		_ = srv.Shutdown(context.Background())
		_ = p.Shutdown(context.Background())
	}()

	conn := dialServer(t, srv.GRPCAddr())
	defer conn.Close()

	mc := collectormetricspb.NewMetricsServiceClient(conn)
	if _, err := mc.Export(context.Background(), sampleMetricsReq()); err != nil {
		t.Fatalf("metrics export: %v", err)
	}
	lc := collectorlogspb.NewLogsServiceClient(conn)
	if _, err := lc.Export(context.Background(), sampleLogsReq()); err != nil {
		t.Fatalf("logs export: %v", err)
	}
	tc := collectortracepb.NewTraceServiceClient(conn)
	if _, err := tc.Export(context.Background(), sampleTracesReq()); err != nil {
		t.Fatalf("traces export: %v", err)
	}

	// Drain through pipeline shutdown to guarantee all batches are written.
	_ = srv.Shutdown(context.Background())
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("pipeline shutdown: %v", err)
	}
	if w.n.Load() != 3 {
		t.Fatalf("wrote %d batches, want 3", w.n.Load())
	}
}

func TestServerPortConflict(t *testing.T) {
	w := &countingWriter{}
	srv, p := startServer(t, w)
	defer func() {
		_ = srv.Shutdown(context.Background())
		_ = p.Shutdown(context.Background())
	}()

	// A second server on the same ports must fail to bind with a hint error.
	cfg := config.Default()
	cfg.Listen = "127.0.0.1"
	// Reuse the first server's bound ports.
	_, _ = srv.GRPCAddr(), srv.HTTPAddr()
	cfg.OTLPGRPCPort = portOf(srv.GRPCAddr())
	cfg.OTLPHTTPPort = portOf(srv.HTTPAddr())

	bus := ingest.NewBus(8)
	p2 := ingest.New(otel.NewParser(otel.Options{}), w, bus, 16)
	srv2 := New(cfg, p2)
	err := srv2.Start(context.Background())
	if err == nil {
		_ = srv2.Shutdown(context.Background())
		t.Fatalf("expected bind conflict error")
	}
}

func TestServerConcurrentExportNoLoss(t *testing.T) {
	w := &countingWriter{}
	srv, p := startServer(t, w)
	defer func() {
		_ = srv.Shutdown(context.Background())
		_ = p.Shutdown(context.Background())
	}()

	conn := dialServer(t, srv.GRPCAddr())
	defer conn.Close()
	mc := collectormetricspb.NewMetricsServiceClient(conn)

	const goroutines = 12
	const perG = 50
	var wg sync.WaitGroup
	var sent atomic.Int64
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				// Retry on Unavailable (backpressure), mirroring an OTLP client,
				// so no data is lost.
				for {
					_, err := mc.Export(context.Background(), sampleMetricsReq())
					if err == nil {
						sent.Add(1)
						break
					}
					// Any error here is the retryable Unavailable; brief backoff.
					time.Sleep(time.Millisecond)
				}
			}
		}()
	}
	wg.Wait()

	_ = srv.Shutdown(context.Background())
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("pipeline shutdown: %v", err)
	}
	want := int64(goroutines * perG)
	if sent.Load() != want {
		t.Fatalf("client sent %d, want %d", sent.Load(), want)
	}
	if w.n.Load() != want {
		t.Fatalf("wrote %d batches, want %d (no loss after retries)", w.n.Load(), want)
	}
}

var _ = ingest.ErrBusy // documents that backpressure is the retryable path above
