package api

import (
	"bufio"
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/openwong2kim/wlog/internal/ingest"
	"github.com/openwong2kim/wlog/internal/model"
)

// --- health ------------------------------------------------------------------

func TestHealth(t *testing.T) {
	env := newTestEnv(t)
	env.counters = ingest.Counters{Received: 10, Accepted: 9, RejectedPermanent: 1, RetrySignaled: 2, BusDropped: 3}

	var got struct {
		Status    string `json:"status"`
		Version   string `json:"version"`
		UptimeSec int64  `json:"uptime_sec"`
		DB        struct {
			Path      string `json:"path"`
			SizeBytes int64  `json:"size_bytes"`
		} `json:"db"`
		Ingest struct {
			Received          int64 `json:"received"`
			Accepted          int64 `json:"accepted"`
			RejectedPermanent int64 `json:"rejected_permanent"`
			RetrySignaled     int64 `json:"retry_signaled"`
			BusDropped        int64 `json:"bus_dropped"`
		} `json:"ingest"`
		Retention struct {
			Policy string `json:"policy"`
		} `json:"retention"`
	}
	if code := env.get("/api/health", &got); code != http.StatusOK {
		t.Fatalf("health status = %d", code)
	}
	if got.Status != "ok" {
		t.Fatalf("status = %q", got.Status)
	}
	if got.Version == "" || !strings.HasPrefix(got.Version, "wlog") {
		t.Fatalf("version = %q", got.Version)
	}
	if got.Ingest.Received != 10 || got.Ingest.Accepted != 9 || got.Ingest.RejectedPermanent != 1 ||
		got.Ingest.RetrySignaled != 2 || got.Ingest.BusDropped != 3 {
		t.Fatalf("ingest counters = %+v", got.Ingest)
	}
	if got.DB.Path != env.dbPath {
		t.Fatalf("db path = %q", got.DB.Path)
	}
	if got.Retention.Policy == "" {
		t.Fatalf("retention policy empty")
	}
}

// --- sessions: empty state ---------------------------------------------------

func TestSessions_Empty(t *testing.T) {
	env := newTestEnv(t)
	var got sessionsResponse
	if code := env.get("/api/sessions", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if got.Total != 0 || len(got.Sessions) != 0 {
		t.Fatalf("expected empty, got %+v", got)
	}
	if got.Meta == nil || got.Meta.EmptyReason != reasonNoData {
		t.Fatalf("expected empty_reason=no_data, got %+v", got.Meta)
	}
}

// --- axis of truth (★) -------------------------------------------------------

// (a) events-only session: tokens/cost aggregate from api_requests.
func TestAxis_EventsOnly(t *testing.T) {
	env := newTestEnv(t)
	env.write(model.Batch{
		Sessions:    []model.Session{seedSession("ev", 1000, 5000, "events", true)},
		APIRequests: []model.APIRequest{apiReq("ev", 2000, "claude-3", 100, 50, 10, 5, 0.0030, 1200)},
	})

	var got sessionsResponse
	env.get("/api/sessions", &got)
	if len(got.Sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(got.Sessions))
	}
	s := got.Sessions[0]
	if s.CostUSD != 0.0030 {
		t.Fatalf("cost = %v want 0.003", s.CostUSD)
	}
	if s.Tokens.Input != 100 || s.Tokens.Output != 50 || s.Tokens.CacheRead != 10 || s.Tokens.CacheCreation != 5 {
		t.Fatalf("tokens = %+v", s.Tokens)
	}
	if s.TokenSource != "events" {
		t.Fatalf("token_source = %q", s.TokenSource)
	}
}

// (b) metrics-only session: tokens/cost fall back to metric_points.
func TestAxis_MetricsOnly(t *testing.T) {
	env := newTestEnv(t)
	env.write(model.Batch{
		Sessions: []model.Session{seedSession("mt", 1000, 8000, "metrics", false)},
		MetricPoints: []model.MetricPoint{
			metricPoint("mt", 2000, metricTokenUsageCC, "input", 200, 0, 1000, 2000),
			metricPoint("mt", 2000, metricTokenUsageCC, "output", 80, 0, 1000, 2001),
			metricPoint("mt", 2000, metricCostUsageCC, "claude-3", 0.0055, 0, 1000, 2002),
		},
	})

	var got sessionsResponse
	env.get("/api/sessions", &got)
	if len(got.Sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(got.Sessions))
	}
	s := got.Sessions[0]
	if s.Tokens.Input != 200 || s.Tokens.Output != 80 {
		t.Fatalf("metric tokens = %+v want in=200 out=80", s.Tokens)
	}
	if s.CostUSD != 0.0055 {
		t.Fatalf("metric cost = %v want 0.0055", s.CostUSD)
	}
	if s.TokenSource != "metrics" {
		t.Fatalf("token_source = %q", s.TokenSource)
	}
}

// (c) MIXED session: api_request events AND active_time metric coexist. Tokens
// come ONLY from events (NOT events+metrics), active_time comes from metrics.
// This guards the no-double-counting invariant (PLAN §6.5).
func TestAxis_MixedNoDoubleCount(t *testing.T) {
	env := newTestEnv(t)
	env.write(model.Batch{
		Sessions: []model.Session{seedSession("mx", 1000, 9000, "events", true)},
		// Event tokens: input=100 output=40 cost=0.0020.
		APIRequests: []model.APIRequest{apiReq("mx", 2000, "claude-3", 100, 40, 0, 0, 0.0020, 500)},
		MetricPoints: []model.MetricPoint{
			// A token.usage metric ALSO present (would double-count if summed).
			metricPoint("mx", 2500, metricTokenUsageCC, "input", 999, 0, 1000, 2500),
			metricPoint("mx", 2500, metricCostUsageCC, "claude-3", 99.0, 0, 1000, 2501),
			// active_time metric (always metrics axis).
			metricPoint("mx", 3000, metricActiveTimeCC, "", 12.5, 1, 1000, 3000),
		},
	})

	var got sessionsResponse
	env.get("/api/sessions", &got)
	if len(got.Sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(got.Sessions))
	}
	s := got.Sessions[0]
	// Tokens/cost must be the EVENT values only — not 100+999 or 0.002+99.
	if s.Tokens.Input != 100 {
		t.Fatalf("mixed input = %d; double counting metrics? (want 100)", s.Tokens.Input)
	}
	if s.Tokens.Output != 40 {
		t.Fatalf("mixed output = %d (want 40)", s.Tokens.Output)
	}
	if s.CostUSD != 0.0020 {
		t.Fatalf("mixed cost = %v; double counting? (want 0.002)", s.CostUSD)
	}

	// active_time is exposed via /api/now snapshot path (metrics axis). Here we
	// assert the cost endpoint also uses events (token_source=events) and does
	// not fold in the metric token rows.
	var cost costResponse
	env.get("/api/cost?session=mx", &cost)
	if cost.TokenSource != "events" {
		t.Fatalf("cost token_source = %q want events", cost.TokenSource)
	}
	var totalInput int64
	for _, p := range cost.Series {
		totalInput += p.Input
	}
	if totalInput != 100 {
		t.Fatalf("cost series input = %d; double counting? (want 100)", totalInput)
	}
}

// --- sessions: sort + filter -------------------------------------------------

func TestSessions_SortAndFilter(t *testing.T) {
	env := newTestEnv(t)
	env.write(model.Batch{
		Sessions: []model.Session{
			seedSession("aaa", 1000, 2000, "events", true),
			seedSession("bbb", 1000, 4000, "events", true),
			seedSession("ccc", 1000, 3000, "events", true),
		},
		APIRequests: []model.APIRequest{
			apiReq("aaa", 1500, "m", 10, 0, 0, 0, 0.01, 0),
			apiReq("bbb", 1500, "m", 20, 0, 0, 0, 0.05, 0),
			apiReq("ccc", 1500, "m", 30, 0, 0, 0, 0.02, 0),
		},
	})

	// Default sort = last_seen desc → bbb, ccc, aaa.
	var byTime sessionsResponse
	env.get("/api/sessions", &byTime)
	if len(byTime.Sessions) != 3 || byTime.Sessions[0].ID != "bbb" || byTime.Sessions[2].ID != "aaa" {
		t.Fatalf("last_seen order wrong: %v", ids(byTime.Sessions))
	}

	// sort=cost desc → bbb(0.05), ccc(0.02), aaa(0.01).
	var byCost sessionsResponse
	env.get("/api/sessions?sort=cost", &byCost)
	if byCost.Sessions[0].ID != "bbb" || byCost.Sessions[1].ID != "ccc" || byCost.Sessions[2].ID != "aaa" {
		t.Fatalf("cost order wrong: %v", ids(byCost.Sessions))
	}

	// sort=tokens desc → ccc(30), bbb(20), aaa(10).
	var byTok sessionsResponse
	env.get("/api/sessions?sort=tokens", &byTok)
	if byTok.Sessions[0].ID != "ccc" || byTok.Sessions[2].ID != "aaa" {
		t.Fatalf("tokens order wrong: %v", ids(byTok.Sessions))
	}

	// q=prefix.
	var q sessionsResponse
	env.get("/api/sessions?q=bb", &q)
	if len(q.Sessions) != 1 || q.Sessions[0].ID != "bbb" {
		t.Fatalf("q filter wrong: %v", ids(q.Sessions))
	}

	// since filter (last_seen >= 3500) → only bbb (4000).
	var since sessionsResponse
	env.get("/api/sessions?since=3500", &since)
	if len(since.Sessions) != 1 || since.Sessions[0].ID != "bbb" {
		t.Fatalf("since filter wrong: %v", ids(since.Sessions))
	}

	// invalid sort → 400.
	if code := env.get("/api/sessions?sort=bogus", nil); code != http.StatusBadRequest {
		t.Fatalf("invalid sort status = %d want 400", code)
	}
}

func TestSession_Detail_AndNotFound(t *testing.T) {
	env := newTestEnv(t)
	env.write(model.Batch{
		Sessions: []model.Session{seedSession("s1", 1000, 5000, "events", true)},
		APIRequests: []model.APIRequest{
			apiReq("s1", 2000, "claude-3", 100, 50, 0, 0, 0.01, 0),
			apiReq("s1", 3000, "claude-4", 200, 60, 0, 0, 0.03, 0),
		},
		ToolDecisions: []model.ToolDecision{
			toolDecision("s1", 2100, "accept", "config", "Bash"),
			toolDecision("s1", 2200, "reject", "user_reject", "Bash"),
		},
	})

	var d sessionDetail
	if code := env.get("/api/sessions/s1", &d); code != http.StatusOK {
		t.Fatalf("detail status = %d", code)
	}
	if d.CostUSD != 0.04 {
		t.Fatalf("detail cost = %v want 0.04", d.CostUSD)
	}
	if len(d.Models) != 2 {
		t.Fatalf("want 2 models, got %d", len(d.Models))
	}
	if d.ToolAccept != 1 || d.ToolReject != 1 || d.ToolAcceptRate != 0.5 {
		t.Fatalf("tool stats = accept %d reject %d rate %v", d.ToolAccept, d.ToolReject, d.ToolAcceptRate)
	}

	if code := env.get("/api/sessions/missing", nil); code != http.StatusNotFound {
		t.Fatalf("missing session status = %d want 404", code)
	}
}

// --- cost: buckets -----------------------------------------------------------

func TestCost_Buckets(t *testing.T) {
	env := newTestEnv(t)
	// Two api_requests in the same hour, one in a different hour.
	h := int64(3600 * 1000)
	env.write(model.Batch{
		Sessions: []model.Session{seedSession("c", 0, 3*h, "events", true)},
		APIRequests: []model.APIRequest{
			apiReq("c", 1, "m1", 10, 5, 0, 0, 0.01, 0),
			apiReq("c", 2, "m1", 20, 5, 0, 0, 0.02, 0),
			apiReq("c", 2*h+5, "m2", 30, 5, 100, 0, 0.04, 0),
		},
	})

	var hour costResponse
	env.get("/api/cost?bucket=hour", &hour)
	if hour.TokenSource != "events" {
		t.Fatalf("token_source = %q", hour.TokenSource)
	}
	if len(hour.Series) != 2 {
		t.Fatalf("want 2 hourly buckets, got %d: %+v", len(hour.Series), hour.Series)
	}
	// First bucket combines the two same-hour requests: cost 0.03 input 30.
	if hour.Series[0].CostUSD != 0.03 || hour.Series[0].Input != 30 {
		t.Fatalf("bucket0 = %+v want cost 0.03 input 30", hour.Series[0])
	}
	if len(hour.ByModel) != 2 {
		t.Fatalf("want 2 models, got %d", len(hour.ByModel))
	}
	// cache_hit_ratio = cache_read/(input+cache_read) = 100/(60+100).
	wantRatio := 100.0 / 160.0
	if diff := hour.CacheHitRatio - wantRatio; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("cache_hit_ratio = %v want %v", hour.CacheHitRatio, wantRatio)
	}

	// day bucket collapses everything into one bucket.
	var day costResponse
	env.get("/api/cost?bucket=day", &day)
	if len(day.Series) != 1 {
		t.Fatalf("want 1 daily bucket, got %d", len(day.Series))
	}

	// invalid bucket → 400.
	if code := env.get("/api/cost?bucket=week", nil); code != http.StatusBadRequest {
		t.Fatalf("invalid bucket status = %d", code)
	}
}

func TestCost_Empty(t *testing.T) {
	env := newTestEnv(t)
	var got costResponse
	if code := env.get("/api/cost", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(got.Series) != 0 || len(got.ByModel) != 0 {
		t.Fatalf("expected empty cost")
	}
	if got.Meta == nil || got.Meta.EmptyReason != reasonNoData {
		t.Fatalf("expected empty_reason=no_data, got %+v", got.Meta)
	}
}

// --- tools -------------------------------------------------------------------

func TestTools_AutoApprovedAndHotspots(t *testing.T) {
	env := newTestEnv(t)
	env.write(model.Batch{
		Sessions: []model.Session{seedSession("t", 0, 100, "events", true)},
		ToolDecisions: []model.ToolDecision{
			toolDecision("t", 1, "accept", "config", "Bash"),
			toolDecision("t", 2, "accept", "hook", "Edit"),
			toolDecision("t", 3, "accept", "user_temporary", "Read"),
			toolDecision("t", 4, "reject", "user_reject", "Bash"),
			toolDecision("t", 5, "reject", "user_reject", "Bash"),
			toolDecision("t", 6, "reject", "user_reject", "Write"),
		},
	})

	var got toolsResponse
	if code := env.get("/api/tools", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	// auto_approved = accepts from config/hook = 2 (Bash config + Edit hook).
	if got.AutoApproved != 2 {
		t.Fatalf("auto_approved = %d want 2", got.AutoApproved)
	}
	if got.Accept != 3 || got.Reject != 3 {
		t.Fatalf("accept/reject = %d/%d want 3/3", got.Accept, got.Reject)
	}
	if got.RejectRate != 0.5 {
		t.Fatalf("reject_rate = %v want 0.5", got.RejectRate)
	}
	// hotspots: Bash has 2 rejects (top), Write 1. Read/Edit have 0 → excluded.
	if len(got.Hotspots) != 2 {
		t.Fatalf("hotspots = %d want 2: %+v", len(got.Hotspots), got.Hotspots)
	}
	if got.Hotspots[0].ToolName != "Bash" || got.Hotspots[0].Reject != 2 {
		t.Fatalf("top hotspot = %+v want Bash/2", got.Hotspots[0])
	}
	if got.Hotspots[0].Accept != 1 {
		t.Fatalf("Bash accept = %d want 1", got.Hotspots[0].Accept)
	}
}

func TestTools_Empty(t *testing.T) {
	env := newTestEnv(t)
	var got toolsResponse
	env.get("/api/tools", &got)
	if got.Meta == nil || got.Meta.EmptyReason != reasonNoData {
		t.Fatalf("expected empty_reason=no_data, got %+v", got.Meta)
	}
}

// --- timeline ----------------------------------------------------------------

func TestTimeline_OrderAndFields(t *testing.T) {
	env := newTestEnv(t)
	env.write(model.Batch{
		Sessions: []model.Session{seedSession("tl", 0, 10000, "events", true)},
		Events:   []model.Event{promptEvent("tl", 1000, 42)},
		APIRequests: []model.APIRequest{
			apiReq("tl", 2000, "claude-3", 100, 50, 10, 5, 0.01, 800),
			apiErr("tl", 5000, "claude-3", 429, "rate limited"),
		},
		ToolDecisions: []model.ToolDecision{toolDecision("tl", 3000, "accept", "config", "Bash")},
		ToolResults:   []model.ToolResult{toolResult("tl", 4000, "Bash", true, 120, "ls -la", "")},
	})

	var got struct {
		Events []map[string]any `json:"events"`
		Meta   *meta            `json:"meta"`
	}
	if code := env.get("/api/sessions/tl/timeline", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(got.Events) != 5 {
		t.Fatalf("want 5 events, got %d", len(got.Events))
	}
	// ts ascending: prompt(1000) api_request(2000) decision(3000) result(4000) api_error(5000).
	wantKinds := []string{"prompt", "api_request", "tool_decision", "tool_result", "api_error"}
	for i, k := range wantKinds {
		if got.Events[i]["kind"] != k {
			t.Fatalf("event[%d] kind = %v want %s", i, got.Events[i]["kind"], k)
		}
	}
	// prompt fields. This event was seeded without stored text (promptEvent), so
	// prompt_length is present and the prompt field is explicitly null.
	if got.Events[0]["prompt_length"].(float64) != 42 {
		t.Fatalf("prompt_length = %v", got.Events[0]["prompt_length"])
	}
	if _, ok := got.Events[0]["prompt"]; !ok {
		t.Fatalf("prompt event must include the prompt key (null when not stored)")
	}
	if got.Events[0]["prompt"] != nil {
		t.Fatalf("prompt = %v, want null (no text stored)", got.Events[0]["prompt"])
	}
	// api_request fields.
	ar := got.Events[1]
	if ar["model"] != "claude-3" || ar["input"].(float64) != 100 || ar["cost_usd"].(float64) != 0.01 {
		t.Fatalf("api_request fields = %+v", ar)
	}
	// tool_result has bash_command present and mcp_server null.
	tr := got.Events[3]
	if tr["bash_command"] != "ls -la" {
		t.Fatalf("tool_result bash = %v", tr["bash_command"])
	}
	if _, ok := tr["mcp_server"]; !ok {
		t.Fatalf("tool_result must include mcp_server key (null)")
	}
	if tr["mcp_server"] != nil {
		t.Fatalf("tool_result mcp_server = %v want null", tr["mcp_server"])
	}
	// api_error fields.
	ae := got.Events[4]
	if ae["status_code"].(float64) != 429 || ae["error"] != "rate limited" {
		t.Fatalf("api_error fields = %+v", ae)
	}
}

// TestTimeline_PromptText asserts the stored prompt text is surfaced on the
// prompt node (the default storage path), alongside prompt_length. A prompt
// stored with --no-store-prompts (no text) is covered by TestTimeline_OrderAndFields.
func TestTimeline_PromptText(t *testing.T) {
	env := newTestEnv(t)
	const text = "fix the parser bug\nin handleUser"
	env.write(model.Batch{
		Sessions: []model.Session{seedSession("pt", 0, 10000, "events", true)},
		Events:   []model.Event{promptEventText("pt", 1000, text)},
	})

	var got struct {
		Events []map[string]any `json:"events"`
	}
	if code := env.get("/api/sessions/pt/timeline", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(got.Events) != 1 {
		t.Fatalf("want 1 event, got %d", len(got.Events))
	}
	ev := got.Events[0]
	if ev["kind"] != "prompt" {
		t.Fatalf("kind = %v, want prompt", ev["kind"])
	}
	if ev["prompt"] != text {
		t.Fatalf("prompt = %q, want %q", ev["prompt"], text)
	}
	if ev["prompt_length"].(float64) != float64(len(text)) {
		t.Fatalf("prompt_length = %v, want %d", ev["prompt_length"], len(text))
	}
}

func TestTimeline_LogsDisabledEmpty(t *testing.T) {
	env := newTestEnv(t)
	// Session exists (from metrics) but has no log events → logs_disabled.
	env.write(model.Batch{
		Sessions:     []model.Session{seedSession("md", 0, 5000, "metrics", false)},
		MetricPoints: []model.MetricPoint{metricPoint("md", 1000, metricTokenUsageCC, "input", 50, 0, 1, 2)},
	})
	var got struct {
		Events []map[string]any `json:"events"`
		Meta   *meta            `json:"meta"`
	}
	if code := env.get("/api/sessions/md/timeline", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(got.Events) != 0 {
		t.Fatalf("want 0 events, got %d", len(got.Events))
	}
	if got.Meta == nil || got.Meta.EmptyReason != reasonLogsDisabled {
		t.Fatalf("expected logs_disabled, got %+v", got.Meta)
	}
}

// --- export ------------------------------------------------------------------

func TestExport_HTTPAndFunc(t *testing.T) {
	env := newTestEnv(t)
	env.write(model.Batch{
		Sessions:      []model.Session{seedSession("ex", 1000, 5000, "events", true)},
		APIRequests:   []model.APIRequest{apiReq("ex", 2000, "claude-3", 100, 50, 0, 0, 0.01, 700)},
		ToolDecisions: []model.ToolDecision{toolDecision("ex", 2100, "accept", "config", "Bash")},
		ToolResults:   []model.ToolResult{toolResult("ex", 2200, "Bash", true, 50, "echo hi", "")},
		Events:        []model.Event{promptEvent("ex", 1500, 7)},
	})

	// HTTP path.
	var doc exportObject
	if code := env.get("/api/export?session=ex", &doc); code != http.StatusOK {
		t.Fatalf("export status = %d", code)
	}
	if doc.Session.ID != "ex" || !doc.Session.HasEvents {
		t.Fatalf("export session meta = %+v", doc.Session)
	}
	if len(doc.APIRequests) != 1 || doc.APIRequests[0].Input != 100 {
		t.Fatalf("export api_requests = %+v", doc.APIRequests)
	}
	if len(doc.ToolDecisions) != 1 || len(doc.ToolResults) != 1 || len(doc.Events) != 1 {
		t.Fatalf("export children wrong: dec %d res %d ev %d",
			len(doc.ToolDecisions), len(doc.ToolResults), len(doc.Events))
	}

	// Reusable ExportSession func writes identical structure to a writer.
	var buf bytes.Buffer
	if err := ExportSession(env.store.ReadDB(), "ex", &buf); err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	if !strings.Contains(buf.String(), `"id": "ex"`) {
		t.Fatalf("ExportSession output missing session id: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"api_requests"`) {
		t.Fatalf("ExportSession output missing api_requests")
	}

	// missing session → 404.
	if code := env.get("/api/export?session=nope", nil); code != http.StatusNotFound {
		t.Fatalf("missing export status = %d want 404", code)
	}
	// missing session param → 400.
	if code := env.get("/api/export", nil); code != http.StatusBadRequest {
		t.Fatalf("no session export status = %d want 400", code)
	}
}

// --- setup -------------------------------------------------------------------

func TestSetup(t *testing.T) {
	env := newTestEnv(t)
	var got setupResponse
	if code := env.get("/api/setup", &got); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(got.Snippet, "CLAUDE_CODE_ENABLE_TELEMETRY") {
		t.Fatalf("snippet missing telemetry env: %q", got.Snippet)
	}
	if got.Ports.GRPC == 0 || got.Ports.HTTP == 0 {
		t.Fatalf("ports = %+v", got.Ports)
	}
}

// --- SSE: tick + log + drop --------------------------------------------------

func TestNow_SSE_TickLogDrop(t *testing.T) {
	env := newTestEnv(t)

	// Seed a snapshot so the first tick carries data.
	env.bus.SetSnapshot(ingest.NowSnapshot{
		SessionID:     "live",
		CostUSD:       0.05,
		Tokens:        model.Tokens{Input: 10, Output: 5},
		ActiveTimeSec: 1.5,
		ActiveTool:    "Bash",
		TS:            12345,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, env.ts.URL+"/api/now", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE GET: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)

	gotStatus, gotTick, gotLog, gotDrop := false, false, false, false

	// Publish a normal log event after the connection is established.
	publishedLog := false
	deadline := time.Now().Add(4 * time.Second)

	var curEvent string
	for time.Now().Before(deadline) && !(gotStatus && gotTick && gotLog && gotDrop) {
		if !sc.Scan() {
			break
		}
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			curEvent = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			switch curEvent {
			case "status":
				if strings.Contains(data, "live") {
					gotStatus = true
				}
			case "tick":
				if strings.Contains(data, `"session_id":"live"`) && strings.Contains(data, `"cost_usd":0.05`) {
					gotTick = true
				}
				if !publishedLog {
					publishedLog = true
					env.bus.Publish(ingest.LogEvent{TS: 99, Kind: "api_request", Summary: "claude-3", Fields: map[string]any{"cost_usd": 0.01}})
					// Force a ring overflow to generate a drop marker.
					forceDrop(env.bus)
				}
			case "log":
				if strings.Contains(data, `"kind":"api_request"`) {
					gotLog = true
				}
			case "drop":
				if strings.Contains(data, `"dropped":`) {
					gotDrop = true
				}
			}
		}
	}

	if !gotStatus {
		t.Fatalf("did not receive status:live")
	}
	if !gotTick {
		t.Fatalf("did not receive tick with snapshot")
	}
	if !gotLog {
		t.Fatalf("did not receive forwarded log event")
	}
	if !gotDrop {
		t.Fatalf("did not receive drop marker")
	}
}

// forceDrop publishes more log events than the ring can hold so the bus emits a
// drop marker on at least one subscriber.
func forceDrop(bus *ingest.Bus) {
	for i := 0; i < 1024; i++ {
		bus.Publish(ingest.LogEvent{TS: int64(i), Kind: "tool_result", Summary: "x", Fields: map[string]any{}})
	}
}

func ids(rows []sessionRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}
