package otel

import (
	"strings"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// Value getters across all AnyValue types.
func TestValue_Getters(t *testing.T) {
	if (Value{any: strVal("hi")}).String() != "hi" {
		t.Fatal("string getter")
	}
	if (Value{any: intVal(42)}).Int() != 42 {
		t.Fatal("int getter")
	}
	if (Value{any: dblVal(3.5)}).Float() != 3.5 {
		t.Fatal("double getter")
	}
	if !(Value{any: boolVal(true)}).Bool() {
		t.Fatal("bool getter")
	}
	// cross-type coercion
	if (Value{any: strVal("123")}).Int() != 123 {
		t.Fatal("string→int coercion")
	}
	if (Value{any: strVal("1.5")}).Float() != 1.5 {
		t.Fatal("string→float coercion")
	}
	if (Value{any: intVal(7)}).Float() != 7 {
		t.Fatal("int→float coercion")
	}
	// zero value
	var z Value
	if !z.IsZero() || z.String() != "" || z.Int() != 0 {
		t.Fatal("zero value")
	}
	// array / kvlist render to JSON strings
	arr := (Value{any: arrVal(intVal(1), strVal("x"))}).String()
	if !strings.HasPrefix(arr, "[") {
		t.Fatalf("array should render JSON array, got %q", arr)
	}
}

// normalizeEventName resolution order + prefix stripping.
func TestNormalizeEventName(t *testing.T) {
	cases := []struct {
		eventName string
		attrName  string
		want      string
	}{
		{"claude_code.api_request", "", "api_request"},
		{"api_request", "", "api_request"},
		{"", "claude_code.tool_result", "tool_result"},
		{"", "user_prompt", "user_prompt"},
		{"claude_code.unknown_x", "ignored", "unknown_x"},
		{"", "", ""},
	}
	for _, c := range cases {
		attrs := map[string]Value{}
		if c.attrName != "" {
			attrs["event.name"] = Value{any: strVal(c.attrName)}
		}
		if got := normalizeEventName(c.eventName, attrs); got != c.want {
			t.Fatalf("normalizeEventName(%q,%q) = %q want %q", c.eventName, c.attrName, got, c.want)
		}
	}
}

// extractDims keeps only whitelist keys.
func TestExtractDims_Whitelist(t *testing.T) {
	attrs := map[string]Value{
		"type":            {any: strVal("input")},
		"model":           {any: strVal("opus")},
		"language":        {any: strVal("go")},
		"service.version": {any: strVal("1.0")}, // excluded
		"user.email":      {any: strVal("a@b")}, // excluded
		"terminal.type":   {any: strVal("xterm")},
	}
	dims := extractDims(attrs)
	if len(dims) != 3 || dims["type"] != "input" || dims["model"] != "opus" || dims["language"] != "go" {
		t.Fatalf("whitelist violated: %+v", dims)
	}
	if _, ok := dims["service.version"]; ok {
		t.Fatal("volatile resource attr leaked into dims")
	}
}

// decodeToolParameters: nested decode, missing key, malformed JSON.
func TestDecodeToolParameters(t *testing.T) {
	// bash_command
	td := decodeToolParameters(map[string]Value{"tool_parameters": {any: strVal(`{"bash_command":"ls"}`)}})
	if td.BashCommand != "ls" {
		t.Fatalf("bash_command: %+v", td)
	}
	// command alias
	td = decodeToolParameters(map[string]Value{"tool_parameters": {any: strVal(`{"command":"pwd"}`)}})
	if td.BashCommand != "pwd" {
		t.Fatalf("command alias: %+v", td)
	}
	// mcp server aliases
	td = decodeToolParameters(map[string]Value{"tool_parameters": {any: strVal(`{"server_name":"gh"}`)}})
	if td.MCPServer != "gh" {
		t.Fatalf("server_name alias: %+v", td)
	}
	// malformed JSON → zero value, no panic
	td = decodeToolParameters(map[string]Value{"tool_parameters": {any: strVal(`{not json`)}})
	if td.BashCommand != "" || td.MCPServer != "" {
		t.Fatalf("malformed JSON should yield zero: %+v", td)
	}
	// missing attr → zero value
	td = decodeToolParameters(map[string]Value{})
	if td != (toolDetail{}) {
		t.Fatalf("missing attr should yield zero: %+v", td)
	}
}

// marshalAttrs enforces the count limit with a truncate marker.
func TestMarshalAttrs_CountLimit(t *testing.T) {
	kvs := make([]*commonpb.KeyValue, 0, maxAttrsCount+50)
	for i := 0; i < maxAttrsCount+50; i++ {
		kvs = append(kvs, kv("k"+itoa(i), intVal(int64(i))))
	}
	out := marshalAttrs(kvs)
	if !strings.Contains(out, truncateMarker) {
		t.Fatalf("expected truncate marker for count overflow")
	}
}

// marshalAttrs enforces the depth limit.
func TestMarshalAttrs_DepthLimit(t *testing.T) {
	// build nested kvlist deeper than maxAttrsDepth
	v := strVal("leaf")
	for i := 0; i < maxAttrsDepth+3; i++ {
		v = kvListVal(kv("n", v))
	}
	out := marshalAttrs([]*commonpb.KeyValue{kv("root", v)})
	if !strings.Contains(out, "depth_exceeded") {
		t.Fatalf("expected depth truncate marker, got %s", out)
	}
}

// marshalAttrs enforces the byte limit.
func TestMarshalAttrs_ByteLimit(t *testing.T) {
	big := strings.Repeat("x", maxAttrsBytes)
	out := marshalAttrs([]*commonpb.KeyValue{
		kv("big", strVal(big)),
		kv("small", strVal("ok")),
	})
	if len(out) > maxAttrsBytes+512 { // marker overhead tolerance
		t.Fatalf("attrs_json exceeded byte budget: %d", len(out))
	}
	if !strings.Contains(out, truncateMarker) {
		t.Fatalf("expected byte truncate marker")
	}
}

// empty attrs yields valid JSON object.
func TestMarshalAttrs_Empty(t *testing.T) {
	if marshalAttrs(nil) != "{}" {
		t.Fatalf("empty attrs must be {}")
	}
}

// tiny int→string for test keys without importing strconv everywhere.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}
