// Package api is the UI-facing HTTP JSON API for wlog (PLAN §10 — frozen
// contract). It serves on-the-fly aggregation queries over the read pool, an
// SSE live stream (/api/now), a session export endpoint shared with the CLI,
// and static UI fallback to internal/web.
//
// Axis-of-truth (PLAN §6.5, REVIEWS A F3) is enforced in every aggregation
// query: token/cost come from api_request events when the session has any,
// otherwise they fall back to the token.usage / cost.usage metric points —
// NEVER both (an EXISTS branch picks exactly one source). active_time is always
// metrics; tool_decision / tool_result are always events. This prevents double
// counting on mixed sessions.
//
// All SQL uses parameter binding. Units (PLAN §10): ts is unix-ms, cost is USD,
// duration is ms, tokens are integers, ratios are 0..1.
package api

import (
	"encoding/json"
	"net/http"
)

// errorCode is the machine-readable error code in the standard error body
// (PLAN §10): {"error":{"code","message"}}.
type errorCode string

const (
	codeBadRequest   errorCode = "bad_request"
	codeNotFound     errorCode = "not_found"
	codeUnauthorized errorCode = "unauthorized"
	codeInternal     errorCode = "internal"
)

// emptyReason values populate meta.empty_reason on a 200 response that carries
// no data (PLAN §10 / DESIGN §3). They map 1:1 to the UI empty-state matrix.
// (A "no_active_session" reason is intentionally absent from REST: cost/tools
// with only metrics keep "no_data" and rely on the token_source banner, and the
// live /api/now stream signals "no active session" via a JSON null session_id in
// its tick payload rather than an empty_reason.)
const (
	reasonNoData       = "no_data"
	reasonLogsDisabled = "logs_disabled"
)

// errorEnvelope is the JSON body for an error response.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    errorCode `json:"code"`
	Message string    `json:"message"`
}

// meta is the optional metadata block attached to list/aggregation responses. A
// non-empty EmptyReason signals the UI which empty-state copy to render.
type meta struct {
	EmptyReason string `json:"empty_reason,omitempty"`
}

// writeJSON marshals v and writes it with the given status. On a marshal
// failure it falls back to a 500 error body so the client always gets valid
// JSON.
func writeJSON(w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "failed to encode response")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// writeError emits the standard error envelope with the given HTTP status.
func writeError(w http.ResponseWriter, status int, code errorCode, msg string) {
	buf, err := json.Marshal(errorEnvelope{Error: errorBody{Code: code, Message: msg}})
	if err != nil {
		// Should never happen for this small struct; degrade to plain text.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// badRequest / notFound / internalError are thin helpers for the common cases.
func badRequest(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusBadRequest, codeBadRequest, msg)
}

func notFound(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusNotFound, codeNotFound, msg)
}

func internalError(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusInternalServerError, codeInternal, msg)
}
