package otel

import (
	"os"
	"path/filepath"
	"testing"

	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// testdataDir is the repo-level testdata fixture directory (owned by this batch).
const testdataDir = "../../testdata"

// writeGolden serializes a proto message to a .binpb fixture under testdata.
// Generation is opt-in (REGEN_GOLDEN=1) so normal runs never mutate fixtures;
// the committed fixtures are the source of truth that the parser must satisfy.
func writeGolden(t *testing.T, name string, msg proto.Message) {
	t.Helper()
	if os.Getenv("REGEN_GOLDEN") == "" {
		return
	}
	b, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	if err := os.MkdirAll(testdataDir, 0o755); err != nil {
		t.Fatalf("mkdir testdata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(testdataDir, name), b, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func readGolden[T proto.Message](t *testing.T, name string, msg T) T {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testdataDir, name))
	if err != nil {
		t.Fatalf("read golden %s: %v (run with REGEN_GOLDEN=1 to generate)", name, err)
	}
	if err := proto.Unmarshal(b, msg); err != nil {
		t.Fatalf("unmarshal %s: %v", name, err)
	}
	return msg
}

// --- canonical fixture messages -------------------------------------------

func goldenMetricsCumulative() *collectormetricspb.ExportMetricsServiceRequest {
	at := []*commonpb.KeyValue{
		{Key: "session.id", Value: strVal("golden-session")},
		{Key: "type", Value: strVal("input")},
	}
	return metricsReq(
		resource(
			kv("service.name", strVal("claude-code")),
			kv("service.version", strVal("1.0.0")),
		),
		sumMetric("claude_code.token.usage", tCumul, true,
			numDP(dp{isInt: true, intValue: 100, startNano: 1_000_000_000, pointNano: 2_000_000_000, attrs: at}),
		),
	)
}

func goldenLogsAPIRequest() *collectorlogspb.ExportLogsServiceRequest {
	return logsReq(
		resource(kv("service.name", strVal("claude-code"))),
		logRecord("claude_code.api_request", 5_000_000_000,
			kv("session.id", strVal("golden-session")),
			kv("model", strVal("claude-opus")),
			kv("input_tokens", intVal(1200)),
			kv("output_tokens", intVal(340)),
			kv("cache_read_tokens", intVal(50)),
			kv("cache_creation_tokens", intVal(7)),
			kv("cost_usd", dblVal(0.0231)),
			kv("duration_ms", intVal(875)),
		),
		logRecord("claude_code.tool_result", 6_000_000_000,
			kv("session.id", strVal("golden-session")),
			kv("tool_name", strVal("Bash")),
			kv("success", boolVal(true)),
			kv("tool_parameters", strVal(`{"bash_command":"go test ./..."}`)),
		),
	)
}

func goldenTraces(t *testing.T) *collectortracepb.ExportTraceServiceRequest {
	return tracesReq(
		resource(kv("service.name", strVal("claude-code"))),
		span(
			hexBytes(t, "0123456789abcdef0123456789abcdef"),
			hexBytes(t, "1122334455667788"),
			nil,
			"POST /v1/messages",
			2_000_000_000, 3_000_000_000,
			tracepb.Status_STATUS_CODE_OK,
			kv("session.id", strVal("golden-session")),
		),
	)
}

// TestGolden_Generate writes the canonical fixtures when REGEN_GOLDEN=1.
func TestGolden_Generate(t *testing.T) {
	writeGolden(t, "metrics_cumulative.binpb", goldenMetricsCumulative())
	writeGolden(t, "logs_api_request.binpb", goldenLogsAPIRequest())
	writeGolden(t, "traces.binpb", goldenTraces(t))
}

// TestGolden_MetricsFixture loads the committed metrics fixture and verifies the
// first cumulative observation baselines to delta 0.
func TestGolden_MetricsFixture(t *testing.T) {
	req := readGolden(t, "metrics_cumulative.binpb", &collectormetricspb.ExportMetricsServiceRequest{})
	p := NewParser(Options{})
	b, rej := p.ParseMetrics(req)
	if rej.Rejected != 0 {
		t.Fatalf("golden metrics rejected: %+v", rej)
	}
	pts := pointsFor(b, "claude_code.token.usage")
	if len(pts) != 1 || pts[0].ValueDelta != 0 {
		t.Fatalf("golden first cumulative must baseline to 0, got %+v", pts)
	}
	if pts[0].SessionID != "golden-session" {
		t.Fatalf("session id wrong: %+v", pts[0])
	}
}

// TestGolden_LogsFixture loads the committed logs fixture and verifies the
// api_request token breakdown and the nested tool_parameters decode.
func TestGolden_LogsFixture(t *testing.T) {
	req := readGolden(t, "logs_api_request.binpb", &collectorlogspb.ExportLogsServiceRequest{})
	p := NewParser(Options{})
	b, _ := p.ParseLogs(req)
	if len(b.APIRequests) != 1 || b.APIRequests[0].Tokens.Input != 1200 {
		t.Fatalf("golden api_request wrong: %+v", b.APIRequests)
	}
	if len(b.ToolResults) != 1 || b.ToolResults[0].BashCommand != "go test ./..." {
		t.Fatalf("golden tool_result nested decode wrong: %+v", b.ToolResults)
	}
	if len(b.Sessions) != 1 || b.Sessions[0].TokenSource != "events" {
		t.Fatalf("golden session token_source wrong: %+v", b.Sessions)
	}
}

// TestGolden_TracesFixture loads the committed traces fixture and verifies span
// parsing.
func TestGolden_TracesFixture(t *testing.T) {
	req := readGolden(t, "traces.binpb", &collectortracepb.ExportTraceServiceRequest{})
	p := NewParser(Options{})
	b, rej := p.ParseTraces(req)
	if rej.Rejected != 0 || len(b.Spans) != 1 {
		t.Fatalf("golden traces wrong: spans=%d rej=%+v", len(b.Spans), rej)
	}
	if b.Spans[0].TraceID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("golden span trace id wrong: %+v", b.Spans[0])
	}
}
