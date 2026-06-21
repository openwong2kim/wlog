package api

import (
	"database/sql"
	"net/http"
	"strings"
)

type toolsBySource struct {
	Source string `json:"source"`
	Accept int64  `json:"accept"`
	Reject int64  `json:"reject"`
}

type toolHotspot struct {
	ToolName string `json:"tool_name"`
	Reject   int64  `json:"reject"`
	Accept   int64  `json:"accept"`
}

type toolsResponse struct {
	AutoApproved int64           `json:"auto_approved"`
	Accept       int64           `json:"accept"`
	Reject       int64           `json:"reject"`
	RejectRate   float64         `json:"reject_rate"`
	BySource     []toolsBySource `json:"by_source"`
	Hotspots     []toolHotspot   `json:"hotspots"`
	Meta         *meta           `json:"meta,omitempty"`
}

// handleTools aggregates tool_decision events (always the events axis). It
// reports auto-approved count (config/hook sourced accepts), accept/reject
// totals, reject rate, per-source breakdown, and the top-5 reject hotspots.
func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	session := strings.TrimSpace(q.Get("session"))
	since := int64(parseIntDefault(q.Get("since"), 0))

	ctx := r.Context()

	var conds []string
	var args []any
	if session != "" {
		conds = append(conds, "td.session_id = ?")
		args = append(args, session)
	}
	if since > 0 {
		conds = append(conds, "td.ts >= ?")
		args = append(args, since)
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	resp := toolsResponse{BySource: []toolsBySource{}, Hotspots: []toolHotspot{}}

	// Totals + auto_approved in one pass.
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN td.decision = 'accept' THEN 1 ELSE 0 END), 0) AS accept,
			COALESCE(SUM(CASE WHEN td.decision = 'reject' THEN 1 ELSE 0 END), 0) AS reject,
			COALESCE(SUM(CASE WHEN td.decision = 'accept' AND td.source IN ('config','hook') THEN 1 ELSE 0 END), 0) AS auto
		FROM tool_decisions td
		`+where, args...).Scan(&resp.Accept, &resp.Reject, &resp.AutoApproved)
	if err != nil {
		internalError(w, "failed to query tool totals")
		return
	}
	if tot := resp.Accept + resp.Reject; tot > 0 {
		resp.RejectRate = float64(resp.Reject) / float64(tot)
	}

	// by_source.
	srows, err := s.db.QueryContext(ctx, `
		SELECT td.source,
		       SUM(CASE WHEN td.decision = 'accept' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN td.decision = 'reject' THEN 1 ELSE 0 END)
		FROM tool_decisions td
		`+where+`
		GROUP BY td.source
		ORDER BY td.source ASC`, args...)
	if err != nil {
		internalError(w, "failed to query tool sources")
		return
	}
	defer srows.Close()
	for srows.Next() {
		var (
			bs  toolsBySource
			src sql.NullString
		)
		if err := srows.Scan(&src, &bs.Accept, &bs.Reject); err != nil {
			internalError(w, "failed to scan tool source")
			return
		}
		bs.Source = src.String
		resp.BySource = append(resp.BySource, bs)
	}
	if err := srows.Err(); err != nil {
		internalError(w, "failed to read tool sources")
		return
	}

	// hotspots: top-5 by reject count.
	hrows, err := s.db.QueryContext(ctx, `
		SELECT td.tool_name,
		       SUM(CASE WHEN td.decision = 'reject' THEN 1 ELSE 0 END) AS reject,
		       SUM(CASE WHEN td.decision = 'accept' THEN 1 ELSE 0 END) AS accept
		FROM tool_decisions td
		`+where+`
		GROUP BY td.tool_name
		HAVING reject > 0
		ORDER BY reject DESC, td.tool_name ASC
		LIMIT 5`, args...)
	if err != nil {
		internalError(w, "failed to query tool hotspots")
		return
	}
	defer hrows.Close()
	for hrows.Next() {
		var (
			h    toolHotspot
			name sql.NullString
		)
		if err := hrows.Scan(&name, &h.Reject, &h.Accept); err != nil {
			internalError(w, "failed to scan tool hotspot")
			return
		}
		h.ToolName = name.String
		resp.Hotspots = append(resp.Hotspots, h)
	}
	if err := hrows.Err(); err != nil {
		internalError(w, "failed to read tool hotspots")
		return
	}

	if resp.Accept == 0 && resp.Reject == 0 {
		resp.Meta = &meta{EmptyReason: reasonNoData}
	}
	writeJSON(w, http.StatusOK, resp)
}
