// Package integration_test assembles the full wlog stack (store + parser +
// ingest pipeline + live bus + OTLP receiver + JSON API) and drives it with the
// synthetic Claude Code telemetry produced by the replay harness (PLAN §14
// integration/E2E). The data is injected through the REAL receiver over gRPC and
// HTTP (not a fake) and the assertions read back the REAL API responses, so the
// whole data path is exercised end to end.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/openwong2kim/wlog/internal/api"
	"github.com/openwong2kim/wlog/internal/config"
	"github.com/openwong2kim/wlog/internal/ingest"
	"github.com/openwong2kim/wlog/internal/otel"
	"github.com/openwong2kim/wlog/internal/receiver"
	"github.com/openwong2kim/wlog/internal/store"
	"github.com/openwong2kim/wlog/tools/replay/synth"

	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// stack is a fully-wired wlog server bound to ephemeral ports plus the httptest
// API server. Close tears everything down in the production order.
type stack struct {
	cfg      *config.Config
	store    *store.Store
	pipeline *ingest.Pipeline
	recv     *receiver.Server
	apiSrv   *httptest.Server
	cancel   context.CancelFunc
}

// newStack builds a real stack on auto-selected OTLP ports backed by an
// in-memory store, with the given pipeline queue size.
func newStack(t *testing.T, queueSize int) *stack {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	cfg := config.Default()
	cfg.Memory = true
	cfg.Listen = "127.0.0.1"
	cfg.OTLPGRPCPort = 0 // ephemeral
	cfg.OTLPHTTPPort = 0 // ephemeral
	cfg.UIPort = 0

	st, err := store.Open("", true, 5000)
	if err != nil {
		cancel()
		t.Fatalf("store.Open: %v", err)
	}

	parser := otel.NewParser(otel.Options{})
	bus := ingest.NewBus(256)
	pipeline := ingest.New(parser, st, bus, queueSize)
	pipeline.Start(ctx)

	recv := receiver.New(cfg, pipeline)
	if err := recv.Start(ctx); err != nil {
		_ = st.Close()
		cancel()
		t.Fatalf("receiver.Start: %v", err)
	}

	apiSrv := httptest.NewServer(api.New(cfg, st.ReadDB(), bus, pipeline.Counters, ":memory:").Handler())

	return &stack{
		cfg:      cfg,
		store:    st,
		pipeline: pipeline,
		recv:     recv,
		apiSrv:   apiSrv,
		cancel:   cancel,
	}
}

func (s *stack) Close() {
	s.apiSrv.Close()
	_ = s.recv.Shutdown(context.Background())
	_ = s.pipeline.Shutdown(context.Background())
	_ = s.store.Close()
	s.cancel()
}

// grpcConn dials the receiver's actual gRPC address.
func (s *stack) grpcConn(t *testing.T) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(s.recv.GRPCAddr(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial grpc: %v", err)
	}
	return conn
}

// injectGRPC sends the whole bundle over gRPC, retrying on backpressure so no
// data is lost (the receiver's retryable contract). It then waits for the
// pipeline to commit everything.
func (s *stack) injectGRPC(t *testing.T, b synth.Bundle) {
	t.Helper()
	conn := s.grpcConn(t)
	defer conn.Close()
	mc := collectormetricspb.NewMetricsServiceClient(conn)
	lc := collectorlogspb.NewLogsServiceClient(conn)
	tc := collectortracepb.NewTraceServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, r := range b.Metrics {
		retryGRPC(t, ctx, func() error { _, e := mc.Export(ctx, r); return e })
	}
	for _, r := range b.Logs {
		retryGRPC(t, ctx, func() error { _, e := lc.Export(ctx, r); return e })
	}
	for _, r := range b.Traces {
		retryGRPC(t, ctx, func() error { _, e := tc.Export(ctx, r); return e })
	}
	s.waitAccepted(t, expectedRequests(b))
}

// retryGRPC retries fn while it returns codes.Unavailable (retryable
// backpressure), failing the test on any other error.
func retryGRPC(t *testing.T, ctx context.Context, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	backoff := 20 * time.Millisecond
	for {
		err := fn()
		if err == nil {
			return
		}
		st, _ := status.FromError(err)
		if st.Code().String() != "Unavailable" || time.Now().After(deadline) {
			t.Fatalf("grpc export: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("grpc export: %v", ctx.Err())
		case <-time.After(backoff):
		}
		if backoff < 400*time.Millisecond {
			backoff *= 2
		}
	}
}

// expectedRequests is the number of OTLP requests the bundle produces (one per
// signal per session).
func expectedRequests(b synth.Bundle) int64 {
	return int64(len(b.Metrics) + len(b.Logs) + len(b.Traces))
}

// waitAccepted blocks until the pipeline has accepted at least n requests (or
// fails after a timeout). Accepted increments once per successfully written
// batch, so it equals the number of requests once the queue is drained.
func (s *stack) waitAccepted(t *testing.T, n int64) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if s.pipeline.Counters().Accepted >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d accepted (have %d)", n, s.pipeline.Counters().Accepted)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// --- HTTP API helpers ------------------------------------------------------

func (s *stack) getJSON(t *testing.T, path string, out any) {
	t.Helper()
	resp, err := http.Get(s.apiSrv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

// --- API response shapes (subset of PLAN §10) ------------------------------

type tokensJSON struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
}

type sessionRow struct {
	ID          string     `json:"id"`
	CostUSD     float64    `json:"cost_usd"`
	Tokens      tokensJSON `json:"tokens"`
	ToolAccept  int64      `json:"tool_accept"`
	ToolReject  int64      `json:"tool_reject"`
	ModelSet    string     `json:"model_set"`
	TokenSource string     `json:"token_source"`
	HasEvents   bool       `json:"has_events"`
}

type sessionsResponse struct {
	Sessions []sessionRow `json:"sessions"`
	Total    int64        `json:"total"`
}

type costByModel struct {
	Model   string  `json:"model"`
	CostUSD float64 `json:"cost_usd"`
}

type costResponse struct {
	Series      []costPoint   `json:"series"`
	ByModel     []costByModel `json:"by_model"`
	TokenSource string        `json:"token_source"`
}

type costPoint struct {
	TS            int64   `json:"ts"`
	CostUSD       float64 `json:"cost_usd"`
	Input         int64   `json:"input"`
	Output        int64   `json:"output"`
	CacheRead     int64   `json:"cache_read"`
	CacheCreation int64   `json:"cache_creation"`
}

type toolsResponse struct {
	AutoApproved int64 `json:"auto_approved"`
	Accept       int64 `json:"accept"`
	Reject       int64 `json:"reject"`
	BySource     []struct {
		Source string `json:"source"`
		Accept int64  `json:"accept"`
		Reject int64  `json:"reject"`
	} `json:"by_source"`
	Hotspots []struct {
		ToolName string `json:"tool_name"`
		Reject   int64  `json:"reject"`
	} `json:"hotspots"`
}

type timelineResponse struct {
	Events []map[string]any `json:"events"`
}

type healthResponse struct {
	Ingest struct {
		Received int64 `json:"received"`
		Accepted int64 `json:"accepted"`
	} `json:"ingest"`
}

const floatEps = 1e-9

func approxEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= floatEps
}

// findSession returns the row for id (or fails).
func findSession(t *testing.T, sessions []sessionRow, id string) sessionRow {
	t.Helper()
	for _, s := range sessions {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("session %q not found in response", id)
	return sessionRow{}
}

// --- Tests -----------------------------------------------------------------

// TestSessionsEventBased verifies /api/sessions reports the cost/tokens/decision
// counts that the api_request and tool_decision EVENTS encode (PLAN §10, §6.5).
func TestSessionsEventBased(t *testing.T) {
	st := newStack(t, 1024)
	defer st.Close()

	opts := synth.DefaultOptions()
	opts.Sessions = 3
	b := synth.Generate(opts)
	st.injectGRPC(t, b)

	var resp sessionsResponse
	st.getJSON(t, "/api/sessions?limit=100", &resp)

	if resp.Total != int64(opts.Sessions) {
		t.Fatalf("total sessions = %d, want %d", resp.Total, opts.Sessions)
	}
	for _, want := range b.Totals {
		got := findSession(t, resp.Sessions, want.SessionID)
		if !approxEqual(got.CostUSD, want.CostUSD) {
			t.Errorf("%s cost = %v, want %v", want.SessionID, got.CostUSD, want.CostUSD)
		}
		if got.Tokens.Input != want.Input || got.Tokens.Output != want.Output ||
			got.Tokens.CacheRead != want.CacheRead || got.Tokens.CacheCreation != want.CacheCreation {
			t.Errorf("%s tokens = %+v, want in=%d out=%d cr=%d cc=%d",
				want.SessionID, got.Tokens, want.Input, want.Output, want.CacheRead, want.CacheCreation)
		}
		if got.ToolAccept != want.ToolAccept || got.ToolReject != want.ToolReject {
			t.Errorf("%s decisions accept/reject = %d/%d, want %d/%d",
				want.SessionID, got.ToolAccept, got.ToolReject, want.ToolAccept, want.ToolReject)
		}
		if got.TokenSource != "events" {
			t.Errorf("%s token_source = %q, want events", want.SessionID, got.TokenSource)
		}
		if !got.HasEvents {
			t.Errorf("%s has_events = false, want true", want.SessionID)
		}
	}
}

// TestNoDoubleCounting is the core dogfood invariant (PLAN §6.5, §14): the SAME
// session carries api_request token/cost EVENTS and token.usage/cost.usage
// METRICS. The API must report the EVENT totals only — never events + metrics.
func TestNoDoubleCounting(t *testing.T) {
	st := newStack(t, 1024)
	defer st.Close()

	b := synth.Generate(synth.DefaultOptions()) // 1 session, both axes present
	st.injectGRPC(t, b)

	want := b.Totals[0]
	// Sanity: the metric token total is non-zero and distinct, so a naive sum
	// would visibly differ from the event total — making this assertion real.
	if want.MetricTokenTotal == 0 {
		t.Fatal("test precondition: metric token total should be non-zero")
	}
	if want.Input+want.Output+want.CacheRead == want.MetricTokenTotal {
		t.Fatal("test precondition: event and metric totals must differ to detect summing")
	}

	var resp sessionsResponse
	st.getJSON(t, "/api/sessions?limit=10", &resp)
	got := findSession(t, resp.Sessions, want.SessionID)

	// Tokens/cost must equal the EVENT totals, not events+metrics.
	if got.Tokens.Input != want.Input {
		t.Errorf("input tokens = %d, want %d (event axis only; metrics must not be summed)", got.Tokens.Input, want.Input)
	}
	if got.Tokens.Output != want.Output {
		t.Errorf("output tokens = %d, want %d", got.Tokens.Output, want.Output)
	}
	if !approxEqual(got.CostUSD, want.CostUSD) {
		t.Errorf("cost = %v, want %v (event axis only)", got.CostUSD, want.CostUSD)
	}
	if got.TokenSource != "events" {
		t.Errorf("token_source = %q, want events", got.TokenSource)
	}

	// /api/cost must also be event-sourced (no summing).
	var cost costResponse
	st.getJSON(t, "/api/cost?bucket=day&session="+want.SessionID, &cost)
	if cost.TokenSource != "events" {
		t.Errorf("cost token_source = %q, want events", cost.TokenSource)
	}
	var seriesCost float64
	var seriesIn int64
	for _, p := range cost.Series {
		seriesCost += p.CostUSD
		seriesIn += p.Input
	}
	if !approxEqual(seriesCost, want.CostUSD) {
		t.Errorf("cost series sum = %v, want %v (event axis)", seriesCost, want.CostUSD)
	}
	if seriesIn != want.Input {
		t.Errorf("cost series input = %d, want %d (event axis)", seriesIn, want.Input)
	}

	// active_time, which comes from METRICS only, must be present and equal the
	// cold-baseline delta sum (PLAN §7 first-observation baseline → delta 0).
	activeSum := st.activeTimeDeltaSum(t, want.SessionID)
	if !approxEqual(activeSum, want.ActiveTimeDeltaSum) {
		t.Errorf("active_time delta sum = %v, want %v (metrics axis)", activeSum, want.ActiveTimeDeltaSum)
	}
	if activeSum == 0 {
		t.Error("active_time should be non-zero (metrics axis must still surface)")
	}
}

// activeTimeDeltaSum reads the summed active_time.total metric deltas for a
// session directly from the store read pool (the metrics axis of truth).
func (s *stack) activeTimeDeltaSum(t *testing.T, sessionID string) float64 {
	t.Helper()
	var sum float64
	err := s.store.ReadDB().QueryRow(`
		SELECT COALESCE(SUM(value_delta), 0) FROM metric_points
		WHERE session_id = ? AND name IN ('active_time.total','claude_code.active_time.total')`,
		sessionID).Scan(&sum)
	if err != nil {
		t.Fatalf("query active_time: %v", err)
	}
	return sum
}

// TestCumulativeDeltaEquivalence injects the same logical data twice — once as
// CUMULATIVE metrics, once as DELTA — into two stacks and asserts the aggregated
// metric results are identical (delta-normalization correctness, PLAN §7).
func TestCumulativeDeltaEquivalence(t *testing.T) {
	run := func(temp synth.Temporality) (float64, float64, float64) {
		st := newStack(t, 1024)
		defer st.Close()
		opts := synth.DefaultOptions()
		opts.Temporality = temp
		opts.IncludeLogs = false // isolate the metric axis (no event tokens)
		opts.IncludeTraces = false
		b := synth.Generate(opts)
		st.injectGRPC(t, b)
		sid := b.Totals[0].SessionID
		active := st.activeTimeDeltaSum(t, sid)
		var cost, tokens float64
		err := st.store.ReadDB().QueryRow(`
			SELECT
			  COALESCE(SUM(CASE WHEN name IN ('cost.usage','claude_code.cost.usage') THEN value_delta ELSE 0 END),0),
			  COALESCE(SUM(CASE WHEN name IN ('token.usage','claude_code.token.usage') THEN value_delta ELSE 0 END),0)
			FROM metric_points WHERE session_id = ?`, sid).Scan(&cost, &tokens)
		if err != nil {
			t.Fatalf("query metrics: %v", err)
		}
		return active, cost, tokens
	}

	cActive, cCost, cTokens := run(synth.Cumulative)
	dActive, dCost, dTokens := run(synth.Delta)

	if !approxEqual(cActive, dActive) {
		t.Errorf("active_time: cumulative=%v delta=%v (must be equal)", cActive, dActive)
	}
	if !approxEqual(cCost, dCost) {
		t.Errorf("cost: cumulative=%v delta=%v (must be equal)", cCost, dCost)
	}
	if !approxEqual(cTokens, dTokens) {
		t.Errorf("tokens: cumulative=%v delta=%v (must be equal)", cTokens, dTokens)
	}
	if cActive == 0 || cCost == 0 || cTokens == 0 {
		t.Errorf("equivalence precondition: metric sums should be non-zero (active=%v cost=%v tokens=%v)", cActive, cCost, cTokens)
	}
}

// TestIdempotency injects the SAME bundle twice over gRPC and asserts the
// aggregates (and row counts) are unchanged — event dedup_key + DELTA UNIQUE
// constraints (PLAN §7, §8, §14).
func TestIdempotency(t *testing.T) {
	st := newStack(t, 1024)
	defer st.Close()

	b := synth.Generate(synth.DefaultOptions())
	st.injectGRPC(t, b)

	var first sessionsResponse
	st.getJSON(t, "/api/sessions?limit=10", &first)
	firstRows := st.rowCounts(t)

	// Inject the exact same data again.
	st.injectGRPC(t, b)

	var second sessionsResponse
	st.getJSON(t, "/api/sessions?limit=10", &second)
	secondRows := st.rowCounts(t)

	if first.Total != second.Total {
		t.Errorf("session total changed on re-inject: %d -> %d", first.Total, second.Total)
	}
	f := findSession(t, first.Sessions, b.Totals[0].SessionID)
	s := findSession(t, second.Sessions, b.Totals[0].SessionID)
	if !approxEqual(f.CostUSD, s.CostUSD) || f.Tokens != s.Tokens {
		t.Errorf("aggregates changed on re-inject: cost %v->%v tokens %+v->%+v",
			f.CostUSD, s.CostUSD, f.Tokens, s.Tokens)
	}
	if f.ToolAccept != s.ToolAccept || f.ToolReject != s.ToolReject {
		t.Errorf("decision counts changed on re-inject: %d/%d -> %d/%d",
			f.ToolAccept, f.ToolReject, s.ToolAccept, s.ToolReject)
	}
	for table, n1 := range firstRows {
		if secondRows[table] != n1 {
			t.Errorf("row count for %s changed on re-inject: %d -> %d", table, n1, secondRows[table])
		}
	}
}

// rowCounts returns the per-table row counts (idempotency evidence).
func (s *stack) rowCounts(t *testing.T) map[string]int64 {
	t.Helper()
	tables := []string{"api_requests", "tool_decisions", "tool_results", "events", "metric_points", "spans"}
	out := make(map[string]int64, len(tables))
	for _, tbl := range tables {
		var n int64
		if err := s.store.ReadDB().QueryRow("SELECT COUNT(*) FROM " + tbl).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		out[tbl] = n
	}
	return out
}

// TestTools verifies /api/tools auto_approved, accept/reject totals, and reject
// hotspots against the generator's ground truth (PLAN §10).
func TestTools(t *testing.T) {
	st := newStack(t, 1024)
	defer st.Close()

	b := synth.Generate(synth.DefaultOptions())
	st.injectGRPC(t, b)
	want := b.Totals[0]

	var resp toolsResponse
	st.getJSON(t, "/api/tools?session="+want.SessionID, &resp)

	if resp.Accept != want.ToolAccept {
		t.Errorf("accept = %d, want %d", resp.Accept, want.ToolAccept)
	}
	if resp.Reject != want.ToolReject {
		t.Errorf("reject = %d, want %d", resp.Reject, want.ToolReject)
	}
	if resp.AutoApproved != want.AutoApproved {
		t.Errorf("auto_approved = %d, want %d", resp.AutoApproved, want.AutoApproved)
	}
	// Bash is rejected once in the synthetic data → must appear in hotspots.
	foundBash := false
	for _, h := range resp.Hotspots {
		if h.ToolName == "Bash" && h.Reject > 0 {
			foundBash = true
		}
	}
	if !foundBash {
		t.Errorf("expected Bash reject hotspot, got %+v", resp.Hotspots)
	}
}

// TestTimelineOrder verifies /api/sessions/:id/timeline is ts-ascending and
// contains the expected event kinds (PLAN §10).
func TestTimelineOrder(t *testing.T) {
	st := newStack(t, 1024)
	defer st.Close()

	b := synth.Generate(synth.DefaultOptions())
	st.injectGRPC(t, b)
	sid := b.Totals[0].SessionID

	var resp timelineResponse
	st.getJSON(t, "/api/sessions/"+sid+"/timeline", &resp)

	if len(resp.Events) == 0 {
		t.Fatal("timeline empty")
	}
	var lastTS float64 = -1
	kinds := map[string]int{}
	for i, ev := range resp.Events {
		ts, _ := ev["ts"].(float64)
		if ts < lastTS {
			t.Fatalf("timeline not ts-ascending at index %d: %v < %v", i, ts, lastTS)
		}
		lastTS = ts
		if k, ok := ev["kind"].(string); ok {
			kinds[k]++
		}
	}
	for _, want := range []string{"prompt", "api_request", "tool_decision", "tool_result"} {
		if kinds[want] == 0 {
			t.Errorf("timeline missing kind %q (got kinds %v)", want, kinds)
		}
	}
	// One api_error is generated.
	if kinds["api_error"] == 0 {
		t.Errorf("timeline missing api_error (got kinds %v)", kinds)
	}
}

// TestHTTPInjection mirrors TestSessionsEventBased but injects over OTLP/HTTP
// (x-protobuf) instead of gRPC, proving both transports feed the same pipeline.
func TestHTTPInjection(t *testing.T) {
	st := newStack(t, 1024)
	defer st.Close()

	b := synth.Generate(synth.DefaultOptions())
	st.injectHTTP(t, b)

	want := b.Totals[0]
	var resp sessionsResponse
	st.getJSON(t, "/api/sessions?limit=10", &resp)
	got := findSession(t, resp.Sessions, want.SessionID)
	if got.Tokens.Input != want.Input || !approxEqual(got.CostUSD, want.CostUSD) {
		t.Errorf("HTTP-injected session = tokens.in %d cost %v, want %d %v",
			got.Tokens.Input, got.CostUSD, want.Input, want.CostUSD)
	}
	if got.TokenSource != "events" {
		t.Errorf("token_source = %q, want events", got.TokenSource)
	}
}

// TestBackpressureNoLoss floods a tiny-queue stack and asserts that, despite
// some retryable rejections, every request eventually lands (client retry → no
// data loss, PLAN §5). With a queue of size 1 the receiver must signal
// Unavailable under load; the retry loop guarantees delivery.
func TestBackpressureNoLoss(t *testing.T) {
	st := newStack(t, 1) // minimal queue to force backpressure
	defer st.Close()

	opts := synth.DefaultOptions()
	opts.Sessions = 5
	b := synth.Generate(opts)

	st.injectGRPC(t, b) // injectGRPC retries on Unavailable

	// All sessions must be present despite backpressure.
	var resp sessionsResponse
	st.getJSON(t, "/api/sessions?limit=100", &resp)
	if resp.Total != int64(opts.Sessions) {
		t.Fatalf("after backpressure flood: total = %d, want %d (data lost)", resp.Total, opts.Sessions)
	}
	for _, want := range b.Totals {
		got := findSession(t, resp.Sessions, want.SessionID)
		if got.Tokens.Input != want.Input {
			t.Errorf("%s input tokens = %d, want %d (data lost under backpressure)",
				want.SessionID, got.Tokens.Input, want.Input)
		}
	}

	// Evidence that backpressure was actually exercised: received > accepted is
	// not guaranteed (retries inflate received), but retry_signaled should be > 0
	// under a size-1 queue with 15 requests. We assert received >= accepted as a
	// floor and that the data is intact (above).
	var health healthResponse
	st.getJSON(t, "/api/health", &health)
	if health.Ingest.Received < health.Ingest.Accepted {
		t.Errorf("received (%d) < accepted (%d): impossible", health.Ingest.Received, health.Ingest.Accepted)
	}
}

// injectHTTP posts the bundle to the receiver's OTLP/HTTP endpoints as
// x-protobuf, then waits for the pipeline to commit.
func (s *stack) injectHTTP(t *testing.T, b synth.Bundle) {
	t.Helper()
	base := "http://" + s.recv.HTTPAddr()
	for _, r := range b.Metrics {
		s.postProto(t, base+"/v1/metrics", r)
	}
	for _, r := range b.Logs {
		s.postProto(t, base+"/v1/logs", r)
	}
	for _, r := range b.Traces {
		s.postProto(t, base+"/v1/traces", r)
	}
	s.waitAccepted(t, expectedRequests(b))
}

// postProto marshals msg to x-protobuf and posts it, retrying on 503.
func (s *stack) postProto(t *testing.T, url string, msg proto.Message) {
	t.Helper()
	body, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	backoff := 20 * time.Millisecond
	for {
		req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/x-protobuf")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", url, err)
		}
		code := resp.StatusCode
		resp.Body.Close()
		if code == http.StatusOK {
			return
		}
		if code != http.StatusServiceUnavailable || time.Now().After(deadline) {
			t.Fatalf("POST %s: status %d", url, code)
		}
		time.Sleep(backoff)
		if backoff < 400*time.Millisecond {
			backoff *= 2
		}
	}
}
