package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/openwong2kim/wlog/internal/model"
)

// applyBatch writes every slice of b inside the supplied transaction. Ordering
// matters: sessions are upserted first so child rows satisfy the foreign key
// (foreign_keys=ON). All inserts are idempotent (ON CONFLICT ... DO NOTHING /
// upsert) so re-delivery and replay never change row counts (PLAN §8 idempotency).
func applyBatch(ctx context.Context, tx *sql.Tx, b model.Batch) error {
	if err := upsertSessions(ctx, tx, b.Sessions); err != nil {
		return err
	}
	if err := insertAPIRequests(ctx, tx, b.APIRequests); err != nil {
		return err
	}
	if err := insertToolDecisions(ctx, tx, b.ToolDecisions); err != nil {
		return err
	}
	if err := insertToolResults(ctx, tx, b.ToolResults); err != nil {
		return err
	}
	if err := insertEvents(ctx, tx, b.Events); err != nil {
		return err
	}
	if err := insertSpans(ctx, tx, b.Spans); err != nil {
		return err
	}
	if err := insertMetricPoints(ctx, tx, b.MetricPoints); err != nil {
		return err
	}
	if err := upsertSeriesStates(ctx, tx, b.SeriesStates); err != nil {
		return err
	}
	return nil
}

// upsertSessions merges session identity/meta. first_seen takes the min and
// last_seen the max across deliveries (order-independent, PLAN §8 / REVIEWS F12).
// token_source/has_events merge: has_events is sticky (once true stays true),
// token_source is overwritten by a non-empty incoming value. Meta fields keep
// the latest non-empty value via COALESCE/NULLIF so a sparse later batch does
// not clobber data. repo/git_branch follow the same non-empty-wins rule (PLAN v3
// §3): the JSONL source fills them, OTLP leaves them empty, and an empty incoming
// value must never overwrite a known one — whichever source supplies a non-empty
// value wins regardless of delivery order.
func upsertSessions(ctx context.Context, tx *sql.Tx, ss []model.Session) error {
	if len(ss) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO sessions
			(id, first_seen, last_seen, app_version, model_set, user_email,
			 org_id, token_source, has_events, repo, git_branch, attrs_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			first_seen   = MIN(sessions.first_seen, excluded.first_seen),
			last_seen    = MAX(sessions.last_seen,  excluded.last_seen),
			app_version  = COALESCE(NULLIF(excluded.app_version, ''),  sessions.app_version),
			model_set    = COALESCE(NULLIF(excluded.model_set, ''),    sessions.model_set),
			user_email   = COALESCE(NULLIF(excluded.user_email, ''),   sessions.user_email),
			org_id       = COALESCE(NULLIF(excluded.org_id, ''),       sessions.org_id),
			token_source = COALESCE(NULLIF(excluded.token_source, ''), sessions.token_source),
			has_events   = MAX(sessions.has_events, excluded.has_events),
			repo         = COALESCE(NULLIF(excluded.repo, ''),         sessions.repo),
			git_branch   = COALESCE(NULLIF(excluded.git_branch, ''),   sessions.git_branch),
			attrs_json   = COALESCE(NULLIF(excluded.attrs_json, ''),   sessions.attrs_json)`)
	if err != nil {
		return fmt.Errorf("store: prepare upsert sessions: %w", err)
	}
	defer stmt.Close()

	for _, s := range ss {
		if _, err := stmt.ExecContext(ctx,
			s.ID, s.FirstSeen, s.LastSeen, s.AppVersion, s.ModelSet, s.UserEmail,
			s.OrgID, s.TokenSource, boolToInt(s.HasEvents), s.Repo, s.GitBranch,
			s.AttrsJSON); err != nil {
			return fmt.Errorf("store: upsert session %q: %w", s.ID, err)
		}
	}
	return nil
}

// insertAPIRequests upserts api_request rows. Cost authority (PLAN v3 §9): the
// JSONL transcript and the OTLP event for the SAME call hash to the SAME
// dedup_key (EQ6), but JSONL's cost is a pricing-table ESTIMATE while OTLP
// carries Claude Code's real cost_usd. So a plain DO NOTHING would let whichever
// arrived first win — and the transcript history scan typically inserts the
// estimate before the live OTLP event lands, freezing an estimate (or $0 for an
// unknown model) over the real figure.
//
// The conflict therefore DOES UPDATE, but only to let the authoritative OTLP
// figure replace a non-authoritative one. The guard
// `excluded.cost_source='otlp' AND api_requests.cost_source IS NOT 'otlp'` means:
//   - OTLP arriving over an existing JSONL-estimate / NULL (pre-v3) / empty row →
//     update cost_usd, the four token counters, duration/error/status and stamp
//     cost_source='otlp'.
//   - A JSONL re-scan (excluded.cost_source!='otlp') → guard false → no-op, so an
//     OTLP-filled row is never clobbered by an estimate (idempotent re-scan).
//   - OTLP re-delivered over an existing OTLP row (api_requests.cost_source IS
//     'otlp') → guard false → no-op (already authoritative; idempotent).
//
// session_id/ts/model/dedup_key/attrs_json are NOT updated: they are identical
// for the same logical call (dedup_key is a hash of session/ts/model/tokens), so
// rewriting them is unnecessary; attrs_json is left as first-writer-wins.
// `IS NOT` is SQLite's null-safe inequality, so a NULL existing cost_source
// (rows written before the v3 column) counts as non-authoritative and is filled.
func insertAPIRequests(ctx context.Context, tx *sql.Tx, rs []model.APIRequest) error {
	if len(rs) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO api_requests
			(session_id, ts, model, input_tokens, output_tokens, cache_read,
			 cache_creation, cost_usd, duration_ms, is_error, status_code,
			 cost_source, dedup_key, attrs_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(dedup_key) DO UPDATE SET
			cost_usd       = excluded.cost_usd,
			input_tokens   = excluded.input_tokens,
			output_tokens  = excluded.output_tokens,
			cache_read     = excluded.cache_read,
			cache_creation = excluded.cache_creation,
			duration_ms    = excluded.duration_ms,
			is_error       = excluded.is_error,
			status_code    = excluded.status_code,
			cost_source    = excluded.cost_source
		WHERE excluded.cost_source = 'otlp' AND api_requests.cost_source IS NOT 'otlp'`)
	if err != nil {
		return fmt.Errorf("store: prepare insert api_requests: %w", err)
	}
	defer stmt.Close()

	for _, r := range rs {
		if _, err := stmt.ExecContext(ctx,
			r.SessionID, r.TS, r.Model, r.Tokens.Input, r.Tokens.Output,
			r.Tokens.CacheRead, r.Tokens.CacheCreation, r.CostUSD, r.DurationMS,
			boolToInt(r.IsError), r.StatusCode, r.CostSource, r.DedupKey, r.AttrsJSON); err != nil {
			return fmt.Errorf("store: insert api_request %q: %w", r.DedupKey, err)
		}
	}
	return nil
}

func insertToolDecisions(ctx context.Context, tx *sql.Tx, ds []model.ToolDecision) error {
	if len(ds) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO tool_decisions
			(session_id, ts, decision, source, tool_name, dedup_key, attrs_json)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(dedup_key) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("store: prepare insert tool_decisions: %w", err)
	}
	defer stmt.Close()

	for _, d := range ds {
		if _, err := stmt.ExecContext(ctx,
			d.SessionID, d.TS, d.Decision, d.Source, d.ToolName, d.DedupKey,
			d.AttrsJSON); err != nil {
			return fmt.Errorf("store: insert tool_decision %q: %w", d.DedupKey, err)
		}
	}
	return nil
}

func insertToolResults(ctx context.Context, tx *sql.Tx, rs []model.ToolResult) error {
	if len(rs) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO tool_results
			(session_id, ts, tool_name, success, duration_ms, bash_command,
			 mcp_server, dedup_key, attrs_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(dedup_key) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("store: prepare insert tool_results: %w", err)
	}
	defer stmt.Close()

	for _, r := range rs {
		if _, err := stmt.ExecContext(ctx,
			r.SessionID, r.TS, r.ToolName, boolToInt(r.Success), r.DurationMS,
			r.BashCommand, r.MCPServer, r.DedupKey, r.AttrsJSON); err != nil {
			return fmt.Errorf("store: insert tool_result %q: %w", r.DedupKey, err)
		}
	}
	return nil
}

func insertEvents(ctx context.Context, tx *sql.Tx, es []model.Event) error {
	if len(es) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO events (session_id, ts, name, dedup_key, attrs_json)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(dedup_key) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("store: prepare insert events: %w", err)
	}
	defer stmt.Close()

	for _, e := range es {
		if _, err := stmt.ExecContext(ctx,
			e.SessionID, e.TS, e.Name, e.DedupKey, e.AttrsJSON); err != nil {
			return fmt.Errorf("store: insert event %q: %w", e.DedupKey, err)
		}
	}
	return nil
}

func insertSpans(ctx context.Context, tx *sql.Tx, sp []model.Span) error {
	if len(sp) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO spans
			(session_id, trace_id, span_id, parent_span_id, name, start_ts,
			 end_ts, status, attrs_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(trace_id, span_id) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("store: prepare insert spans: %w", err)
	}
	defer stmt.Close()

	for _, s := range sp {
		if _, err := stmt.ExecContext(ctx,
			s.SessionID, s.TraceID, s.SpanID, s.ParentSpanID, s.Name, s.StartTS,
			s.EndTS, s.Status, s.AttrsJSON); err != nil {
			return fmt.Errorf("store: insert span %s/%s: %w", s.TraceID, s.SpanID, err)
		}
	}
	return nil
}

func insertMetricPoints(ctx context.Context, tx *sql.Tx, mps []model.MetricPoint) error {
	if len(mps) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO metric_points
			(session_id, ts, name, attr_key, value_delta, value_kind, series_key,
			 start_unixnano, time_unixnano, attrs_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(series_key, start_unixnano, time_unixnano) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("store: prepare insert metric_points: %w", err)
	}
	defer stmt.Close()

	for _, m := range mps {
		if _, err := stmt.ExecContext(ctx,
			m.SessionID, m.TS, m.Name, m.AttrKey, m.ValueDelta, m.ValueKind,
			m.SeriesKey, m.StartUnixNano, m.TimeUnixNano, m.AttrsJSON); err != nil {
			return fmt.Errorf("store: insert metric_point %q: %w", m.SeriesKey, err)
		}
	}
	return nil
}

// upsertSeriesStates persists each delta cursor. Updated atomically in the same
// transaction as the metric_points inserts (PLAN §7).
func upsertSeriesStates(ctx context.Context, tx *sql.Tx, sts []model.SeriesState) error {
	if len(sts) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO series_state
			(series_key, last_value, last_start_unixnano, last_point_unixnano,
			 temporality, baseline_known, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(series_key) DO UPDATE SET
			last_value          = excluded.last_value,
			last_start_unixnano = excluded.last_start_unixnano,
			last_point_unixnano = excluded.last_point_unixnano,
			temporality         = excluded.temporality,
			baseline_known      = excluded.baseline_known,
			updated             = excluded.updated`)
	if err != nil {
		return fmt.Errorf("store: prepare upsert series_state: %w", err)
	}
	defer stmt.Close()

	for _, st := range sts {
		if _, err := stmt.ExecContext(ctx,
			st.Key, st.LastValue, st.LastStartUnixNano, st.LastPointUnixNano,
			st.Temporality, boolToInt(st.BaselineKnown), st.Updated); err != nil {
			return fmt.Errorf("store: upsert series_state %q: %w", st.Key, err)
		}
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
