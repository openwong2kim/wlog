package otel

import (
	"encoding/hex"
	"time"

	"github.com/openwong2kim/wlog/internal/model"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// ParseTraces converts an OTLP traces export request into a model.Batch of raw
// spans (PLAN §6.4 — M1 stores spans raw; visual timeline linking is M2).
// trace_id / span_id / parent_span_id are lowercase hex. Status is the textual
// status code. Sessions are upserted from span/resource attributes (spans do
// not flip HasEvents, which tracks log events specifically).
//
// Spans missing both trace_id and span_id cannot satisfy the store's
// UNIQUE(trace_id, span_id) identity and are counted in RejectInfo.
func (p *Parser) ParseTraces(req *collectortracepb.ExportTraceServiceRequest) (model.Batch, RejectInfo) {
	var batch model.Batch
	var rej RejectInfo
	if req == nil {
		return batch, rej
	}

	nowMillis := time.Now().UnixMilli()
	sessions := newSessionAccumulator()

	for _, rs := range req.GetResourceSpans() {
		resAttrs := attrsToMap(resourceAttrs(rs.GetResource()))
		for _, ss := range rs.GetScopeSpans() {
			for _, span := range ss.GetSpans() {
				if span == nil {
					continue
				}
				traceID := hexID(span.GetTraceId())
				spanID := hexID(span.GetSpanId())
				if traceID == "" && spanID == "" {
					rej.add(1, "span_missing_ids")
					continue
				}

				attrs := attrsToMap(span.GetAttributes())
				sid := sessionID(attrs, resAttrs)

				startTS := nanoToMillis(span.GetStartTimeUnixNano())
				endTS := nanoToMillis(span.GetEndTimeUnixNano())
				ts := startTS
				if ts == 0 {
					ts = endTS
				}
				if ts == 0 {
					ts = nowMillis
				}

				batch.Spans = append(batch.Spans, model.Span{
					TraceID:      traceID,
					SpanID:       spanID,
					ParentSpanID: hexID(span.GetParentSpanId()),
					SessionID:    sid,
					Name:         span.GetName(),
					StartTS:      startTS,
					EndTS:        endTS,
					Status:       statusText(span.GetStatus()),
					AttrsJSON:    marshalAttrs(span.GetAttributes()),
				})

				if sid != "" {
					sessions.observe(sid, ts, attrs, resAttrs)
				}
			}
		}
	}

	// Guarantee a sessions row for every distinct child SessionID (including the
	// empty/anonymous id) so no span INSERT violates the sessions FK and forces a
	// full-batch rollback (C2).
	sessions.ensureForBatch(&batch)
	return batch, rej
}

// hexID renders an OTLP binary id (trace/span) as lowercase hex. Empty input
// yields "".
func hexID(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return hex.EncodeToString(b)
}

// statusText maps an OTLP span Status to a stable textual code.
func statusText(s *tracepb.Status) string {
	switch s.GetCode() {
	case tracepb.Status_STATUS_CODE_OK:
		return "ok"
	case tracepb.Status_STATUS_CODE_ERROR:
		return "error"
	default:
		return "unset"
	}
}
