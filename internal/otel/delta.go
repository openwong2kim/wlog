package otel

import (
	"sync"

	"github.com/openwong2kim/wlog/internal/model"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

// Temporality mirrors the OTLP aggregation temporality enum, narrowed to the
// values the normalizer reasons about. It is persisted in SeriesState.Temporality.
const (
	temporalityUnspecified = int32(metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_UNSPECIFIED)
	temporalityDelta       = int32(metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA)
	temporalityCumulative  = int32(metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE)
)

// Normalizer converts OTLP Sum data points into per-point deltas, applying the
// delta temporality state machine of PLAN §7. It is concurrency-safe: a single
// mutex guards the in-memory series cursor map so that concurrent ingest
// streams (the receiver fans many gRPC/HTTP calls into one parser) observe a
// consistent, serialized view of each series.
//
// State is replayed at startup via Seed (from the persisted series_state table
// through model.SeriesStateStore) so a restart resumes without re-baselining.
type Normalizer struct {
	mu     sync.Mutex
	series map[string]model.SeriesState

	// active is the in-flight transaction recording pre-images of every series
	// key mutated since Begin, so a failed WriteBatch can be rolled back (C3).
	// At most one transaction is active at a time: the ingest pipeline is a
	// single serial worker (see ingest.Pipeline), so a batch is fully
	// parsed→written→committed/rolled-back before the next begins. When nil, the
	// normalizer behaves as before (no recording overhead).
	active *Txn
}

// NewNormalizer returns an empty, ready-to-use Normalizer.
func NewNormalizer() *Normalizer {
	return &Normalizer{series: make(map[string]model.SeriesState)}
}

// Txn captures the pre-image of every series cursor mutated while it is active,
// so the caller can atomically undo the in-memory state changes a parse pass
// made if the corresponding WriteBatch fails (C3). It is not safe for concurrent
// use; the single ingest worker owns it for the duration of one batch.
type Txn struct {
	n *Normalizer
	// prev holds the cursor value each key had BEFORE the first mutation within
	// this transaction. existed reports whether the key was present at all (so
	// Rollback can delete keys that the transaction newly created). A key is
	// recorded at most once (its first mutation), preserving the true pre-image.
	prev    map[string]model.SeriesState
	existed map[string]bool
	done    bool
}

// Begin starts a transaction that records series-state pre-images. Mutations
// made by normalize after Begin (and before Commit/Rollback) can be undone via
// Txn.Rollback. The returned Txn must be ended exactly once with Commit or
// Rollback. Calling Begin while a transaction is already active replaces it
// (the prior recorder is dropped); callers must not interleave transactions.
func (n *Normalizer) Begin() *Txn {
	n.mu.Lock()
	defer n.mu.Unlock()
	tx := &Txn{
		n:       n,
		prev:    make(map[string]model.SeriesState),
		existed: make(map[string]bool),
	}
	n.active = tx
	return tx
}

// recordLocked snapshots the pre-image of key the first time it is mutated under
// this transaction. Caller holds n.mu.
func (n *Normalizer) recordLocked(key string) {
	tx := n.active
	if tx == nil {
		return
	}
	if _, seen := tx.prev[key]; seen {
		return // already captured this key's pre-image in this txn
	}
	prev, ok := n.series[key]
	tx.prev[key] = prev
	tx.existed[key] = ok
}

// Commit finalizes the transaction, confirming the in-memory state changes (the
// corresponding WriteBatch succeeded). After Commit the recorded pre-images are
// discarded. Idempotent: a second Commit/Rollback is a no-op.
func (tx *Txn) Commit() {
	if tx == nil {
		return
	}
	tx.n.mu.Lock()
	defer tx.n.mu.Unlock()
	if tx.done {
		return
	}
	tx.done = true
	if tx.n.active == tx {
		tx.n.active = nil
	}
}

// Rollback restores every series cursor mutated under this transaction to its
// pre-image, undoing the in-memory advance so the next point computes its delta
// against the last DURABLE state (C3 — no permanent delta loss on write
// failure). Idempotent.
func (tx *Txn) Rollback() {
	if tx == nil {
		return
	}
	tx.n.mu.Lock()
	defer tx.n.mu.Unlock()
	if tx.done {
		return
	}
	tx.done = true
	if tx.n.active == tx {
		tx.n.active = nil
	}
	for key, prev := range tx.prev {
		if tx.existed[key] {
			tx.n.series[key] = prev
		} else {
			delete(tx.n.series, key)
		}
	}
}

// Seed loads persisted series cursors, replacing any in-memory state. It is
// intended to run once at startup before ingestion begins. A nil/empty slice
// leaves the normalizer empty (cold start → first observations baseline).
func (n *Normalizer) Seed(states []model.SeriesState) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.series = make(map[string]model.SeriesState, len(states))
	for _, s := range states {
		if s.Key == "" {
			continue
		}
		n.series[s.Key] = s
	}
}

// normResult is the outcome of normalizing a single data point.
type normResult struct {
	// delta is the per-point value to persist (model.MetricPoint.ValueDelta).
	delta float64
	// state is the updated series cursor to include in the batch so the writer
	// can atomically persist it alongside the metric point.
	state model.SeriesState
	// stale is true when the point arrived out of order (point_time <
	// last_point_time) and was ignored; the caller must NOT emit a metric point
	// and must NOT include the (unchanged) state.
	stale bool
}

// normalize applies the §7 state machine for one Sum data point.
//
// Inputs:
//   - seriesKey:   model.SeriesKey(metricName, whitelisted dims)
//   - temporality: OTLP aggregation temporality of the owning Sum
//   - value:       the data point's absolute value (cumulative) or delta value
//   - startNano:   start_time_unix_nano of the point
//   - pointNano:   time_unix_nano of the point
//   - nowMillis:   wall clock for the SeriesState.Updated bookkeeping field
//
// CUMULATIVE rules:
//   - stale:  point_time < last_point_time → ignore (do not mistake reordering
//     for a reset).
//   - reset:  start != last_start OR value < last_value → delta = value
//     (re-baseline; the counter restarted).
//   - normal: delta = value - last_value.
//   - first observation (baseline_known == false): baseline = value, delta = 0,
//     baseline_known ← true (prevents a startup spike). Subsequent points count
//     only deltas.
//
// DELTA rules:
//   - delta = value (idempotency is enforced downstream by the
//     UNIQUE(series_key, start_unixnano, time_unixnano) store constraint).
func (n *Normalizer) normalize(seriesKey string, temporality int32, value float64, startNano, pointNano int64, nowMillis int64) normResult {
	n.mu.Lock()
	defer n.mu.Unlock()

	prev, known := n.series[seriesKey]

	// DELTA temporality: the value already is the per-point delta. We still
	// track the cursor (last_point/last_start) so stale-ordering and idempotency
	// reasoning stay consistent, but delta is taken verbatim.
	if temporality == temporalityDelta {
		next := model.SeriesState{
			Key:               seriesKey,
			LastValue:         value,
			LastStartUnixNano: startNano,
			LastPointUnixNano: pointNano,
			Temporality:       temporalityDelta,
			BaselineKnown:     true,
			Updated:           nowMillis,
		}
		n.recordLocked(seriesKey)
		n.series[seriesKey] = next
		return normResult{delta: value, state: next}
	}

	// Treat UNSPECIFIED as CUMULATIVE: Claude Code counters are monotonic
	// cumulative sums, and assuming cumulative is the safe (no-spike) default.

	// First observation for this series → establish baseline, emit delta 0.
	if !known || !prev.BaselineKnown {
		next := model.SeriesState{
			Key:               seriesKey,
			LastValue:         value,
			LastStartUnixNano: startNano,
			LastPointUnixNano: pointNano,
			Temporality:       temporalityCumulative,
			BaselineKnown:     true,
			Updated:           nowMillis,
		}
		n.recordLocked(seriesKey)
		n.series[seriesKey] = next
		return normResult{delta: 0, state: next}
	}

	// Stale / out-of-order: a point older than the last accepted point must not
	// re-baseline the series. Ignore it (no metric point, no state change).
	if pointNano < prev.LastPointUnixNano {
		return normResult{stale: true}
	}

	var delta float64
	switch {
	case startNano != prev.LastStartUnixNano || value < prev.LastValue:
		// Reset: the counter restarted (new collection start window or the
		// absolute value went backwards). The current value is itself the delta
		// for this window; re-baseline from here.
		delta = value
	default:
		// Normal monotonic progression.
		delta = value - prev.LastValue
	}

	next := model.SeriesState{
		Key:               seriesKey,
		LastValue:         value,
		LastStartUnixNano: startNano,
		LastPointUnixNano: pointNano,
		Temporality:       temporalityCumulative,
		BaselineKnown:     true,
		Updated:           nowMillis,
	}
	n.recordLocked(seriesKey)
	n.series[seriesKey] = next
	return normResult{delta: delta, state: next}
}

// snapshot returns a copy of the current series state for a key (test/debug
// aid). The bool reports presence.
func (n *Normalizer) snapshot(key string) (model.SeriesState, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	s, ok := n.series[key]
	return s, ok
}
