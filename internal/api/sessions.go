package api

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
)

// Metric name constants for the axis-of-truth fallback (PLAN §6.5). Claude Code
// emits the "claude_code." prefix; the parser stores the raw name, so both
// spellings are matched.
const (
	metricTokenUsage   = "token.usage"
	metricTokenUsageCC = "claude_code.token.usage"
	metricCostUsage    = "cost.usage"
	metricCostUsageCC  = "claude_code.cost.usage"
	metricActiveTime   = "active_time.total"
	metricActiveTimeCC = "claude_code.active_time.total"
)

// tokensJSON is the four-counter token block used across the API (PLAN §10).
type tokensJSON struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
}

// sessionRow is one entry in the /api/sessions list.
type sessionRow struct {
	ID             string     `json:"id"`
	FirstSeen      int64      `json:"first_seen"`
	LastSeen       int64      `json:"last_seen"`
	CostUSD        float64    `json:"cost_usd"`
	Tokens         tokensJSON `json:"tokens"`
	DurationSec    float64    `json:"duration_sec"`
	ToolAccept     int64      `json:"tool_accept"`
	ToolReject     int64      `json:"tool_reject"`
	ToolAcceptRate float64    `json:"tool_accept_rate"`
	ModelSet       string     `json:"model_set"`
	TokenSource    string     `json:"token_source"`
	HasEvents      bool       `json:"has_events"`
}

type sessionsResponse struct {
	Sessions []sessionRow `json:"sessions"`
	Total    int64        `json:"total"`
	Meta     *meta        `json:"meta,omitempty"`
}

// handleSessions lists sessions with limit/offset/since/sort/order/q filters
// and on-the-fly cost/token aggregation per the axis-of-truth rule.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit := parseIntDefault(q.Get("limit"), 50)
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	offset := parseIntDefault(q.Get("offset"), 0)
	if offset < 0 {
		offset = 0
	}
	since := parseIntDefault(q.Get("since"), 0)

	sortCol, err := sessionSortColumn(q.Get("sort"))
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	order := "DESC"
	if strings.EqualFold(q.Get("order"), "asc") {
		order = "ASC"
	}
	search := strings.TrimSpace(q.Get("q"))

	ctx := r.Context()

	// Build WHERE clause from since + q (parameter-bound).
	var where []string
	var args []any
	if since > 0 {
		where = append(where, "s.last_seen >= ?")
		args = append(args, since)
	}
	if search != "" {
		// q matches session id prefix or a date (last_seen / first_seen as a
		// date string). We keep it to id-prefix OR a numeric date range when the
		// query looks like a date; otherwise id-prefix only.
		where = append(where, "s.id LIKE ?")
		args = append(args, search+"%")
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = "WHERE " + strings.Join(where, " AND ")
	}

	// Total count (pre-pagination).
	var total int64
	countSQL := "SELECT COUNT(*) FROM sessions s " + whereSQL
	if err := s.db.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		internalError(w, "failed to count sessions")
		return
	}

	// The aggregate expressions implement the axis-of-truth EXISTS branch:
	// token/cost come from api_requests when the session has any, else from the
	// token.usage / cost.usage metric points. Never both.
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			s.id, s.first_seen, s.last_seen, s.model_set,
			`+tokenSourceExpr("s")+` AS token_source,
			s.has_events,
			`+costExpr("s")+` AS cost_usd,
			`+tokenExpr("s", "input")+`  AS in_tok,
			`+tokenExpr("s", "output")+` AS out_tok,
			`+tokenExpr("s", "cache_read")+`     AS cr_tok,
			`+tokenExpr("s", "cache_creation")+` AS cc_tok,
			(SELECT COUNT(*) FROM tool_decisions td WHERE td.session_id = s.id AND td.decision = 'accept') AS accept,
			(SELECT COUNT(*) FROM tool_decisions td WHERE td.session_id = s.id AND td.decision = 'reject') AS reject
		FROM sessions s
		`+whereSQL+`
		ORDER BY `+sortCol+` `+order+`, s.id ASC
		LIMIT ? OFFSET ?`,
		append(append([]any{}, args...), limit, offset)...)
	if err != nil {
		internalError(w, "failed to query sessions")
		return
	}
	defer rows.Close()

	out := make([]sessionRow, 0, limit)
	for rows.Next() {
		var (
			row       sessionRow
			modelSet  sql.NullString
			tokenSrc  sql.NullString
			hasEvents int
		)
		if err := rows.Scan(
			&row.ID, &row.FirstSeen, &row.LastSeen, &modelSet, &tokenSrc, &hasEvents,
			&row.CostUSD, &row.Tokens.Input, &row.Tokens.Output,
			&row.Tokens.CacheRead, &row.Tokens.CacheCreation,
			&row.ToolAccept, &row.ToolReject,
		); err != nil {
			internalError(w, "failed to scan session")
			return
		}
		row.ModelSet = modelSet.String
		row.TokenSource = tokenSrc.String
		row.HasEvents = hasEvents != 0
		row.DurationSec = float64(row.LastSeen-row.FirstSeen) / 1000.0
		if row.DurationSec < 0 {
			row.DurationSec = 0
		}
		if tot := row.ToolAccept + row.ToolReject; tot > 0 {
			row.ToolAcceptRate = float64(row.ToolAccept) / float64(tot)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		internalError(w, "failed to read sessions")
		return
	}

	resp := sessionsResponse{Sessions: out, Total: total}
	if total == 0 {
		resp.Meta = &meta{EmptyReason: reasonNoData}
	}
	writeJSON(w, http.StatusOK, resp)
}

// sessionDetail is the /api/sessions/:id body (PLAN §10).
type sessionDetail struct {
	ID             string           `json:"id"`
	FirstSeen      int64            `json:"first_seen"`
	LastSeen       int64            `json:"last_seen"`
	CostUSD        float64          `json:"cost_usd"`
	Tokens         tokensJSON       `json:"tokens"`
	DurationSec    float64          `json:"duration_sec"`
	ToolAccept     int64            `json:"tool_accept"`
	ToolReject     int64            `json:"tool_reject"`
	ToolAcceptRate float64          `json:"tool_accept_rate"`
	Models         []modelBreakdown `json:"models"`
	ModelSet       string           `json:"model_set"`
	TokenSource    string           `json:"token_source"`
	HasEvents      bool             `json:"has_events"`
}

type modelBreakdown struct {
	Model   string     `json:"model"`
	CostUSD float64    `json:"cost_usd"`
	Tokens  tokensJSON `json:"tokens"`
}

// handleSession returns a single session summary with per-model breakdown.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		badRequest(w, "missing session id")
		return
	}
	ctx := r.Context()

	var (
		detail    sessionDetail
		modelSet  sql.NullString
		tokenSrc  sql.NullString
		hasEvents int
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT
			s.id, s.first_seen, s.last_seen, s.model_set,
			`+tokenSourceExpr("s")+` AS token_source,
			s.has_events,
			`+costExpr("s")+` AS cost_usd,
			`+tokenExpr("s", "input")+`  AS in_tok,
			`+tokenExpr("s", "output")+` AS out_tok,
			`+tokenExpr("s", "cache_read")+`     AS cr_tok,
			`+tokenExpr("s", "cache_creation")+` AS cc_tok,
			(SELECT COUNT(*) FROM tool_decisions td WHERE td.session_id = s.id AND td.decision = 'accept') AS accept,
			(SELECT COUNT(*) FROM tool_decisions td WHERE td.session_id = s.id AND td.decision = 'reject') AS reject
		FROM sessions s
		WHERE s.id = ?`, id).Scan(
		&detail.ID, &detail.FirstSeen, &detail.LastSeen, &modelSet, &tokenSrc, &hasEvents,
		&detail.CostUSD, &detail.Tokens.Input, &detail.Tokens.Output,
		&detail.Tokens.CacheRead, &detail.Tokens.CacheCreation,
		&detail.ToolAccept, &detail.ToolReject,
	)
	if err == sql.ErrNoRows {
		notFound(w, "session not found")
		return
	}
	if err != nil {
		internalError(w, "failed to query session")
		return
	}

	detail.ModelSet = modelSet.String
	detail.TokenSource = tokenSrc.String
	detail.HasEvents = hasEvents != 0
	detail.DurationSec = float64(detail.LastSeen-detail.FirstSeen) / 1000.0
	if detail.DurationSec < 0 {
		detail.DurationSec = 0
	}
	if tot := detail.ToolAccept + detail.ToolReject; tot > 0 {
		detail.ToolAcceptRate = float64(detail.ToolAccept) / float64(tot)
	}

	models, err := s.modelBreakdown(ctx, id)
	if err != nil {
		internalError(w, "failed to query model breakdown")
		return
	}
	detail.Models = models

	writeJSON(w, http.StatusOK, detail)
}

// modelBreakdown returns per-model cost/token rows for a session, sourced from
// api_requests when the session has any api_request, else from the cost/token
// metrics. The branch is the per-session EXISTS(api_requests) test — identical
// to costExpr / tokenExpr — NOT the has_events flag (which is set by any log
// event, so a prompt-only, metric-cost session would otherwise show an empty
// model breakdown while its session cost is metric-sourced). This keeps the
// breakdown's source consistent with the session totals (never both sources).
func (s *Server) modelBreakdown(ctx context.Context, id string) ([]modelBreakdown, error) {
	out := []modelBreakdown{}

	var hasAPIRequests int
	if err := s.db.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM api_requests WHERE session_id = ?)", id).Scan(&hasAPIRequests); err != nil {
		return nil, err
	}

	if hasAPIRequests != 0 {
		rows, err := s.db.QueryContext(ctx, `
			SELECT model,
			       SUM(cost_usd), SUM(input_tokens), SUM(output_tokens),
			       SUM(cache_read), SUM(cache_creation)
			FROM api_requests
			WHERE session_id = ?
			GROUP BY model
			ORDER BY SUM(cost_usd) DESC, model ASC`, id)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				mb    modelBreakdown
				model sql.NullString
			)
			if err := rows.Scan(&model, &mb.CostUSD, &mb.Tokens.Input, &mb.Tokens.Output,
				&mb.Tokens.CacheRead, &mb.Tokens.CacheCreation); err != nil {
				return nil, err
			}
			mb.Model = model.String
			out = append(out, mb)
		}
		return out, rows.Err()
	}

	// Metrics fallback: cost.usage keyed by model (attr_key=model), token.usage
	// keyed by token type. Per-model token attribution is not available from the
	// metric stream (token.usage is dimensioned by type, not model), so the
	// breakdown carries cost per model with zero tokens — the aggregate session
	// tokens above remain the source of truth for totals.
	rows, err := s.db.QueryContext(ctx, `
		SELECT attr_key AS model, SUM(value_delta)
		FROM metric_points
		WHERE session_id = ? AND name IN (?, ?)
		GROUP BY attr_key
		ORDER BY SUM(value_delta) DESC, attr_key ASC`,
		id, metricCostUsage, metricCostUsageCC)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			mb    modelBreakdown
			model sql.NullString
		)
		if err := rows.Scan(&model, &mb.CostUSD); err != nil {
			return nil, err
		}
		mb.Model = model.String
		out = append(out, mb)
	}
	return out, rows.Err()
}

// costExpr builds the axis-of-truth cost expression for a session aliased as
// `alias`: SUM(api_requests.cost_usd) when the session has any api_requests,
// otherwise SUM of the cost.usage metric deltas. Never both.
// tokenSourceExpr computes the displayed token_source label from the SAME
// per-session axis-of-truth as costExpr/tokenExpr, so the badge never
// contradicts the numbers it sits next to: a session whose values come from
// api_request events is "events"; one that falls back to token/cost metrics is
// "metrics"; one with neither is "". This replaces the coarse ingestion-time
// sessions.token_source column, which could mislabel a session that emitted
// both metrics and api_request events (real Claude Code sessions emit both).
func tokenSourceExpr(alias string) string {
	return `CASE
		WHEN EXISTS (SELECT 1 FROM api_requests ar WHERE ar.session_id = ` + alias + `.id) THEN 'events'
		WHEN EXISTS (SELECT 1 FROM metric_points mp WHERE mp.session_id = ` + alias + `.id
			AND mp.name IN ('` + metricTokenUsage + `','` + metricTokenUsageCC + `','` + metricCostUsage + `','` + metricCostUsageCC + `')) THEN 'metrics'
		ELSE ''
		END`
}

func costExpr(alias string) string {
	return `CASE WHEN EXISTS (SELECT 1 FROM api_requests ar WHERE ar.session_id = ` + alias + `.id)
		THEN COALESCE((SELECT SUM(ar.cost_usd) FROM api_requests ar WHERE ar.session_id = ` + alias + `.id), 0)
		ELSE COALESCE((SELECT SUM(mp.value_delta) FROM metric_points mp
			WHERE mp.session_id = ` + alias + `.id AND mp.name IN ('` + metricCostUsage + `','` + metricCostUsageCC + `')), 0)
		END`
}

// tokenExpr builds the axis-of-truth token expression for a given token kind
// (one of input|output|cache_read|cache_creation). When the session has
// api_requests it sums the matching column; otherwise it sums token.usage metric
// deltas filtered by the metric attr_key (token type). Never both.
func tokenExpr(alias, kind string) string {
	col := map[string]string{
		"input":          "input_tokens",
		"output":         "output_tokens",
		"cache_read":     "cache_read",
		"cache_creation": "cache_creation",
	}[kind]
	// Claude Code token.usage types. Accept both camelCase and snake_case for the
	// cache types to be robust to drift.
	var attrFilter string
	switch kind {
	case "input":
		attrFilter = "mp.attr_key IN ('input','input_tokens')"
	case "output":
		attrFilter = "mp.attr_key IN ('output','output_tokens')"
	case "cache_read":
		attrFilter = "mp.attr_key IN ('cacheRead','cache_read','cache_read_tokens')"
	case "cache_creation":
		attrFilter = "mp.attr_key IN ('cacheCreation','cache_creation','cache_creation_tokens')"
	}
	return `CASE WHEN EXISTS (SELECT 1 FROM api_requests ar WHERE ar.session_id = ` + alias + `.id)
		THEN COALESCE((SELECT SUM(ar.` + col + `) FROM api_requests ar WHERE ar.session_id = ` + alias + `.id), 0)
		ELSE COALESCE((SELECT CAST(SUM(mp.value_delta) AS INTEGER) FROM metric_points mp
			WHERE mp.session_id = ` + alias + `.id
			  AND mp.name IN ('` + metricTokenUsage + `','` + metricTokenUsageCC + `')
			  AND ` + attrFilter + `), 0)
		END`
}

// sessionSortColumn maps the sort query param to a safe column expression. An
// unknown value is an error (no injection: the result is a fixed literal).
func sessionSortColumn(sort string) (string, error) {
	switch sort {
	case "", "last_seen":
		return "s.last_seen", nil
	case "cost":
		return costExpr("s"), nil
	case "tokens":
		return "(" + tokenExpr("s", "input") + " + " + tokenExpr("s", "output") + ")", nil
	default:
		return "", &apiError{"invalid sort: " + sort}
	}
}

// apiError is a tiny error type for parameter validation messages.
type apiError struct{ msg string }

func (e *apiError) Error() string { return e.msg }

// parseIntDefault parses s as an int, returning def on empty/invalid input.
func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
