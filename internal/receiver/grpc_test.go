package receiver

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openwong2kim/wlog/internal/ingest"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// fakeSubmitter records submitted kinds and can be configured to return ErrBusy.
type fakeSubmitter struct {
	count   atomic.Int64
	busy    atomic.Bool
	metrics atomic.Int64
	logs    atomic.Int64
	traces  atomic.Int64
}

func (f *fakeSubmitter) Submit(kind ingest.RequestKind, _ any) error {
	if f.busy.Load() {
		return ingest.ErrBusy
	}
	f.count.Add(1)
	switch kind {
	case ingest.KindMetrics:
		f.metrics.Add(1)
	case ingest.KindLogs:
		f.logs.Add(1)
	case ingest.KindTraces:
		f.traces.Add(1)
	}
	return nil
}

// startBufGRPC serves the three OTLP services over an in-memory bufconn and
// returns a connected client conn plus a cleanup func.
func startBufGRPC(t *testing.T, sub submitter, maxRecv int64) (*grpc.ClientConn, func()) {
	t.Helper()
	return startBufGRPCAuth(t, sub, maxRecv, "", nil)
}

// startBufGRPCAuth is like startBufGRPC but registers the server with authToken
// and dials with the supplied per-RPC credentials (nil = none). It lets the F1
// auth tests vary the token presented by the client.
func startBufGRPCAuth(t *testing.T, sub submitter, maxRecv int64, authToken string, creds credentials.PerRPCCredentials) (*grpc.ClientConn, func()) {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	srv := registerGRPC(sub, maxRecv, authToken)
	go func() { _ = srv.Serve(lis) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dialOpts := []grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
	if creds != nil {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(creds))
	}
	conn, err := grpc.DialContext(ctx, "bufnet", dialOpts...)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	cleanup := func() {
		_ = conn.Close()
		srv.Stop()
		_ = lis.Close()
	}
	return conn, cleanup
}

// bearerCreds is a grpc.PerRPCCredentials that sends "authorization: Bearer
// <token>" metadata. It runs over an insecure (bufconn) transport in tests.
type bearerCreds struct{ token string }

func (b bearerCreds) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}
func (b bearerCreds) RequireTransportSecurity() bool { return false }

func sampleMetricsReq() *collectormetricspb.ExportMetricsServiceRequest {
	return &collectormetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Metrics: []*metricspb.Metric{{Name: "claude_code.cost.usage"}},
			}},
		}},
	}
}

func sampleLogsReq() *collectorlogspb.ExportLogsServiceRequest {
	return &collectorlogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
				EventName:  "claude_code.api_request",
				Attributes: []*commonpb.KeyValue{{Key: "session.id", Value: strVal("s1")}},
			}}}},
		}},
	}
}

func sampleTracesReq() *collectortracepb.ExportTraceServiceRequest {
	return &collectortracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
				TraceId: []byte("0123456789abcdef"),
				SpanId:  []byte("01234567"),
				Name:    "op",
			}}}},
		}},
	}
}

func strVal(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}

func TestGRPCExportThreeServices(t *testing.T) {
	sub := &fakeSubmitter{}
	conn, cleanup := startBufGRPC(t, sub, 16*1024*1024)
	defer cleanup()

	ctx := context.Background()

	mc := collectormetricspb.NewMetricsServiceClient(conn)
	if _, err := mc.Export(ctx, sampleMetricsReq()); err != nil {
		t.Fatalf("metrics export: %v", err)
	}
	lc := collectorlogspb.NewLogsServiceClient(conn)
	if _, err := lc.Export(ctx, sampleLogsReq()); err != nil {
		t.Fatalf("logs export: %v", err)
	}
	tc := collectortracepb.NewTraceServiceClient(conn)
	if _, err := tc.Export(ctx, sampleTracesReq()); err != nil {
		t.Fatalf("traces export: %v", err)
	}

	if sub.metrics.Load() != 1 || sub.logs.Load() != 1 || sub.traces.Load() != 1 {
		t.Fatalf("unexpected submit counts: m=%d l=%d t=%d",
			sub.metrics.Load(), sub.logs.Load(), sub.traces.Load())
	}
}

func TestGRPCBackpressureUnavailable(t *testing.T) {
	sub := &fakeSubmitter{}
	sub.busy.Store(true)
	conn, cleanup := startBufGRPC(t, sub, 16*1024*1024)
	defer cleanup()

	mc := collectormetricspb.NewMetricsServiceClient(conn)
	_, err := mc.Export(context.Background(), sampleMetricsReq())
	if err == nil {
		t.Fatalf("expected Unavailable error on full queue")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unavailable {
		t.Fatalf("expected codes.Unavailable, got %v", err)
	}
	// RetryInfo detail must be present so the client retries.
	foundRetry := false
	for _, d := range st.Details() {
		if _, ok := d.(*errdetails.RetryInfo); ok {
			foundRetry = true
		}
	}
	if !foundRetry {
		t.Fatalf("expected RetryInfo detail on Unavailable status")
	}
}

func TestGRPCGzipCompression(t *testing.T) {
	sub := &fakeSubmitter{}
	conn, cleanup := startBufGRPC(t, sub, 16*1024*1024)
	defer cleanup()

	// Request gzip on the call; the registered gzip codec must decode it.
	mc := collectormetricspb.NewMetricsServiceClient(conn)
	_, err := mc.Export(context.Background(), sampleMetricsReq(),
		grpc.UseCompressor("gzip"))
	if err != nil {
		t.Fatalf("gzip metrics export: %v", err)
	}
	if sub.metrics.Load() != 1 {
		t.Fatalf("gzip export not submitted: %d", sub.metrics.Load())
	}
}

// TestGRPCAuthRejectsNonLoopbackWithoutToken is the F1 regression for gRPC: with
// an auth token configured, a non-loopback peer (bufconn) that sends no token
// must be rejected with codes.Unauthenticated.
func TestGRPCAuthRejectsNonLoopbackWithoutToken(t *testing.T) {
	sub := &fakeSubmitter{}
	conn, cleanup := startBufGRPCAuth(t, sub, 16*1024*1024, "s3cr3t", nil)
	defer cleanup()

	mc := collectormetricspb.NewMetricsServiceClient(conn)
	_, err := mc.Export(context.Background(), sampleMetricsReq())
	if err == nil {
		t.Fatal("expected Unauthenticated without a token")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unauthenticated {
		t.Fatalf("expected codes.Unauthenticated, got %v", err)
	}
	if sub.metrics.Load() != 0 {
		t.Fatalf("unauthenticated request must not be submitted: %d", sub.metrics.Load())
	}
}

// TestGRPCAuthRejectsWrongToken verifies a mismatched token is rejected.
func TestGRPCAuthRejectsWrongToken(t *testing.T) {
	sub := &fakeSubmitter{}
	conn, cleanup := startBufGRPCAuth(t, sub, 16*1024*1024, "s3cr3t", bearerCreds{token: "nope"})
	defer cleanup()

	mc := collectormetricspb.NewMetricsServiceClient(conn)
	_, err := mc.Export(context.Background(), sampleMetricsReq())
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unauthenticated {
		t.Fatalf("expected codes.Unauthenticated for wrong token, got %v", err)
	}
	if sub.metrics.Load() != 0 {
		t.Fatalf("wrong-token request must not be submitted: %d", sub.metrics.Load())
	}
}

// TestGRPCAuthAcceptsValidToken verifies a matching token passes the interceptor.
func TestGRPCAuthAcceptsValidToken(t *testing.T) {
	sub := &fakeSubmitter{}
	conn, cleanup := startBufGRPCAuth(t, sub, 16*1024*1024, "s3cr3t", bearerCreds{token: "s3cr3t"})
	defer cleanup()

	mc := collectormetricspb.NewMetricsServiceClient(conn)
	if _, err := mc.Export(context.Background(), sampleMetricsReq()); err != nil {
		t.Fatalf("valid token export: %v", err)
	}
	if sub.metrics.Load() != 1 {
		t.Fatalf("valid-token request not submitted: %d", sub.metrics.Load())
	}
}

// TestGRPCAuthGuardUnit exercises grpcAuthorized's loopback bypass directly,
// since bufconn peers are never loopback.
func TestGRPCAuthGuardUnit(t *testing.T) {
	// A loopback peer is exempt even with no metadata.
	loopCtx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5000},
	})
	if !grpcAuthorized(loopCtx, "s3cr3t") {
		t.Error("loopback peer should be exempt from auth")
	}
	// A non-loopback peer with the right metadata token passes.
	remoteCtx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 5), Port: 5000},
	})
	okCtx := metadata.NewIncomingContext(remoteCtx,
		metadata.Pairs("authorization", "Bearer s3cr3t"))
	if !grpcAuthorized(okCtx, "s3cr3t") {
		t.Error("non-loopback peer with valid token should pass")
	}
	// Non-loopback, no metadata: rejected.
	if grpcAuthorized(remoteCtx, "s3cr3t") {
		t.Error("non-loopback peer without token should be rejected")
	}
}

func TestMapSubmitErr(t *testing.T) {
	if mapSubmitErr(nil) != nil {
		t.Fatalf("nil error should map to nil")
	}
	st, _ := status.FromError(mapSubmitErr(ingest.ErrBusy))
	if st.Code() != codes.Unavailable {
		t.Fatalf("ErrBusy should map to Unavailable, got %v", st.Code())
	}
	st2, _ := status.FromError(mapSubmitErr(errors.New("boom")))
	if st2.Code() != codes.Unavailable {
		t.Fatalf("generic error should map to Unavailable, got %v", st2.Code())
	}
}
