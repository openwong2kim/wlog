// Package synth synthesizes realistic Claude Code OTLP telemetry (metrics +
// logs + traces for one or more sessions) for the wlog dogfood harness and the
// integration tests (PLAN §2 dogfood, §14 tests).
//
// Generate is importable so the integration tests can inject the synthetic data
// straight into an in-process receiver without a network round-trip, while the
// replay CLI uses the same Generate output and ships it over gRPC/HTTP. The
// generator is deterministic for a given Options (fixed pseudo-sequences, no
// time.Now), so tests can assert exact equality against the returned ground
// truth.
package synth

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Temporality selects how the synthetic cumulative counter metrics are encoded.
// Cumulative emits increasing absolute values (exercising the delta normalizer);
// Delta emits the per-step deltas directly. Both encode the SAME underlying data
// so the aggregated result must be identical (cumulative/delta equivalence test).
type Temporality string

const (
	// Cumulative encodes monotonic absolute counter values (CUMULATIVE).
	Cumulative Temporality = "cumulative"
	// Delta encodes per-point deltas directly (DELTA).
	Delta Temporality = "delta"
)

// Options configures synthetic session generation.
type Options struct {
	// Sessions is the number of distinct sessions to synthesize (>=1).
	Sessions int
	// Temporality selects cumulative vs delta metric encoding.
	Temporality Temporality
	// BaseTimeUnixNano anchors synthetic event times. Zero uses a fixed
	// deterministic epoch so generated data is reproducible.
	BaseTimeUnixNano uint64
	// SessionIDPrefix prefixes generated session ids (default "replay-sess-").
	SessionIDPrefix string
	// IncludeMetrics, IncludeLogs, IncludeTraces gate each signal. A bare
	// Options{} (all-false) produces all three (the mixed-axis dogfood default).
	IncludeMetrics bool
	IncludeLogs    bool
	IncludeTraces  bool
}

// DefaultOptions returns Options producing one cumulative session with all three
// signals, anchored at the fixed deterministic epoch.
func DefaultOptions() Options {
	return Options{
		Sessions:         1,
		Temporality:      Cumulative,
		BaseTimeUnixNano: FixedEpochNano,
		SessionIDPrefix:  "replay-sess-",
		IncludeMetrics:   true,
		IncludeLogs:      true,
		IncludeTraces:    true,
	}
}

// FixedEpochNano is the deterministic anchor (2026-01-01T00:00:00Z) used when
// Options.BaseTimeUnixNano is zero.
const FixedEpochNano uint64 = 1767225600_000_000_000

// SessionTotals is the exact ground truth for one synthesized session: the
// numbers the API MUST report for that session's data. The generator computes
// these independently from the emitted records so the integration assertions are
// genuine (not self-fulfilling).
type SessionTotals struct {
	SessionID string
	// Token/cost ground truth from the api_request EVENTS (the axis of truth;
	// metrics must NOT be summed on top).
	CostUSD       float64
	Input         int64
	Output        int64
	CacheRead     int64
	CacheCreation int64
	// Tool decision ground truth.
	ToolAccept int64
	ToolReject int64
	// AutoApproved = accepts whose source is config or hook.
	AutoApproved int64
	// ToolResults is the number of tool_result events.
	ToolResults int64
	// APIErrors is the number of api_error events.
	APIErrors int64
	// Prompts is the number of user_prompt events.
	Prompts int64
	// ActiveTimeSec is the final cumulative active_time.total for the session
	// (LAST of cumulative, PLAN §7 semantics).
	ActiveTimeSec float64
	// ActiveTimeDeltaSum is the SUM of per-point active_time deltas a cold delta
	// normalizer produces: the first observation establishes a baseline (delta 0)
	// so the sum equals final - first, NOT final. This is the value the pipeline
	// /api surface currently aggregates (sum of stored metric_point deltas), and
	// is identical for cumulative and delta encodings (the equivalence property).
	ActiveTimeDeltaSum float64
	// LinesAddedDeltaSum / LinesRemovedDeltaSum are the cold-baseline delta sums
	// for lines_of_code.count{type} (final - first), exercised by metrics.
	LinesAddedDeltaSum   float64
	LinesRemovedDeltaSum float64
	// MetricTokenTotal is the cumulative token.usage total emitted as METRICS.
	// It must NOT appear in the token/cost axis because the session has
	// api_request events (double-count guard).
	MetricTokenTotal int64
	// Spans is the number of trace spans emitted for this session.
	Spans int64
	// Models is the set of models the session used.
	Models []string
}

// Bundle is the full output of Generate: the three signal request slices ready
// to send, plus the per-session ground truth for assertions.
type Bundle struct {
	Metrics []*collectormetricspb.ExportMetricsServiceRequest
	Logs    []*collectorlogspb.ExportLogsServiceRequest
	Traces  []*collectortracepb.ExportTraceServiceRequest
	Totals  []SessionTotals
}

// TotalsByID returns the SessionTotals for the given session id (or false).
func (b Bundle) TotalsByID(id string) (SessionTotals, bool) {
	for _, t := range b.Totals {
		if t.SessionID == id {
			return t, true
		}
	}
	return SessionTotals{}, false
}

// Generate synthesizes realistic Claude Code OTLP for opts.Sessions sessions and
// returns the request slices plus the ground-truth totals. Deterministic for a
// given Options.
//
// Each session contains api_request events (multiple models; input/output/cache
// tokens + cost_usd + duration_ms) with one api_error; tool_decision events
// (accept/reject; sources config/hook/user_permanent/user_temporary) and
// tool_result events (Bash/Edit/Read, some failures); user_prompt events with
// prompt_length; and cumulative metrics for token.usage{type,model},
// cost.usage{model}, active_time.total, lines_of_code.count{type},
// code_edit_tool.decision. The api_request token/cost EVENTS and the token.usage
// METRICS share the SAME session.id so callers can prove no double counting.
func Generate(opts Options) Bundle {
	if opts.Sessions < 1 {
		opts.Sessions = 1
	}
	if opts.Temporality == "" {
		opts.Temporality = Cumulative
	}
	if opts.BaseTimeUnixNano == 0 {
		opts.BaseTimeUnixNano = FixedEpochNano
	}
	if opts.SessionIDPrefix == "" {
		opts.SessionIDPrefix = "replay-sess-"
	}
	if !opts.IncludeMetrics && !opts.IncludeLogs && !opts.IncludeTraces {
		opts.IncludeMetrics, opts.IncludeLogs, opts.IncludeTraces = true, true, true
	}

	var b Bundle
	for i := 0; i < opts.Sessions; i++ {
		sid := fmt.Sprintf("%s%03d", opts.SessionIDPrefix, i)
		base := opts.BaseTimeUnixNano + uint64(i)*uint64(time.Hour)
		s := generateSession(sid, base, opts.Temporality)
		if opts.IncludeLogs {
			b.Logs = append(b.Logs, s.logs)
		}
		if opts.IncludeMetrics {
			b.Metrics = append(b.Metrics, s.metrics)
		}
		if opts.IncludeTraces {
			b.Traces = append(b.Traces, s.traces)
		}
		b.Totals = append(b.Totals, s.totals)
	}
	return b
}

// sessionData is the per-session generation result.
type sessionData struct {
	logs    *collectorlogspb.ExportLogsServiceRequest
	metrics *collectormetricspb.ExportMetricsServiceRequest
	traces  *collectortracepb.ExportTraceServiceRequest
	totals  SessionTotals
}

// apiReqSpec is one synthetic api_request, parameterized so totals and the
// emitted record agree exactly.
type apiReqSpec struct {
	model         string
	input         int64
	output        int64
	cacheRead     int64
	cacheCreation int64
	cost          float64
	durationMS    int64
	isError       bool
	statusCode    int64
}

func generateSession(sid string, base uint64, temp Temporality) sessionData {
	res := resource(
		kv("service.name", strVal("claude-code")),
		kv("service.version", strVal("1.0.42")),
		kv("app.version", strVal("1.0.42")),
		kv("user.email", strVal("dev@example.com")),
		kv("session.id", strVal(sid)),
		kv("terminal.type", strVal("xterm")),
	)

	specs := []apiReqSpec{
		{model: "claude-opus-4", input: 1200, output: 800, cacheRead: 0, cacheCreation: 4096, cost: 0.090, durationMS: 5300},
		{model: "claude-opus-4", input: 300, output: 1500, cacheRead: 4096, cacheCreation: 0, cost: 0.075, durationMS: 8100},
		{model: "claude-sonnet-4", input: 500, output: 600, cacheRead: 2048, cacheCreation: 0, cost: 0.012, durationMS: 2200},
		{model: "claude-haiku-4", input: 200, output: 150, cacheRead: 1024, cacheCreation: 0, cost: 0.001, durationMS: 700},
		{model: "claude-sonnet-4", isError: true, statusCode: 529, durationMS: 1200},
	}

	var (
		records  []*logspb.LogRecord
		totals   = SessionTotals{SessionID: sid}
		modelSet = map[string]bool{}
		tNano    = base
	)
	step := func() uint64 { tNano += uint64(2 * time.Second); return tNano }

	records = append(records, promptRecord(step(), 142))
	totals.Prompts++

	for _, sp := range specs {
		records = append(records, apiRequestRecord(step(), sp))
		if sp.isError {
			totals.APIErrors++
		} else {
			totals.CostUSD += sp.cost
			totals.Input += sp.input
			totals.Output += sp.output
			totals.CacheRead += sp.cacheRead
			totals.CacheCreation += sp.cacheCreation
		}
		if sp.model != "" {
			modelSet[sp.model] = true
		}
	}

	toolEvents := []struct {
		tool     string
		decision string
		source   string
		success  bool
		bash     string
	}{
		{tool: "Bash", decision: "accept", source: "config", success: true, bash: "go build ./..."},
		{tool: "Edit", decision: "accept", source: "hook", success: true},
		{tool: "Read", decision: "accept", source: "user_temporary", success: true},
		{tool: "Bash", decision: "reject", source: "user_permanent", success: false, bash: "rm -rf /"},
		{tool: "Edit", decision: "reject", source: "user_temporary", success: false},
		{tool: "Bash", decision: "accept", source: "config", success: false, bash: "go test ./... # flaky"},
	}
	for _, te := range toolEvents {
		records = append(records, toolDecisionRecord(step(), te.tool, te.decision, te.source))
		if te.decision == "accept" {
			totals.ToolAccept++
			if te.source == "config" || te.source == "hook" {
				totals.AutoApproved++
			}
			records = append(records, toolResultRecord(step(), te.tool, te.success, te.bash))
			totals.ToolResults++
		} else {
			totals.ToolReject++
		}
	}

	for m := range modelSet {
		totals.Models = append(totals.Models, m)
	}

	logsReq := &collectorlogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			Resource: res,
			ScopeLogs: []*logspb.ScopeLogs{{
				Scope:      &commonpb.InstrumentationScope{Name: "com.anthropic.claude_code"},
				LogRecords: records,
			}},
		}},
	}

	metricsReq, mt := buildMetrics(res, sid, base, temp)
	totals.MetricTokenTotal = mt.tokenTotal
	totals.ActiveTimeSec = mt.activeFinal
	totals.ActiveTimeDeltaSum = mt.activeDeltaSum
	totals.LinesAddedDeltaSum = mt.linesAddedDeltaSum
	totals.LinesRemovedDeltaSum = mt.linesRemovedDeltaSum

	tracesReq := buildTraces(res, sid, base)
	totals.Spans = 2

	return sessionData{logs: logsReq, metrics: metricsReq, traces: tracesReq, totals: totals}
}

func promptRecord(tNano uint64, promptLen int64) *logspb.LogRecord {
	return logRecord("claude_code.user_prompt", tNano,
		kv("event.name", strVal("user_prompt")),
		kv("prompt_length", intVal(promptLen)),
	)
}

func apiRequestRecord(tNano uint64, sp apiReqSpec) *logspb.LogRecord {
	if sp.isError {
		return logRecord("claude_code.api_error", tNano,
			kv("model", strVal(sp.model)),
			kv("error", strVal("overloaded_error")),
			kv("status_code", intVal(sp.statusCode)),
		)
	}
	return logRecord("claude_code.api_request", tNano,
		kv("model", strVal(sp.model)),
		kv("input_tokens", intVal(sp.input)),
		kv("output_tokens", intVal(sp.output)),
		kv("cache_read_tokens", intVal(sp.cacheRead)),
		kv("cache_creation_tokens", intVal(sp.cacheCreation)),
		kv("cost_usd", dblVal(sp.cost)),
		kv("duration_ms", intVal(sp.durationMS)),
	)
}

func toolDecisionRecord(tNano uint64, tool, decision, source string) *logspb.LogRecord {
	return logRecord("claude_code.tool_decision", tNano,
		kv("tool_name", strVal(tool)),
		kv("decision", strVal(decision)),
		kv("source", strVal(source)),
	)
}

func toolResultRecord(tNano uint64, tool string, success bool, bash string) *logspb.LogRecord {
	attrs := []*commonpb.KeyValue{
		kv("tool_name", strVal(tool)),
		kv("success", boolVal(success)),
		kv("duration_ms", intVal(1500)),
	}
	if bash != "" {
		attrs = append(attrs, kv("tool_parameters", strVal(`{"bash_command":`+jsonQuote(bash)+`}`)))
	}
	return logRecord("claude_code.tool_result", tNano, attrs...)
}

// metricTotals are the per-session ground-truth values derived from the metric
// sequences, returned alongside the metrics request.
type metricTotals struct {
	tokenTotal           int64
	activeFinal          float64
	activeDeltaSum       float64
	linesAddedDeltaSum   float64
	linesRemovedDeltaSum float64
}

// deltaSum returns the cold-baseline delta sum of a cumulative series: the first
// observation establishes a baseline (delta 0), so the sum is last - first.
func deltaSumInt(cumul []int64) float64 {
	if len(cumul) == 0 {
		return 0
	}
	return float64(cumul[len(cumul)-1] - cumul[0])
}
func deltaSumFloat(cumul []float64) float64 {
	if len(cumul) == 0 {
		return 0
	}
	return cumul[len(cumul)-1] - cumul[0]
}

// buildMetrics constructs the metrics request and returns the ground-truth
// metric totals for the session. All counter metrics are emitted as a 3-point
// increasing sequence so the cumulative→delta normalization is exercised; in
// delta mode the same increments are sent as per-step deltas.
func buildMetrics(res *resourcepb.Resource, sid string, base uint64, temp Temporality) (*collectormetricspb.ExportMetricsServiceRequest, metricTotals) {
	startNano := base
	var mt metricTotals

	tokenSeries := []struct {
		ttype string
		model string
		cumul []int64
	}{
		{ttype: "input", model: "claude-opus-4", cumul: []int64{1200, 1500, 1500}},
		{ttype: "output", model: "claude-opus-4", cumul: []int64{800, 2300, 2300}},
		{ttype: "cacheRead", model: "claude-opus-4", cumul: []int64{0, 4096, 4096}},
	}
	var tokenMetrics []*metricspb.Metric
	var metricTokenTotal int64
	for _, ts := range tokenSeries {
		dps := counterDPsInt(startNano, base, ts.cumul, temp,
			kv("type", strVal(ts.ttype)),
			kv("model", strVal(ts.model)),
			kv("session.id", strVal(sid)),
		)
		tokenMetrics = append(tokenMetrics, sumMetric("claude_code.token.usage", aggTemporality(temp), true, dps...))
		metricTokenTotal += ts.cumul[len(ts.cumul)-1]
	}
	mt.tokenTotal = metricTokenTotal

	costDPs := counterDPsFloat(startNano, base, []float64{0.090, 0.165, 0.165}, temp,
		kv("model", strVal("claude-opus-4")),
		kv("session.id", strVal(sid)),
	)
	costMetric := sumMetric("claude_code.cost.usage", aggTemporality(temp), true, costDPs...)

	activeCumul := []float64{30, 75, 128}
	activeDPs := counterDPsFloat(startNano, base, activeCumul, temp, kv("session.id", strVal(sid)))
	activeMetric := sumMetric("claude_code.active_time.total", aggTemporality(temp), true, activeDPs...)
	mt.activeFinal = activeCumul[len(activeCumul)-1]
	mt.activeDeltaSum = deltaSumFloat(activeCumul)

	locAddedCumul := []int64{40, 120, 156}
	locRemovedCumul := []int64{5, 18, 22}
	mt.linesAddedDeltaSum = deltaSumInt(locAddedCumul)
	mt.linesRemovedDeltaSum = deltaSumInt(locRemovedCumul)
	locAdded := counterDPsInt(startNano, base, locAddedCumul, temp,
		kv("type", strVal("added")), kv("session.id", strVal(sid)))
	locRemoved := counterDPsInt(startNano, base, locRemovedCumul, temp,
		kv("type", strVal("removed")), kv("session.id", strVal(sid)))
	locMetricAdded := sumMetric("claude_code.lines_of_code.count", aggTemporality(temp), true, locAdded...)
	locMetricRemoved := sumMetric("claude_code.lines_of_code.count", aggTemporality(temp), true, locRemoved...)

	editDecision := counterDPsInt(startNano, base, []int64{1, 2, 3}, temp,
		kv("decision", strVal("accept")),
		kv("language", strVal("go")),
		kv("tool_name", strVal("Edit")),
		kv("session.id", strVal(sid)),
	)
	editMetric := sumMetric("claude_code.code_edit_tool.decision", aggTemporality(temp), true, editDecision...)

	allMetrics := append([]*metricspb.Metric{}, tokenMetrics...)
	allMetrics = append(allMetrics, costMetric, activeMetric, locMetricAdded, locMetricRemoved, editMetric)

	req := &collectormetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: res,
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Scope:   &commonpb.InstrumentationScope{Name: "com.anthropic.claude_code"},
				Metrics: allMetrics,
			}},
		}},
	}
	return req, mt
}

func buildTraces(res *resourcepb.Resource, sid string, base uint64) *collectortracepb.ExportTraceServiceRequest {
	traceID := deriveID(sid, 16)
	rootSpan := deriveID(sid+"-root", 8)
	childSpan := deriveID(sid+"-child", 8)
	return &collectortracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: res,
			ScopeSpans: []*tracepb.ScopeSpans{{
				Scope: &commonpb.InstrumentationScope{Name: "com.anthropic.claude_code"},
				Spans: []*tracepb.Span{
					{
						TraceId:           traceID,
						SpanId:            rootSpan,
						Name:              "session",
						StartTimeUnixNano: base,
						EndTimeUnixNano:   base + uint64(60*time.Second),
						Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
						Attributes:        []*commonpb.KeyValue{kv("session.id", strVal(sid))},
					},
					{
						TraceId:           traceID,
						SpanId:            childSpan,
						ParentSpanId:      rootSpan,
						Name:              "api_request",
						StartTimeUnixNano: base + uint64(2*time.Second),
						EndTimeUnixNano:   base + uint64(7*time.Second),
						Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
						Attributes:        []*commonpb.KeyValue{kv("session.id", strVal(sid))},
					},
				},
			}},
		}},
	}
}

// counterDPsInt builds NumberDataPoints for an integer cumulative series. In
// cumulative mode the absolute cumul values are sent verbatim; in delta mode the
// consecutive differences are sent.
//
// Equivalence note (PLAN §7): the cumulative path establishes a baseline on the
// FIRST observation and emits delta 0 for it (a fresh receiver did not see the
// counter accumulate before it started). To make the two encodings represent the
// identical observed reality — so the aggregated sums are equal — the delta
// encoding's first point is therefore also 0 (the pre-start accumulation is
// unknown to a delta exporter that started with the receiver). Both encodings
// then sum to (last - first).
func counterDPsInt(startNano, base uint64, cumul []int64, temp Temporality, attrs ...*commonpb.KeyValue) []*metricspb.NumberDataPoint {
	out := make([]*metricspb.NumberDataPoint, 0, len(cumul))
	prev := firstOrZeroInt(cumul)
	for i, c := range cumul {
		point := base + uint64(i+1)*uint64(time.Second)
		v := c
		if temp == Delta {
			v = c - prev // first point: c - cumul[0] = 0 (baseline)
		}
		prev = c
		out = append(out, &metricspb.NumberDataPoint{
			StartTimeUnixNano: startNano,
			TimeUnixNano:      point,
			Value:             &metricspb.NumberDataPoint_AsInt{AsInt: v},
			Attributes:        cloneKVs(attrs),
		})
	}
	return out
}

// counterDPsFloat is counterDPsInt for double-valued cumulative series (same
// first-observation baseline equivalence as counterDPsInt).
func counterDPsFloat(startNano, base uint64, cumul []float64, temp Temporality, attrs ...*commonpb.KeyValue) []*metricspb.NumberDataPoint {
	out := make([]*metricspb.NumberDataPoint, 0, len(cumul))
	prev := firstOrZeroFloat(cumul)
	for i, c := range cumul {
		point := base + uint64(i+1)*uint64(time.Second)
		v := c
		if temp == Delta {
			v = c - prev // first point: c - cumul[0] = 0 (baseline)
		}
		prev = c
		out = append(out, &metricspb.NumberDataPoint{
			StartTimeUnixNano: startNano,
			TimeUnixNano:      point,
			Value:             &metricspb.NumberDataPoint_AsDouble{AsDouble: v},
			Attributes:        cloneKVs(attrs),
		})
	}
	return out
}

func firstOrZeroInt(s []int64) int64 {
	if len(s) == 0 {
		return 0
	}
	return s[0]
}
func firstOrZeroFloat(s []float64) float64 {
	if len(s) == 0 {
		return 0
	}
	return s[0]
}

func aggTemporality(t Temporality) metricspb.AggregationTemporality {
	if t == Delta {
		return metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
	}
	return metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE
}

// --- OTLP builders ---------------------------------------------------------

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
func kv(k string, v *commonpb.AnyValue) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: v}
}
func resource(kvs ...*commonpb.KeyValue) *resourcepb.Resource {
	return &resourcepb.Resource{Attributes: kvs}
}

func logRecord(eventName string, timeNano uint64, attrs ...*commonpb.KeyValue) *logspb.LogRecord {
	return &logspb.LogRecord{
		TimeUnixNano: timeNano,
		EventName:    eventName,
		Attributes:   attrs,
	}
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

func cloneKVs(kvs []*commonpb.KeyValue) []*commonpb.KeyValue {
	out := make([]*commonpb.KeyValue, len(kvs))
	copy(out, kvs)
	return out
}

// deriveID derives a deterministic n-byte id from a seed string.
func deriveID(seed string, n int) []byte {
	sum := sha256.Sum256([]byte(seed))
	if n > len(sum) {
		n = len(sum)
	}
	out := make([]byte, n)
	copy(out, sum[:n])
	return out
}

// jsonQuote returns the JSON-quoted form of s (including surrounding quotes).
func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
