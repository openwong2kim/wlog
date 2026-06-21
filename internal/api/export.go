package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// handleExport streams a session export as JSON. Only format=json is supported
// in M1 (CSV is a follow-up). The aggregation/serialization is shared with the
// CLI via ExportSession.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	session := strings.TrimSpace(q.Get("session"))
	if session == "" {
		badRequest(w, "missing session")
		return
	}
	format := q.Get("format")
	if format == "" {
		format = "json"
	}
	if format != "json" {
		badRequest(w, "unsupported format: "+format)
		return
	}

	// Verify the session exists so a bad id is a 404 before we start streaming.
	var exists int
	if err := s.db.QueryRowContext(r.Context(),
		"SELECT EXISTS(SELECT 1 FROM sessions WHERE id = ?)", session).Scan(&exists); err != nil {
		internalError(w, "failed to query session")
		return
	}
	if exists == 0 {
		notFound(w, "session not found")
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "wlog-"+session+".json"))
	w.WriteHeader(http.StatusOK)

	if err := exportSession(r.Context(), s.db, session, w); err != nil {
		// Headers are already sent; best effort is to stop writing. Logging is the
		// caller's concern (Batch G wraps the server).
		return
	}
}

// ExportSession writes a complete JSON export of one session to w using the
// given read DB. It is the single source of truth for both the HTTP export
// endpoint and the CLI `wlog export` command (PLAN §10). The output is a single
// JSON object: {session, api_requests, tool_decisions, tool_results, events}.
//
// A missing session is not an error here (the caller checks existence); a
// session with no rows produces empty arrays.
func ExportSession(db *sql.DB, sessionID string, w io.Writer) error {
	return exportSession(context.Background(), db, sessionID, w)
}

// exportObject is the top-level export document. Slices are non-nil so they
// always serialize as [] rather than null.
type exportObject struct {
	Session       exportSessionMeta    `json:"session"`
	APIRequests   []exportAPIRequest   `json:"api_requests"`
	ToolDecisions []exportToolDecision `json:"tool_decisions"`
	ToolResults   []exportToolResult   `json:"tool_results"`
	Events        []exportEvent        `json:"events"`
}

type exportSessionMeta struct {
	ID          string `json:"id"`
	FirstSeen   int64  `json:"first_seen"`
	LastSeen    int64  `json:"last_seen"`
	AppVersion  string `json:"app_version"`
	ModelSet    string `json:"model_set"`
	UserEmail   string `json:"user_email"`
	OrgID       string `json:"org_id"`
	TokenSource string `json:"token_source"`
	HasEvents   bool   `json:"has_events"`
}

type exportAPIRequest struct {
	TS            int64   `json:"ts"`
	Model         string  `json:"model"`
	Input         int64   `json:"input"`
	Output        int64   `json:"output"`
	CacheRead     int64   `json:"cache_read"`
	CacheCreation int64   `json:"cache_creation"`
	CostUSD       float64 `json:"cost_usd"`
	DurationMS    int64   `json:"duration_ms"`
	IsError       bool    `json:"is_error"`
	StatusCode    int64   `json:"status_code"`
}

type exportToolDecision struct {
	TS       int64  `json:"ts"`
	Decision string `json:"decision"`
	Source   string `json:"source"`
	ToolName string `json:"tool_name"`
}

type exportToolResult struct {
	TS          int64  `json:"ts"`
	ToolName    string `json:"tool_name"`
	Success     bool   `json:"success"`
	DurationMS  int64  `json:"duration_ms"`
	BashCommand string `json:"bash_command"`
	MCPServer   string `json:"mcp_server"`
}

type exportEvent struct {
	TS   int64  `json:"ts"`
	Name string `json:"name"`
}

// exportSession is the context-aware implementation behind ExportSession and the
// HTTP handler.
func exportSession(ctx context.Context, db *sql.DB, sessionID string, w io.Writer) error {
	doc := exportObject{
		APIRequests:   []exportAPIRequest{},
		ToolDecisions: []exportToolDecision{},
		ToolResults:   []exportToolResult{},
		Events:        []exportEvent{},
	}

	// Session summary (identity/meta only — aggregation is a query concern).
	var (
		appVer, modelSet, userEmail, orgID, tokenSrc sql.NullString
		hasEvents                                    int
	)
	err := db.QueryRowContext(ctx, `
		SELECT id, first_seen, last_seen, app_version, model_set, user_email,
		       org_id, token_source, has_events
		FROM sessions WHERE id = ?`, sessionID).Scan(
		&doc.Session.ID, &doc.Session.FirstSeen, &doc.Session.LastSeen,
		&appVer, &modelSet, &userEmail, &orgID, &tokenSrc, &hasEvents)
	if err == sql.ErrNoRows {
		return fmt.Errorf("api: export: session %q not found", sessionID)
	}
	if err != nil {
		return fmt.Errorf("api: export: query session: %w", err)
	}
	doc.Session.AppVersion = appVer.String
	doc.Session.ModelSet = modelSet.String
	doc.Session.UserEmail = userEmail.String
	doc.Session.OrgID = orgID.String
	doc.Session.TokenSource = tokenSrc.String
	doc.Session.HasEvents = hasEvents != 0

	if err := exportAPIRequests(ctx, db, sessionID, &doc); err != nil {
		return err
	}
	if err := exportToolDecisions(ctx, db, sessionID, &doc); err != nil {
		return err
	}
	if err := exportToolResults(ctx, db, sessionID, &doc); err != nil {
		return err
	}
	if err := exportEvents(ctx, db, sessionID, &doc); err != nil {
		return err
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("api: export: encode: %w", err)
	}
	return nil
}

func exportAPIRequests(ctx context.Context, db *sql.DB, sessionID string, doc *exportObject) error {
	rows, err := db.QueryContext(ctx, `
		SELECT ts, model, input_tokens, output_tokens, cache_read, cache_creation,
		       cost_usd, duration_ms, is_error, status_code
		FROM api_requests WHERE session_id = ? ORDER BY ts ASC, id ASC`, sessionID)
	if err != nil {
		return fmt.Errorf("api: export: query api_requests: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			r     exportAPIRequest
			model sql.NullString
			isErr int
		)
		if err := rows.Scan(&r.TS, &model, &r.Input, &r.Output, &r.CacheRead,
			&r.CacheCreation, &r.CostUSD, &r.DurationMS, &isErr, &r.StatusCode); err != nil {
			return fmt.Errorf("api: export: scan api_request: %w", err)
		}
		r.Model = model.String
		r.IsError = isErr != 0
		doc.APIRequests = append(doc.APIRequests, r)
	}
	return rows.Err()
}

func exportToolDecisions(ctx context.Context, db *sql.DB, sessionID string, doc *exportObject) error {
	rows, err := db.QueryContext(ctx, `
		SELECT ts, decision, source, tool_name
		FROM tool_decisions WHERE session_id = ? ORDER BY ts ASC, id ASC`, sessionID)
	if err != nil {
		return fmt.Errorf("api: export: query tool_decisions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			d                        exportToolDecision
			decision, source, toolNm sql.NullString
		)
		if err := rows.Scan(&d.TS, &decision, &source, &toolNm); err != nil {
			return fmt.Errorf("api: export: scan tool_decision: %w", err)
		}
		d.Decision, d.Source, d.ToolName = decision.String, source.String, toolNm.String
		doc.ToolDecisions = append(doc.ToolDecisions, d)
	}
	return rows.Err()
}

func exportToolResults(ctx context.Context, db *sql.DB, sessionID string, doc *exportObject) error {
	rows, err := db.QueryContext(ctx, `
		SELECT ts, tool_name, success, duration_ms, bash_command, mcp_server
		FROM tool_results WHERE session_id = ? ORDER BY ts ASC, id ASC`, sessionID)
	if err != nil {
		return fmt.Errorf("api: export: query tool_results: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			r         exportToolResult
			toolNm    sql.NullString
			success   int
			bash, mcp sql.NullString
		)
		if err := rows.Scan(&r.TS, &toolNm, &success, &r.DurationMS, &bash, &mcp); err != nil {
			return fmt.Errorf("api: export: scan tool_result: %w", err)
		}
		r.ToolName, r.Success = toolNm.String, success != 0
		r.BashCommand, r.MCPServer = bash.String, mcp.String
		doc.ToolResults = append(doc.ToolResults, r)
	}
	return rows.Err()
}

func exportEvents(ctx context.Context, db *sql.DB, sessionID string, doc *exportObject) error {
	rows, err := db.QueryContext(ctx, `
		SELECT ts, name FROM events WHERE session_id = ? ORDER BY ts ASC, id ASC`, sessionID)
	if err != nil {
		return fmt.Errorf("api: export: query events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			e    exportEvent
			name sql.NullString
		)
		if err := rows.Scan(&e.TS, &name); err != nil {
			return fmt.Errorf("api: export: scan event: %w", err)
		}
		e.Name = name.String
		doc.Events = append(doc.Events, e)
	}
	return rows.Err()
}
