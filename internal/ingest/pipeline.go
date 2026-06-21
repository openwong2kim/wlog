package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openwong2kim/wlog/internal/model"
	"github.com/openwong2kim/wlog/internal/otel"
	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

// RequestKind identifies which OTLP signal a submitted request carries so the
// pipeline worker dispatches it to the right Parser method.
type RequestKind int

const (
	// KindMetrics is an *collectormetricspb.ExportMetricsServiceRequest.
	KindMetrics RequestKind = iota
	// KindLogs is an *collectorlogspb.ExportLogsServiceRequest.
	KindLogs
	// KindTraces is an *collectortracepb.ExportTraceServiceRequest.
	KindTraces
	// KindBatch is an already-parsed model.Batch (the JSONL transcript source,
	// PLAN v3 feature 0): it skips the OTLP parse step and goes straight to the
	// writer. Carried in request.batch, not request.req.
	KindBatch
)

// ErrBusy is returned by Submit when the bounded request queue is full. It is
// retryable: the receiver must signal the client to retry (gRPC Unavailable +
// RetryInfo / HTTP 503 + Retry-After) and must NOT acknowledge the data
// (REVIEWS B, PLAN §5 — no silent drops).
var ErrBusy = errors.New("ingest: queue full (retryable)")

// errStopped is returned internally when Submit is called after Shutdown.
var errStopped = errors.New("ingest: pipeline stopped")

// Counters is a point-in-time snapshot of the pipeline counters for /api/health
// (PLAN §10). Received/Accepted/RejectedPermanent/RetrySignaled come from the
// pipeline; BusDropped comes from the live bus's per-subscriber ring overflow.
type Counters struct {
	Received          int64
	Accepted          int64
	RejectedPermanent int64
	RetrySignaled     int64
	BusDropped        int64
}

// request is a unit of work on the bounded queue. For OTLP kinds req holds the
// proto export request; for KindBatch batch holds an already-parsed model.Batch
// and req is nil.
type request struct {
	kind  RequestKind
	req   any
	batch model.Batch
}

// apiContrib records what one api_request dedup_key contributed to the live
// snapshot so a later, more authoritative sighting of the same key can REPLACE
// (not re-add) the contribution. source is the CostSource of the sighting that
// is currently reflected (CostSourceOTLP wins over CostSourceJSONLEstimate).
type apiContrib struct {
	cost   float64
	tokens model.Tokens
	source string
}

// Pipeline normalizes OTLP requests and persists them through the single store
// writer, updating the live bus as it goes. A single worker goroutine owns the
// parse→write→publish sequence so ordering and the in-memory current-session
// accumulation need no extra locking.
type Pipeline struct {
	parser *otel.Parser
	writer model.Writer
	bus    *Bus

	queue chan request

	// counters (atomic; read lock-free by Counters()).
	received          atomic.Int64
	accepted          atomic.Int64
	rejectedPermanent atomic.Int64
	retrySignaled     atomic.Int64

	// in-memory current-session accumulation. Owned exclusively by the worker
	// goroutine (no lock needed) and mirrored into the bus snapshot.
	curSession string
	curCost    float64
	curTokens  model.Tokens
	curActive  float64
	curTool    string

	// seenAPI tracks, for the CURRENT session only, the api_request dedup_keys
	// already folded into the live snapshot and the cost/token contribution each
	// one made. It mirrors the store's dedup_key idempotency in the live view: the
	// store collapses an api_request seen via BOTH the JSONL transcript (a
	// pricing-table ESTIMATE) and OTLP (Claude Code's authoritative cost) to ONE
	// row, but the raw per-batch accumulation would add BOTH, doubling the live
	// Now cost/tokens (P1). Keyed by dedup_key so the same logical call is counted
	// once; the stored contribution lets a later authoritative OTLP record REPLACE
	// an earlier JSONL estimate (OTLP-wins, matching persisted cost_source) instead
	// of merely being skipped. Reset on session switch (bounded to one session).
	seenAPI map[string]apiContrib

	// seenEvent tracks the dedup_keys of tool_result / tool_decision / generic
	// Event records already reflected in the live stream for the CURRENT session,
	// so a record seen via both sources (or a JSONL holdback re-read) publishes its
	// LogEvent and advances the active-tool hint only ONCE. Reset on session switch.
	seenEvent map[string]struct{}

	// writeTimeout bounds a single WriteBatch so a stuck writer cannot wedge the
	// worker indefinitely (the store enforces its own serialization).
	writeTimeout time.Duration

	startOnce sync.Once
	stopOnce  sync.Once
	stopped   atomic.Bool

	// submitMu guards the queue send against the Shutdown close: Submit takes the
	// read lock (concurrent sends allowed), Shutdown takes the write lock to
	// close, so a send can never race with the close (no send-on-closed panic).
	submitMu sync.RWMutex

	done chan struct{} // closed when the worker goroutine exits
}

// New constructs a Pipeline. queueSize bounds the in-flight request queue; a
// size <= 0 uses a small default. The parser must be Seeded by the caller (or
// via Seed below) before ingestion begins so delta normalization resumes from
// persisted series state.
func New(parser *otel.Parser, w model.Writer, bus *Bus, queueSize int) *Pipeline {
	if queueSize <= 0 {
		queueSize = 1024
	}
	return &Pipeline{
		parser:       parser,
		writer:       w,
		bus:          bus,
		queue:        make(chan request, queueSize),
		writeTimeout: 30 * time.Second,
		done:         make(chan struct{}),
	}
}

// Seed replays persisted series cursors into the parser's delta normalizer so a
// restart resumes without re-baselining (PLAN §7). It exposes the parser Seed
// path so the app can wire store.LoadSeriesStates → parser.Seed. Call before
// Start / before any Submit.
func (p *Pipeline) Seed(states []model.SeriesState) { p.parser.Seed(states) }

// Submit enqueues an OTLP request non-blockingly. On a full queue it returns
// ErrBusy (retryable) and does NOT take ownership of the data, so the receiver
// can signal the client to retry. After Shutdown it returns errStopped.
//
// req must match kind: KindMetrics → *ExportMetricsServiceRequest, KindLogs →
// *ExportLogsServiceRequest, KindTraces → *ExportTraceServiceRequest.
func (p *Pipeline) Submit(kind RequestKind, req any) error {
	p.submitMu.RLock()
	defer p.submitMu.RUnlock()
	if p.stopped.Load() {
		return errStopped
	}
	p.received.Add(1)
	select {
	case p.queue <- request{kind: kind, req: req}:
		return nil
	default:
		p.retrySignaled.Add(1)
		return ErrBusy
	}
}

// SubmitBatch enqueues an ALREADY-PARSED batch (the JSONL transcript source,
// PLAN v3 feature 0) onto the same bounded worker queue as OTLP submissions, so
// the single-worker write→updateBus→publish sequence and ordering guarantees are
// reused unchanged. The batch is persisted through the same store writer with
// the same dedup_key idempotency, so a row already ingested via OTLP collapses
// to one row (EQ6) — re-submitting the same transcript is harmless.
//
// Unlike metrics, a JSONL batch needs no delta-normalizer transaction: the
// jsonl parser maps per-request absolute token/cost usage to api_request rows
// (PLAN v3 §9) and emits no MetricPoints/SeriesStates to normalize.
//
// On a full queue it returns ErrBusy (retryable): the caller (the JSONL source)
// should back off and retry rather than drop the batch. After Shutdown it
// returns errStopped.
func (p *Pipeline) SubmitBatch(b model.Batch) error {
	p.submitMu.RLock()
	defer p.submitMu.RUnlock()
	if p.stopped.Load() {
		return errStopped
	}
	p.received.Add(1)
	select {
	case p.queue <- request{kind: KindBatch, batch: b}:
		return nil
	default:
		p.retrySignaled.Add(1)
		return ErrBusy
	}
}

// Start launches the single worker goroutine. It is idempotent. The worker runs
// until ctx is cancelled or Shutdown is called.
func (p *Pipeline) Start(ctx context.Context) {
	p.startOnce.Do(func() {
		go p.worker(ctx)
	})
}

// worker is the single goroutine that drains the queue. It processes requests
// until the queue is closed (by Shutdown) and then exits, having drained every
// buffered request.
func (p *Pipeline) worker(ctx context.Context) {
	defer close(p.done)
	for {
		select {
		case req, ok := <-p.queue:
			if !ok {
				return // queue closed by Shutdown and fully drained
			}
			p.process(ctx, req)
		case <-ctx.Done():
			// Context cancelled: drain whatever is already buffered, then exit.
			// process ignores this ctx for writes (it uses an independent write
			// context, C4), so the drain commits every buffered batch even though
			// the root ctx is already cancelled.
			p.drain(ctx)
			return
		}
	}
}

// drain processes all currently-buffered requests without blocking on new ones.
func (p *Pipeline) drain(ctx context.Context) {
	for {
		select {
		case req, ok := <-p.queue:
			if !ok {
				return
			}
			p.process(ctx, req)
		default:
			return
		}
	}
}

// process parses one request, writes the resulting batch, and updates the bus.
//
// The ctx argument is NOT used for the write: once a request has been dequeued
// it has already been accepted off the wire, so it must be durably persisted
// even if the root application context is cancelled (e.g. SIGINT mid-drain).
// The write therefore runs under an independent context bounded only by the
// write timeout (C4 — no data loss on shutdown). For metrics, the delta
// normalizer's in-memory series advance is wrapped in a transaction that is
// committed only after a successful write and rolled back on failure, so a
// failed write never permanently loses a delta (C3).
func (p *Pipeline) process(_ context.Context, r request) {
	var (
		batch model.Batch
		rej   otel.RejectInfo
		txn   *otel.Txn
	)
	switch r.kind {
	case KindMetrics:
		if req, ok := r.req.(*collectormetricspb.ExportMetricsServiceRequest); ok {
			// Begin the series-state transaction BEFORE parsing so every cursor
			// mutation ParseMetrics makes is recorded for potential rollback (C3).
			txn = p.parser.BeginMetricsTxn()
			batch, rej = p.parser.ParseMetrics(req)
		}
	case KindLogs:
		if req, ok := r.req.(*collectorlogspb.ExportLogsServiceRequest); ok {
			batch, rej = p.parser.ParseLogs(req)
		}
	case KindTraces:
		if req, ok := r.req.(*collectortracepb.ExportTraceServiceRequest); ok {
			batch, rej = p.parser.ParseTraces(req)
		}
	case KindBatch:
		// Already parsed (JSONL transcript source). No OTLP parse, no delta
		// transaction (txn stays nil — Txn.Commit/Rollback are nil-safe). The
		// batch flows through the identical write→updateBus→publish path below.
		batch = r.batch
	}

	if rej.Rejected > 0 {
		p.rejectedPermanent.Add(int64(rej.Rejected))
	}

	// Independent write context: a dequeued request is owned by the pipeline and
	// must be committed regardless of the root ctx's state (C4). Only the write
	// timeout bounds it.
	wctx, cancel := context.WithTimeout(context.Background(), p.writeTimeout)
	err := p.writer.WriteBatch(wctx, batch)
	cancel()
	if err != nil {
		// A write failure is not retryable from the client's perspective (the
		// request was accepted off the wire); count nothing extra and skip the
		// bus update so the live view is not corrupted by a half-applied batch.
		// Roll back the delta normalizer so the in-memory series state matches the
		// (unchanged) durable state and the next point's delta is computed
		// correctly (C3).
		txn.Rollback()
		return
	}

	// Write committed: confirm the in-memory series-state advance (C3).
	txn.Commit()
	p.accepted.Add(1)
	p.updateBus(batch)
}

// updateBus refreshes the in-memory current-session accumulation from the batch
// and publishes each event as a LogEvent. The current session is the most
// recent session.id observed in the batch; accumulation resets when it changes.
//
// Live accumulation honors the SAME dedup_key idempotency the store enforces
// (P1): the JSONL transcript and OTLP can describe the SAME api_request (or a
// JSONL watcher can re-read a holdback region), and while the store collapses
// them to one row, naively summing every batch would count cost/tokens TWICE in
// the live Now view. We therefore fold each api_request into the snapshot at most
// once per dedup_key, and let an authoritative OTLP figure replace an earlier
// JSONL estimate (OTLP-wins, matching the persisted cost_source). Per-event
// LogEvents are likewise published once per dedup_key (publishBatchEvents).
func (p *Pipeline) updateBus(b model.Batch) {
	now := time.Now().UnixMilli()

	// Determine the most recent session id from the batch (prefer the last
	// api_request, then any event/tool/metric/session). A batch typically
	// belongs to one session, but we take the last-seen id defensively.
	if sid := latestSessionID(b); sid != "" && sid != p.curSession {
		p.curSession = sid
		p.curCost = 0
		p.curTokens = model.Tokens{}
		p.curActive = 0
		p.curTool = ""
		// New session: the dedup sets are scoped to the current session only, so
		// drop the old session's keys (bounded memory) and start fresh.
		p.seenAPI = nil
		p.seenEvent = nil
	}

	// Accumulate tokens/cost from api_request events belonging to the current
	// session (token/cost axis source = events, PLAN §6.5), deduped by dedup_key.
	for i := range b.APIRequests {
		r := b.APIRequests[i]
		if r.SessionID != "" && r.SessionID != p.curSession {
			continue
		}
		p.accumulateAPIRequest(r)
	}

	// active_time is sourced from metrics (PLAN §6.5 / Eng F9): sum the
	// active_time deltas for the current session. The delta normalizer already
	// produced per-point deltas (each metric point is unique by the series triple,
	// so there is no cross-source double-count to guard here), so summing yields
	// the running total.
	for _, m := range b.MetricPoints {
		if m.SessionID != "" && m.SessionID != p.curSession {
			continue
		}
		if m.Name == "active_time.total" || m.Name == "claude_code.active_time.total" {
			p.curActive += m.ValueDelta
		}
	}

	// The most recent tool_result with no later decision is the "active tool"
	// hint; absent any signal it stays as-is. tool_result implies completion, so
	// we surface the last tool name seen. Only NEW (unseen dedup_key) results
	// advance the hint, so a re-read/cross-source duplicate does not re-trigger it.
	for _, tr := range b.ToolResults {
		if tr.SessionID != "" && tr.SessionID != p.curSession {
			continue
		}
		if _, dup := p.seenEvent[tr.DedupKey]; dup && tr.DedupKey != "" {
			continue
		}
		p.curTool = tr.ToolName
	}

	p.bus.SetSnapshot(NowSnapshot{
		SessionID:     p.curSession,
		CostUSD:       p.curCost,
		Tokens:        p.curTokens,
		ActiveTimeSec: p.curActive,
		ActiveTool:    p.curTool,
		TS:            now,
	})

	p.publishBatchEvents(b)
}

// accumulateAPIRequest folds one api_request into the live snapshot at most once
// per dedup_key (P1). The first sighting of a key adds its cost/tokens; a repeat
// sighting is normally skipped (no double-count). The exception is cost
// authority: if the reflected contribution came from a JSONL estimate and a later
// sighting is authoritative OTLP, we REPLACE the estimate's contribution with the
// OTLP figure (subtract the old, add the new) so the live view matches the
// persisted OTLP-wins resolution. An empty dedup_key (should not happen for a
// parsed api_request) is always accumulated so a missing key never hides a real
// cost.
func (p *Pipeline) accumulateAPIRequest(r model.APIRequest) {
	add := func() {
		p.curCost += r.CostUSD
		p.curTokens.Input += r.Tokens.Input
		p.curTokens.Output += r.Tokens.Output
		p.curTokens.CacheRead += r.Tokens.CacheRead
		p.curTokens.CacheCreation += r.Tokens.CacheCreation
	}

	if r.DedupKey == "" {
		add()
		return
	}

	prev, seen := p.seenAPI[r.DedupKey]
	if !seen {
		add()
		if p.seenAPI == nil {
			p.seenAPI = make(map[string]apiContrib)
		}
		p.seenAPI[r.DedupKey] = apiContrib{cost: r.CostUSD, tokens: r.Tokens, source: r.CostSource}
		return
	}

	// Already counted. Only act again to upgrade a non-authoritative contribution
	// to the authoritative OTLP one (OTLP-wins, mirroring store cost_source). Any
	// other repeat (OTLP→OTLP, JSONL→JSONL, OTLP→JSONL) is a no-op: the value is
	// already reflected once and must not be added again.
	if r.CostSource == model.CostSourceOTLP && prev.source != model.CostSourceOTLP {
		// Subtract the previously-reflected (estimate) contribution, add the OTLP one.
		p.curCost += r.CostUSD - prev.cost
		p.curTokens.Input += r.Tokens.Input - prev.tokens.Input
		p.curTokens.Output += r.Tokens.Output - prev.tokens.Output
		p.curTokens.CacheRead += r.Tokens.CacheRead - prev.tokens.CacheRead
		p.curTokens.CacheCreation += r.Tokens.CacheCreation - prev.tokens.CacheCreation
		p.seenAPI[r.DedupKey] = apiContrib{cost: r.CostUSD, tokens: r.Tokens, source: r.CostSource}
	}
}

// latestSessionID returns the most relevant session id for the batch, preferring
// the explicitly-attributed records.
func latestSessionID(b model.Batch) string {
	for i := len(b.APIRequests) - 1; i >= 0; i-- {
		if b.APIRequests[i].SessionID != "" {
			return b.APIRequests[i].SessionID
		}
	}
	for i := len(b.ToolResults) - 1; i >= 0; i-- {
		if b.ToolResults[i].SessionID != "" {
			return b.ToolResults[i].SessionID
		}
	}
	for i := len(b.ToolDecisions) - 1; i >= 0; i-- {
		if b.ToolDecisions[i].SessionID != "" {
			return b.ToolDecisions[i].SessionID
		}
	}
	for i := len(b.Events) - 1; i >= 0; i-- {
		if b.Events[i].SessionID != "" {
			return b.Events[i].SessionID
		}
	}
	for i := len(b.MetricPoints) - 1; i >= 0; i-- {
		if b.MetricPoints[i].SessionID != "" {
			return b.MetricPoints[i].SessionID
		}
	}
	for i := len(b.Sessions) - 1; i >= 0; i-- {
		if b.Sessions[i].ID != "" {
			return b.Sessions[i].ID
		}
	}
	return ""
}

// publishBatchEvents turns each persisted record in the batch into a LogEvent on
// the bus (per PLAN §10 timeline kind fields). Each record is published at most
// once per dedup_key (P1): the JSONL transcript and OTLP can carry the SAME
// logical event (or a JSONL watcher can re-read a holdback region), and the store
// collapses them to one row — so the live stream must not emit the duplicate
// either, or the timeline shows the same call twice. dedup_key is a content hash
// that already includes the session id, so a single seen-set (reset on session
// switch to bound memory) never collides across sessions. A record with an empty
// dedup_key (not expected for parsed records) is always published rather than
// risk hiding a real event.
func (p *Pipeline) publishBatchEvents(b model.Batch) {
	for _, r := range b.APIRequests {
		if p.eventSeen(r.DedupKey) {
			continue
		}
		kind := "api_request"
		if r.IsError {
			kind = "api_error"
		}
		fields := map[string]any{
			"model":          r.Model,
			"input":          r.Tokens.Input,
			"output":         r.Tokens.Output,
			"cache_read":     r.Tokens.CacheRead,
			"cache_creation": r.Tokens.CacheCreation,
			"cost_usd":       r.CostUSD,
			"duration_ms":    r.DurationMS,
		}
		if r.IsError {
			fields["status_code"] = r.StatusCode
		}
		p.bus.Publish(LogEvent{TS: r.TS, Kind: kind, Summary: r.Model, Fields: fields})
	}
	for _, d := range b.ToolDecisions {
		if p.eventSeen(d.DedupKey) {
			continue
		}
		p.bus.Publish(LogEvent{TS: d.TS, Kind: "tool_decision", Summary: d.ToolName, Fields: map[string]any{
			"decision":  d.Decision,
			"source":    d.Source,
			"tool_name": d.ToolName,
		}})
	}
	for _, tr := range b.ToolResults {
		if p.eventSeen(tr.DedupKey) {
			continue
		}
		fields := map[string]any{
			"tool_name":   tr.ToolName,
			"success":     tr.Success,
			"duration_ms": tr.DurationMS,
		}
		if tr.BashCommand != "" {
			fields["bash_command"] = tr.BashCommand
		}
		if tr.MCPServer != "" {
			fields["mcp_server"] = tr.MCPServer
		}
		p.bus.Publish(LogEvent{TS: tr.TS, Kind: "tool_result", Summary: tr.ToolName, Fields: fields})
	}
	for _, e := range b.Events {
		if p.eventSeen(e.DedupKey) {
			continue
		}
		// Generic events (user_prompt, mcp_server_connection, unknown names) keep
		// their structured payload only in AttrsJSON. Surface a curated set of
		// kind-relevant fields so the tail/SSE can render a per-kind summary
		// (e.g. user_prompt → prompt_length) instead of an empty object (F6).
		p.bus.Publish(LogEvent{TS: e.TS, Kind: e.Name, Summary: e.Name, Fields: eventFields(e.AttrsJSON)})
	}
}

// eventSeen reports whether a LogEvent has already been published for dedupKey in
// the current session, recording it as seen on the first call. An empty key is
// never recorded and never reported as seen, so a record lacking a dedup_key is
// always published. Caller is the single worker goroutine, so the map needs no
// lock.
func (p *Pipeline) eventSeen(dedupKey string) bool {
	if dedupKey == "" {
		return false
	}
	if _, ok := p.seenEvent[dedupKey]; ok {
		return true
	}
	if p.seenEvent == nil {
		p.seenEvent = make(map[string]struct{})
	}
	p.seenEvent[dedupKey] = struct{}{}
	return false
}

// eventTailKeys is the curated allowlist of attribute keys surfaced onto a
// generic LogEvent's Fields for the live tail / SSE summary (F6). It is
// intentionally small: free-text/sensitive payloads (prompt text, raw commands)
// are NOT included — only summary-friendly scalars. Redaction upstream (C1) also
// ensures sensitive values are already "[redacted]" when --no-store-prompts is
// set, but we exclude them here regardless.
var eventTailKeys = []string{
	"prompt_length",
	"model",
	"cost_usd",
	"tool_name",
	"decision",
	"source",
	"success",
	"server_name",
	"duration_ms",
}

// eventFields decodes a generic event's attrs_json and returns the curated
// subset of summary fields present (F6). A nil/empty/invalid attrs_json yields a
// non-nil empty map so callers always have a usable (never nil) Fields value.
func eventFields(attrsJSON string) map[string]any {
	out := make(map[string]any)
	if attrsJSON == "" {
		return out
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(attrsJSON), &m); err != nil {
		return out
	}
	for _, k := range eventTailKeys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}

// Counters returns a snapshot of the pipeline counters (for /api/health).
func (p *Pipeline) Counters() Counters {
	return Counters{
		Received:          p.received.Load(),
		Accepted:          p.accepted.Load(),
		RejectedPermanent: p.rejectedPermanent.Load(),
		RetrySignaled:     p.retrySignaled.Load(),
		BusDropped:        p.bus.Dropped(),
	}
}

// Shutdown stops accepting new work, drains the queue (committing the buffered
// requests through the writer), and waits for the worker to exit or the context
// deadline, whichever comes first. Idempotent.
func (p *Pipeline) Shutdown(ctx context.Context) error {
	p.stopOnce.Do(func() {
		// Take the write lock so no Submit is mid-send while we flip stopped and
		// close the queue: closing the queue makes the worker drain remaining
		// items then return, and the lock makes the close race-free.
		p.submitMu.Lock()
		p.stopped.Store(true)
		close(p.queue)
		p.submitMu.Unlock()
	})
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
