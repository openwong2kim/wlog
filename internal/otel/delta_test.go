package otel

import (
	"sync"
	"testing"

	"github.com/openwong2kim/wlog/internal/model"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

// session attr used across delta tests (so series carry a session.id dim).
func sidAttr(id string) *commonpb.KeyValue { return kv("session.id", strVal(id)) }

// collectPoints runs ParseMetrics and returns the metric points for a given
// metric name, in arrival order.
func pointsFor(b model.Batch, name string) []model.MetricPoint {
	var out []model.MetricPoint
	for _, mp := range b.MetricPoints {
		if mp.Name == name {
			out = append(out, mp)
		}
	}
	return out
}

// Golden: CUMULATIVE normal progression → first point baselines (delta 0),
// subsequent points emit cur-last.
func TestDelta_CumulativeNormal(t *testing.T) {
	p := NewParser(Options{})
	name := "claude_code.token.usage"

	// First observation: cumulative 100 → baseline, delta 0.
	b1, rej1 := p.ParseMetrics(metricsReq(nil,
		sumMetric(name, tCumul, true,
			numDP(dp{isInt: true, intValue: 100, startNano: 1000, pointNano: 2000,
				attrs: []*commonpb.KeyValue{sidAttr("s1"), kv("type", strVal("input"))}}),
		),
	))
	if rej1.Rejected != 0 {
		t.Fatalf("unexpected reject: %+v", rej1)
	}
	pts := pointsFor(b1, name)
	if len(pts) != 1 || pts[0].ValueDelta != 0 {
		t.Fatalf("first observation must baseline to delta 0, got %+v", pts)
	}

	// Next cumulative 150 → delta 50.
	b2, _ := p.ParseMetrics(metricsReq(nil,
		sumMetric(name, tCumul, true,
			numDP(dp{isInt: true, intValue: 150, startNano: 1000, pointNano: 3000,
				attrs: []*commonpb.KeyValue{sidAttr("s1"), kv("type", strVal("input"))}}),
		),
	))
	pts = pointsFor(b2, name)
	if len(pts) != 1 || pts[0].ValueDelta != 50 {
		t.Fatalf("expected delta 50, got %+v", pts)
	}

	// Next cumulative 175 → delta 25.
	b3, _ := p.ParseMetrics(metricsReq(nil,
		sumMetric(name, tCumul, true,
			numDP(dp{isInt: true, intValue: 175, startNano: 1000, pointNano: 4000,
				attrs: []*commonpb.KeyValue{sidAttr("s1"), kv("type", strVal("input"))}}),
		),
	))
	pts = pointsFor(b3, name)
	if len(pts) != 1 || pts[0].ValueDelta != 25 {
		t.Fatalf("expected delta 25, got %+v", pts)
	}
}

// Golden: DELTA temporality → delta = value verbatim.
func TestDelta_DeltaTemporality(t *testing.T) {
	p := NewParser(Options{})
	name := "claude_code.token.usage"

	for i, want := range []float64{10, 20, 5} {
		b, _ := p.ParseMetrics(metricsReq(nil,
			sumMetric(name, tDelta, true,
				numDP(dp{isInt: true, intValue: int64(want), startNano: 1000, pointNano: uint64(2000 + i*1000),
					attrs: []*commonpb.KeyValue{sidAttr("s1")}}),
			),
		))
		pts := pointsFor(b, name)
		if len(pts) != 1 || pts[0].ValueDelta != want {
			t.Fatalf("DELTA point %d: want %v got %+v", i, want, pts)
		}
	}
}

// Golden: reset by start-time change → delta = current value (re-baseline).
func TestDelta_ResetByStartChange(t *testing.T) {
	p := NewParser(Options{})
	name := "cost.usage"
	at := []*commonpb.KeyValue{sidAttr("s1"), kv("model", strVal("opus"))}

	// baseline 100 @ start=1000
	p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 100, startNano: 1000, pointNano: 2000, attrs: at}))))
	// normal 120 @ start=1000 → delta 20
	b2, _ := p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 120, startNano: 1000, pointNano: 3000, attrs: at}))))
	if pointsFor(b2, name)[0].ValueDelta != 20 {
		t.Fatalf("expected 20 before reset, got %v", pointsFor(b2, name)[0].ValueDelta)
	}
	// reset: new start window 5000, value 30 → delta 30 (NOT 30-120 negative)
	b3, _ := p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 30, startNano: 5000, pointNano: 6000, attrs: at}))))
	if got := pointsFor(b3, name)[0].ValueDelta; got != 30 {
		t.Fatalf("reset(start change) expected delta 30, got %v", got)
	}
}

// Golden: reset by counter going backwards (cur < last) → delta = current value.
func TestDelta_ResetByDecrease(t *testing.T) {
	p := NewParser(Options{})
	name := "lines_of_code.count"
	at := []*commonpb.KeyValue{sidAttr("s1"), kv("type", strVal("added"))}

	p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 500, startNano: 1000, pointNano: 2000, attrs: at})))) // baseline
	// cur 200 < last 500, same start → reset, delta 200
	b, _ := p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 200, startNano: 1000, pointNano: 3000, attrs: at}))))
	if got := pointsFor(b, name)[0].ValueDelta; got != 200 {
		t.Fatalf("reset(decrease) expected delta 200, got %v", got)
	}
}

// Golden: stale point (point_time < last_point_time) is ignored — no metric
// point emitted, series cursor unchanged.
func TestDelta_StaleIgnored(t *testing.T) {
	p := NewParser(Options{})
	name := "claude_code.token.usage"
	at := []*commonpb.KeyValue{sidAttr("s1"), kv("type", strVal("output"))}

	p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 100, startNano: 1000, pointNano: 5000, attrs: at})))) // baseline @ t=5000
	p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 150, startNano: 1000, pointNano: 6000, attrs: at})))) // delta 50 @ t=6000

	// Stale: point_time 4000 < last 6000 → ignored.
	b, rej := p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 130, startNano: 1000, pointNano: 4000, attrs: at}))))
	if len(pointsFor(b, name)) != 0 {
		t.Fatalf("stale point must NOT emit a metric point, got %+v", pointsFor(b, name))
	}
	if len(b.SeriesStates) != 0 {
		t.Fatalf("stale point must not update series state, got %+v", b.SeriesStates)
	}
	if rej.Rejected != 0 {
		t.Fatalf("stale is not a reject, got %+v", rej)
	}

	// Sanity: a fresh in-order point still produces the correct delta from 150.
	b2, _ := p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 200, startNano: 1000, pointNano: 7000, attrs: at}))))
	if got := pointsFor(b2, name)[0].ValueDelta; got != 50 {
		t.Fatalf("after stale, cursor should be 150; delta to 200 = 50, got %v", got)
	}
}

// Golden: first observation baselines to delta 0 (spike prevention) and marks
// baseline_known.
func TestDelta_FirstObservationBaseline(t *testing.T) {
	p := NewParser(Options{})
	name := "claude_code.token.usage"

	b, _ := p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 99999, startNano: 1000, pointNano: 2000,
			attrs: []*commonpb.KeyValue{sidAttr("s1")}}))))
	pts := pointsFor(b, name)
	if len(pts) != 1 || pts[0].ValueDelta != 0 {
		t.Fatalf("first observation must be delta 0 regardless of magnitude, got %+v", pts)
	}
	if len(b.SeriesStates) != 1 || !b.SeriesStates[0].BaselineKnown {
		t.Fatalf("baseline_known must be set, got %+v", b.SeriesStates)
	}
	if b.SeriesStates[0].LastValue != 99999 {
		t.Fatalf("baseline last_value should be 99999, got %v", b.SeriesStates[0].LastValue)
	}
}

// Golden: changing ONLY a volatile resource attr (service.version) must keep the
// same series key (no count explosion). Series key uses whitelist dims only.
func TestDelta_ResourceAttrChangeSameSeries(t *testing.T) {
	p := NewParser(Options{})
	name := "claude_code.token.usage"
	dpAttrs := []*commonpb.KeyValue{sidAttr("s1"), kv("type", strVal("input"))}

	// Observation 1 with service.version=1.0.0 on the RESOURCE.
	res1 := resource(kv("service.version", strVal("1.0.0")), kv("user.email", strVal("a@b.c")))
	b1, _ := p.ParseMetrics(metricsReq(res1, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 100, startNano: 1000, pointNano: 2000, attrs: dpAttrs}))))
	key1 := pointsFor(b1, name)[0].SeriesKey

	// Observation 2 with a DIFFERENT service.version + email (volatile) → must
	// be the SAME series, so cumulative 150 yields a clean delta 50 (not a
	// re-baseline spike).
	res2 := resource(kv("service.version", strVal("9.9.9")), kv("user.email", strVal("x@y.z")))
	b2, _ := p.ParseMetrics(metricsReq(res2, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 150, startNano: 1000, pointNano: 3000, attrs: dpAttrs}))))
	key2 := pointsFor(b2, name)[0].SeriesKey

	if key1 != key2 {
		t.Fatalf("series key must be identical across volatile resource attr change:\n  %s\n  %s", key1, key2)
	}
	if got := pointsFor(b2, name)[0].ValueDelta; got != 50 {
		t.Fatalf("same series → expected clean delta 50, got %v (count explosion!)", got)
	}
}

// Golden: multiple distinct series in one request are normalized independently.
func TestDelta_MultiSeries(t *testing.T) {
	p := NewParser(Options{})
	name := "claude_code.token.usage"

	mk := func(typ string, v int64, pt uint64) *metricspb.NumberDataPoint {
		return numDP(dp{isInt: true, intValue: v, startNano: 1000, pointNano: pt,
			attrs: []*commonpb.KeyValue{sidAttr("s1"), kv("type", strVal(typ))}})
	}

	// baselines for two series (type=input, type=output).
	p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true, mk("input", 100, 2000), mk("output", 10, 2000))))
	// next: input 130 (delta 30), output 25 (delta 15).
	b, _ := p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true, mk("input", 130, 3000), mk("output", 25, 3000))))

	got := map[string]float64{}
	for _, mp := range pointsFor(b, name) {
		got[mp.AttrKey] = mp.ValueDelta
	}
	if got["input"] != 30 || got["output"] != 15 {
		t.Fatalf("multi-series independence broken: %+v", got)
	}
}

// Golden: re-sent DELTA points carry identical (series_key, start, time) so the
// store UNIQUE constraint dedups. The parser must produce identical keys/coords
// for the duplicate (idempotency at the normalization layer).
func TestDelta_ResendIdempotentCoordinates(t *testing.T) {
	p := NewParser(Options{})
	name := "claude_code.token.usage"
	at := []*commonpb.KeyValue{sidAttr("s1"), kv("type", strVal("input"))}

	mk := func() *collectormetricspb.ExportMetricsServiceRequest {
		return metricsReq(nil, sumMetric(name, tDelta, true,
			numDP(dp{isInt: true, intValue: 42, startNano: 1000, pointNano: 2000, attrs: at})))
	}
	b1, _ := p.ParseMetrics(mk())
	b2, _ := p.ParseMetrics(mk()) // re-send

	a, c := pointsFor(b1, name)[0], pointsFor(b2, name)[0]
	if a.SeriesKey != c.SeriesKey || a.StartUnixNano != c.StartUnixNano || a.TimeUnixNano != c.TimeUnixNano {
		t.Fatalf("re-send must yield identical dedup coordinates:\n %+v\n %+v", a, c)
	}
	if a.ValueDelta != 42 || c.ValueDelta != 42 {
		t.Fatalf("DELTA re-send values should both be 42, got %v %v", a.ValueDelta, c.ValueDelta)
	}
}

// Seed restores series state so a "restart" resumes deltas without re-baselining.
func TestDelta_SeedResumesWithoutRebaseline(t *testing.T) {
	p1 := NewParser(Options{})
	name := "claude_code.token.usage"
	at := []*commonpb.KeyValue{sidAttr("s1"), kv("type", strVal("input"))}

	p1.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 100, startNano: 1000, pointNano: 2000, attrs: at}))))
	b, _ := p1.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 150, startNano: 1000, pointNano: 3000, attrs: at}))))
	state := b.SeriesStates[len(b.SeriesStates)-1]

	// New parser (simulated restart) seeded from persisted state.
	p2 := NewParser(Options{})
	p2.Seed([]model.SeriesState{state})

	// Cumulative 200 should yield delta 50 (resumed from last_value 150), NOT a
	// fresh baseline of delta 0.
	b2, _ := p2.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 200, startNano: 1000, pointNano: 4000, attrs: at}))))
	if got := pointsFor(b2, name)[0].ValueDelta; got != 50 {
		t.Fatalf("seeded parser should resume: expected delta 50, got %v", got)
	}
}

// C3 regression: a transaction that is rolled back must restore the in-memory
// series cursor to its pre-image, so the NEXT point computes its delta against
// the last DURABLE (committed) state instead of a phantom advanced state. This
// models a failed WriteBatch: the cursor advance must be undone or the delta is
// permanently lost.
func TestDelta_TxnRollbackRestoresState(t *testing.T) {
	p := NewParser(Options{})
	name := "claude_code.token.usage"
	at := []*commonpb.KeyValue{sidAttr("s1"), kv("type", strVal("input"))}

	// Committed baseline: cumulative 100 (delta 0). Begin+commit a txn around it.
	tx0 := p.BeginMetricsTxn()
	p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 100, startNano: 1000, pointNano: 2000, attrs: at}))))
	tx0.Commit()

	// Next batch: cumulative 150 (delta 50) is parsed, advancing the cursor to
	// last_value 150 — but the write FAILS, so we roll back.
	tx1 := p.BeginMetricsTxn()
	b1, _ := p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 150, startNano: 1000, pointNano: 3000, attrs: at}))))
	if got := pointsFor(b1, name)[0].ValueDelta; got != 50 {
		t.Fatalf("pre-rollback delta should be 50, got %v", got)
	}
	tx1.Rollback() // simulate WriteBatch failure → undo the cursor advance

	// Re-send the SAME cumulative 150 (the OTLP client retries). With rollback the
	// cursor is back at 100, so the delta is again 50 — NOT 0 (which would be the
	// loss bug: 150-150 against the phantom advanced state).
	tx2 := p.BeginMetricsTxn()
	b2, _ := p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 150, startNano: 1000, pointNano: 3000, attrs: at}))))
	tx2.Commit()
	if got := pointsFor(b2, name)[0].ValueDelta; got != 50 {
		t.Fatalf("after rollback, retried point must re-derive delta 50 (no loss), got %v", got)
	}

	// And a subsequent in-order point (cumulative 200) yields delta 50 from 150.
	tx3 := p.BeginMetricsTxn()
	b3, _ := p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 200, startNano: 1000, pointNano: 4000, attrs: at}))))
	tx3.Commit()
	if got := pointsFor(b3, name)[0].ValueDelta; got != 50 {
		t.Fatalf("post-recovery delta should be 50 (200-150), got %v", got)
	}
}

// C3 regression: rolling back a txn that FIRST OBSERVED a series (created the
// cursor) must DELETE the cursor, so a re-sent first point baselines again
// (delta 0) rather than being treated as an established series.
func TestDelta_TxnRollbackDeletesNewlyCreatedSeries(t *testing.T) {
	p := NewParser(Options{})
	name := "claude_code.token.usage"
	at := []*commonpb.KeyValue{sidAttr("brand-new"), kv("type", strVal("input"))}

	tx := p.BeginMetricsTxn()
	b, _ := p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 42, startNano: 1000, pointNano: 2000, attrs: at}))))
	if pointsFor(b, name)[0].ValueDelta != 0 {
		t.Fatalf("first observation should baseline to delta 0")
	}
	key := b.SeriesStates[0].Key
	if _, ok := p.norm.snapshot(key); !ok {
		t.Fatalf("series should exist after first observation")
	}
	tx.Rollback()

	if _, ok := p.norm.snapshot(key); ok {
		t.Fatalf("rollback of a newly-created series must delete its cursor")
	}

	// Re-send the same first point: must baseline again (delta 0), proving the
	// series was truly removed (not lingering as an established cursor).
	tx2 := p.BeginMetricsTxn()
	b2, _ := p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 42, startNano: 1000, pointNano: 2000, attrs: at}))))
	tx2.Commit()
	if pointsFor(b2, name)[0].ValueDelta != 0 {
		t.Fatalf("after rollback, re-sent first point must baseline to delta 0 again")
	}
}

// C3: Commit confirms the advance (the opposite of Rollback) — a sanity guard
// that committing does NOT undo state.
func TestDelta_TxnCommitKeepsState(t *testing.T) {
	p := NewParser(Options{})
	name := "claude_code.token.usage"
	at := []*commonpb.KeyValue{sidAttr("s1"), kv("type", strVal("input"))}

	tx0 := p.BeginMetricsTxn()
	p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 100, startNano: 1000, pointNano: 2000, attrs: at}))))
	tx0.Commit()

	tx1 := p.BeginMetricsTxn()
	p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 150, startNano: 1000, pointNano: 3000, attrs: at}))))
	tx1.Commit() // committed → state stays at 150

	tx2 := p.BeginMetricsTxn()
	b, _ := p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
		numDP(dp{isInt: true, intValue: 175, startNano: 1000, pointNano: 4000, attrs: at}))))
	tx2.Commit()
	if got := pointsFor(b, name)[0].ValueDelta; got != 25 {
		t.Fatalf("after commit, delta should be 25 (175-150), got %v", got)
	}
}

// Concurrency: many goroutines normalizing distinct series must not race
// (run with -race). Each series is independent so deltas stay correct.
func TestDelta_ConcurrentDistinctSeries(t *testing.T) {
	p := NewParser(Options{})
	name := "claude_code.token.usage"

	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			sid := sidAttr("sess-" + string(rune('A'+id)))
			at := []*commonpb.KeyValue{sid, kv("type", strVal("input"))}
			// baseline then a delta
			p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
				numDP(dp{isInt: true, intValue: 100, startNano: 1000, pointNano: 2000, attrs: at}))))
			p.ParseMetrics(metricsReq(nil, sumMetric(name, tCumul, true,
				numDP(dp{isInt: true, intValue: 110, startNano: 1000, pointNano: 3000, attrs: at}))))
		}(w)
	}
	wg.Wait()
}
