package otel

import (
	"math"
	"time"

	"github.com/openwong2kim/wlog/internal/model"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// ValueKind classifies how a metric point's delta is aggregated downstream
// (PLAN §7 / Eng F9). Stored in model.MetricPoint.ValueKind.
const (
	// ValueKindSum: tokens / cost / lines_of_code / decision counts → summed.
	// active_time is also a Sum metric: its per-point deltas are summed to yield
	// the observed active time (PLAN §7 — delta-sum aggregation, NOT a LAST of a
	// cumulative gauge). The normalizer turns the cumulative counter into
	// per-point deltas and summing them recovers the total active time observed.
	ValueKindSum = 0
	// ValueKindGauge: a genuinely instantaneous gauge reading (a Gauge metric) →
	// not delta-normalized; we store the reading itself as the point value.
	ValueKindGauge = 1
)

// ParseMetrics converts an OTLP metrics export request into a model.Batch.
// Sum data points are routed through the Normalizer (delta temporality →
// per-point deltas) and emitted as MetricPoints; the updated SeriesStates ride
// along in the same batch so the writer persists them atomically. Gauge points
// are recorded as-is (their instantaneous value as the delta). Session identity
// is upserted from datapoint/resource attributes.
//
// Data points that cannot be parsed (no numeric value, no resolvable session,
// or an unsupported metric shape) are counted in RejectInfo rather than
// silently dropped (PLAN §5 partial_success).
func (p *Parser) ParseMetrics(req *collectormetricspb.ExportMetricsServiceRequest) (model.Batch, RejectInfo) {
	var batch model.Batch
	var rej RejectInfo
	if req == nil {
		return batch, rej
	}

	nowMillis := time.Now().UnixMilli()
	sessions := newSessionAccumulator()

	for _, rm := range req.GetResourceMetrics() {
		resAttrs := attrsToMap(resourceAttrs(rm.GetResource()))
		for _, sm := range rm.GetScopeMetrics() {
			for _, metric := range sm.GetMetrics() {
				switch {
				case metric.GetSum() != nil:
					p.parseSum(metric, metric.GetSum(), resAttrs, nowMillis, &batch, &rej, sessions)
				case metric.GetGauge() != nil:
					p.parseGauge(metric, metric.GetGauge(), resAttrs, nowMillis, &batch, &rej, sessions)
				default:
					// Histogram / ExponentialHistogram / Summary are out of M1
					// scope (Claude Code emits counters/gauges). Count their
					// points as rejected so the count is honest.
					rej.add(metricPointCount(metric), "unsupported_metric_type")
				}
			}
		}
	}

	// Guarantee a sessions row for every distinct child SessionID (including the
	// empty/anonymous id) so no metric_point INSERT violates the sessions FK and
	// forces a full-batch rollback (C2).
	sessions.ensureForBatch(&batch)
	return batch, rej
}

// parseSum normalizes every data point of a Sum metric.
func (p *Parser) parseSum(metric *metricspb.Metric, sum *metricspb.Sum, resAttrs map[string]Value, nowMillis int64, batch *model.Batch, rej *RejectInfo, sessions *sessionAccumulator) {
	temporality := int32(sum.GetAggregationTemporality())
	name := metric.GetName()

	for _, dp := range sum.GetDataPoints() {
		dpAttrs := attrsToMap(dp.GetAttributes())
		sid := sessionID(dpAttrs, resAttrs)

		value, ok := numberValue(dp)
		if !ok {
			rej.add(1, "non_numeric_metric_point")
			continue
		}
		// NaN/Inf would poison the delta normalizer's series state and cannot be
		// serialized by encoding/json (it rejects non-finite floats), so reject the
		// point before it touches any state (C10).
		if math.IsNaN(value) || math.IsInf(value, 0) {
			rej.add(1, "non_finite_metric_point")
			continue
		}

		// Series key uses ONLY whitelisted dims (+ session.id resolved into the
		// dim map). Volatile resource attrs are excluded so a restart/version
		// bump does not fork the series.
		dims := extractDims(dpAttrs)
		if sid != "" {
			dims["session.id"] = sid
		}
		seriesKey := model.SeriesKey(name, dims)

		startNano := int64(dp.GetStartTimeUnixNano())
		pointNano := int64(dp.GetTimeUnixNano())

		res := p.norm.normalize(seriesKey, temporality, value, startNano, pointNano, nowMillis)
		if res.stale {
			// Out-of-order point: ignored by design (not a reject — it is a
			// correctly handled duplicate/reorder, not a parse failure).
			continue
		}

		ts := nanoToMillis(uint64(pointNano))
		if ts == 0 {
			ts = nowMillis
		}

		batch.MetricPoints = append(batch.MetricPoints, model.MetricPoint{
			SessionID:     sid,
			TS:            ts,
			Name:          name,
			AttrKey:       primaryAttrKey(dims),
			ValueDelta:    res.delta,
			ValueKind:     ValueKindSum,
			SeriesKey:     seriesKey,
			StartUnixNano: startNano,
			TimeUnixNano:  pointNano,
			AttrsJSON:     marshalAttrs(dp.GetAttributes()),
		})
		batch.SeriesStates = append(batch.SeriesStates, res.state)

		if sid != "" {
			sessions.observe(sid, ts, dpAttrs, resAttrs)
			sessions.markTokenSource(sid, name)
		}
	}
}

// parseGauge records gauge points. Gauges are instantaneous and not subject to
// delta normalization; we store the reading itself as the point value.
func (p *Parser) parseGauge(metric *metricspb.Metric, gauge *metricspb.Gauge, resAttrs map[string]Value, nowMillis int64, batch *model.Batch, rej *RejectInfo, sessions *sessionAccumulator) {
	name := metric.GetName()
	for _, dp := range gauge.GetDataPoints() {
		dpAttrs := attrsToMap(dp.GetAttributes())
		sid := sessionID(dpAttrs, resAttrs)

		value, ok := numberValue(dp)
		if !ok {
			rej.add(1, "non_numeric_metric_point")
			continue
		}
		// Reject non-finite gauge readings: they cannot be JSON-serialized and
		// would corrupt any downstream aggregation (C10).
		if math.IsNaN(value) || math.IsInf(value, 0) {
			rej.add(1, "non_finite_metric_point")
			continue
		}

		dims := extractDims(dpAttrs)
		if sid != "" {
			dims["session.id"] = sid
		}
		seriesKey := model.SeriesKey(name, dims)

		startNano := int64(dp.GetStartTimeUnixNano())
		pointNano := int64(dp.GetTimeUnixNano())
		ts := nanoToMillis(uint64(pointNano))
		if ts == 0 {
			ts = nowMillis
		}

		batch.MetricPoints = append(batch.MetricPoints, model.MetricPoint{
			SessionID:     sid,
			TS:            ts,
			Name:          name,
			AttrKey:       primaryAttrKey(dims),
			ValueDelta:    value,
			ValueKind:     ValueKindGauge,
			SeriesKey:     seriesKey,
			StartUnixNano: startNano,
			TimeUnixNano:  pointNano,
			AttrsJSON:     marshalAttrs(dp.GetAttributes()),
		})

		if sid != "" {
			sessions.observe(sid, ts, dpAttrs, resAttrs)
		}
	}
}

// numberValue extracts the numeric value of a NumberDataPoint, handling the
// AsInt / AsDouble oneof. Returns ok=false when neither is set.
func numberValue(dp *metricspb.NumberDataPoint) (float64, bool) {
	switch v := dp.GetValue().(type) {
	case *metricspb.NumberDataPoint_AsDouble:
		return v.AsDouble, true
	case *metricspb.NumberDataPoint_AsInt:
		return float64(v.AsInt), true
	default:
		return 0, false
	}
}

// metricPointCount totals the data points across a metric's data-bearing field,
// used to attribute reject counts to the right number of dropped points.
func metricPointCount(metric *metricspb.Metric) int {
	switch {
	case metric.GetSum() != nil:
		return len(metric.GetSum().GetDataPoints())
	case metric.GetGauge() != nil:
		return len(metric.GetGauge().GetDataPoints())
	case metric.GetHistogram() != nil:
		return len(metric.GetHistogram().GetDataPoints())
	case metric.GetExponentialHistogram() != nil:
		return len(metric.GetExponentialHistogram().GetDataPoints())
	case metric.GetSummary() != nil:
		return len(metric.GetSummary().GetDataPoints())
	default:
		return 1
	}
}

// primaryAttrKey picks a representative non-session dim for the metric_points
// attr_key column (e.g. token type / model / language). Deterministic.
func primaryAttrKey(dims map[string]string) string {
	for _, k := range []string{"type", "model", "language"} {
		if v, ok := dims[k]; ok && v != "" {
			return v
		}
	}
	return ""
}

// resourceAttrs is a nil-safe accessor for a Resource's attribute list. The
// proto getter already handles a nil receiver, so a typed-nil *Resource is safe.
func resourceAttrs(r *resourcepb.Resource) []*commonpb.KeyValue {
	return r.GetAttributes()
}
