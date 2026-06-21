package model

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// DedupKey returns a deterministic idempotency key for an event. Parts are
// hashed in the given input order (no sorting) and the result is the first
// 16 hex characters of the SHA-256 digest. A NUL separator prevents
// adjacent-part ambiguity.
func DedupKey(parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// The functions below are the SINGLE SOURCE OF TRUTH for the dedup_key part
// order of each record type. Both ingestion paths — OTLP (internal/otel) and
// the Claude Code transcript JSONL parser (internal/jsonl) — call these, so two
// sources reporting the SAME logical call necessarily produce the SAME
// dedup_key and the store's UNIQUE constraint collapses them to one row (no
// double-counting; PLAN v3 §9 EQ6, the cost-accuracy gate).
//
// The timestamp part (tsNanos) is the event's instant rendered in base 10 as a
// unix-nanosecond string. CRITICAL for api_request (C1, EQ6): both paths must
// feed the SAME resolution. The transcript timestamp is millisecond-precision
// (RFC3339 ms), so the JSONL parser renders ms→ns with the low six digits zero
// (jsonl.nanosFromMillis); the OTLP path therefore truncates its raw
// LogRecord.TimeUnixNano to ms before rendering it the same way
// (otel.dedupNanosFromMillis) instead of using the raw nanoseconds. If it used
// raw ns, the two keys would disagree for essentially every call (the ns almost
// never lands exactly on a ms boundary), so the SAME api_request would land in
// the api_requests table TWICE and SUM(cost_usd)/tokens would DOUBLE — this is
// NOT a harmless duplicate: "single source" (PLAN §6.5) selects the events vs.
// metrics axis for token/cost, it does NOT deduplicate two rows already in the
// api_requests table. (Residual risk after the ms fix: if OTLP and the
// transcript record the same call at ms instants that differ by >=1ms, both
// rows still survive; in practice both are timestamps of the same logical call
// and agree once truncated to ms. Documented in PLAN §9.)

// APIRequestDedupKey builds the idempotency key for an api_request / api_error
// record. eventName is "api_request" or "api_error" (so a request and a later
// error at the same instant do not collide). tsNanos is the base-10 unix-nano
// timestamp; input/output are the base-10 token counts.
func APIRequestDedupKey(sessionID, eventName, tsNanos, model, inputTokens, outputTokens string) string {
	return DedupKey(sessionID, eventName, tsNanos, model, inputTokens, outputTokens)
}

// ToolResultDedupKey builds the idempotency key for a tool_result record. This
// is the CROSS-SOURCE key: both the OTLP path and the JSONL path (when the
// transcript tool_result carries no per-result discriminator) call it, so the
// SAME Bash/MCP run seen via both sources collapses to one row (EQ6, PLAN v3 §9).
// Its part order must not change or the OTLP↔JSONL keys would diverge.
func ToolResultDedupKey(sessionID, tsNanos, toolName string) string {
	return DedupKey(sessionID, "tool_result", tsNanos, toolName)
}

// ToolResultDedupKeyWithDiscriminator builds the idempotency key for a
// tool_result record that carries a per-result discriminator (the transcript's
// tool_use_id). It is used by the JSONL parser to keep MULTIPLE tool_results of
// the SAME tool_name on ONE transcript line distinct: those results share the
// line's single top-level timestamp, so the plain (session+ts+tool_name) key
// would collide and the tool_results.dedup_key UNIQUE constraint would drop all
// but the first — losing parallel/batched command history (e.g. two Bash runs
// completing on one line).
//
// The discriminator is folded in as a trailing part so a key WITH a discriminator
// can never equal a key WITHOUT one (the no-discriminator path keeps using
// ToolResultDedupKey above). Claude Code's OTLP tool_result event carries no
// tool_use_id (confirmed: its attributes are session.id/tool_name/success/
// duration_ms/tool_parameters only — internal/otel/logs.go appendToolResult), so
// this key is JSONL-internal: it sacrifices OTLP↔JSONL collapse for a discriminated
// tool_result, which is acceptable because tool_result is command-history display
// only and carries NO cost/token (those live in api_request, unaffected).
func ToolResultDedupKeyWithDiscriminator(sessionID, tsNanos, toolName, discriminator string) string {
	return DedupKey(sessionID, "tool_result", tsNanos, toolName, discriminator)
}

// ToolDecisionDedupKey builds the idempotency key for a tool_decision record.
// (tool_decision is currently OTLP-only — the JSONL transcript carries no
// permission decisions, PLAN v3 §9 EQ5 — but the constructor lives here so the
// part order stays centralized if that ever changes.)
func ToolDecisionDedupKey(sessionID, tsNanos, toolName, decision, source string) string {
	return DedupKey(sessionID, "tool_decision", tsNanos, toolName, decision, source)
}

// EventDedupKey builds the idempotency key for a generic Event record. name is
// the (prefix-normalized) event name; extra is a name-specific discriminator
// (the OTLP path passes the log body; the JSONL path passes a stable per-record
// id) so distinct events at the same instant do not collide.
func EventDedupKey(sessionID, name, tsNanos, extra string) string {
	return DedupKey(sessionID, name, tsNanos, extra)
}

// SeriesKey returns a deterministic series identifier for a metric. The dims
// map keys are sorted and stably serialized so the key is independent of map
// iteration order, then hashed to a SHA-256 hex digest.
func SeriesKey(metricName string, dims map[string]string) string {
	keys := make([]string, 0, len(dims))
	for k := range dims {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	sb.WriteString(metricName)
	for _, k := range keys {
		sb.WriteByte(0)
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(dims[k])
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])
}
