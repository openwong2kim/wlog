package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
)

// timelineEvent is one node in a session timeline. Fields are kind-specific
// (PLAN §10); we emit each node as an ordered field map so the JSON carries
// exactly the contract fields for its kind (e.g. tool_result always includes
// bash_command and mcp_server, as value or explicit null). seq preserves the
// per-source append order for a stable sort on equal ts.
type timelineEvent struct {
	ts     int64
	seq    int
	kind   string
	fields map[string]any
}

// MarshalJSON renders {ts, kind, ...kind fields}.
func (e timelineEvent) MarshalJSON() ([]byte, error) {
	m := make(map[string]any, len(e.fields)+2)
	for k, v := range e.fields {
		m[k] = v
	}
	m["ts"] = e.ts
	m["kind"] = e.kind
	return json.Marshal(m)
}

type timelineResponse struct {
	Events []timelineEvent `json:"events"`
	Meta   *meta           `json:"meta,omitempty"`
}

// handleTimeline returns the merged, ts-ascending event timeline for a session:
// user_prompt events, api_request / api_error, tool_decision, and tool_result.
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		badRequest(w, "missing session id")
		return
	}
	ctx := r.Context()

	// Confirm the session exists so a missing session is a 404 (not an empty 200).
	var exists int
	if err := s.db.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM sessions WHERE id = ?)", id).Scan(&exists); err != nil {
		internalError(w, "failed to query session")
		return
	}
	if exists == 0 {
		notFound(w, "session not found")
		return
	}

	events := make([]timelineEvent, 0, 64)
	if err := s.appendPrompts(ctx, id, &events); err != nil {
		internalError(w, "failed to query prompts")
		return
	}
	if err := s.appendAPIRequests(ctx, id, &events); err != nil {
		internalError(w, "failed to query api requests")
		return
	}
	if err := s.appendToolDecisions(ctx, id, &events); err != nil {
		internalError(w, "failed to query tool decisions")
		return
	}
	if err := s.appendToolResults(ctx, id, &events); err != nil {
		internalError(w, "failed to query tool results")
		return
	}

	// Stable sort by ts ascending; seq breaks ties deterministically (prompts <
	// api < decisions < results within an identical ts).
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].ts != events[j].ts {
			return events[i].ts < events[j].ts
		}
		return events[i].seq < events[j].seq
	})

	resp := timelineResponse{Events: events}
	if len(events) == 0 {
		resp.Meta = &meta{EmptyReason: reasonLogsDisabled}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) appendPrompts(ctx context.Context, id string, out *[]timelineEvent) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, attrs_json FROM events
		WHERE session_id = ? AND name = 'user_prompt'`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			ts    int64
			attrs sql.NullString
		)
		if err := rows.Scan(&ts, &attrs); err != nil {
			return err
		}
		f := map[string]any{}
		if pl, ok := promptLength(attrs.String); ok {
			f["prompt_length"] = pl
		} else {
			f["prompt_length"] = nil
		}
		// prompt text is present only when the prompt was stored (default). With
		// --no-store-prompts the parser persists prompt_length but omits "prompt",
		// so the field is null and the UI shows the "not stored" placeholder
		// instead of leaking nothing. prompt_length is always kept.
		if txt, ok := promptText(attrs.String); ok {
			f["prompt"] = txt
		} else {
			f["prompt"] = nil
		}
		*out = append(*out, timelineEvent{ts: ts, seq: 0, kind: "prompt", fields: f})
	}
	return rows.Err()
}

func (s *Server) appendAPIRequests(ctx context.Context, id string, out *[]timelineEvent) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, model, input_tokens, output_tokens, cache_read, cache_creation,
		       cost_usd, duration_ms, is_error, status_code, attrs_json
		FROM api_requests WHERE session_id = ?`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			ts, in, outTok, cr, cc, dur, status int64
			model                               sql.NullString
			cost                                float64
			isErr                               int
			attrs                               sql.NullString
		)
		if err := rows.Scan(&ts, &model, &in, &outTok, &cr, &cc, &cost, &dur, &isErr, &status, &attrs); err != nil {
			return err
		}
		if isErr != 0 {
			f := map[string]any{
				"model":       model.String,
				"status_code": status,
			}
			if msg, ok := attrString(attrs.String, "error", "error_message", "message"); ok {
				f["error"] = msg
			} else {
				f["error"] = nil
			}
			*out = append(*out, timelineEvent{ts: ts, seq: 1, kind: "api_error", fields: f})
			continue
		}
		f := map[string]any{
			"model":          model.String,
			"input":          in,
			"output":         outTok,
			"cache_read":     cr,
			"cache_creation": cc,
			"cost_usd":       cost,
			"duration_ms":    dur,
		}
		*out = append(*out, timelineEvent{ts: ts, seq: 1, kind: "api_request", fields: f})
	}
	return rows.Err()
}

func (s *Server) appendToolDecisions(ctx context.Context, id string, out *[]timelineEvent) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, decision, source, tool_name FROM tool_decisions WHERE session_id = ?`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			ts                       int64
			decision, source, toolNm sql.NullString
		)
		if err := rows.Scan(&ts, &decision, &source, &toolNm); err != nil {
			return err
		}
		*out = append(*out, timelineEvent{ts: ts, seq: 2, kind: "tool_decision", fields: map[string]any{
			"decision":  decision.String,
			"source":    source.String,
			"tool_name": toolNm.String,
		}})
	}
	return rows.Err()
}

func (s *Server) appendToolResults(ctx context.Context, id string, out *[]timelineEvent) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, tool_name, success, duration_ms, bash_command, mcp_server
		FROM tool_results WHERE session_id = ?`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			ts, dur   int64
			toolNm    sql.NullString
			success   int
			bash, mcp sql.NullString
		)
		if err := rows.Scan(&ts, &toolNm, &success, &dur, &bash, &mcp); err != nil {
			return err
		}
		f := map[string]any{
			"tool_name":   toolNm.String,
			"success":     success != 0,
			"duration_ms": dur,
		}
		// bash_command|null and mcp_server|null are always present (PLAN §10):
		// empty / NULL renders as JSON null so the UI can show the "details off"
		// placeholder distinctly.
		if bash.Valid && bash.String != "" {
			f["bash_command"] = bash.String
		} else {
			f["bash_command"] = nil
		}
		if mcp.Valid && mcp.String != "" {
			f["mcp_server"] = mcp.String
		} else {
			f["mcp_server"] = nil
		}
		*out = append(*out, timelineEvent{ts: ts, seq: 3, kind: "tool_result", fields: f})
	}
	return rows.Err()
}

// promptLength extracts the integer "prompt_length" from an attrs_json object.
func promptLength(attrsJSON string) (int64, bool) {
	if attrsJSON == "" {
		return 0, false
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(attrsJSON), &obj); err != nil {
		return 0, false
	}
	v, ok := obj["prompt_length"]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return i, true
	default:
		return 0, false
	}
}

// promptText extracts the "prompt" string from a user_prompt attrs_json object.
// ok is false when the key is absent (the --no-store-prompts case, where only
// prompt_length is persisted) or is not a non-empty string, so the caller can
// emit JSON null and the UI can distinguish "not stored" from an empty prompt.
func promptText(attrsJSON string) (string, bool) {
	return attrString(attrsJSON, "prompt")
}

// attrString extracts the first present string value among keys from attrs_json.
func attrString(attrsJSON string, keys ...string) (string, bool) {
	if attrsJSON == "" {
		return "", false
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(attrsJSON), &obj); err != nil {
		return "", false
	}
	for _, k := range keys {
		if v, ok := obj[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s, true
			}
		}
	}
	return "", false
}
