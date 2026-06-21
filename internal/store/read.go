package store

// read.go holds the server-independent read path that backs `wlog last`
// (PLAN v3 §1) and `wlog statusline` (PLAN v3 §2). Both commands read the DB
// directly without starting the OTLP receiver / HTTP server, so the queries live
// in the store package rather than in api. They go through ReadDB() (the WAL read
// pool) and never touch the single writer connection, preserving the
// single-writer / read-pool concurrency model (PLAN §8). The wlog server may be
// writing the same file concurrently; WAL lets these reads run without blocking.
//
// The cost/token aggregation mirrors the api package's axis-of-truth rule (PLAN
// §6.5): per session, token/cost come from api_requests when the session has any,
// otherwise from the token.usage / cost.usage metric deltas — never both. The api
// package keeps its own copies of these CASE expressions (api imports store, not
// the reverse, so they cannot share a symbol without an import cycle); the SQL
// here is deliberately equivalent so `wlog last` and the /api/sessions detail
// report identical numbers for the same session. Both ultimately derive from the
// same stored rows.
//
// statusline is on a ≤100ms budget (PLAN v3 §2): every method here is a single
// indexed query (idx_sessions_last_seen for the latest session,
// idx_*_session_ts for the per-session scans) with no full-table scan.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/openwong2kim/wlog/internal/model"
)

// Metric-name constants for the axis-of-truth metric fallback. Claude Code emits
// the "claude_code." prefix; the otel parser stores the raw name, so both
// spellings are matched. These mirror the api package's constants (kept in sync
// by the shared meaning, not a shared symbol — see package doc).
const (
	metricTokenUsage   = "token.usage"
	metricTokenUsageCC = "claude_code.token.usage"
	metricCostUsage    = "cost.usage"
	metricCostUsageCC  = "claude_code.cost.usage"
)

// SessionSummary is the header aggregate for `wlog last` (PLAN v3 §1 section ①+②
// header fields). It is the server-independent equivalent of the /api/sessions/:id
// detail. Cost and Tokens follow the per-session axis-of-truth (events else
// metrics, never both). CacheHitRatio = CacheRead/(Input+CacheRead), clamped to
// 0..1. AutoApproved counts accept decisions whose source is config or hook (the
// non-interactive approvals). ToolAccept/ToolReject are tool_decision counts.
// Repo/GitBranch are the JSONL-sourced aggregation dimensions (empty when
// unknown). TokenSource is "events" | "metrics" | "" computed from the same axis
// as the numbers (so the badge never contradicts them). This type is exported for
// the app layer (`wlog last`, T4).
type SessionSummary struct {
	ID            string
	FirstSeen     int64 // unix-ms
	LastSeen      int64 // unix-ms
	CostUSD       float64
	Tokens        model.Tokens
	CacheHitRatio float64
	ToolAccept    int64
	ToolReject    int64
	AutoApproved  int64
	Repo          string
	GitBranch     string
	ModelSet      string
	TokenSource   string // "events" | "metrics" | ""
}

// SessionSnapshot is the lightweight per-session view for `wlog statusline`
// (PLAN v3 §2): the running cost, tokens, cache hit ratio, reject count, and the
// name of the most recent tool_result (the "Bash running"-style indicator). It is
// a strict subset of SessionSummary plus LastTool, assembled from the same single
// aggregate row to keep statusline within its latency budget. Exported for the
// app layer (T4).
type SessionSnapshot struct {
	ID            string
	CostUSD       float64
	Tokens        model.Tokens
	CacheHitRatio float64
	ToolReject    int64
	LastTool      string // tool_name of the most recent tool_result, "" if none
}

// UserPrompt is one user_prompt event for `wlog last`'s prompt section. The
// prompt text and length live in the event's attrs_json (no dedicated columns),
// so UserPromptsBySession decodes them here. Stored is false when the prompt was
// recorded with --no-store-prompts (length present, text omitted): the caller
// then shows "(미저장)" instead of text. Text is "" when Stored is false.
type UserPrompt struct {
	TS     int64
	Text   string
	Length int64
	Stored bool
}

// sessionAggregateSelect is the shared single-row aggregate that both
// SessionSummary and SessionSnapshot are built from. Selecting it once keeps the
// two public methods consistent and statusline fast (one indexed query). All
// token/cost expressions are the per-session axis-of-truth (events else metrics).
// It is a package var (not a const) because the token expressions are assembled
// at init by tokenExpr(); the final string is fixed (no user input — the only
// bound parameter is the session id placeholder), so it carries no injection risk.
var sessionAggregateSelect = `
	SELECT
		s.id, s.first_seen, s.last_seen, s.model_set,
		COALESCE(s.repo, ''), COALESCE(s.git_branch, ''),
		` + tokenSourceExpr + ` AS token_source,
		` + costExpr + ` AS cost_usd,
		` + tokenExprInput + `          AS in_tok,
		` + tokenExprOutput + `         AS out_tok,
		` + tokenExprCacheRead + `      AS cr_tok,
		` + tokenExprCacheCreation + `  AS cc_tok,
		(SELECT COUNT(*) FROM tool_decisions td WHERE td.session_id = s.id AND td.decision = 'accept') AS accept,
		(SELECT COUNT(*) FROM tool_decisions td WHERE td.session_id = s.id AND td.decision = 'reject') AS reject,
		(SELECT COUNT(*) FROM tool_decisions td WHERE td.session_id = s.id AND td.decision = 'accept'
			AND td.source IN ('config','hook')) AS auto_approved
	FROM sessions s
	WHERE s.id = ?`

// scanSessionAggregate scans one sessionAggregateSelect row into a SessionSummary.
// CacheHitRatio is derived from the scanned tokens.
func scanSessionAggregate(row *sql.Row) (SessionSummary, error) {
	var (
		sm        SessionSummary
		modelSet  sql.NullString
		tokenSrc  sql.NullString
		repo, brn string
	)
	err := row.Scan(
		&sm.ID, &sm.FirstSeen, &sm.LastSeen, &modelSet, &repo, &brn, &tokenSrc,
		&sm.CostUSD, &sm.Tokens.Input, &sm.Tokens.Output,
		&sm.Tokens.CacheRead, &sm.Tokens.CacheCreation,
		&sm.ToolAccept, &sm.ToolReject, &sm.AutoApproved,
	)
	if err != nil {
		return SessionSummary{}, err
	}
	sm.ModelSet = modelSet.String
	sm.TokenSource = tokenSrc.String
	sm.Repo = repo
	sm.GitBranch = brn
	sm.CacheHitRatio = cacheHitRatio(sm.Tokens.CacheRead, sm.Tokens.Input)
	return sm, nil
}

// LatestSessionID returns the id of the most recently seen session
// (ORDER BY last_seen DESC, id ASC LIMIT 1, using idx_sessions_last_seen). It
// returns "" with a nil error when there are no sessions — an empty DB is a
// normal state for `wlog last` / statusline, not an error.
func (s *Store) LatestSessionID(ctx context.Context) (string, error) {
	var id string
	err := s.read.QueryRowContext(ctx,
		`SELECT id FROM sessions ORDER BY last_seen DESC, id ASC LIMIT 1`).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: latest session id: %w", err)
	}
	return id, nil
}

// SessionSummary returns the header aggregate for the given session id. The
// boolean result is false (with a nil error) when no session with that id exists,
// so a caller can render a "no such session" message without treating absence as
// an error.
func (s *Store) SessionSummary(ctx context.Context, id string) (SessionSummary, bool, error) {
	row := s.read.QueryRowContext(ctx, sessionAggregateSelect, id)
	sm, err := scanSessionAggregate(row)
	if err == sql.ErrNoRows {
		return SessionSummary{}, false, nil
	}
	if err != nil {
		return SessionSummary{}, false, fmt.Errorf("store: session summary %q: %w", id, err)
	}
	return sm, true, nil
}

// SessionSnapshot returns the lightweight statusline view for the given session
// id. found is false (nil error) when the session does not exist. It runs the
// shared aggregate once plus one indexed lookup for the latest tool_result name,
// staying within the statusline latency budget (PLAN v3 §2).
func (s *Store) SessionSnapshot(ctx context.Context, id string) (SessionSnapshot, bool, error) {
	row := s.read.QueryRowContext(ctx, sessionAggregateSelect, id)
	sm, err := scanSessionAggregate(row)
	if err == sql.ErrNoRows {
		return SessionSnapshot{}, false, nil
	}
	if err != nil {
		return SessionSnapshot{}, false, fmt.Errorf("store: session snapshot %q: %w", id, err)
	}

	snap := SessionSnapshot{
		ID:            sm.ID,
		CostUSD:       sm.CostUSD,
		Tokens:        sm.Tokens,
		CacheHitRatio: sm.CacheHitRatio,
		ToolReject:    sm.ToolReject,
	}

	// Most recent tool_result for this session (idx_tool_results_session_ts; ts
	// DESC LIMIT 1). Absence is not an error — a session may have no tool yet.
	var lastTool sql.NullString
	err = s.read.QueryRowContext(ctx, `
		SELECT tool_name FROM tool_results
		WHERE session_id = ?
		ORDER BY ts DESC, id DESC
		LIMIT 1`, id).Scan(&lastTool)
	if err != nil && err != sql.ErrNoRows {
		return SessionSnapshot{}, false, fmt.Errorf("store: snapshot last tool %q: %w", id, err)
	}
	snap.LastTool = lastTool.String
	return snap, true, nil
}

// ToolDecisionsBySession returns the session's tool_decision rows in ts-ascending
// order (idx_tool_decisions_session_ts). When onlyReject is true only reject
// decisions are returned (the reject list `wlog last` prints). The returned
// slice is non-nil but may be empty.
func (s *Store) ToolDecisionsBySession(ctx context.Context, id string, onlyReject bool) ([]model.ToolDecision, error) {
	q := `
		SELECT session_id, ts, decision, source, tool_name, dedup_key, attrs_json
		FROM tool_decisions
		WHERE session_id = ?`
	if onlyReject {
		q += ` AND decision = 'reject'`
	}
	q += ` ORDER BY ts ASC, id ASC`

	rows, err := s.read.QueryContext(ctx, q, id)
	if err != nil {
		return nil, fmt.Errorf("store: tool decisions %q: %w", id, err)
	}
	defer rows.Close()

	out := []model.ToolDecision{}
	for rows.Next() {
		var (
			d                             model.ToolDecision
			decision, source, name, attrs sql.NullString
		)
		if err := rows.Scan(&d.SessionID, &d.TS, &decision, &source, &name,
			&d.DedupKey, &attrs); err != nil {
			return nil, fmt.Errorf("store: scan tool decision: %w", err)
		}
		d.Decision = decision.String
		d.Source = source.String
		d.ToolName = name.String
		d.AttrsJSON = attrs.String
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate tool decisions %q: %w", id, err)
	}
	return out, nil
}

// ToolResultsBySession returns up to limit of the session's most recent
// tool_result rows, returned in ts-ASCENDING order so a caller can print them
// chronologically (e.g. `wlog last`'s "bash (last N)" block). That is: the limit
// selects the newest N (ts DESC), but the slice is then ordered oldest→newest.
// limit <= 0 returns all rows. bash_command / mcp_server are included. The
// returned slice is non-nil but may be empty. Uses idx_tool_results_session_ts.
func (s *Store) ToolResultsBySession(ctx context.Context, id string, limit int) ([]model.ToolResult, error) {
	// Inner query takes the newest `limit` by ts DESC; the outer query re-sorts
	// them ascending so the caller prints oldest first. A non-positive limit means
	// "all rows" (a large sentinel keeps the query shape uniform).
	lim := limit
	if lim <= 0 {
		lim = -1 // SQLite: LIMIT -1 means no limit
	}
	rows, err := s.read.QueryContext(ctx, `
		SELECT session_id, ts, tool_name, success, duration_ms, bash_command,
		       mcp_server, dedup_key, attrs_json
		FROM (
			SELECT * FROM tool_results
			WHERE session_id = ?
			ORDER BY ts DESC, id DESC
			LIMIT ?
		)
		ORDER BY ts ASC, id ASC`, id, lim)
	if err != nil {
		return nil, fmt.Errorf("store: tool results %q: %w", id, err)
	}
	defer rows.Close()

	out := []model.ToolResult{}
	for rows.Next() {
		var (
			r         model.ToolResult
			name      sql.NullString
			success   int
			bash, mcp sql.NullString
			attrs     sql.NullString
		)
		if err := rows.Scan(&r.SessionID, &r.TS, &name, &success, &r.DurationMS,
			&bash, &mcp, &r.DedupKey, &attrs); err != nil {
			return nil, fmt.Errorf("store: scan tool result: %w", err)
		}
		r.ToolName = name.String
		r.Success = success != 0
		r.BashCommand = bash.String
		r.MCPServer = mcp.String
		r.AttrsJSON = attrs.String
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate tool results %q: %w", id, err)
	}
	return out, nil
}

// UserPromptsBySession returns up to limit of the session's most recent
// user_prompt events, returned in ts-ASCENDING order (newest N selected by ts
// DESC, then re-sorted oldest→newest) so the caller prints them chronologically
// — the same shape as ToolResultsBySession's "last N" block. limit <= 0 returns
// all rows. The prompt text and length are decoded from attrs_json: a prompt
// stored with --no-store-prompts carries prompt_length but no "prompt" string,
// so Stored is false and Text is "". Uses idx_events_session_ts.
func (s *Store) UserPromptsBySession(ctx context.Context, id string, limit int) ([]UserPrompt, error) {
	lim := limit
	if lim <= 0 {
		lim = -1 // SQLite: LIMIT -1 means no limit
	}
	rows, err := s.read.QueryContext(ctx, `
		SELECT ts, attrs_json FROM (
			SELECT ts, attrs_json, id FROM events
			WHERE session_id = ? AND name = 'user_prompt'
			ORDER BY ts DESC, id DESC
			LIMIT ?
		)
		ORDER BY ts ASC, id ASC`, id, lim)
	if err != nil {
		return nil, fmt.Errorf("store: user prompts %q: %w", id, err)
	}
	defer rows.Close()

	out := []UserPrompt{}
	for rows.Next() {
		var (
			ts    int64
			attrs sql.NullString
		)
		if err := rows.Scan(&ts, &attrs); err != nil {
			return nil, fmt.Errorf("store: scan user prompt: %w", err)
		}
		p := UserPrompt{TS: ts}
		p.Text, p.Length, p.Stored = decodePromptAttrs(attrs.String)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate user prompts %q: %w", id, err)
	}
	return out, nil
}

// decodePromptAttrs pulls the prompt text and length out of a user_prompt
// attrs_json object. stored is true only when a non-empty "prompt" string is
// present (the default path); with --no-store-prompts the text is absent so
// stored is false and text is "" while length (always written) still parses. A
// missing/corrupt blob yields ("", 0, false).
func decodePromptAttrs(attrsJSON string) (text string, length int64, stored bool) {
	if attrsJSON == "" {
		return "", 0, false
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(attrsJSON), &obj); err != nil {
		return "", 0, false
	}
	if v, ok := obj["prompt_length"]; ok {
		if f, ok := v.(float64); ok {
			length = int64(f)
		}
	}
	if v, ok := obj["prompt"]; ok {
		if s, ok := v.(string); ok && s != "" {
			text = s
			stored = true
		}
	}
	return text, length, stored
}

// --- axis-of-truth SQL expressions (PLAN §6.5) ---
//
// These mirror the api package's costExpr/tokenExpr/tokenSourceExpr but are
// pre-bound to the table alias "s" (sessions) used by sessionAggregateSelect, so
// they are plain string constants here rather than functions. The semantics are
// identical: per session, sum api_requests when the session has any, else sum the
// matching metric deltas — never both.

const tokenSourceExpr = `CASE
	WHEN EXISTS (SELECT 1 FROM api_requests ar WHERE ar.session_id = s.id) THEN 'events'
	WHEN EXISTS (SELECT 1 FROM metric_points mp WHERE mp.session_id = s.id
		AND mp.name IN ('` + metricTokenUsage + `','` + metricTokenUsageCC + `','` + metricCostUsage + `','` + metricCostUsageCC + `')) THEN 'metrics'
	ELSE ''
	END`

const costExpr = `CASE WHEN EXISTS (SELECT 1 FROM api_requests ar WHERE ar.session_id = s.id)
	THEN COALESCE((SELECT SUM(ar.cost_usd) FROM api_requests ar WHERE ar.session_id = s.id), 0)
	ELSE COALESCE((SELECT SUM(mp.value_delta) FROM metric_points mp
		WHERE mp.session_id = s.id AND mp.name IN ('` + metricCostUsage + `','` + metricCostUsageCC + `')), 0)
	END`

// tokenExpr builds the axis-of-truth token expression for one token kind. The
// four pre-built constants below are used by sessionAggregateSelect.
func tokenExpr(col, attrFilter string) string {
	return `CASE WHEN EXISTS (SELECT 1 FROM api_requests ar WHERE ar.session_id = s.id)
		THEN COALESCE((SELECT SUM(ar.` + col + `) FROM api_requests ar WHERE ar.session_id = s.id), 0)
		ELSE COALESCE((SELECT CAST(SUM(mp.value_delta) AS INTEGER) FROM metric_points mp
			WHERE mp.session_id = s.id
			  AND mp.name IN ('` + metricTokenUsage + `','` + metricTokenUsageCC + `')
			  AND ` + attrFilter + `), 0)
		END`
}

// Accept both camelCase and snake_case attr keys for cache types to be robust to
// Claude Code key drift (matches the api package).
var (
	tokenExprInput         = tokenExpr("input_tokens", "mp.attr_key IN ('input','input_tokens')")
	tokenExprOutput        = tokenExpr("output_tokens", "mp.attr_key IN ('output','output_tokens')")
	tokenExprCacheRead     = tokenExpr("cache_read", "mp.attr_key IN ('cacheRead','cache_read','cache_read_tokens')")
	tokenExprCacheCreation = tokenExpr("cache_creation", "mp.attr_key IN ('cacheCreation','cache_creation','cache_creation_tokens')")
)

// cacheHitRatio = cache_read / (input + cache_read), clamped to 0..1. A zero (or
// negative) denominator yields 0. Matches the api package definition (PLAN §4
// Cost / DESIGN §4).
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
