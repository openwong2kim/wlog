package otel

import (
	"encoding/json"
	"math"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

// Non-numeric data point (no AsInt/AsDouble) is rejected, not silently dropped.
func TestMetrics_RejectNonNumeric(t *testing.T) {
	p := NewParser(Options{})
	// hand-build a Sum with a data point that has no value oneof set.
	bad := &metricspb.NumberDataPoint{TimeUnixNano: 2000, Attributes: []*commonpb.KeyValue{sidAttr("s1")}}
	req := metricsReq(nil, &metricspb.Metric{
		Name: "claude_code.token.usage",
		Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
			AggregationTemporality: tCumul, DataPoints: []*metricspb.NumberDataPoint{bad},
		}},
	})
	b, rej := p.ParseMetrics(req)
	if len(b.MetricPoints) != 0 {
		t.Fatalf("non-numeric dp must not emit a point")
	}
	if rej.Rejected != 1 || rej.Reason != "non_numeric_metric_point" {
		t.Fatalf("expected 1 reject, got %+v", rej)
	}
}

// Histogram (unsupported in M1) → its points are counted as rejected.
func TestMetrics_RejectUnsupportedType(t *testing.T) {
	p := NewParser(Options{})
	req := metricsReq(nil, &metricspb.Metric{
		Name: "some.histogram",
		Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{
			DataPoints: []*metricspb.HistogramDataPoint{{}, {}},
		}},
	})
	_, rej := p.ParseMetrics(req)
	if rej.Rejected != 2 || rej.Reason != "unsupported_metric_type" {
		t.Fatalf("expected 2 rejects for histogram, got %+v", rej)
	}
}

// Gauge points are recorded with the reading as the value and ValueKindGauge.
func TestMetrics_Gauge(t *testing.T) {
	p := NewParser(Options{})
	b, _ := p.ParseMetrics(metricsReq(nil,
		gaugeMetric("active_time.total",
			numDP(dp{value: 12.5, startNano: 1000, pointNano: 2000,
				attrs: []*commonpb.KeyValue{sidAttr("s1")}}),
		),
	))
	pts := pointsFor(b, "active_time.total")
	if len(pts) != 1 || pts[0].ValueDelta != 12.5 || pts[0].ValueKind != ValueKindGauge {
		t.Fatalf("gauge handling wrong: %+v", pts)
	}
}

// Mixed axis: a session can have api_request token events AND active_time
// metrics. The metrics path must NOT pin token_source to "events" (that is the
// event path's job) — here only metrics are present so token_source stays
// "metrics" for a token metric. This guards against double-counting wiring.
func TestMetrics_TokenSourceMetricsFallback(t *testing.T) {
	p := NewParser(Options{})
	// token.usage metric for s1, plus baseline then delta
	at := []*commonpb.KeyValue{sidAttr("s1"), kv("type", strVal("input"))}
	p.ParseMetrics(metricsReq(nil, sumMetric("claude_code.token.usage", tCumul, true,
		numDP(dp{isInt: true, intValue: 100, startNano: 1000, pointNano: 2000, attrs: at}))))
	b, _ := p.ParseMetrics(metricsReq(nil, sumMetric("claude_code.token.usage", tCumul, true,
		numDP(dp{isInt: true, intValue: 150, startNano: 1000, pointNano: 3000, attrs: at}))))
	if len(b.Sessions) != 1 || b.Sessions[0].TokenSource != "metrics" {
		t.Fatalf("token metric should set token_source=metrics, got %+v", b.Sessions)
	}
	if b.Sessions[0].HasEvents {
		t.Fatalf("metrics-only session must not have has_events")
	}
}

// C10 regression: NaN / +Inf / -Inf metric data points must be REJECTED (not
// silently dropped, not stored). They must not touch the normalizer's series
// state, and a valid neighboring point in the same series must still normalize
// correctly afterward (state uncorrupted).
func TestMetrics_RejectNonFiniteSum(t *testing.T) {
	for _, tc := range []struct {
		name string
		val  float64
	}{
		{"nan", math.NaN()},
		{"posinf", math.Inf(1)},
		{"neginf", math.Inf(-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewParser(Options{})
			at := []*commonpb.KeyValue{sidAttr("s1"), kv("type", strVal("input"))}
			// baseline a real point so the series has known state.
			p.ParseMetrics(metricsReq(nil, sumMetric("claude_code.token.usage", tCumul, true,
				numDP(dp{value: 100, startNano: 1000, pointNano: 2000, attrs: at}))))

			// Inject the non-finite point.
			b, rej := p.ParseMetrics(metricsReq(nil, sumMetric("claude_code.token.usage", tCumul, true,
				numDP(dp{value: tc.val, startNano: 1000, pointNano: 3000, attrs: at}))))
			if len(pointsFor(b, "claude_code.token.usage")) != 0 {
				t.Fatalf("non-finite point must not emit a metric point")
			}
			if len(b.SeriesStates) != 0 {
				t.Fatalf("non-finite point must not produce a series state")
			}
			if rej.Rejected != 1 || rej.Reason != "non_finite_metric_point" {
				t.Fatalf("expected 1 non-finite reject, got %+v", rej)
			}

			// State uncorrupted: next valid cumulative 150 → delta 50 (from 100).
			b2, _ := p.ParseMetrics(metricsReq(nil, sumMetric("claude_code.token.usage", tCumul, true,
				numDP(dp{value: 150, startNano: 1000, pointNano: 4000, attrs: at}))))
			pts := pointsFor(b2, "claude_code.token.usage")
			if len(pts) != 1 || pts[0].ValueDelta != 50 {
				t.Fatalf("series state corrupted by non-finite point: %+v", pts)
			}
			// The emitted delta must be JSON-serializable (encoding/json rejects
			// non-finite); this guards against a poisoned delta sneaking through.
			if _, err := json.Marshal(pts[0].ValueDelta); err != nil {
				t.Fatalf("emitted delta not JSON-serializable: %v", err)
			}
		})
	}
}

// C10 regression: a non-finite GAUGE reading is rejected too.
func TestMetrics_RejectNonFiniteGauge(t *testing.T) {
	p := NewParser(Options{})
	b, rej := p.ParseMetrics(metricsReq(nil,
		gaugeMetric("active_time.total",
			numDP(dp{value: math.NaN(), startNano: 1000, pointNano: 2000,
				attrs: []*commonpb.KeyValue{sidAttr("s1")}}),
		),
	))
	if len(pointsFor(b, "active_time.total")) != 0 {
		t.Fatalf("non-finite gauge must not emit a metric point")
	}
	if rej.Rejected != 1 || rej.Reason != "non_finite_metric_point" {
		t.Fatalf("expected 1 non-finite gauge reject, got %+v", rej)
	}
}

// active_time gauge / non-token metric must NOT set token_source.
func TestMetrics_NonTokenMetricNoSource(t *testing.T) {
	p := NewParser(Options{})
	at := []*commonpb.KeyValue{sidAttr("s1")}
	p.ParseMetrics(metricsReq(nil, sumMetric("active_time.total", tCumul, true,
		numDP(dp{isInt: true, intValue: 10, startNano: 1000, pointNano: 2000, attrs: at}))))
	b, _ := p.ParseMetrics(metricsReq(nil, sumMetric("active_time.total", tCumul, true,
		numDP(dp{isInt: true, intValue: 20, startNano: 1000, pointNano: 3000, attrs: at}))))
	if b.Sessions[0].TokenSource != "" {
		t.Fatalf("active_time must not set token_source, got %q", b.Sessions[0].TokenSource)
	}
}
