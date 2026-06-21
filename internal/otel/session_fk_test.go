package otel

import (
	"testing"

	"github.com/openwong2kim/wlog/internal/model"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// C2 regression (logs): a log record with NO resolvable session.id carries
// SessionID "" but MUST still have a matching sessions row (the empty session)
// so the child INSERT does not violate the sessions FK and roll back the whole
// batch. Mixed batch: one record has a session, one does not — both children and
// both session rows (including the empty one) must be present.
func TestC2_LogsSessionlessRecordHasSessionRow(t *testing.T) {
	p := NewParser(Options{})
	b, _ := p.ParseLogs(logsReq(nil,
		// has session
		logRecord("claude_code.user_prompt", 1_000_000_000,
			kv("session.id", strVal("s1")), kv("prompt_length", intVal(5))),
		// NO session.id anywhere → SessionID ""
		logRecord("claude_code.user_prompt", 2_000_000_000,
			kv("prompt_length", intVal(7))),
	))

	if len(b.Events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(b.Events))
	}

	childIDs := map[string]bool{}
	for _, e := range b.Events {
		childIDs[e.SessionID] = true
	}
	if !childIDs[""] {
		t.Fatalf("expected an empty-session child record in this fixture")
	}
	assertSessionsCoverChildren(t, sessionIDSet(b.Sessions), childIDs)

	// The synthesized empty session must carry sane time bounds from its record.
	for _, s := range b.Sessions {
		if s.ID == "" {
			if s.FirstSeen != 2000 || s.LastSeen != 2000 {
				t.Fatalf("empty session should take record ts (2000ms), got first=%d last=%d", s.FirstSeen, s.LastSeen)
			}
		}
	}
}

// C2 regression (metrics): a DELTA metric data point with no session.id
// (SessionID "") must still have a matching empty sessions row.
func TestC2_MetricsSessionlessPointHasSessionRow(t *testing.T) {
	p := NewParser(Options{})
	withS := []*commonpb.KeyValue{sidAttr("s1"), kv("type", strVal("input"))}
	noS := []*commonpb.KeyValue{kv("type", strVal("output"))}
	b, rej := p.ParseMetrics(metricsReq(nil,
		sumMetric("claude_code.token.usage", metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA, true,
			numDP(dp{isInt: true, intValue: 10, startNano: 1000, pointNano: 2000, attrs: withS}),
			numDP(dp{isInt: true, intValue: 20, startNano: 1000, pointNano: 2000, attrs: noS}),
		),
	))
	if rej.Rejected != 0 {
		t.Fatalf("unexpected reject: %+v", rej)
	}

	childIDs := map[string]bool{}
	for _, mp := range b.MetricPoints {
		childIDs[mp.SessionID] = true
	}
	if !childIDs[""] {
		t.Fatalf("expected a sessionless metric point in this fixture, got %+v", b.MetricPoints)
	}
	assertSessionsCoverChildren(t, sessionIDSet(b.Sessions), childIDs)
}

// C2 regression (traces): a span with no session.id (SessionID "") must still
// have a matching empty sessions row.
func TestC2_TracesSessionlessSpanHasSessionRow(t *testing.T) {
	p := NewParser(Options{})
	tid := hexBytes(t, "00112233445566778899aabbccddeeff")
	sid := hexBytes(t, "0011223344556677")
	b, rej := p.ParseTraces(tracesReq(nil,
		span(tid, sid, nil, "no-session-span", 1_000_000_000, 2_000_000_000, tracepb.Status_STATUS_CODE_OK),
	))
	if rej.Rejected != 0 {
		t.Fatalf("unexpected reject: %+v", rej)
	}
	if len(b.Spans) != 1 || b.Spans[0].SessionID != "" {
		t.Fatalf("expected one sessionless span, got %+v", b.Spans)
	}
	if !sessionIDSet(b.Sessions)[""] {
		t.Fatalf("sessionless span has no matching empty sessions row (FK violation): %+v", b.Sessions)
	}
}

// sessionIDSet returns the set of session ids present in the batch's Sessions.
func sessionIDSet(sessions []model.Session) map[string]bool {
	out := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		out[s.ID] = true
	}
	return out
}

// assertSessionsCoverChildren fails if any child SessionID lacks a sessions row.
func assertSessionsCoverChildren(t *testing.T, sessIDs, childIDs map[string]bool) {
	t.Helper()
	for id := range childIDs {
		if !sessIDs[id] {
			t.Fatalf("child SessionID %q has no matching sessions row (FK violation)", id)
		}
	}
}
