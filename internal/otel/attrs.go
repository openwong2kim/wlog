package otel

import (
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// firstInt returns the int64 value of the first present key in attrs.
func firstInt(attrs map[string]Value, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := attrs[k]; ok && !v.IsZero() {
			return v.Int()
		}
	}
	return 0
}

// firstFloat returns the float64 value of the first present key in attrs.
func firstFloat(attrs map[string]Value, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := attrs[k]; ok && !v.IsZero() {
			return v.Float()
		}
	}
	return 0
}

// firstString returns the first present non-empty string among keys.
func firstString(attrs map[string]Value, keys ...string) string {
	for _, k := range keys {
		if v, ok := attrs[k]; ok {
			if s := v.String(); s != "" {
				return s
			}
		}
	}
	return ""
}

// bodyString renders a log record body as a string for dedup hashing. Empty
// when the body is absent.
func bodyString(lr interface{ GetBody() *commonpb.AnyValue }) string {
	if lr == nil {
		return ""
	}
	return Value{any: lr.GetBody()}.String()
}

// sensitiveAttrKeys is the set of attribute keys whose values may carry raw user
// input (prompt text) or raw shell commands. When --no-store-prompts is set
// (Options.NoStorePrompts), these are redacted before the attribute list is
// serialized into AttrsJSON so the sensitive payload never reaches the store.
// This covers the free-text prompt family AND the shell command family that
// can otherwise leak through the raw tool_parameters / command attributes (the
// extracted BashCommand column is masked separately in appendToolResult).
var sensitiveAttrKeys = map[string]struct{}{
	// Prompt / free-text family.
	"prompt":      {},
	"prompt_text": {},
	"user_prompt": {},
	"content":     {},
	"message":     {},
	// Shell-command family (raw and nested-blob forms).
	"tool_parameters": {},
	"command":         {},
	"full_command":    {},
	"bash_command":    {},
}

// maskSensitiveAttrs returns a copy of the attribute list with every
// sensitive key (prompt + shell command family) redacted (used when
// --no-store-prompts is set). The original slice is not mutated; non-sensitive
// keys pass through unchanged so counts/metadata stay accurate.
func maskSensitiveAttrs(kvs []*commonpb.KeyValue) []*commonpb.KeyValue {
	if len(kvs) == 0 {
		return kvs
	}
	out := make([]*commonpb.KeyValue, 0, len(kvs))
	for _, kv := range kvs {
		if kv == nil {
			continue
		}
		if _, sensitive := sensitiveAttrKeys[kv.GetKey()]; sensitive {
			out = append(out, &commonpb.KeyValue{
				Key:   kv.GetKey(),
				Value: stringAnyValue(maskedValue),
			})
			continue
		}
		out = append(out, kv)
	}
	return out
}

// stringAnyValue wraps a string in an OTLP AnyValue.
func stringAnyValue(s string) *commonpb.AnyValue {
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
}
