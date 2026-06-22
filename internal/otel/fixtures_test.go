package otel

import (
	"encoding/hex"
	"testing"

	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// --- AnyValue / KeyValue builders -----------------------------------------

func strVal(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}
func intVal(i int64) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: i}}
}
func dblVal(f float64) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: f}}
}
func boolVal(b bool) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: b}}
}
func arrVal(vs ...*commonpb.AnyValue) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{
		ArrayValue: &commonpb.ArrayValue{Values: vs},
	}}
}
func kvListVal(kvs ...*commonpb.KeyValue) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{
		KvlistValue: &commonpb.KeyValueList{Values: kvs},
	}}
}
func kv(k string, v *commonpb.AnyValue) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: v}
}

func resource(kvs ...*commonpb.KeyValue) *resourcepb.Resource {
	return &resourcepb.Resource{Attributes: kvs}
}

// --- Metrics builders ------------------------------------------------------

type dp struct {
	value     float64
	isInt     bool
	intValue  int64
	startNano uint64
	pointNano uint64
	attrs     []*commonpb.KeyValue
}

func numDP(d dp) *metricspb.NumberDataPoint {
	p := &metricspb.NumberDataPoint{
		StartTimeUnixNano: d.startNano,
		TimeUnixNano:      d.pointNano,
		Attributes:        d.attrs,
	}
	if d.isInt {
		p.Value = &metricspb.NumberDataPoint_AsInt{AsInt: d.intValue}
	} else {
		p.Value = &metricspb.NumberDataPoint_AsDouble{AsDouble: d.value}
	}
	return p
}

func sumMetric(name string, temporality metricspb.AggregationTemporality, monotonic bool, dps ...*metricspb.NumberDataPoint) *metricspb.Metric {
	return &metricspb.Metric{
		Name: name,
		Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
			AggregationTemporality: temporality,
			IsMonotonic:            monotonic,
			DataPoints:             dps,
		}},
	}
}

func gaugeMetric(name string, dps ...*metricspb.NumberDataPoint) *metricspb.Metric {
	return &metricspb.Metric{
		Name: name,
		Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: dps}},
	}
}

func metricsReq(res *resourcepb.Resource, metrics ...*metricspb.Metric) *collectormetricspb.ExportMetricsServiceRequest {
	return &collectormetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: res,
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Metrics: metrics,
			}},
		}},
	}
}

// --- Logs builders ---------------------------------------------------------

func logRecord(eventName string, timeNano uint64, attrs ...*commonpb.KeyValue) *logspb.LogRecord {
	return &logspb.LogRecord{
		TimeUnixNano: timeNano,
		EventName:    eventName,
		Attributes:   attrs,
	}
}

func logsReq(res *resourcepb.Resource, records ...*logspb.LogRecord) *collectorlogspb.ExportLogsServiceRequest {
	return &collectorlogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			Resource: res,
			ScopeLogs: []*logspb.ScopeLogs{{
				LogRecords: records,
			}},
		}},
	}
}

// --- Traces builders -------------------------------------------------------

func hexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func span(traceID, spanID, parentID []byte, name string, start, end uint64, code tracepb.Status_StatusCode, attrs ...*commonpb.KeyValue) *tracepb.Span {
	return &tracepb.Span{
		TraceId:           traceID,
		SpanId:            spanID,
		ParentSpanId:      parentID,
		Name:              name,
		StartTimeUnixNano: start,
		EndTimeUnixNano:   end,
		Status:            &tracepb.Status{Code: code},
		Attributes:        attrs,
	}
}

func tracesReq(res *resourcepb.Resource, spans ...*tracepb.Span) *collectortracepb.ExportTraceServiceRequest {
	return &collectortracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: res,
			ScopeSpans: []*tracepb.ScopeSpans{{
				Spans: spans,
			}},
		}},
	}
}

// --- shared helpers --------------------------------------------------------

const (
	tDelta = metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
	tCumul = metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE
)
