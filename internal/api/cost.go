package api

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"strings"

	"github.com/openwong2kim/wlog/internal/pricing"
)

// costPoint is one time-bucket in the cost series (PLAN §10). CumCostUSD is the
// running total of CostUSD over all buckets up to and including this one (series
// is ts-ascending), so a chart can draw cumulative spend without re-summing.
type costPoint struct {
	TS            int64   `json:"ts"`
	CostUSD       float64 `json:"cost_usd"`
	CumCostUSD    float64 `json:"cum_cost_usd"`
	Input         int64   `json:"input"`
	Output        int64   `json:"output"`
	CacheRead     int64   `json:"cache_read"`
	CacheCreation int64   `json:"cache_creation"`
}

type costByModel struct {
	Model   string  `json:"model"`
	CostUSD float64 `json:"cost_usd"`
	Tokens  int64   `json:"tokens"`
	Pct     float64 `json:"pct"`
}

type costResponse struct {
	Series        []costPoint   `json:"series"`
	ByModel       []costByModel `json:"by_model"`
	CacheHitRatio float64       `json:"cache_hit_ratio"`
	TokenSource   string        `json:"token_source"`

	// Dollar/rate rollups (PLAN: Cost screen must show money, not just tokens).
	TotalCostUSD       float64  `json:"total_cost_usd"`        // sum of all series cost (scope total)
	BurnRateUSDPerMin  float64  `json:"burn_rate_usd_per_min"` // total cost / scope span in minutes
	BurnRateTokPerMin  float64  `json:"burn_rate_tok_per_min"` // total tokens / scope span in minutes
	CacheSavedUSD      *float64 `json:"cache_saved_usd"`       // estimated $ saved by cache reads; null when no priced model contributed
	CacheSavedEstimate bool     `json:"cache_saved_estimated"` // true: derived from the static price table (an estimate)

	Meta *meta `json:"meta,omitempty"`
}

// handleCost returns a time-bucketed cost/token series, per-model breakdown,
// cache hit ratio, and the token_source. Source selection is PER SESSION (PLAN
// §6.5 axis-of-truth): a session with any api_request is aggregated from events,
// a session with none falls back to its token.usage / cost.usage metric points.
// The two sources are summed ACROSS sessions but never within one session, so a
// metrics-only session is never dropped just because some other session has
// events. token_source reflects which sources contributed: "events", "metrics",
// or "mixed" when both kinds of session are present in scope.
func (s *Server) handleCost(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	bucket := q.Get("bucket")
	switch bucket {
	case "", "hour":
		bucket = "hour"
	case "day":
	default:
		badRequest(w, "invalid bucket: "+bucket)
		return
	}
	// SQLite ms-epoch bucket truncation: hour = 3600_000 ms, day = 86400_000 ms.
	bucketMS := int64(3600 * 1000)
	if bucket == "day" {
		bucketMS = 86400 * 1000
	}

	since := int64(parseIntDefault(q.Get("since"), 0))
	model := strings.TrimSpace(q.Get("model"))
	session := strings.TrimSpace(q.Get("session"))

	ctx := r.Context()

	// Determine which session kinds are in scope so token_source can report
	// events / metrics / mixed. The series/by_model aggregation itself selects
	// the source per session (see costSeriesEvents / costSeriesMetrics, both of
	// which restrict to their own session set), so the two sources are never
	// summed within a single session.
	hasEventSessions, hasMetricSessions, err := s.scopeSources(ctx, since, model, session)
	if err != nil {
		internalError(w, "failed to determine token source")
		return
	}

	resp := costResponse{Series: []costPoint{}, ByModel: []costByModel{}}
	resp.TokenSource = tokenSourceLabel(hasEventSessions, hasMetricSessions)

	// Accumulate series buckets and per-model rows across BOTH source sets, then
	// fold them so an event session and a metric-only session land in the same
	// bucket / model entry.
	series := map[int64]*costPoint{}
	byModel := map[string]*costByModel{}
	var sumInput, sumCacheRead int64
	// Per-model cache_read tokens, used for the price-table cache-savings
	// estimate. Event sessions attribute cache_read to a model directly; the
	// metric pass folds in unattributed cache_read under "" (no model), which is
	// excluded from the estimate but flips the "estimated" flag context (see
	// cacheSaved).
	modelCacheRead := map[string]int64{}

	if hasEventSessions {
		if err := s.costSeriesEvents(ctx, series, byModel, &sumInput, &sumCacheRead, modelCacheRead, bucketMS, since, model, session); err != nil {
			internalError(w, "failed to query cost series")
			return
		}
	}
	if hasMetricSessions {
		if err := s.costSeriesMetrics(ctx, series, byModel, &sumInput, &sumCacheRead, modelCacheRead, bucketMS, since, model, session); err != nil {
			internalError(w, "failed to query cost series")
			return
		}
	}

	resp.Series = sortedSeries(series)
	resp.ByModel = sortedByModel(byModel)
	resp.CacheHitRatio = cacheHitRatio(sumCacheRead, sumInput)

	// Dollar rollups derived from the assembled series (same axis-of-truth as the
	// per-bucket cost — no extra aggregation, no double counting).
	resp.TotalCostUSD = seriesTotalCost(resp.Series)
	resp.BurnRateUSDPerMin, resp.BurnRateTokPerMin = burnRates(resp.Series, bucketMS)
	resp.CacheSavedUSD, resp.CacheSavedEstimate = cacheSaved(modelCacheRead)

	if len(resp.Series) == 0 && len(resp.ByModel) == 0 {
		resp.Meta = &meta{EmptyReason: reasonNoData}
	}
	writeJSON(w, http.StatusOK, resp)
}

// tokenSourceLabel maps the (events present, metrics present) booleans to the
// token_source response field: "mixed" when both, else the single source, else
// "" (no data).
func tokenSourceLabel(hasEvents, hasMetrics bool) string {
	switch {
	case hasEvents && hasMetrics:
		return "mixed"
	case hasEvents:
		return "events"
	case hasMetrics:
		return "metrics"
	default:
		return ""
	}
}

// scopeSources reports, for the (since, model, session) scope, whether any
// event session (a session owning a matching api_request) and any metric-only
// session (a session with matching cost/token metric points and NO api_request)
// exist. These two booleans drive token_source and which aggregation passes run.
func (s *Server) scopeSources(ctx context.Context, since int64, model, session string) (hasEvents, hasMetrics bool, err error) {
	where, args := apiReqScope(since, model, session)
	var n int
	if err = s.db.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM api_requests ar "+where+")", args...).Scan(&n); err != nil {
		return false, false, err
	}
	hasEvents = n != 0

	// Metric-only sessions: a metric_points row in scope whose session has no
	// api_request at all (so it is never double-counted against the event pass).
	mconds, margs := metricScopeConds(since, model, session)
	q := `SELECT EXISTS(
		SELECT 1 FROM metric_points mp
		WHERE ` + strings.Join(mconds, " AND ") + `
		  AND NOT EXISTS (SELECT 1 FROM api_requests ar WHERE ar.session_id = mp.session_id))`
	if err = s.db.QueryRowContext(ctx, q, margs...).Scan(&n); err != nil {
		return false, false, err
	}
	hasMetrics = n != 0
	return hasEvents, hasMetrics, nil
}

// apiReqScope builds the WHERE clause + args for api_requests scoped by since,
// model, session (all parameter-bound, all optional).
func apiReqScope(since int64, model, session string) (string, []any) {
	var conds []string
	var args []any
	if since > 0 {
		conds = append(conds, "ar.ts >= ?")
		args = append(args, since)
	}
	if model != "" {
		conds = append(conds, "ar.model = ?")
		args = append(args, model)
	}
	if session != "" {
		conds = append(conds, "ar.session_id = ?")
		args = append(args, session)
	}
	if len(conds) == 0 {
		return "", args
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

// costSeriesEvents folds the api_request aggregation for event sessions into the
// shared series + byModel maps and the running input / cache_read totals. It
// implicitly restricts to event sessions because api_requests only exist for
// them, so it never overlaps the metric-only pass.
func (s *Server) costSeriesEvents(ctx context.Context, series map[int64]*costPoint, byModel map[string]*costByModel, sumInput, sumCacheRead *int64, modelCacheRead map[string]int64, bucketMS, since int64, model, session string) error {
	where, args := apiReqScope(since, model, session)

	// Series: bucket = (ts / bucketMS) * bucketMS.
	seriesArgs := append([]any{bucketMS, bucketMS}, args...)
	rows, err := s.db.QueryContext(ctx, `
		SELECT (ar.ts / ?) * ? AS bkt,
		       SUM(ar.cost_usd), SUM(ar.input_tokens), SUM(ar.output_tokens),
		       SUM(ar.cache_read), SUM(ar.cache_creation)
		FROM api_requests ar
		`+where+`
		GROUP BY bkt
		ORDER BY bkt ASC`, seriesArgs...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p costPoint
		if err := rows.Scan(&p.TS, &p.CostUSD, &p.Input, &p.Output, &p.CacheRead, &p.CacheCreation); err != nil {
			return err
		}
		addSeriesPoint(series, p)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// by_model + cache_hit_ratio contributions.
	mrows, err := s.db.QueryContext(ctx, `
		SELECT ar.model,
		       SUM(ar.cost_usd),
		       SUM(ar.input_tokens + ar.output_tokens),
		       SUM(ar.input_tokens), SUM(ar.cache_read)
		FROM api_requests ar
		`+where+`
		GROUP BY ar.model
		ORDER BY SUM(ar.cost_usd) DESC, ar.model ASC`, args...)
	if err != nil {
		return err
	}
	defer mrows.Close()

	for mrows.Next() {
		var (
			m             sql.NullString
			cost          float64
			tokens        int64
			input, cacheR int64
		)
		if err := mrows.Scan(&m, &cost, &tokens, &input, &cacheR); err != nil {
			return err
		}
		addModel(byModel, m.String, cost, tokens)
		*sumInput += input
		*sumCacheRead += cacheR
		modelCacheRead[m.String] += cacheR
	}
	return mrows.Err()
}

// costSeriesMetrics folds the cost.usage / token.usage metric aggregation for
// metric-only sessions into the shared series + byModel maps and the running
// totals. It restricts to sessions that have NO api_request (the metric_points
// NOT EXISTS guard) so it never overlaps the event pass — the per-session
// axis-of-truth selection. Token attribution by type comes from token.usage
// attr_key; per-model cost from cost.usage attr_key (tokens per model are not
// available from the metric stream, so those rows carry zero tokens).
func (s *Server) costSeriesMetrics(ctx context.Context, series map[int64]*costPoint, byModel map[string]*costByModel, sumInput, sumCacheRead *int64, modelCacheRead map[string]int64, bucketMS, since int64, model, session string) error {
	// Series: cost from cost.usage, tokens from token.usage, bucketed by ts.
	conds, args := metricScopeConds(since, model, session)
	where := "WHERE " + strings.Join(conds, " AND ")

	seriesArgs := append([]any{bucketMS, bucketMS}, args...)
	rows, err := s.db.QueryContext(ctx, `
		SELECT (mp.ts / ?) * ? AS bkt,
		       SUM(CASE WHEN mp.name IN ('`+metricCostUsage+`','`+metricCostUsageCC+`') THEN mp.value_delta ELSE 0 END) AS cost,
		       CAST(SUM(CASE WHEN mp.attr_key IN ('input','input_tokens') THEN mp.value_delta ELSE 0 END) AS INTEGER) AS in_tok,
		       CAST(SUM(CASE WHEN mp.attr_key IN ('output','output_tokens') THEN mp.value_delta ELSE 0 END) AS INTEGER) AS out_tok,
		       CAST(SUM(CASE WHEN mp.attr_key IN ('cacheRead','cache_read','cache_read_tokens') THEN mp.value_delta ELSE 0 END) AS INTEGER) AS cr_tok,
		       CAST(SUM(CASE WHEN mp.attr_key IN ('cacheCreation','cache_creation','cache_creation_tokens') THEN mp.value_delta ELSE 0 END) AS INTEGER) AS cc_tok
		FROM metric_points mp
		`+where+`
		GROUP BY bkt
		ORDER BY bkt ASC`, seriesArgs...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p costPoint
		if err := rows.Scan(&p.TS, &p.CostUSD, &p.Input, &p.Output, &p.CacheRead, &p.CacheCreation); err != nil {
			return err
		}
		*sumInput += p.Input
		*sumCacheRead += p.CacheRead
		// token.usage cache_read carries no model dimension, so it is attributed
		// to the unknown-model key "". cacheSaved excludes it from the priced
		// estimate but treats its presence as "estimated" (some savings exist
		// that we cannot price).
		modelCacheRead[""] += p.CacheRead
		addSeriesPoint(series, p)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// by_model: cost.usage grouped by attr_key (model), metric-only sessions only.
	var mconds []string
	var margs []any
	mconds = append(mconds, "mp.name IN (?, ?)")
	margs = append(margs, metricCostUsage, metricCostUsageCC)
	if since > 0 {
		mconds = append(mconds, "mp.ts >= ?")
		margs = append(margs, since)
	}
	if session != "" {
		mconds = append(mconds, "mp.session_id = ?")
		margs = append(margs, session)
	}
	if model != "" {
		mconds = append(mconds, "mp.attr_key = ?")
		margs = append(margs, model)
	}
	mconds = append(mconds, "NOT EXISTS (SELECT 1 FROM api_requests ar WHERE ar.session_id = mp.session_id)")
	mrows, err := s.db.QueryContext(ctx, `
		SELECT mp.attr_key, SUM(mp.value_delta)
		FROM metric_points mp
		WHERE `+strings.Join(mconds, " AND ")+`
		GROUP BY mp.attr_key
		ORDER BY SUM(mp.value_delta) DESC, mp.attr_key ASC`, margs...)
	if err != nil {
		return err
	}
	defer mrows.Close()
	for mrows.Next() {
		var (
			m    sql.NullString
			cost float64
		)
		if err := mrows.Scan(&m, &cost); err != nil {
			return err
		}
		addModel(byModel, m.String, cost, 0)
	}
	return mrows.Err()
}

// metricScopeConds builds the WHERE conditions + args for the metric-only series
// pass: cost/token metric points scoped by since/model/session AND owned by a
// session that has no api_request (the per-session axis-of-truth guard).
func metricScopeConds(since int64, model, session string) ([]string, []any) {
	var conds []string
	var args []any
	conds = append(conds, "mp.name IN (?, ?, ?, ?)")
	args = append(args, metricCostUsage, metricCostUsageCC, metricTokenUsage, metricTokenUsageCC)
	if since > 0 {
		conds = append(conds, "mp.ts >= ?")
		args = append(args, since)
	}
	if session != "" {
		conds = append(conds, "mp.session_id = ?")
		args = append(args, session)
	}
	// model filter applies only to cost.usage rows (attr_key=model). We keep all
	// token.usage rows so the token columns are populated regardless.
	if model != "" {
		conds = append(conds, "(mp.name IN (?, ?) OR mp.attr_key = ?)")
		args = append(args, metricTokenUsage, metricTokenUsageCC, model)
	}
	// Restrict to metric-only sessions: no api_request exists for the session.
	conds = append(conds, "NOT EXISTS (SELECT 1 FROM api_requests ar WHERE ar.session_id = mp.session_id)")
	return conds, args
}

// addSeriesPoint folds one bucket aggregate into the series map keyed by ts.
func addSeriesPoint(series map[int64]*costPoint, p costPoint) {
	cur, ok := series[p.TS]
	if !ok {
		cp := p
		series[p.TS] = &cp
		return
	}
	cur.CostUSD += p.CostUSD
	cur.Input += p.Input
	cur.Output += p.Output
	cur.CacheRead += p.CacheRead
	cur.CacheCreation += p.CacheCreation
}

// addModel folds one model aggregate into the byModel map keyed by model name.
func addModel(byModel map[string]*costByModel, model string, cost float64, tokens int64) {
	cur, ok := byModel[model]
	if !ok {
		byModel[model] = &costByModel{Model: model, CostUSD: cost, Tokens: tokens}
		return
	}
	cur.CostUSD += cost
	cur.Tokens += tokens
}

// sortedSeries returns the series points ordered by ts ascending, with
// CumCostUSD filled as the running total of CostUSD (so it is monotonically
// non-decreasing across the returned slice — every bucket's cost is >= 0).
func sortedSeries(series map[int64]*costPoint) []costPoint {
	out := make([]costPoint, 0, len(series))
	for _, p := range series {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	var cum float64
	for i := range out {
		cum += out[i].CostUSD
		out[i].CumCostUSD = cum
	}
	return out
}

// seriesTotalCost sums the per-bucket cost across the whole series (the scope
// total). It equals the last bucket's CumCostUSD when the series is non-empty.
func seriesTotalCost(series []costPoint) float64 {
	var total float64
	for i := range series {
		total += series[i].CostUSD
	}
	return total
}

// burnRates derives spend/token rate per minute from the assembled series. The
// span is the time from the first bucket start to the END of the last bucket
// (last bucket start + bucketMS), so a single-bucket scope reports a finite rate
// over one bucket width rather than dividing by zero. An empty series yields
// (0, 0).
func burnRates(series []costPoint, bucketMS int64) (usdPerMin, tokPerMin float64) {
	if len(series) == 0 {
		return 0, 0
	}
	var totalCost float64
	var totalTok int64
	for i := range series {
		totalCost += series[i].CostUSD
		totalTok += series[i].Input + series[i].Output + series[i].CacheRead + series[i].CacheCreation
	}
	spanMS := (series[len(series)-1].TS + bucketMS) - series[0].TS
	if spanMS <= 0 {
		spanMS = bucketMS
	}
	spanMin := float64(spanMS) / 60000.0
	if spanMin <= 0 {
		return 0, 0
	}
	return totalCost / spanMin, float64(totalTok) / spanMin
}

// cacheSaved estimates the USD saved by prompt-cache reads using the static
// price table. It sums (input - cache_read) rate * tokens for every priced
// model that contributed cache_read tokens. The returned pointer is nil when no
// priced model contributed any cache_read (so the frontend renders "—" rather
// than "$0.00"). estimated is true whenever any cache_read existed — whether
// priced (a true price-table estimate) or unpriced (under the "" key, i.e. we
// know savings happened but cannot price them).
func cacheSaved(modelCacheRead map[string]int64) (*float64, bool) {
	var saved float64
	var anyPriced bool
	var anyCacheRead bool
	for model, cr := range modelCacheRead {
		if cr <= 0 {
			continue
		}
		anyCacheRead = true
		if v, ok := pricing.CacheSavedUSD(model, cr); ok {
			saved += v
			anyPriced = true
		}
	}
	if !anyPriced {
		// No model we could price contributed cache reads: report null. estimated
		// still reflects whether unpriced cache reads exist.
		return nil, anyCacheRead
	}
	return &saved, true
}

// sortedByModel returns the per-model rows ordered by cost descending (model
// ascending on ties) with pct recomputed against the combined total cost.
func sortedByModel(byModel map[string]*costByModel) []costByModel {
	out := make([]costByModel, 0, len(byModel))
	var totalCost float64
	for _, m := range byModel {
		totalCost += m.CostUSD
	}
	for _, m := range byModel {
		row := *m
		if totalCost > 0 {
			row.Pct = row.CostUSD / totalCost
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CostUSD != out[j].CostUSD {
			return out[i].CostUSD > out[j].CostUSD
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// cacheHitRatio = cache_read / (input + cache_read), clamped to 0..1. Zero
// denominator yields 0.
func cacheHitRatio(cacheRead, input int64) float64 {
	denom := input + cacheRead
	if denom <= 0 {
		return 0
	}
	r := float64(cacheRead) / float64(denom)
	if r < 0 {
		return 0
	}
	if r > 1 {
		return 1
	}
	return r
}
