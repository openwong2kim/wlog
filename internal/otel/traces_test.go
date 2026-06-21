package otel

import (
	"testing"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Golden: span parsing → model.Span with hex ids, times, status, session.
func TestTraces_SpanRawStore(t *testing.T) {
	p := NewParser(Options{})
	tid := hexBytes(t, "0123456789abcdef0123456789abcdef")
	sid := hexBytes(t, "1122334455667788")
	pid := hexBytes(t, "8877665544332211")

	b, rej := p.ParseTraces(tracesReq(resource(kv("session.id", strVal("s1"))),
		span(tid, sid, pid, "POST /v1/messages", 2_000_000_000, 3_000_000_000,
			tracepb.Status_STATUS_CODE_OK,
			kv("session.id", strVal("s1")), kv("http.method", strVal("POST"))),
	))
	if rej.Rejected != 0 {
		t.Fatalf("unexpected reject: %+v", rej)
	}
	if len(b.Spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(b.Spans))
	}
	sp := b.Spans[0]
	if sp.TraceID != "0123456789abcdef0123456789abcdef" || sp.SpanID != "1122334455667788" ||
		sp.ParentSpanID != "8877665544332211" {
		t.Fatalf("hex ids wrong: %+v", sp)
	}
	if sp.Name != "POST /v1/messages" || sp.StartTS != 2000 || sp.EndTS != 3000 {
		t.Fatalf("span fields wrong: %+v", sp)
	}
	if sp.Status != "ok" || sp.SessionID != "s1" {
		t.Fatalf("status/session wrong: %+v", sp)
	}
	if len(b.Sessions) != 1 || b.Sessions[0].HasEvents {
		t.Fatalf("span should upsert session without flipping has_events: %+v", b.Sessions)
	}
}

// Span with error status maps to "error".
func TestTraces_ErrorStatus(t *testing.T) {
	p := NewParser(Options{})
	b, _ := p.ParseTraces(tracesReq(nil,
		span(hexBytes(t, "aa"), hexBytes(t, "bb"), nil, "op", 1_000_000_000, 0,
			tracepb.Status_STATUS_CODE_ERROR),
	))
	if b.Spans[0].Status != "error" {
		t.Fatalf("expected error status, got %q", b.Spans[0].Status)
	}
}

// Span missing both ids is rejected (cannot satisfy UNIQUE identity).
func TestTraces_MissingIDsRejected(t *testing.T) {
	p := NewParser(Options{})
	b, rej := p.ParseTraces(tracesReq(nil,
		span(nil, nil, nil, "no-ids", 1_000_000_000, 2_000_000_000, tracepb.Status_STATUS_CODE_UNSET),
	))
	if len(b.Spans) != 0 {
		t.Fatalf("span with no ids must not be stored, got %+v", b.Spans)
	}
	if rej.Rejected != 1 || rej.Reason != "span_missing_ids" {
		t.Fatalf("expected 1 reject (span_missing_ids), got %+v", rej)
	}
}
