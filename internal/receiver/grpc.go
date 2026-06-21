package receiver

import (
	"context"
	"errors"
	"time"

	"github.com/openwong2kim/wlog/internal/ingest"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	_ "google.golang.org/grpc/encoding/gzip" // register the gzip compressor/decompressor
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

// retryAfter is the RetryInfo delay advertised on backpressure (gRPC). Short so
// a healthy queue recovers quickly; the client honours it as a hint.
const retryAfter = 200 * time.Millisecond

// submitter is the subset of *ingest.Pipeline the gRPC services depend on.
// Narrowing the dependency keeps the handlers trivially testable.
type submitter interface {
	Submit(kind ingest.RequestKind, req any) error
}

// busyStatus builds the retryable Unavailable status with a RetryInfo detail so
// the OTLP client retries instead of treating the data as accepted (REVIEWS B).
func busyStatus() error {
	st := status.New(codes.Unavailable, "ingest queue full; retry")
	if withDetails, err := st.WithDetails(&errdetails.RetryInfo{
		RetryDelay: durationpb.New(retryAfter),
	}); err == nil {
		return withDetails.Err()
	}
	return st.Err()
}

// mapSubmitErr converts an ingest.Submit error into the appropriate gRPC status.
// ErrBusy → Unavailable+RetryInfo (retryable). Any other error (e.g. pipeline
// stopped) → Unavailable without retry hint.
func mapSubmitErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ingest.ErrBusy):
		return busyStatus()
	default:
		return status.Error(codes.Unavailable, "ingest unavailable")
	}
}

// metricsService implements collector/metrics/v1 MetricsServiceServer.
type metricsService struct {
	collectormetricspb.UnimplementedMetricsServiceServer
	p submitter
}

func (s *metricsService) Export(_ context.Context, req *collectormetricspb.ExportMetricsServiceRequest) (*collectormetricspb.ExportMetricsServiceResponse, error) {
	if err := s.p.Submit(ingest.KindMetrics, req); err != nil {
		return nil, mapSubmitErr(err)
	}
	// Permanent rejects (parse failures) are surfaced as partial_success by the
	// pipeline's counters, not synchronously here: the request is accepted off
	// the wire and normalized asynchronously. A full ACK (empty partial_success)
	// is correct for the retryable model — anything unparseable is counted, not
	// re-requested. Return a bare success response.
	return &collectormetricspb.ExportMetricsServiceResponse{}, nil
}

// logsService implements collector/logs/v1 LogsServiceServer.
type logsService struct {
	collectorlogspb.UnimplementedLogsServiceServer
	p submitter
}

func (s *logsService) Export(_ context.Context, req *collectorlogspb.ExportLogsServiceRequest) (*collectorlogspb.ExportLogsServiceResponse, error) {
	if err := s.p.Submit(ingest.KindLogs, req); err != nil {
		return nil, mapSubmitErr(err)
	}
	return &collectorlogspb.ExportLogsServiceResponse{}, nil
}

// traceService implements collector/trace/v1 TraceServiceServer.
type traceService struct {
	collectortracepb.UnimplementedTraceServiceServer
	p submitter
}

func (s *traceService) Export(_ context.Context, req *collectortracepb.ExportTraceServiceRequest) (*collectortracepb.ExportTraceServiceResponse, error) {
	if err := s.p.Submit(ingest.KindTraces, req); err != nil {
		return nil, mapSubmitErr(err)
	}
	return &collectortracepb.ExportTraceServiceResponse{}, nil
}

// registerGRPC builds a *grpc.Server with the configured max receive size and
// registers all three OTLP collector services against the given submitter. When
// authToken is non-empty an auth UnaryInterceptor gates every RPC (F1).
func registerGRPC(p submitter, maxRecvBytes int64, authToken string) *grpc.Server {
	if maxRecvBytes <= 0 || maxRecvBytes > int64(^uint32(0)) {
		maxRecvBytes = 16 * 1024 * 1024
	}
	opts := []grpc.ServerOption{grpc.MaxRecvMsgSize(int(maxRecvBytes))}
	if authToken != "" {
		opts = append(opts, grpc.UnaryInterceptor(authUnaryInterceptor(authToken)))
	}
	srv := grpc.NewServer(opts...)
	collectormetricspb.RegisterMetricsServiceServer(srv, &metricsService{p: p})
	collectorlogspb.RegisterLogsServiceServer(srv, &logsService{p: p})
	collectortracepb.RegisterTraceServiceServer(srv, &traceService{p: p})
	return srv
}

// authUnaryInterceptor enforces the F1 bearer-token policy on gRPC: a
// non-loopback peer must send metadata "authorization: Bearer <token>" matching
// the configured token, else the RPC is rejected with codes.Unauthenticated.
// Loopback peers (local Claude Code) are exempt. authToken is guaranteed
// non-empty by the caller.
func authUnaryInterceptor(authToken string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !grpcAuthorized(ctx, authToken) {
			return nil, status.Error(codes.Unauthenticated, "missing or invalid auth token")
		}
		return handler(ctx, req)
	}
}

// grpcAuthorized reports whether the RPC in ctx satisfies the auth policy. A
// loopback peer is always allowed; otherwise the metadata bearer token must
// match. A peer that cannot be resolved fails closed (auth required).
func grpcAuthorized(ctx context.Context, authToken string) bool {
	if pr, ok := peer.FromContext(ctx); ok && pr.Addr != nil && peerIsLoopback(pr.Addr.String()) {
		return true
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	for _, v := range md.Get("authorization") {
		if tokenEqual(bearerToken(v), authToken) {
			return true
		}
	}
	return false
}
