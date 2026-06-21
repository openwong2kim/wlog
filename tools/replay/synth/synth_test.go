package synth

import (
	"testing"

	"github.com/openwong2kim/wlog/internal/otel"
	"google.golang.org/protobuf/proto"
)

// TestGenerateDeterministic verifies Generate is reproducible: the same Options
// produce byte-identical marshaled OTLP (the integration tests rely on this).
func TestGenerateDeterministic(t *testing.T) {
	a := Generate(DefaultOptions())
	b := Generate(DefaultOptions())

	if len(a.Logs) != len(b.Logs) || len(a.Metrics) != len(b.Metrics) || len(a.Traces) != len(b.Traces) {
		t.Fatalf("signal counts differ between runs")
	}
	for i := range a.Logs {
		x, _ := proto.Marshal(a.Logs[i])
		y, _ := proto.Marshal(b.Logs[i])
		if string(x) != string(y) {
			t.Errorf("logs[%d] not deterministic", i)
		}
	}
	for i := range a.Metrics {
		x, _ := proto.Marshal(a.Metrics[i])
		y, _ := proto.Marshal(b.Metrics[i])
		if string(x) != string(y) {
			t.Errorf("metrics[%d] not deterministic", i)
		}
	}
}

// TestGenerateSessionsCount verifies the requested number of sessions is
// produced with distinct, prefixed ids.
func TestGenerateSessionsCount(t *testing.T) {
	opts := DefaultOptions()
	opts.Sessions = 4
	opts.SessionIDPrefix = "test-"
	b := Generate(opts)
	if len(b.Totals) != 4 {
		t.Fatalf("totals = %d, want 4", len(b.Totals))
	}
	seen := map[string]bool{}
	for _, tot := range b.Totals {
		if seen[tot.SessionID] {
			t.Fatalf("duplicate session id %q", tot.SessionID)
		}
		seen[tot.SessionID] = true
		if tot.SessionID[:5] != "test-" {
			t.Errorf("session id %q missing prefix", tot.SessionID)
		}
	}
}

// TestTotalsMatchParser is the keystone check that the generator's declared
// ground truth (SessionTotals) agrees with what the REAL otel parser extracts.
// If this drifts, every integration assertion built on Totals is invalid — so
// this guards the harness itself.
func TestTotalsMatchParser(t *testing.T) {
	b := Generate(DefaultOptions())
	p := otel.NewParser(otel.Options{})

	logsBatch, _ := p.ParseLogs(b.Logs[0])
	metBatch, _ := p.ParseMetrics(b.Metrics[0])
	traceBatch, _ := p.ParseTraces(b.Traces[0])
	want := b.Totals[0]

	var cost float64
	var in, out, cr, cc int64
	var apiErrors, prompts int64
	for _, r := range logsBatch.APIRequests {
		if r.IsError {
			apiErrors++
			continue
		}
		cost += r.CostUSD
		in += r.Tokens.Input
		out += r.Tokens.Output
		cr += r.Tokens.CacheRead
		cc += r.Tokens.CacheCreation
	}
	for _, e := range logsBatch.Events {
		if e.Name == "user_prompt" {
			prompts++
		}
	}
	if in != want.Input || out != want.Output || cr != want.CacheRead || cc != want.CacheCreation {
		t.Errorf("token totals parser(in=%d out=%d cr=%d cc=%d) != declared(%d/%d/%d/%d)",
			in, out, cr, cc, want.Input, want.Output, want.CacheRead, want.CacheCreation)
	}
	if !floatNear(cost, want.CostUSD) {
		t.Errorf("cost parser=%v declared=%v", cost, want.CostUSD)
	}
	if apiErrors != want.APIErrors {
		t.Errorf("api_errors parser=%d declared=%d", apiErrors, want.APIErrors)
	}
	if prompts != want.Prompts {
		t.Errorf("prompts parser=%d declared=%d", prompts, want.Prompts)
	}

	var accept, reject int64
	for _, d := range logsBatch.ToolDecisions {
		if d.Decision == "accept" {
			accept++
		} else {
			reject++
		}
	}
	if accept != want.ToolAccept || reject != want.ToolReject {
		t.Errorf("decisions parser(accept=%d reject=%d) != declared(%d/%d)",
			accept, reject, want.ToolAccept, want.ToolReject)
	}
	if int64(len(logsBatch.ToolResults)) != want.ToolResults {
		t.Errorf("tool_results parser=%d declared=%d", len(logsBatch.ToolResults), want.ToolResults)
	}

	// active_time delta sum (cold baseline → final - first).
	var active float64
	for _, m := range metBatch.MetricPoints {
		if m.Name == "claude_code.active_time.total" {
			active += m.ValueDelta
		}
	}
	if !floatNear(active, want.ActiveTimeDeltaSum) {
		t.Errorf("active_time parser delta sum=%v declared=%v", active, want.ActiveTimeDeltaSum)
	}

	if int64(len(traceBatch.Spans)) != want.Spans {
		t.Errorf("spans parser=%d declared=%d", len(traceBatch.Spans), want.Spans)
	}
}

// TestEventMetricTotalsDiffer documents the no-double-count precondition: the
// event token total and the metric token total are intentionally different
// magnitudes, so any accidental summing of the two axes is detectable.
func TestEventMetricTotalsDiffer(t *testing.T) {
	tot := Generate(DefaultOptions()).Totals[0]
	eventTokens := tot.Input + tot.Output + tot.CacheRead + tot.CacheCreation
	if eventTokens == 0 || tot.MetricTokenTotal == 0 {
		t.Fatal("both axes should be non-zero")
	}
	if eventTokens == tot.MetricTokenTotal {
		t.Fatal("event and metric token totals must differ to detect double-counting")
	}
}

func floatNear(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= 1e-9
}
