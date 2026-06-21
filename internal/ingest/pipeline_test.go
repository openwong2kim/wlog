package ingest

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openwong2kim/wlog/internal/model"
	"github.com/openwong2kim/wlog/internal/otel"
	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

// fakeWriter is a model.Writer that records batches. It can be made to block or
// fail to exercise the worker's error/drain paths.
type fakeWriter struct {
	mu      sync.Mutex
	batches []model.Batch
	count   atomic.Int64

	failNext atomic.Bool
	block    chan struct{} // when non-nil, WriteBatch waits on it
}

func (f *fakeWriter) WriteBatch(ctx context.Context, b model.Batch) error {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.failNext.Swap(false) {
		return errors.New("fake write failure")
	}
	f.mu.Lock()
	f.batches = append(f.batches, b)
	f.mu.Unlock()
	f.count.Add(1)
	return nil
}

func (f *fakeWriter) total() int64 { return f.count.Load() }

func (f *fakeWriter) all() []model.Batch {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]model.Batch, len(f.batches))
	copy(out, f.batches)
	return out
}

// strAttr / intAttr build OTLP KeyValues.
func strAttr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}
func intAttr(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

// apiRequestLogs builds a logs export request with one api_request record for
// the given session/model with input/output token counts and cost.
func apiRequestLogs(sessionID, model string, in, out int64, cost float64) *collectorlogspb.ExportLogsServiceRequest {
	rec := &logspb.LogRecord{
		TimeUnixNano: uint64(time.Now().UnixNano()),
		EventName:    "claude_code.api_request",
		Attributes: []*commonpb.KeyValue{
			strAttr("session.id", sessionID),
			strAttr("model", model),
			intAttr("input_tokens", in),
			intAttr("output_tokens", out),
			{Key: "cost_usd", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: cost}}},
			intAttr("duration_ms", 42),
		},
	}
	return &collectorlogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{rec}}},
		}},
	}
}

// tokenUsageMetrics builds a metrics export with one CUMULATIVE token.usage Sum
// data point for session s, type=input, at the given cumulative value and point
// time. Reused to drive delta normalization through the pipeline.
func tokenUsageMetrics(sid string, cumulative int64, pointNano uint64) *collectormetricspb.ExportMetricsServiceRequest {
	dp := &metricspb.NumberDataPoint{
		StartTimeUnixNano: 1000,
		TimeUnixNano:      pointNano,
		Value:             &metricspb.NumberDataPoint_AsInt{AsInt: cumulative},
		Attributes: []*commonpb.KeyValue{
			strAttr("session.id", sid),
			strAttr("type", "input"),
		},
	}
	return &collectormetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Metrics: []*metricspb.Metric{{
					Name: "claude_code.token.usage",
					Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
						AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
						IsMonotonic:            true,
						DataPoints:             []*metricspb.NumberDataPoint{dp},
					}},
				}},
			}},
		}},
	}
}

// userPromptLogs builds a logs export with one user_prompt record carrying a
// prompt_length attribute (a non-sensitive summary field used by the tail).
func userPromptLogs(sessionID string, promptLength int64) *collectorlogspb.ExportLogsServiceRequest {
	rec := &logspb.LogRecord{
		TimeUnixNano: uint64(time.Now().UnixNano()),
		EventName:    "claude_code.user_prompt",
		Attributes: []*commonpb.KeyValue{
			strAttr("session.id", sessionID),
			intAttr("prompt_length", promptLength),
		},
	}
	return &collectorlogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{rec}}},
		}},
	}
}

func newTestPipeline(t *testing.T, w model.Writer, queue int) (*Pipeline, *Bus) {
	t.Helper()
	bus := NewBus(64)
	p := New(otel.NewParser(otel.Options{}), w, bus, queue)
	return p, bus
}

// F6 regression: a user_prompt published as a LogEvent must carry the
// kind-relevant summary field (prompt_length) in Fields, so the tail can render
// a per-kind summary instead of an empty object.
func TestPipelineUserPromptLogEventHasFields(t *testing.T) {
	w := &fakeWriter{}
	p, bus := newTestPipeline(t, w, 16)
	sub := bus.Subscribe()
	defer sub.Close()
	p.Start(context.Background())
	defer func() { _ = p.Shutdown(context.Background()) }()

	if err := p.Submit(KindLogs, userPromptLogs("s1", 123)); err != nil {
		t.Fatalf("submit: %v", err)
	}

	msgs := drainWithin(sub, 1, time.Second)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 published log event, got %d", len(msgs))
	}
	ev := msgs[0].Event
	if ev.Kind != "user_prompt" {
		t.Fatalf("unexpected kind: %q", ev.Kind)
	}
	if ev.Fields == nil {
		t.Fatalf("LogEvent.Fields must not be nil")
	}
	pl, ok := ev.Fields["prompt_length"]
	if !ok {
		t.Fatalf("user_prompt LogEvent.Fields must contain prompt_length, got %+v", ev.Fields)
	}
	// JSON-decoded numbers come back as float64.
	if f, isNum := pl.(float64); !isNum || f != 123 {
		t.Fatalf("prompt_length should be 123, got %v (%T)", pl, pl)
	}
}

func TestPipelineSubmitParseWrite(t *testing.T) {
	w := &fakeWriter{}
	p, bus := newTestPipeline(t, w, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	if err := p.Submit(KindLogs, apiRequestLogs("sess-1", "claude-x", 100, 50, 0.25)); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Wait for the write to land.
	waitFor(t, time.Second, func() bool { return w.total() == 1 })

	batches := w.all()
	if len(batches) != 1 || len(batches[0].APIRequests) != 1 {
		t.Fatalf("unexpected batches: %+v", batches)
	}
	got := batches[0].APIRequests[0]
	if got.SessionID != "sess-1" || got.Model != "claude-x" || got.Tokens.Input != 100 {
		t.Fatalf("unexpected api request: %+v", got)
	}

	// Bus snapshot must reflect the accumulation.
	snap := bus.Snapshot()
	if snap.SessionID != "sess-1" || snap.CostUSD != 0.25 || snap.Tokens.Input != 100 {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}

	c := p.Counters()
	if c.Received != 1 || c.Accepted != 1 || c.RejectedPermanent != 0 || c.RetrySignaled != 0 {
		t.Fatalf("unexpected counters: %+v", c)
	}
}

func TestPipelineQueueFullBusyRetryable(t *testing.T) {
	// Block the writer so the single in-flight item never completes, then fill
	// the tiny queue and assert the next Submit is retryable ErrBusy.
	w := &fakeWriter{block: make(chan struct{})}
	p, _ := newTestPipeline(t, w, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	// First submit: worker picks it up and blocks on the writer.
	if err := p.Submit(KindLogs, apiRequestLogs("s", "m", 1, 1, 0)); err != nil {
		t.Fatalf("submit 1: %v", err)
	}
	// Give the worker a moment to dequeue the first item.
	waitFor(t, time.Second, func() bool { return p.Counters().Received == 1 })

	// Now fill the queue (cap 1) and then overflow.
	var busy bool
	for i := 0; i < 10; i++ {
		err := p.Submit(KindLogs, apiRequestLogs("s", "m", 1, 1, 0))
		if errors.Is(err, ErrBusy) {
			busy = true
			break
		}
	}
	if !busy {
		t.Fatalf("expected ErrBusy on a full queue")
	}
	if p.Counters().RetrySignaled == 0 {
		t.Fatalf("expected RetrySignaled > 0")
	}

	// Unblock and clean up.
	close(w.block)
	_ = p.Shutdown(context.Background())
}

func TestPipelineWriteFailureDoesNotAccept(t *testing.T) {
	w := &fakeWriter{}
	w.failNext.Store(true)
	p, bus := newTestPipeline(t, w, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	if err := p.Submit(KindLogs, apiRequestLogs("s", "m", 1, 1, 1)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitFor(t, time.Second, func() bool { return p.Counters().Received == 1 })
	// Let the worker process (and fail) the write.
	time.Sleep(50 * time.Millisecond)

	if c := p.Counters(); c.Accepted != 0 {
		t.Fatalf("write failure must not increment Accepted: %+v", c)
	}
	if snap := bus.Snapshot(); snap.SessionID != "" {
		t.Fatalf("bus must not update on write failure: %+v", snap)
	}
}

func TestPipelineDrainShutdown(t *testing.T) {
	w := &fakeWriter{}
	p, _ := newTestPipeline(t, w, 256)
	p.Start(context.Background())

	const n = 100
	for i := 0; i < n; i++ {
		if err := p.Submit(KindLogs, apiRequestLogs("s", "m", 1, 1, 0)); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if w.total() != n {
		t.Fatalf("drain shutdown lost data: wrote %d, want %d", w.total(), n)
	}
	// Submit after shutdown must be rejected (not panic).
	if err := p.Submit(KindLogs, apiRequestLogs("s", "m", 1, 1, 0)); err == nil {
		t.Fatalf("expected error submitting after shutdown")
	}
}

func TestPipelineSeed(t *testing.T) {
	// Seed must be callable and feed the parser without error; we assert it does
	// not panic and that subsequent ingestion still works.
	w := &fakeWriter{}
	p, _ := newTestPipeline(t, w, 16)
	p.Seed([]model.SeriesState{{
		Key:               "claude_code.token.usage|model=claude-x|session.id=s",
		LastValue:         10,
		LastStartUnixNano: 1,
		LastPointUnixNano: 2,
		Temporality:       1,
		BaselineKnown:     true,
	}})
	p.Start(context.Background())
	defer func() { _ = p.Shutdown(context.Background()) }()

	if err := p.Submit(KindLogs, apiRequestLogs("s", "claude-x", 5, 5, 0)); err != nil {
		t.Fatalf("submit after seed: %v", err)
	}
	waitFor(t, time.Second, func() bool { return w.total() == 1 })
}

func TestPipelineSessionSwitchResetsAccumulation(t *testing.T) {
	w := &fakeWriter{}
	p, bus := newTestPipeline(t, w, 64)
	p.Start(context.Background())
	defer func() { _ = p.Shutdown(context.Background()) }()

	if err := p.Submit(KindLogs, apiRequestLogs("s1", "m", 100, 0, 1.0)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return w.total() == 1 })
	if snap := bus.Snapshot(); snap.SessionID != "s1" || snap.CostUSD != 1.0 {
		t.Fatalf("after s1: %+v", snap)
	}

	if err := p.Submit(KindLogs, apiRequestLogs("s2", "m", 10, 0, 0.5)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return w.total() == 2 })
	snap := bus.Snapshot()
	if snap.SessionID != "s2" || snap.CostUSD != 0.5 || snap.Tokens.Input != 10 {
		t.Fatalf("session switch did not reset accumulation: %+v", snap)
	}
}

func TestPipelineConcurrentSubmitNoLoss(t *testing.T) {
	w := &fakeWriter{}
	// Large queue so no retryable rejections under this concurrency.
	p, _ := newTestPipeline(t, w, 4096)
	p.Start(context.Background())

	const goroutines = 16
	const perG = 100
	var wg sync.WaitGroup
	var retried atomic.Int64
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				for {
					err := p.Submit(KindLogs, apiRequestLogs("s", "m", 1, 1, 0))
					if err == nil {
						break
					}
					if errors.Is(err, ErrBusy) {
						retried.Add(1)
						continue // retry, mirroring OTLP client behavior (no loss)
					}
					t.Errorf("unexpected submit error: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	want := int64(goroutines * perG)
	if w.total() != want {
		t.Fatalf("lost data under concurrency: wrote %d, want %d (retries=%d)", w.total(), want, retried.Load())
	}
}

// C4 regression: a request that is already dequeued/accepted must be committed
// even if the ROOT application context is cancelled (e.g. SIGINT). The write
// must run under an independent context, so the writer must NOT observe a
// cancelled ctx. We gate the writer until after we cancel the root ctx, then
// assert the batch still lands and the writer's ctx was live.
func TestPipelineCancelledRootCtxStillCommits(t *testing.T) {
	release := make(chan struct{})
	w := &ctxObservingWriter{gate: release}
	p, _ := newTestPipeline(t, w, 16)

	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)

	// Submit a request; the worker dequeues it and blocks in the writer on the
	// gate (so the write has started but not finished).
	if err := p.Submit(KindLogs, apiRequestLogs("s-shutdown", "m", 7, 3, 0.1)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitFor(t, time.Second, func() bool { return w.entered.Load() })

	// Cancel the ROOT ctx while the write is in flight, then release the writer.
	cancel()
	close(release)

	// The batch must still commit, and the writer must have seen a non-cancelled
	// context (C4: write ctx is independent of the root ctx).
	waitFor(t, time.Second, func() bool { return w.committed.Load() == 1 })
	if w.sawCancelled.Load() {
		t.Fatalf("writer observed a cancelled context; write ctx must be independent of root ctx (C4)")
	}
	_ = p.Shutdown(context.Background())
}

// C4 regression: requests buffered in the queue when the root ctx is cancelled
// must be drained and committed (not dropped). The worker drains on ctx.Done
// using the independent write ctx.
func TestPipelineRootCancelDrainsBufferedRequests(t *testing.T) {
	// Block the first write so the queue accumulates buffered requests behind it.
	w := &ctxObservingWriter{gate: make(chan struct{})}
	p, _ := newTestPipeline(t, w, 64)
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)

	const n = 10
	for i := 0; i < n; i++ {
		if err := p.Submit(KindLogs, apiRequestLogs("s", "m", 1, 1, 0)); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	// Wait until the worker has dequeued the first item (now blocked in writer).
	waitFor(t, time.Second, func() bool { return w.entered.Load() })

	// Cancel root ctx, then release the writer gate so all writes proceed.
	cancel()
	close(w.gate)

	// Every buffered request must commit despite the cancelled root ctx.
	waitFor(t, 2*time.Second, func() bool { return w.committed.Load() == int64(n) })
	if w.sawCancelled.Load() {
		t.Fatalf("a drained write saw a cancelled ctx; must be independent (C4)")
	}
	_ = p.Shutdown(context.Background())
}

// C3 regression (pipeline-level): when a metrics WriteBatch FAILS, the delta
// normalizer's series-state advance must be rolled back, so the retried point
// re-derives the correct delta instead of permanently losing it. Single worker
// → strict ordering, so we can control which batch fails.
func TestPipelineMetricsWriteFailureRecoversDelta(t *testing.T) {
	w := &deltaProbeWriter{}
	p, _ := newTestPipeline(t, w, 16)
	p.Start(context.Background())
	defer func() { _ = p.Shutdown(context.Background()) }()

	// 1) Baseline cumulative 100 → delta 0, write succeeds.
	if err := p.Submit(KindMetrics, tokenUsageMetrics("s1", 100, 2000)); err != nil {
		t.Fatalf("submit baseline: %v", err)
	}
	waitFor(t, time.Second, func() bool { return w.calls.Load() == 1 })

	// 2) Cumulative 150 → delta 50 in-memory, but the WRITE FAILS → rollback.
	w.failNext.Store(true)
	if err := p.Submit(KindMetrics, tokenUsageMetrics("s1", 150, 3000)); err != nil {
		t.Fatalf("submit failing: %v", err)
	}
	waitFor(t, time.Second, func() bool { return w.calls.Load() == 2 })

	// 3) Retry the SAME cumulative 150. Without rollback the cursor would already
	// be at 150 and the delta would be 0 (lost). With C3 rollback the cursor is
	// back at 100, so the delta is the correct 50.
	if err := p.Submit(KindMetrics, tokenUsageMetrics("s1", 150, 3000)); err != nil {
		t.Fatalf("submit retry: %v", err)
	}
	waitFor(t, time.Second, func() bool { return w.calls.Load() == 3 && w.committed.Load() == 2 })

	last := w.lastTokenDelta.Load()
	if last == nil || *last != 50 {
		got := "nil"
		if last != nil {
			got = floatStr(*last)
		}
		t.Fatalf("retried point must re-derive delta 50 after rollback (no loss), got %s", got)
	}
}

// deltaProbeWriter fails the next write when failNext is set and records the
// token.usage delta of the most recently COMMITTED batch.
type deltaProbeWriter struct {
	calls          atomic.Int64
	committed      atomic.Int64
	failNext       atomic.Bool
	lastTokenDelta atomic.Pointer[float64]
}

func (w *deltaProbeWriter) WriteBatch(_ context.Context, b model.Batch) error {
	w.calls.Add(1)
	if w.failNext.Swap(false) {
		return errors.New("injected metrics write failure")
	}
	for _, mp := range b.MetricPoints {
		if mp.Name == "claude_code.token.usage" {
			v := mp.ValueDelta
			w.lastTokenDelta.Store(&v)
		}
	}
	w.committed.Add(1)
	return nil
}

func floatStr(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// ctxObservingWriter records whether the context it was handed was already
// cancelled, gates entry on a channel, and counts commits.
type ctxObservingWriter struct {
	gate         chan struct{}
	entered      atomic.Bool
	committed    atomic.Int64
	sawCancelled atomic.Bool
	once         sync.Once
}

func (w *ctxObservingWriter) WriteBatch(ctx context.Context, b model.Batch) error {
	w.entered.Store(true)
	if w.gate != nil {
		w.once.Do(func() { <-w.gate }) // block only the first write
	}
	if ctx.Err() != nil {
		w.sawCancelled.Store(true)
	}
	w.committed.Add(1)
	return nil
}

// --- SubmitBatch (KindBatch / JSONL source path) ---------------------------

// sampleBatch builds a one-session, one-api_request batch used to exercise the
// SubmitBatch path (already-parsed, as the JSONL source delivers).
func sampleBatch(sid, mdl string, in, out int64, cost float64) model.Batch {
	return model.Batch{
		Sessions: []model.Session{{
			ID: sid, FirstSeen: 1000, LastSeen: 2000,
			TokenSource: "events", HasEvents: true,
		}},
		APIRequests: []model.APIRequest{{
			SessionID: sid, TS: 1500, Model: mdl,
			Tokens:  model.Tokens{Input: in, Output: out},
			CostUSD: cost,
			DedupKey: model.APIRequestDedupKey(sid, "api_request", "1500000000", mdl,
				strconv.FormatInt(in, 10), strconv.FormatInt(out, 10)),
		}},
	}
}

// TestPipelineSubmitBatchWriteAndBus verifies an already-parsed batch is
// persisted through the writer and reflected on the live bus (snapshot + a
// published api_request log event), reusing the OTLP write→updateBus→publish
// path.
func TestPipelineSubmitBatchWriteAndBus(t *testing.T) {
	w := &fakeWriter{}
	p, bus := newTestPipeline(t, w, 64)
	sub := bus.Subscribe()
	defer sub.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	if err := p.SubmitBatch(sampleBatch("sess-b", "claude-z", 200, 80, 0.5)); err != nil {
		t.Fatalf("SubmitBatch: %v", err)
	}

	waitFor(t, time.Second, func() bool { return w.total() == 1 })

	batches := w.all()
	if len(batches) != 1 || len(batches[0].APIRequests) != 1 {
		t.Fatalf("unexpected batches: %+v", batches)
	}
	if got := batches[0].APIRequests[0]; got.SessionID != "sess-b" || got.Model != "claude-z" || got.Tokens.Input != 200 {
		t.Fatalf("unexpected api request: %+v", got)
	}

	// Bus snapshot reflects accumulation.
	snap := bus.Snapshot()
	if snap.SessionID != "sess-b" || snap.CostUSD != 0.5 || snap.Tokens.Input != 200 {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}

	// A log event was published for the api_request.
	msgs := drainWithin(sub, 1, time.Second)
	if len(msgs) != 1 || msgs[0].Event.Kind != "api_request" {
		t.Fatalf("expected one published api_request log event, got %+v", msgs)
	}

	c := p.Counters()
	if c.Received != 1 || c.Accepted != 1 || c.RejectedPermanent != 0 {
		t.Fatalf("unexpected counters: %+v", c)
	}
}

// apiRequestBatch builds a one-session batch with a single api_request whose
// dedup_key is derived from (session, ts, model, in, out) via the shared
// constructor, with an explicit CostSource. Two batches built with the SAME
// session/ts/model/tokens therefore share a dedup_key — exactly the JSONL↔OTLP
// collision the store collapses to one row (EQ6).
func apiRequestBatch(sid, mdl string, in, out int64, cost float64, source string) model.Batch {
	return model.Batch{
		Sessions: []model.Session{{
			ID: sid, FirstSeen: 1000, LastSeen: 2000,
			TokenSource: "events", HasEvents: true,
		}},
		APIRequests: []model.APIRequest{{
			SessionID: sid, TS: 1500, Model: mdl,
			Tokens:     model.Tokens{Input: in, Output: out},
			CostUSD:    cost,
			CostSource: source,
			DedupKey: model.APIRequestDedupKey(sid, "api_request", "1500000000", mdl,
				strconv.FormatInt(in, 10), strconv.FormatInt(out, 10)),
		}},
	}
}

// TestPipelineLiveSnapshotDedupNoDoubleCount is the P1 regression: the SAME
// api_request seen via BOTH the JSONL transcript (a pricing-table ESTIMATE) and
// OTLP (Claude Code's authoritative cost) — or a JSONL watcher re-reading a
// holdback region — produces the SAME dedup_key. The store collapses it to one
// row, but the live Now snapshot used to add BOTH, doubling cost/tokens. The live
// accumulation must fold each dedup_key once; the authoritative OTLP figure must
// REPLACE the earlier estimate (OTLP-wins), not stack on it. The duplicate live
// LogEvent must also be suppressed.
func TestPipelineLiveSnapshotDedupNoDoubleCount(t *testing.T) {
	w := &fakeWriter{}
	p, bus := newTestPipeline(t, w, 64)
	sub := bus.Subscribe()
	defer sub.Close()
	p.Start(context.Background())
	defer func() { _ = p.Shutdown(context.Background()) }()

	const (
		sid      = "sess-dup"
		mdl      = "claude-dup"
		in, out  = int64(100), int64(40)
		estCost  = 0.20 // JSONL pricing-table estimate
		otlpCost = 0.50 // authoritative OTLP cost for the SAME call
	)

	// 1) JSONL estimate lands first (the history scan typically precedes the live
	// OTLP event). Snapshot reflects the estimate exactly once.
	if err := p.SubmitBatch(apiRequestBatch(sid, mdl, in, out, estCost, model.CostSourceJSONLEstimate)); err != nil {
		t.Fatalf("submit jsonl estimate: %v", err)
	}
	waitFor(t, time.Second, func() bool { return w.total() == 1 })
	if snap := bus.Snapshot(); snap.CostUSD != estCost || snap.Tokens.Input != in || snap.Tokens.Output != out {
		t.Fatalf("after jsonl estimate, snapshot should reflect it once: %+v", snap)
	}

	// 2) The SAME call arrives via OTLP (same dedup_key) with the authoritative
	// cost. The live cost/tokens must NOT double — they must become the OTLP figure
	// (replace, OTLP-wins), and the token counts (identical here) stay single.
	if err := p.SubmitBatch(apiRequestBatch(sid, mdl, in, out, otlpCost, model.CostSourceOTLP)); err != nil {
		t.Fatalf("submit otlp authoritative: %v", err)
	}
	waitFor(t, time.Second, func() bool { return w.total() == 2 })

	snap := bus.Snapshot()
	if snap.Tokens.Input != in || snap.Tokens.Output != out {
		t.Fatalf("tokens DOUBLE-COUNTED on dedup_key collision: got input=%d output=%d, want %d/%d (counted once)",
			snap.Tokens.Input, snap.Tokens.Output, in, out)
	}
	if snap.CostUSD != otlpCost {
		t.Fatalf("live cost wrong after OTLP over JSONL estimate: got %v, want %v (OTLP-wins, no double-count of %v+%v)",
			snap.CostUSD, otlpCost, estCost, otlpCost)
	}

	// 3) A redundant re-delivery of the OTLP record (same dedup_key) is a complete
	// no-op for the live snapshot (idempotent).
	if err := p.SubmitBatch(apiRequestBatch(sid, mdl, in, out, otlpCost, model.CostSourceOTLP)); err != nil {
		t.Fatalf("submit otlp redelivery: %v", err)
	}
	waitFor(t, time.Second, func() bool { return w.total() == 3 })
	if snap := bus.Snapshot(); snap.CostUSD != otlpCost || snap.Tokens.Input != in {
		t.Fatalf("redelivery must be a no-op on the snapshot: %+v", snap)
	}

	// publishBatchEvents must have suppressed the duplicates: exactly ONE
	// api_request LogEvent for the three submissions of the same dedup_key.
	msgs := drainWithin(sub, 2, 300*time.Millisecond) // ask for 2; we must observe only 1
	apiEvents := 0
	for _, m := range msgs {
		if m.Kind == "log" && (m.Event.Kind == "api_request" || m.Event.Kind == "api_error") {
			apiEvents++
		}
	}
	if apiEvents != 1 {
		t.Fatalf("publishBatchEvents must emit the api_request log ONCE per dedup_key, got %d", apiEvents)
	}
}

// TestPipelineSubmitBatchQueueFullBusy asserts SubmitBatch returns the retryable
// ErrBusy on a full queue (so the JSONL source backs off rather than dropping).
func TestPipelineSubmitBatchQueueFullBusy(t *testing.T) {
	w := &fakeWriter{block: make(chan struct{})}
	p, _ := newTestPipeline(t, w, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	// First batch: worker dequeues it and blocks in the writer.
	if err := p.SubmitBatch(sampleBatch("s", "m", 1, 1, 0)); err != nil {
		t.Fatalf("submit 1: %v", err)
	}
	waitFor(t, time.Second, func() bool { return p.Counters().Received == 1 })

	var busy bool
	for i := 0; i < 10; i++ {
		if errors.Is(p.SubmitBatch(sampleBatch("s", "m", 1, 1, 0)), ErrBusy) {
			busy = true
			break
		}
	}
	if !busy {
		t.Fatalf("expected ErrBusy on a full queue")
	}
	if p.Counters().RetrySignaled == 0 {
		t.Fatalf("expected RetrySignaled > 0")
	}

	close(w.block)
	_ = p.Shutdown(context.Background())
}

// TestPipelineSubmitBatchDrainShutdown asserts batches buffered at Shutdown are
// drained and committed (no loss), and SubmitBatch after Shutdown is rejected.
func TestPipelineSubmitBatchDrainShutdown(t *testing.T) {
	w := &fakeWriter{}
	p, _ := newTestPipeline(t, w, 256)
	p.Start(context.Background())

	const n = 50
	for i := 0; i < n; i++ {
		if err := p.SubmitBatch(sampleBatch("s", "m", 1, 1, 0)); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if w.total() != n {
		t.Fatalf("drain shutdown lost data: wrote %d, want %d", w.total(), n)
	}
	if err := p.SubmitBatch(sampleBatch("s", "m", 1, 1, 0)); err == nil {
		t.Fatalf("expected error submitting after shutdown")
	}
}

// waitFor polls cond until it returns true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}
