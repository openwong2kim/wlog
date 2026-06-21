package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openwong2kim/wlog/internal/config"
	"github.com/openwong2kim/wlog/internal/ingest"
	"github.com/openwong2kim/wlog/internal/model"
)

// --- C5: /api/cost mixed source (per-session axis-of-truth) ------------------

// An event session and a metric-only session coexist in scope. Both must appear
// in the cost series / by_model, and token_source must report "mixed". The bug
// was a single events-vs-metrics decision for the whole query, which dropped
// every metric-only session as soon as any api_request existed anywhere.
func TestCost_MixedSources(t *testing.T) {
	env := newTestEnv(t)
	h := int64(3600 * 1000)
	env.write(model.Batch{
		Sessions: []model.Session{
			seedSession("ev", 0, h, "events", true),
			seedSession("mt", 0, h, "metrics", false),
		},
		// Event session: cost 0.02, input 100, output 40 in bucket 0.
		APIRequests: []model.APIRequest{
			apiReq("ev", 10, "claude-ev", 100, 40, 0, 0, 0.02, 0),
		},
		// Metric-only session: cost 0.05, input 200, output 60 in bucket 0.
		MetricPoints: []model.MetricPoint{
			metricPoint("mt", 20, metricTokenUsageCC, "input", 200, 0, 1, 2),
			metricPoint("mt", 20, metricTokenUsageCC, "output", 60, 0, 1, 3),
			metricPoint("mt", 20, metricCostUsageCC, "claude-mt", 0.05, 0, 1, 4),
		},
	})

	var got costResponse
	if code := env.get("/api/cost?bucket=hour", &got); code != http.StatusOK {
		t.Fatalf("cost status = %d", code)
	}
	if got.TokenSource != "mixed" {
		t.Fatalf("token_source = %q want mixed", got.TokenSource)
	}

	// Both sessions land in the single bucket 0: combined cost + tokens.
	if len(got.Series) != 1 {
		t.Fatalf("want 1 bucket (combined), got %d: %+v", len(got.Series), got.Series)
	}
	p := got.Series[0]
	if !approx(p.CostUSD, 0.07) {
		t.Fatalf("combined cost = %v want 0.07 (0.02 events + 0.05 metrics)", p.CostUSD)
	}
	if p.Input != 300 || p.Output != 100 {
		t.Fatalf("combined tokens = in %d out %d want in 300 out 100", p.Input, p.Output)
	}

	// by_model must include BOTH the event model and the metric model.
	models := map[string]float64{}
	for _, m := range got.ByModel {
		models[m.Model] = m.CostUSD
	}
	if !approx(models["claude-ev"], 0.02) {
		t.Fatalf("by_model missing/incorrect claude-ev: %+v", got.ByModel)
	}
	if !approx(models["claude-mt"], 0.05) {
		t.Fatalf("by_model missing/incorrect claude-mt (metric-only dropped?): %+v", got.ByModel)
	}
}

// Real Claude Code sessions emit BOTH api_request events AND token.usage /
// cost.usage metrics, so the coarse ingestion-time sessions.token_source column
// could say "metrics" on a session whose values are actually event-sourced.
// The displayed token_source must be computed from the same axis-of-truth as
// the values (EXISTS api_requests -> "events"), never the stored column.
// Regression for the real-data dogfood finding.
func TestSessions_TokenSourceLabelMatchesValues(t *testing.T) {
	env := newTestEnv(t)
	h := int64(3600 * 1000)
	env.write(model.Batch{
		Sessions: []model.Session{
			// Stored label deliberately WRONG ("metrics") to mimic the
			// ingestion-time guess on a real session carrying both signals.
			seedSession("real", 0, h, "metrics", true),
		},
		APIRequests: []model.APIRequest{
			apiReq("real", 10, "claude-haiku", 18, 148, 67445, 24985, 0.0575, 0),
		},
		MetricPoints: []model.MetricPoint{
			metricPoint("real", 20, metricTokenUsageCC, "input", 18, 0, 1, 2),
			metricPoint("real", 20, metricCostUsageCC, "claude-haiku", 0.0575, 0, 1, 3),
		},
	})

	var list sessionsResponse
	if code := env.get("/api/sessions", &list); code != http.StatusOK {
		t.Fatalf("sessions status = %d", code)
	}
	if len(list.Sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(list.Sessions))
	}
	s := list.Sessions[0]
	if s.TokenSource != "events" {
		t.Fatalf("list token_source = %q want events (session has api_requests, ignore stored 'metrics')", s.TokenSource)
	}
	// And the values must be the event-sourced ones (not double-counted with metrics).
	if s.Tokens.Input != 18 || !approx(s.CostUSD, 0.0575) {
		t.Fatalf("values not event-sourced: in=%d cost=%v want in=18 cost=0.0575", s.Tokens.Input, s.CostUSD)
	}

	var detail sessionDetail
	if code := env.get("/api/sessions/real", &detail); code != http.StatusOK {
		t.Fatalf("detail status = %d", code)
	}
	if detail.TokenSource != "events" {
		t.Fatalf("detail token_source = %q want events", detail.TokenSource)
	}
}

// Sanity: an events-only scope still reports token_source "events" and a
// metrics-only scope reports "metrics" (the non-mixed branches of the fix).
func TestCost_SingleSourceLabels(t *testing.T) {
	env := newTestEnv(t)
	env.write(model.Batch{
		Sessions:    []model.Session{seedSession("ev", 0, 100, "events", true)},
		APIRequests: []model.APIRequest{apiReq("ev", 10, "m", 10, 5, 0, 0, 0.01, 0)},
	})
	var ev costResponse
	env.get("/api/cost", &ev)
	if ev.TokenSource != "events" {
		t.Fatalf("events-only token_source = %q want events", ev.TokenSource)
	}

	env2 := newTestEnv(t)
	env2.write(model.Batch{
		Sessions: []model.Session{seedSession("mt", 0, 100, "metrics", false)},
		MetricPoints: []model.MetricPoint{
			metricPoint("mt", 10, metricTokenUsageCC, "input", 50, 0, 1, 2),
			metricPoint("mt", 10, metricCostUsageCC, "claude-mt", 0.03, 0, 1, 3),
		},
	})
	var mt costResponse
	env2.get("/api/cost", &mt)
	if mt.TokenSource != "metrics" {
		t.Fatalf("metrics-only token_source = %q want metrics", mt.TokenSource)
	}
}

// --- C6: modelBreakdown branches on EXISTS(api_requests), not has_events -----

// A session with log events (has_events=true via a prompt) but NO api_request:
// its cost is metric-sourced, and its model breakdown must also be metric-sourced
// (non-empty), consistent with the session cost. The bug branched modelBreakdown
// on has_events, returning the (empty) api_requests breakdown for this session.
func TestSession_ModelBreakdown_PromptOnlyMetricCost(t *testing.T) {
	env := newTestEnv(t)
	env.write(model.Batch{
		// has_events=true: a prompt event exists, but there are no api_requests.
		Sessions: []model.Session{seedSession("po", 1000, 5000, "metrics", true)},
		Events:   []model.Event{promptEvent("po", 1500, 12)},
		MetricPoints: []model.MetricPoint{
			metricPoint("po", 2000, metricTokenUsageCC, "input", 150, 0, 1, 2),
			metricPoint("po", 2000, metricTokenUsageCC, "output", 70, 0, 1, 3),
			metricPoint("po", 2000, metricCostUsageCC, "claude-x", 0.04, 0, 1, 4),
		},
	})

	var d sessionDetail
	if code := env.get("/api/sessions/po", &d); code != http.StatusOK {
		t.Fatalf("detail status = %d", code)
	}
	// Session cost is metric-sourced.
	if !approx(d.CostUSD, 0.04) {
		t.Fatalf("session cost = %v want 0.04 (metric)", d.CostUSD)
	}
	if d.TokenSource != "metrics" {
		t.Fatalf("token_source = %q want metrics", d.TokenSource)
	}
	// Model breakdown must NOT be empty — it falls back to cost.usage by model.
	if len(d.Models) != 1 {
		t.Fatalf("want 1 model (metric fallback), got %d: %+v", len(d.Models), d.Models)
	}
	if d.Models[0].Model != "claude-x" || !approx(d.Models[0].CostUSD, 0.04) {
		t.Fatalf("model breakdown = %+v want claude-x/0.04", d.Models[0])
	}
	// Breakdown total cost must be consistent with the session cost.
	var sum float64
	for _, m := range d.Models {
		sum += m.CostUSD
	}
	if !approx(sum, d.CostUSD) {
		t.Fatalf("breakdown cost sum %v != session cost %v", sum, d.CostUSD)
	}
}

// --- F1: --auth-token enforcement (loopback-exempt) --------------------------

// authEnv builds a Server with a fixed auth token and returns its Handler, so we
// can drive requests with arbitrary RemoteAddr values via httptest.NewRecorder.
func authHandler(t *testing.T, token string) http.Handler {
	t.Helper()
	cfg := config.Default()
	cfg.AuthToken = token
	srv := New(cfg, nil, ingest.NewBus(0), func() ingest.Counters { return ingest.Counters{} }, "")
	return srv.Handler()
}

func TestAuth_TokenEnforcement(t *testing.T) {
	const token = "s3cr3t-token"
	h := authHandler(t, token)

	do := func(remoteAddr, target string, header http.Header) int {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.RemoteAddr = remoteAddr
		for k, vs := range header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// (a) loopback peer, no token → exempt, passes (200 from /api/setup).
	if code := do("127.0.0.1:54321", "/api/setup", nil); code != http.StatusOK {
		t.Fatalf("(a) loopback no-token status = %d want 200", code)
	}
	// loopback v6 is also exempt.
	if code := do("[::1]:54321", "/api/setup", nil); code != http.StatusOK {
		t.Fatalf("(a) loopback ::1 no-token status = %d want 200", code)
	}

	// (b) non-loopback peer, no token → 401.
	if code := do("203.0.113.7:40000", "/api/setup", nil); code != http.StatusUnauthorized {
		t.Fatalf("(b) non-loopback no-token status = %d want 401", code)
	}
	// non-loopback with a WRONG token → 401.
	wrong := http.Header{"Authorization": {"Bearer not-the-token"}}
	if code := do("203.0.113.7:40000", "/api/setup", wrong); code != http.StatusUnauthorized {
		t.Fatalf("(b) non-loopback wrong-token status = %d want 401", code)
	}

	// (c) non-loopback peer, correct Bearer token → passes.
	good := http.Header{"Authorization": {"Bearer " + token}}
	if code := do("203.0.113.7:40000", "/api/setup", good); code != http.StatusOK {
		t.Fatalf("(c) non-loopback bearer-token status = %d want 200", code)
	}

	// (d) non-loopback SSE-style ?token= query param → passes. /api/setup is used
	// (no streaming) so the assertion is purely on the auth gate, not SSE wiring.
	if code := do("203.0.113.7:40000", "/api/setup?token="+token, nil); code != http.StatusOK {
		t.Fatalf("(d) non-loopback ?token= status = %d want 200", code)
	}
	// wrong ?token= → 401.
	if code := do("203.0.113.7:40000", "/api/setup?token=nope", nil); code != http.StatusUnauthorized {
		t.Fatalf("(d) non-loopback wrong ?token= status = %d want 401", code)
	}

	// The auth gate also wraps static (non-/api) serving: a non-loopback request
	// for the UI root without a token is rejected.
	if code := do("203.0.113.7:40000", "/", nil); code != http.StatusUnauthorized {
		t.Fatalf("static non-loopback no-token status = %d want 401", code)
	}
}

// When no auth token is configured, non-loopback requests pass unchanged (the
// middleware is a pass-through — bind policy is enforced at config.Validate).
func TestAuth_DisabledWhenNoToken(t *testing.T) {
	h := authHandler(t, "")
	req := httptest.NewRequest(http.MethodGet, "/api/setup", nil)
	req.RemoteAddr = "203.0.113.7:40000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-token-config non-loopback status = %d want 200", rec.Code)
	}
}

// --- C7: /api/cost dollar rollups (cum_cost / total / burn rate / cache saved) -

// cum_cost_usd accumulates monotonically across the ts-ascending series, and
// total_cost_usd equals the plain sum of per-bucket cost. Two hour buckets are
// seeded so the running total is observable.
func TestCost_CumulativeAndTotal(t *testing.T) {
	env := newTestEnv(t)
	h := int64(3600 * 1000)
	env.write(model.Batch{
		Sessions: []model.Session{seedSession("ev", 0, 3*h, "events", true)},
		APIRequests: []model.APIRequest{
			apiReq("ev", 10, "claude-sonnet", 100, 40, 0, 0, 0.02, 0),  // bucket 0
			apiReq("ev", h+10, "claude-sonnet", 50, 20, 0, 0, 0.03, 0), // bucket 1
			apiReq("ev", h+20, "claude-sonnet", 50, 20, 0, 0, 0.05, 0), // bucket 1
		},
	})

	var got costResponse
	if code := env.get("/api/cost?bucket=hour", &got); code != http.StatusOK {
		t.Fatalf("cost status = %d", code)
	}
	if len(got.Series) != 2 {
		t.Fatalf("want 2 buckets, got %d: %+v", len(got.Series), got.Series)
	}

	// cum_cost_usd is monotonically non-decreasing and equals the running sum.
	var run float64
	var prev float64 = -1
	for i, p := range got.Series {
		run += p.CostUSD
		if !approx(p.CumCostUSD, run) {
			t.Fatalf("bucket %d cum_cost_usd = %v want running sum %v", i, p.CumCostUSD, run)
		}
		if p.CumCostUSD < prev {
			t.Fatalf("cum_cost_usd decreased at bucket %d: %v < %v", i, p.CumCostUSD, prev)
		}
		prev = p.CumCostUSD
	}

	// total_cost_usd = sum of series cost = last cum value.
	if !approx(got.TotalCostUSD, 0.10) {
		t.Fatalf("total_cost_usd = %v want 0.10", got.TotalCostUSD)
	}
	if !approx(got.TotalCostUSD, got.Series[len(got.Series)-1].CumCostUSD) {
		t.Fatalf("total_cost_usd %v != last cum %v", got.TotalCostUSD, got.Series[len(got.Series)-1].CumCostUSD)
	}

	// burn_rate over a 2-hour span (first bucket start 0 to end of bucket 1 =
	// 2h = 120 min): 0.10 / 120.
	if !approxTol(got.BurnRateUSDPerMin, 0.10/120.0, 1e-6) {
		t.Fatalf("burn_rate_usd_per_min = %v want %v", got.BurnRateUSDPerMin, 0.10/120.0)
	}
}

// Single-bucket and empty scopes must not divide by zero: a single bucket uses
// one bucket-width as the span; an empty scope yields 0 rates.
func TestCost_BurnRateZeroDivisionGuard(t *testing.T) {
	// Single bucket: span = one hour (60 min). cost 0.06 -> 0.001/min.
	env := newTestEnv(t)
	env.write(model.Batch{
		Sessions:    []model.Session{seedSession("ev", 0, 100, "events", true)},
		APIRequests: []model.APIRequest{apiReq("ev", 10, "claude-haiku", 10, 5, 0, 0, 0.06, 0)},
	})
	var one costResponse
	if code := env.get("/api/cost?bucket=hour", &one); code != http.StatusOK {
		t.Fatalf("cost status = %d", code)
	}
	if len(one.Series) != 1 {
		t.Fatalf("want 1 bucket, got %d", len(one.Series))
	}
	if !approxTol(one.BurnRateUSDPerMin, 0.06/60.0, 1e-6) {
		t.Fatalf("single-bucket burn_rate_usd_per_min = %v want %v (one bucket width)", one.BurnRateUSDPerMin, 0.06/60.0)
	}
	if one.BurnRateTokPerMin <= 0 {
		t.Fatalf("single-bucket burn_rate_tok_per_min = %v want > 0", one.BurnRateTokPerMin)
	}

	// Empty scope: no data, rates 0, total 0, no NaN/Inf.
	env2 := newTestEnv(t)
	var empty costResponse
	if code := env2.get("/api/cost?bucket=hour", &empty); code != http.StatusOK {
		t.Fatalf("cost status = %d", code)
	}
	if empty.TotalCostUSD != 0 || empty.BurnRateUSDPerMin != 0 || empty.BurnRateTokPerMin != 0 {
		t.Fatalf("empty scope rates not zero: total=%v usd/min=%v tok/min=%v",
			empty.TotalCostUSD, empty.BurnRateUSDPerMin, empty.BurnRateTokPerMin)
	}
}

// cache_saved_usd is a positive price-table estimate for known models with
// cache reads, and estimated=true flags it. haiku: (1 - 0.10)/1e6 * 1_000_000.
func TestCost_CacheSavedKnownModel(t *testing.T) {
	env := newTestEnv(t)
	env.write(model.Batch{
		Sessions: []model.Session{seedSession("ev", 0, 100, "events", true)},
		APIRequests: []model.APIRequest{
			// 1,000,000 cache_read tokens on haiku, 500,000 on opus.
			apiReq("ev", 10, "claude-haiku-4", 100, 40, 1_000_000, 0, 0.05, 0),
			apiReq("ev", 20, "claude-opus-4", 100, 40, 500_000, 0, 0.20, 0),
		},
	})
	var got costResponse
	if code := env.get("/api/cost?bucket=hour", &got); code != http.StatusOK {
		t.Fatalf("cost status = %d", code)
	}
	if got.CacheSavedUSD == nil {
		t.Fatalf("cache_saved_usd = null want a positive estimate")
	}
	// haiku (1-0.10)/1e6*1e6 = 0.90 ; opus (15-1.5)/1e6*5e5 = 6.75 ; total 7.65.
	if !approxTol(*got.CacheSavedUSD, 0.90+6.75, 1e-6) {
		t.Fatalf("cache_saved_usd = %v want 7.65", *got.CacheSavedUSD)
	}
	if !got.CacheSavedEstimate {
		t.Fatalf("cache_saved_estimated = false want true")
	}
}

// No cache reads at all -> cache_saved_usd is null and estimated is false (no
// savings exist to estimate).
func TestCost_CacheSavedZeroWhenNoCacheReads(t *testing.T) {
	env := newTestEnv(t)
	env.write(model.Batch{
		Sessions:    []model.Session{seedSession("ev", 0, 100, "events", true)},
		APIRequests: []model.APIRequest{apiReq("ev", 10, "claude-haiku-4", 100, 40, 0, 0, 0.05, 0)},
	})
	var got costResponse
	if code := env.get("/api/cost?bucket=hour", &got); code != http.StatusOK {
		t.Fatalf("cost status = %d", code)
	}
	if got.CacheSavedUSD != nil {
		t.Fatalf("cache_saved_usd = %v want null (no cache reads)", *got.CacheSavedUSD)
	}
	if got.CacheSavedEstimate {
		t.Fatalf("cache_saved_estimated = true want false (no cache reads)")
	}
}

// approx reports float equality within a small epsilon (cost arithmetic).
func approx(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}

// approxTol reports float equality within a caller-supplied tolerance, for rate
// arithmetic where the 1e-9 epsilon is too tight.
func approxTol(a, b, tol float64) bool {
	d := a - b
	return d < tol && d > -tol
}
