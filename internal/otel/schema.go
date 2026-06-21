// Package otel converts OTLP export requests (metrics, logs, traces) into the
// wlog domain model (internal/model). It implements the Claude Code OTel schema
// mapping (PLAN §6) and the delta temporality normalization (PLAN §7).
//
// The package depends only on model + the OTLP proto types; it never touches
// the store. Series-state persistence is provided through the
// model.SeriesStateStore interface (dependency inversion) and replayed into the
// Normalizer via Seed.
package otel

import (
	"encoding/json"
	"strconv"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// claudeCodePrefix is stripped from event names. Claude Code emits names like
// "claude_code.api_request"; we normalize to the bare "api_request".
const claudeCodePrefix = "claude_code."

// Known normalized event names (PLAN §6.1). Unknown names are preserved as-is
// (after prefix stripping) and stored as generic Events.
const (
	EventAPIRequest          = "api_request"
	EventAPIError            = "api_error"
	EventToolResult          = "tool_result"
	EventToolDecision        = "tool_decision"
	EventUserPrompt          = "user_prompt"
	EventMCPServerConnection = "mcp_server_connection"
)

// attrs_json serialization limits (PLAN §17 / REVIEWS D). Exceeding any limit
// inserts a truncate marker rather than dropping the record.
const (
	maxAttrsCount = 256
	maxAttrsBytes = 64 * 1024 // 64KB
	maxAttrsDepth = 8
)

// truncateMarker is the sentinel key inserted into attrs_json when a limit is
// hit so the truncation is visible downstream.
const truncateMarker = "__truncated__"

// dimWhitelist is the set of attribute keys allowed into a metric series key
// (PLAN §7 / REVIEWS A F1). Volatile resource attributes (service.version,
// app.version, user.email, terminal.type, ...) are deliberately excluded so a
// restart or version bump does not fork the series and cause a count explosion.
var dimWhitelist = map[string]struct{}{
	"type":       {},
	"model":      {},
	"language":   {},
	"session.id": {},
}

// normalizeEventName resolves the canonical event name for a log record.
// Resolution order (PLAN §6.1): LogRecord.EventName (OTLP 1.x field) first, then
// the "event.name" attribute fallback, then strip the "claude_code." prefix.
func normalizeEventName(eventName string, attrs map[string]Value) string {
	name := strings.TrimSpace(eventName)
	if name == "" {
		if v, ok := attrs["event.name"]; ok {
			name = strings.TrimSpace(v.String())
		}
	}
	name = strings.TrimPrefix(name, claudeCodePrefix)
	return name
}

// Value is a typed wrapper around an OTLP AnyValue. It exposes total-typed
// getters (every AnyValue variant) with safe zero-value fallbacks so callers
// never need to type-switch the oneof directly.
type Value struct {
	any *commonpb.AnyValue
}

// String returns the value coerced to a string. String values pass through;
// numeric/bool values are formatted; arrays/kvlists are JSON-encoded; bytes are
// best-effort UTF-8. An empty/nil value yields "".
func (v Value) String() string {
	if v.any == nil {
		return ""
	}
	switch x := v.any.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'g', -1, 64)
	case *commonpb.AnyValue_BytesValue:
		return string(x.BytesValue)
	case *commonpb.AnyValue_ArrayValue, *commonpb.AnyValue_KvlistValue:
		b, err := json.Marshal(anyValueToGo(v.any, 0))
		if err != nil {
			return ""
		}
		return string(b)
	default:
		return ""
	}
}

// Int returns the value as an int64. Int values pass through; doubles truncate;
// bools map to 0/1; string values are parsed when numeric. Otherwise 0.
func (v Value) Int() int64 {
	if v.any == nil {
		return 0
	}
	switch x := v.any.GetValue().(type) {
	case *commonpb.AnyValue_IntValue:
		return x.IntValue
	case *commonpb.AnyValue_DoubleValue:
		return int64(x.DoubleValue)
	case *commonpb.AnyValue_BoolValue:
		if x.BoolValue {
			return 1
		}
		return 0
	case *commonpb.AnyValue_StringValue:
		if n, err := strconv.ParseInt(strings.TrimSpace(x.StringValue), 10, 64); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(x.StringValue), 64); err == nil {
			return int64(f)
		}
		return 0
	default:
		return 0
	}
}

// Float returns the value as a float64. Doubles/ints pass through; numeric
// strings are parsed; bools map to 0/1. Otherwise 0.
func (v Value) Float() float64 {
	if v.any == nil {
		return 0
	}
	switch x := v.any.GetValue().(type) {
	case *commonpb.AnyValue_DoubleValue:
		return x.DoubleValue
	case *commonpb.AnyValue_IntValue:
		return float64(x.IntValue)
	case *commonpb.AnyValue_BoolValue:
		if x.BoolValue {
			return 1
		}
		return 0
	case *commonpb.AnyValue_StringValue:
		if f, err := strconv.ParseFloat(strings.TrimSpace(x.StringValue), 64); err == nil {
			return f
		}
		return 0
	default:
		return 0
	}
}

// Bool returns the value as a bool. Bool values pass through; non-zero numbers
// are true; strings parse "true"/"1"/etc. Otherwise false.
func (v Value) Bool() bool {
	if v.any == nil {
		return false
	}
	switch x := v.any.GetValue().(type) {
	case *commonpb.AnyValue_BoolValue:
		return x.BoolValue
	case *commonpb.AnyValue_IntValue:
		return x.IntValue != 0
	case *commonpb.AnyValue_DoubleValue:
		return x.DoubleValue != 0
	case *commonpb.AnyValue_StringValue:
		b, err := strconv.ParseBool(strings.TrimSpace(x.StringValue))
		if err != nil {
			return false
		}
		return b
	default:
		return false
	}
}

// IsZero reports whether the value is absent (no underlying AnyValue).
func (v Value) IsZero() bool { return v.any == nil }

// attrsToMap converts an OTLP []*KeyValue list into a lookup map of typed
// Values. Later keys win on collision (proto attribute lists are de facto
// unique, but we are defensive).
func attrsToMap(kvs []*commonpb.KeyValue) map[string]Value {
	m := make(map[string]Value, len(kvs))
	for _, kv := range kvs {
		if kv == nil || kv.GetKey() == "" {
			continue
		}
		m[kv.GetKey()] = Value{any: kv.GetValue()}
	}
	return m
}

// extractDims pulls the whitelisted series dimensions (PLAN §7) out of an
// attribute map. Only {type, model, language, session.id} with non-empty values
// are included; everything else (volatile resource attrs, metadata) is excluded
// from the series key.
func extractDims(attrs map[string]Value) map[string]string {
	dims := make(map[string]string)
	for k := range dimWhitelist {
		if v, ok := attrs[k]; ok {
			if s := v.String(); s != "" {
				dims[k] = s
			}
		}
	}
	return dims
}

// sessionID resolves the session id with a datapoint/logrecord-first, resource-
// fallback strategy (PLAN §6.2). Claude Code carries "session.id"; we also accept
// the legacy "session_id" spelling.
func sessionID(primary, resource map[string]Value) string {
	for _, src := range []map[string]Value{primary, resource} {
		for _, key := range []string{"session.id", "session_id"} {
			if v, ok := src[key]; ok {
				if s := strings.TrimSpace(v.String()); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

// stringAttr returns the first present, non-empty string value among the given
// keys, searching primary then resource scope.
func stringAttr(primary, resource map[string]Value, keys ...string) string {
	for _, src := range []map[string]Value{primary, resource} {
		for _, k := range keys {
			if v, ok := src[k]; ok {
				if s := strings.TrimSpace(v.String()); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

// toolDetail holds the fields decoded from a tool_parameters JSON string
// attribute (PLAN §6.1 / §6.3). Claude Code nests tool detail inside this string
// rather than exposing direct columns, so we decode defensively.
type toolDetail struct {
	BashCommand string
	FullCommand string
	MCPServer   string
}

// decodeToolParameters parses the tool_parameters attribute (a JSON-encoded
// string) and extracts bash_command / full_command / mcp_server. Missing,
// empty, or malformed JSON yields a zero toolDetail (never an error path —
// version drift must not break ingestion).
func decodeToolParameters(attrs map[string]Value) toolDetail {
	var td toolDetail
	v, ok := attrs["tool_parameters"]
	if !ok {
		return td
	}
	raw := strings.TrimSpace(v.String())
	if raw == "" {
		return td
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return td
	}
	td.BashCommand = jsonString(obj, "bash_command", "command")
	td.FullCommand = jsonString(obj, "full_command")
	td.MCPServer = jsonString(obj, "mcp_server", "server_name", "server")
	return td
}

// jsonString returns the first present string-coercible value among keys in a
// decoded JSON object.
func jsonString(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		v, ok := obj[k]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case string:
			if s := strings.TrimSpace(t); s != "" {
				return s
			}
		case float64:
			return strconv.FormatFloat(t, 'g', -1, 64)
		case bool:
			return strconv.FormatBool(t)
		}
	}
	return ""
}

// marshalAttrs serializes an OTLP attribute list to a bounded JSON object
// string for the attrs_json column. Limits (PLAN §17): at most maxAttrsCount
// keys, maxAttrsDepth nesting, and maxAttrsBytes total — exceeding any inserts a
// truncate marker. Returns "{}" for an empty list (never empty string, so the
// column is always valid JSON).
func marshalAttrs(kvs []*commonpb.KeyValue) string {
	if len(kvs) == 0 {
		return "{}"
	}
	out := make(map[string]any, len(kvs))
	truncated := false
	count := 0
	for _, kv := range kvs {
		if kv == nil || kv.GetKey() == "" {
			continue
		}
		if count >= maxAttrsCount {
			truncated = true
			break
		}
		out[kv.GetKey()] = anyValueToGo(kv.GetValue(), 0)
		count++
	}
	if truncated {
		out[truncateMarker] = "attribute_count_exceeded"
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "{}"
	}
	if len(b) > maxAttrsBytes {
		// Re-encode a marker-only object preserving as many small scalar keys
		// as fit, rather than emitting oversized JSON.
		return marshalAttrsTruncatedBytes(out)
	}
	return string(b)
}

// marshalAttrsTruncatedBytes builds a byte-bounded attrs_json. It greedily adds
// keys (deterministically, sorted) until adding the next would exceed the byte
// budget, then appends a truncate marker.
func marshalAttrsTruncatedBytes(full map[string]any) string {
	keys := make([]string, 0, len(full))
	for k := range full {
		if k == truncateMarker {
			continue
		}
		keys = append(keys, k)
	}
	// Stable order so truncation is deterministic across re-sends (idempotency).
	sortStrings(keys)

	acc := map[string]any{truncateMarker: "byte_size_exceeded"}
	for _, k := range keys {
		candidate := make(map[string]any, len(acc)+1)
		for ck, cv := range acc {
			candidate[ck] = cv
		}
		candidate[k] = full[k]
		b, err := json.Marshal(candidate)
		if err != nil {
			continue
		}
		if len(b) > maxAttrsBytes {
			break
		}
		acc = candidate
	}
	b, err := json.Marshal(acc)
	if err != nil {
		return `{"` + truncateMarker + `":"byte_size_exceeded"}`
	}
	return string(b)
}

// sortStrings is a tiny insertion sort to avoid pulling sort into hot paths'
// allocation profile; the slices here are small (≤256).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// anyValueToGo converts an OTLP AnyValue into a plain Go value for JSON
// encoding, enforcing the depth limit (PLAN §17). At/over the limit, nested
// arrays/maps collapse to a truncate marker string instead of recursing.
func anyValueToGo(v *commonpb.AnyValue, depth int) any {
	if v == nil {
		return nil
	}
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		return x.BoolValue
	case *commonpb.AnyValue_IntValue:
		return x.IntValue
	case *commonpb.AnyValue_DoubleValue:
		return x.DoubleValue
	case *commonpb.AnyValue_BytesValue:
		return string(x.BytesValue)
	case *commonpb.AnyValue_ArrayValue:
		if depth >= maxAttrsDepth {
			return truncateMarker + ":depth_exceeded"
		}
		arr := x.ArrayValue.GetValues()
		out := make([]any, 0, len(arr))
		for _, e := range arr {
			out = append(out, anyValueToGo(e, depth+1))
		}
		return out
	case *commonpb.AnyValue_KvlistValue:
		if depth >= maxAttrsDepth {
			return truncateMarker + ":depth_exceeded"
		}
		kvs := x.KvlistValue.GetValues()
		out := make(map[string]any, len(kvs))
		for _, kv := range kvs {
			if kv == nil || kv.GetKey() == "" {
				continue
			}
			out[kv.GetKey()] = anyValueToGo(kv.GetValue(), depth+1)
		}
		return out
	default:
		return nil
	}
}

// nanoToMillis converts a unix-nanosecond timestamp to unix-milliseconds.
// Zero in yields zero out (caller decides on a receive-time fallback).
func nanoToMillis(ns uint64) int64 {
	if ns == 0 {
		return 0
	}
	return int64(ns / 1_000_000)
}

// dedupNanosFromMillis renders a unix-MILLISECOND instant as the base-10
// unix-NANOSECOND string used in an api_request dedup_key (C1, EQ6). It is the
// byte-for-byte counterpart of the JSONL parser's nanosFromMillis: the OTLP
// path truncates LogRecord.TimeUnixNano to ms (nanoToMillis) before calling
// this, so both ingestion paths feed the SAME ms-resolution timestamp into the
// shared model.APIRequestDedupKey and the same logical call collapses to one
// row. A zero instant renders "0", matching the JSONL helper exactly.
func dedupNanosFromMillis(ms int64) string {
	if ms == 0 {
		return "0"
	}
	return strconv.FormatInt(ms*1_000_000, 10)
}
