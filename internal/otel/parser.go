package otel

import "github.com/openwong2kim/wlog/internal/model"

// Options configures parser behaviour.
type Options struct {
	// NoStorePrompts, when true, masks sensitive free-text fields before they
	// are persisted: user_prompt bodies/attributes and tool bash/full commands
	// are replaced with a redaction marker (PLAN §13 / --no-store-prompts). The
	// records themselves are still stored (with masked content) so counts and
	// timing stay accurate.
	NoStorePrompts bool
}

// RejectInfo reports data points permanently rejected during parsing (a parse
// failure, not transient backpressure). It feeds the OTLP partial_success
// response and the /api/health rejected_permanent counter (PLAN §5). Rejected
// points are NOT silently dropped: they are counted with a reason.
type RejectInfo struct {
	// Rejected is the number of data points/records that could not be parsed.
	Rejected int
	// Reason is the most recent reject reason (coarse, for diagnostics).
	Reason string
}

// add records n rejected points with a reason.
func (r *RejectInfo) add(n int, reason string) {
	if n <= 0 {
		return
	}
	r.Rejected += n
	if reason != "" {
		r.Reason = reason
	}
}

// Parser converts OTLP export requests into model.Batch values. It owns a
// concurrency-safe delta Normalizer so a single Parser instance can be shared
// across the receiver's concurrent gRPC/HTTP handlers. The three Parse* methods
// are individually safe for concurrent use.
type Parser struct {
	opts Options
	norm *Normalizer
}

// NewParser returns a Parser with a fresh (un-seeded) delta normalizer. Call
// Seed before ingestion to resume series state from the store.
func NewParser(opts Options) *Parser {
	return &Parser{opts: opts, norm: NewNormalizer()}
}

// Seed replays persisted series cursors into the delta normalizer so a restart
// resumes without re-baselining (PLAN §7). It should be called once at startup,
// before any Parse* call, typically with the result of
// model.SeriesStateStore.LoadSeriesStates.
func (p *Parser) Seed(states []model.SeriesState) {
	p.norm.Seed(states)
}

// BeginMetricsTxn starts a delta-normalization transaction. The single ingest
// worker calls this immediately before ParseMetrics so that, if the resulting
// WriteBatch fails, Txn.Rollback can undo the in-memory series-state advances
// and the next data point computes its delta against the last DURABLE state
// (C3 — no permanent delta loss on write failure). On a successful write the
// worker calls Txn.Commit. Only ParseMetrics mutates series state, so logs/
// traces need no transaction.
//
// Transactions must not be interleaved: the caller is a serial worker that
// fully resolves (Commit or Rollback) one batch before beginning the next.
func (p *Parser) BeginMetricsTxn() *Txn { return p.norm.Begin() }
