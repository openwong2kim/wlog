package jsonl

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/openwong2kim/wlog/internal/model"
	"github.com/openwong2kim/wlog/internal/otel"
	collectorlogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// parse is a test helper that runs Parse over a transcript built from lines.
func parse(t *testing.T, opts Options, lines ...string) (model.Batch, Stats) {
	t.Helper()
	b, st, err := Parse(strings.NewReader(strings.Join(lines, "\n")+"\n"), opts)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	return b, st
}

// --- synthetic transcript line builders (match the confirmed real schema) ---

// assistantLine builds an assistant message line with usage and optional
// content blocks (already-marshaled JSON for content).
func assistantLine(sid, ts, mdl string, in, out, cacheCreate, cacheRead int64, contentJSON string) string {
	if contentJSON == "" {
		contentJSON = `[{"type":"text","text":"ok"}]`
	}
	return `{"type":"assistant","uuid":"u-` + ts + `","timestamp":"` + ts + `","sessionId":"` + sid +
		`","cwd":"D:\\proj\\myrepo","gitBranch":"main","version":"2.1.143","message":{"role":"assistant","model":"` + mdl +
		`","id":"msg_x","usage":{"input_tokens":` + itoa(in) +
		`,"output_tokens":` + itoa(out) +
		`,"cache_creation_input_tokens":` + itoa(cacheCreate) +
		`,"cache_read_input_tokens":` + itoa(cacheRead) +
		`},"content":` + contentJSON + `}}`
}

// userPromptLine builds a plain-string user prompt line.
func userPromptLine(sid, ts, prompt string) string {
	pb, _ := json.Marshal(prompt)
	return `{"type":"user","uuid":"up-` + ts + `","timestamp":"` + ts + `","sessionId":"` + sid +
		`","cwd":"D:\\proj\\myrepo","gitBranch":"main","message":{"role":"user","content":` + string(pb) + `}}`
}

// userToolResultLine builds a user message carrying a tool_result block.
func userToolResultLine(sid, ts, toolUseID string, isError *bool, content string) string {
	cb, _ := json.Marshal(content)
	errPart := ""
	if isError != nil {
		errPart = `,"is_error":` + strconv.FormatBool(*isError)
	}
	return `{"type":"user","uuid":"ur-` + ts + `","timestamp":"` + ts + `","sessionId":"` + sid +
		`","cwd":"D:\\proj\\myrepo","gitBranch":"main","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + toolUseID +
		`"` + errPart + `,"content":` + string(cb) + `}]}}`
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

func boolPtr(b bool) *bool { return &b }

// --- tests -----------------------------------------------------------------

// TestAssistantUsageToAPIRequest verifies an assistant line with usage maps to
// one APIRequest with the four token counters and a pricing-derived cost.
func TestAssistantUsageToAPIRequest(t *testing.T) {
	b, st := parse(t, Options{},
		assistantLine("s1", "2026-05-18T13:43:39.311Z", "claude-opus-4-7", 6, 29, 8228, 48622, ""),
	)

	if len(b.APIRequests) != 1 {
		t.Fatalf("APIRequests = %d, want 1", len(b.APIRequests))
	}
	r := b.APIRequests[0]
	if r.SessionID != "s1" {
		t.Errorf("SessionID = %q, want s1", r.SessionID)
	}
	if r.Model != "claude-opus-4-7" {
		t.Errorf("Model = %q", r.Model)
	}
	if r.Tokens != (model.Tokens{Input: 6, Output: 29, CacheCreation: 8228, CacheRead: 48622}) {
		t.Errorf("Tokens = %+v", r.Tokens)
	}
	if r.IsError {
		t.Errorf("IsError = true, want false (no api_error in transcript)")
	}
	// Cost = opus: in 6*15 + out 29*75 + cacheRead 48622*1.5 + cacheCreate 8228*18.75, /1e6.
	wantCost := (6*15.0 + 29*75.0 + 48622*1.5 + 8228*18.75) / 1e6
	if !approx(r.CostUSD, wantCost) {
		t.Errorf("CostUSD = %v, want %v", r.CostUSD, wantCost)
	}
	if st.Mapped != 1 {
		t.Errorf("Stats.Mapped = %d, want 1", st.Mapped)
	}
}

// TestUnknownModelZeroCost verifies an unrecognized model yields $0 (no panic,
// no silent wrong number) — PLAN v3 §9 Failure Modes.
func TestUnknownModelZeroCost(t *testing.T) {
	b, _ := parse(t, Options{},
		assistantLine("s1", "2026-05-18T13:43:39.311Z", "some-unknown-model", 100, 200, 0, 0, ""),
	)
	if len(b.APIRequests) != 1 {
		t.Fatalf("APIRequests = %d", len(b.APIRequests))
	}
	if b.APIRequests[0].CostUSD != 0 {
		t.Errorf("unknown-model CostUSD = %v, want 0", b.APIRequests[0].CostUSD)
	}
}

// TestToolUseResultJoinBash verifies a Bash tool_use (assistant) joins with its
// tool_result (user) into one ToolResult carrying the command and success.
func TestToolUseResultJoinBash(t *testing.T) {
	toolUse := `[{"type":"text","text":"running"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"go test ./...","description":"run tests"}}]`
	b, _ := parse(t, Options{},
		assistantLine("s1", "2026-05-18T13:43:40.000Z", "claude-opus-4-7", 1, 1, 0, 0, toolUse),
		userToolResultLine("s1", "2026-05-18T13:43:41.000Z", "toolu_1", boolPtr(false), "ok\nPASS"),
	)

	if len(b.ToolResults) != 1 {
		t.Fatalf("ToolResults = %d, want 1", len(b.ToolResults))
	}
	tr := b.ToolResults[0]
	if tr.ToolName != "Bash" {
		t.Errorf("ToolName = %q, want Bash", tr.ToolName)
	}
	if tr.BashCommand != "go test ./..." {
		t.Errorf("BashCommand = %q", tr.BashCommand)
	}
	if !tr.Success {
		t.Errorf("Success = false, want true (is_error=false)")
	}
	if tr.MCPServer != "" {
		t.Errorf("MCPServer = %q, want empty", tr.MCPServer)
	}
}

// TestToolResultErrorFlag verifies is_error=true → Success=false, and an absent
// is_error defaults to success.
func TestToolResultErrorFlag(t *testing.T) {
	toolUse := `[{"type":"tool_use","id":"toolu_e","name":"Bash","input":{"command":"false"}}]`
	b, _ := parse(t, Options{},
		assistantLine("s1", "2026-05-18T13:00:00.000Z", "claude-haiku-4", 1, 1, 0, 0, toolUse),
		userToolResultLine("s1", "2026-05-18T13:00:01.000Z", "toolu_e", boolPtr(true), "boom"),
	)
	if len(b.ToolResults) != 1 || b.ToolResults[0].Success {
		t.Fatalf("expected one failing ToolResult, got %+v", b.ToolResults)
	}

	// absent is_error → success
	toolUse2 := `[{"type":"tool_use","id":"toolu_ok","name":"Read","input":{"file_path":"x"}}]`
	b2, _ := parse(t, Options{},
		assistantLine("s2", "2026-05-18T13:00:00.000Z", "claude-haiku-4", 1, 1, 0, 0, toolUse2),
		userToolResultLine("s2", "2026-05-18T13:00:01.000Z", "toolu_ok", nil, "file contents"),
	)
	if len(b2.ToolResults) != 1 || !b2.ToolResults[0].Success {
		t.Fatalf("absent is_error should be success, got %+v", b2.ToolResults)
	}
}

// TestMCPServerExtraction verifies an mcp__<server>__<tool> tool name yields the
// server segment in ToolResult.MCPServer.
func TestMCPServerExtraction(t *testing.T) {
	toolUse := `[{"type":"tool_use","id":"toolu_m","name":"mcp__wmux__browser_open","input":{}}]`
	b, _ := parse(t, Options{},
		assistantLine("s1", "2026-05-18T13:00:00.000Z", "claude-opus-4-7", 1, 1, 0, 0, toolUse),
		userToolResultLine("s1", "2026-05-18T13:00:01.000Z", "toolu_m", boolPtr(false), "opened"),
	)
	if len(b.ToolResults) != 1 {
		t.Fatalf("ToolResults = %d", len(b.ToolResults))
	}
	if b.ToolResults[0].MCPServer != "wmux" {
		t.Errorf("MCPServer = %q, want wmux", b.ToolResults[0].MCPServer)
	}
	if b.ToolResults[0].ToolName != "mcp__wmux__browser_open" {
		t.Errorf("ToolName = %q", b.ToolResults[0].ToolName)
	}
}

// TestSessionRepoBranch verifies cwd → Session.Repo (project dir name) and
// gitBranch → Session.GitBranch (PLAN v3 feature 3).
func TestSessionRepoBranch(t *testing.T) {
	b, _ := parse(t, Options{},
		assistantLine("s1", "2026-05-18T13:00:00.000Z", "claude-opus-4-7", 1, 1, 0, 0, ""),
	)
	if len(b.Sessions) != 1 {
		t.Fatalf("Sessions = %d, want 1", len(b.Sessions))
	}
	s := b.Sessions[0]
	if s.Repo != "myrepo" {
		t.Errorf("Repo = %q, want myrepo (from cwd D:\\proj\\myrepo)", s.Repo)
	}
	if s.GitBranch != "main" {
		t.Errorf("GitBranch = %q, want main", s.GitBranch)
	}
	if s.AppVersion != "2.1.143" {
		t.Errorf("AppVersion = %q", s.AppVersion)
	}
	if s.TokenSource != "events" {
		t.Errorf("TokenSource = %q, want events", s.TokenSource)
	}
	if !s.HasEvents {
		t.Errorf("HasEvents = false, want true")
	}
}

// TestRepoFromCWD exercises the cwd → repo derivation directly across path
// styles and edge cases.
func TestRepoFromCWD(t *testing.T) {
	cases := map[string]string{
		`D:\proj\myrepo`:      "myrepo",
		`D:\proj\myrepo\`:     "myrepo",
		`/home/me/work/wlog`:  "wlog",
		`/home/me/work/wlog/`: "wlog",
		`C:\Users\rizz`:       "rizz",
		`C:\`:                 "", // drive root → no useful name
		``:                    "",
		`   `:                 "",
	}
	for in, want := range cases {
		if got := repoFromCWD(in); got != want {
			t.Errorf("repoFromCWD(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFirstNonEmptyMetadataWins verifies a later line that omits cwd/gitBranch
// does not clobber values an earlier line supplied (matches the store upsert
// rule).
func TestFirstNonEmptyMetadataWins(t *testing.T) {
	// First line has repo/branch; second line (a "mode" metadata line) omits them.
	lineWithMeta := assistantLine("s1", "2026-05-18T13:00:00.000Z", "claude-opus-4-7", 1, 1, 0, 0, "")
	lineNoMeta := `{"type":"mode","mode":"normal","sessionId":"s1","timestamp":"2026-05-18T13:05:00.000Z"}`
	b, _ := parse(t, Options{}, lineWithMeta, lineNoMeta)
	if len(b.Sessions) != 1 {
		t.Fatalf("Sessions = %d", len(b.Sessions))
	}
	if b.Sessions[0].Repo != "myrepo" || b.Sessions[0].GitBranch != "main" {
		t.Errorf("metadata clobbered: Repo=%q Branch=%q", b.Sessions[0].Repo, b.Sessions[0].GitBranch)
	}
	// last_seen should extend to the later metadata line's timestamp.
	if b.Sessions[0].LastSeen <= b.Sessions[0].FirstSeen {
		t.Errorf("LastSeen %d should be > FirstSeen %d", b.Sessions[0].LastSeen, b.Sessions[0].FirstSeen)
	}
}

// TestNoStorePromptsMasking verifies prompt text and bash commands are redacted
// and the raw line is not stored when NoStorePrompts is set.
func TestNoStorePromptsMasking(t *testing.T) {
	toolUse := `[{"type":"tool_use","id":"toolu_s","name":"Bash","input":{"command":"cat /etc/secret"}}]`
	b, _ := parse(t, Options{NoStorePrompts: true},
		userPromptLine("s1", "2026-05-18T12:00:00.000Z", "my secret prompt"),
		assistantLine("s1", "2026-05-18T12:00:01.000Z", "claude-opus-4-7", 1, 1, 0, 0, toolUse),
		userToolResultLine("s1", "2026-05-18T12:00:02.000Z", "toolu_s", boolPtr(false), "secret output"),
	)

	// bash command masked.
	if len(b.ToolResults) != 1 {
		t.Fatalf("ToolResults = %d", len(b.ToolResults))
	}
	if b.ToolResults[0].BashCommand != maskedValue {
		t.Errorf("BashCommand = %q, want %q", b.ToolResults[0].BashCommand, maskedValue)
	}
	// tool_result raw attrs must not contain the secret output.
	if strings.Contains(b.ToolResults[0].AttrsJSON, "secret output") {
		t.Errorf("tool_result attrs leaked output under NoStorePrompts: %s", b.ToolResults[0].AttrsJSON)
	}

	// prompt event: no prompt text, length present.
	var promptEv *model.Event
	for i := range b.Events {
		if b.Events[i].Name == "user_prompt" {
			promptEv = &b.Events[i]
		}
	}
	if promptEv == nil {
		t.Fatal("no user_prompt event")
	}
	if strings.Contains(promptEv.AttrsJSON, "my secret prompt") {
		t.Errorf("prompt text leaked under NoStorePrompts: %s", promptEv.AttrsJSON)
	}
	attrs := map[string]any{}
	_ = json.Unmarshal([]byte(promptEv.AttrsJSON), &attrs)
	if attrs["prompt_length"] == nil {
		t.Errorf("prompt_length missing: %s", promptEv.AttrsJSON)
	}
}

// TestPromptStoredWhenAllowed verifies that without NoStorePrompts the prompt
// text and prompt_length are recorded.
func TestPromptStoredWhenAllowed(t *testing.T) {
	b, _ := parse(t, Options{},
		userPromptLine("s1", "2026-05-18T12:00:00.000Z", "hello world"),
	)
	var ev *model.Event
	for i := range b.Events {
		if b.Events[i].Name == "user_prompt" {
			ev = &b.Events[i]
		}
	}
	if ev == nil {
		t.Fatal("no user_prompt event")
	}
	attrs := map[string]any{}
	if err := json.Unmarshal([]byte(ev.AttrsJSON), &attrs); err != nil {
		t.Fatalf("attrs not JSON: %v", err)
	}
	if attrs["prompt"] != "hello world" {
		t.Errorf("prompt = %v, want hello world", attrs["prompt"])
	}
	if attrs["prompt_length"] != float64(len("hello world")) {
		t.Errorf("prompt_length = %v", attrs["prompt_length"])
	}
}

// userArrayContentLine builds a user message whose content is an explicit block
// array (already-marshaled JSON), exercising the array form of message.content.
func userArrayContentLine(sid, ts, uuid, contentJSON string) string {
	return `{"type":"user","uuid":"` + uuid + `","timestamp":"` + ts + `","sessionId":"` + sid +
		`","cwd":"D:\\proj\\myrepo","gitBranch":"main","message":{"role":"user","content":` + contentJSON + `}}`
}

// TestUserArrayTextBlockMappedToPrompt is the I4 regression: a user message whose
// content is an ARRAY containing a text block (user input) must produce a
// user_prompt Event — previously it was a silent no-op (PLAN v3 §9 "조용한 손실
// 금지"). Multiple text blocks on one line each map and get distinct dedup keys.
func TestUserArrayTextBlockMappedToPrompt(t *testing.T) {
	content := `[{"type":"text","text":"first question"},{"type":"text","text":"second question"}]`
	b, st := parse(t, Options{},
		userArrayContentLine("s1", "2026-05-18T12:00:00.000Z", "u-arr-1", content),
	)

	var prompts []model.Event
	for _, e := range b.Events {
		if e.Name == "user_prompt" {
			prompts = append(prompts, e)
		}
	}
	if len(prompts) != 2 {
		t.Fatalf("user_prompt events = %d, want 2 (one per text block)", len(prompts))
	}
	if st.Mapped != 2 {
		t.Errorf("Stats.Mapped = %d, want 2", st.Mapped)
	}
	// Distinct dedup keys (uuid + block index) so the two are not collapsed.
	if prompts[0].DedupKey == prompts[1].DedupKey {
		t.Fatalf("text blocks share a dedup_key (would collapse): %s", prompts[0].DedupKey)
	}
	// Content + length recorded (prompts enabled).
	got := map[string]bool{}
	for _, ev := range prompts {
		attrs := map[string]any{}
		if err := json.Unmarshal([]byte(ev.AttrsJSON), &attrs); err != nil {
			t.Fatalf("attrs not JSON: %v", err)
		}
		if s, ok := attrs["prompt"].(string); ok {
			got[s] = true
		}
	}
	if !got["first question"] || !got["second question"] {
		t.Errorf("array text prompts not recorded: %v", got)
	}
}

// TestUserArrayTextBlockMasked verifies array text-block prompts honor
// NoStorePrompts: the text never reaches attrs, but prompt_length still does.
func TestUserArrayTextBlockMasked(t *testing.T) {
	content := `[{"type":"text","text":"my array secret"}]`
	b, _ := parse(t, Options{NoStorePrompts: true},
		userArrayContentLine("s1", "2026-05-18T12:00:00.000Z", "u-arr-2", content),
	)
	var ev *model.Event
	for i := range b.Events {
		if b.Events[i].Name == "user_prompt" {
			ev = &b.Events[i]
		}
	}
	if ev == nil {
		t.Fatal("no user_prompt event for array text block")
	}
	if strings.Contains(ev.AttrsJSON, "my array secret") {
		t.Errorf("array text prompt leaked under NoStorePrompts: %s", ev.AttrsJSON)
	}
	attrs := map[string]any{}
	_ = json.Unmarshal([]byte(ev.AttrsJSON), &attrs)
	if attrs["prompt_length"] != float64(len("my array secret")) {
		t.Errorf("prompt_length wrong/missing: %v", attrs["prompt_length"])
	}
}

// TestUserArrayMixedTextAndToolResult verifies a single array carrying BOTH a
// text block and a tool_result block yields one user_prompt AND one tool_result
// (no double-count, no loss).
func TestUserArrayMixedTextAndToolResult(t *testing.T) {
	// Buffer a tool_use first so the tool_result joins.
	toolUse := `[{"type":"tool_use","id":"toolu_mix","name":"Bash","input":{"command":"echo hi"}}]`
	content := `[{"type":"text","text":"follow-up note"},{"type":"tool_result","tool_use_id":"toolu_mix","is_error":false,"content":"hi"}]`
	b, _ := parse(t, Options{},
		assistantLine("s1", "2026-05-18T12:00:00.000Z", "claude-opus-4-7", 1, 1, 0, 0, toolUse),
		userArrayContentLine("s1", "2026-05-18T12:00:01.000Z", "u-arr-3", content),
	)

	prompts := 0
	for _, e := range b.Events {
		if e.Name == "user_prompt" {
			prompts++
		}
	}
	if prompts != 1 {
		t.Errorf("user_prompt events = %d, want 1", prompts)
	}
	if len(b.ToolResults) != 1 {
		t.Errorf("ToolResults = %d, want 1", len(b.ToolResults))
	}
	if len(b.ToolResults) == 1 && b.ToolResults[0].BashCommand != "echo hi" {
		t.Errorf("tool_result not joined to tool_use: %+v", b.ToolResults[0])
	}
}

// TestUserLineTwoSameToolResultsBothPreserved is the P2 review regression: a
// single user line (one transcript record, ONE top-level timestamp) carrying TWO
// tool_result blocks of the SAME tool_name (e.g. two parallel Bash runs completing
// together) must yield TWO ToolResults with DISTINCT dedup_keys. Before the fix
// the key was (session + ts + tool_name) for both — identical — so the
// tool_results.dedup_key UNIQUE constraint would drop one and that command history
// was silently lost. The tool_use_id discriminator now keeps them distinct.
func TestUserLineTwoSameToolResultsBothPreserved(t *testing.T) {
	// Two Bash tool_use blocks (distinct ids, distinct commands), then ONE user line
	// completing BOTH at the same instant.
	toolUse := `[{"type":"tool_use","id":"toolu_a","name":"Bash","input":{"command":"echo a"}},` +
		`{"type":"tool_use","id":"toolu_b","name":"Bash","input":{"command":"echo b"}}]`
	results := `[{"type":"tool_result","tool_use_id":"toolu_a","is_error":false,"content":"a"},` +
		`{"type":"tool_result","tool_use_id":"toolu_b","is_error":false,"content":"b"}]`

	b, _ := parse(t, Options{},
		assistantLine("s1", "2026-05-18T14:00:00.000Z", "claude-opus-4-7", 1, 1, 0, 0, toolUse),
		userArrayContentLine("s1", "2026-05-18T14:00:01.000Z", "u-two-tr", results),
	)

	if len(b.ToolResults) != 2 {
		t.Fatalf("ToolResults = %d, want 2 (both same-tool results on one line preserved)", len(b.ToolResults))
	}

	// Distinct dedup_keys: otherwise the store's UNIQUE(dedup_key) drops one.
	if k0, k1 := b.ToolResults[0].DedupKey, b.ToolResults[1].DedupKey; k0 == k1 {
		t.Fatalf("the two same-tool tool_results share a dedup_key %q (one would be dropped by the UNIQUE constraint)", k0)
	}

	// Both bash commands recovered and joined to the right tool_use (the
	// discriminator did not scramble the join).
	got := map[string]bool{}
	for _, tr := range b.ToolResults {
		if tr.ToolName != "Bash" {
			t.Errorf("ToolResult.ToolName = %q, want Bash", tr.ToolName)
		}
		got[tr.BashCommand] = true
	}
	if !got["echo a"] || !got["echo b"] {
		t.Fatalf("expected both bash commands (echo a, echo b), got %v", got)
	}

	// The discriminated key equals the discriminated constructor (ms instant as ns,
	// tool_use_id folded in) — proving the per-result discriminator is the tool_use_id.
	tsMillis := parseTimestamp("2026-05-18T14:00:01.000Z")
	wantA := model.ToolResultDedupKeyWithDiscriminator("s1", strconv.FormatInt(tsMillis*1_000_000, 10), "Bash", "toolu_a")
	foundA := false
	for _, tr := range b.ToolResults {
		if tr.DedupKey == wantA {
			foundA = true
		}
	}
	if !foundA {
		t.Fatalf("no tool_result carries the discriminated key for toolu_a (%q): keys = %q, %q", wantA, b.ToolResults[0].DedupKey, b.ToolResults[1].DedupKey)
	}
}

// TestIncompleteAndUnknownLinesSkipped verifies corrupt/incomplete lines are
// skipped and counted (no silent loss, no fatal), and unknown types produce no
// records but are counted as decoded.
func TestIncompleteAndUnknownLinesSkipped(t *testing.T) {
	good := assistantLine("s1", "2026-05-18T13:00:00.000Z", "claude-opus-4-7", 1, 1, 0, 0, "")
	incomplete := `{"type":"assistant","timestamp":"2026-05-18T13:00:01.000Z","sessionId":"s1","message":{"role":"assist` // truncated
	unknown := `{"type":"file-history-snapshot","sessionId":"s1"}`
	blank := `   `

	b, st := parse(t, Options{}, good, incomplete, unknown, blank)

	if len(b.APIRequests) != 1 {
		t.Errorf("APIRequests = %d, want 1 (only the good line)", len(b.APIRequests))
	}
	if st.Malformed != 1 {
		t.Errorf("Stats.Malformed = %d, want 1 (the truncated line)", st.Malformed)
	}
	// good + incomplete + unknown are non-blank lines (blank is skipped before counting).
	if st.Lines != 3 {
		t.Errorf("Stats.Lines = %d, want 3", st.Lines)
	}
	// decoded = good + unknown (incomplete failed).
	if st.Decoded != 2 {
		t.Errorf("Stats.Decoded = %d, want 2", st.Decoded)
	}
}

// TestUnpairedToolUseNotEmitted verifies a tool_use whose tool_result never
// arrives produces no ToolResult (it will be paired on the next scan once the
// result line lands; emitting a result without its outcome would be misleading).
func TestUnpairedToolUseNotEmitted(t *testing.T) {
	toolUse := `[{"type":"tool_use","id":"toolu_open","name":"Bash","input":{"command":"sleep 1"}}]`
	b, _ := parse(t, Options{},
		assistantLine("s1", "2026-05-18T13:00:00.000Z", "claude-opus-4-7", 1, 1, 0, 0, toolUse),
	)
	if len(b.ToolResults) != 0 {
		t.Errorf("ToolResults = %d, want 0 (unpaired tool_use)", len(b.ToolResults))
	}
	// The api_request for the assistant line is still recorded.
	if len(b.APIRequests) != 1 {
		t.Errorf("APIRequests = %d, want 1", len(b.APIRequests))
	}
}

// TestPendingByteOffset_NoneWhenAllPaired verifies Stats.PendingByteOffset is -1
// when every tool_use was matched by a tool_result (the caller may consume the
// whole buffer).
func TestPendingByteOffset_NoneWhenAllPaired(t *testing.T) {
	toolUse := `[{"type":"tool_use","id":"toolu_p","name":"Bash","input":{"command":"ls"}}]`
	_, st := parse(t, Options{},
		assistantLine("s1", "2026-05-18T13:00:00.000Z", "claude-opus-4-7", 1, 1, 0, 0, toolUse),
		userToolResultLine("s1", "2026-05-18T13:00:01.000Z", "toolu_p", boolPtr(false), "ok"),
	)
	if st.PendingByteOffset != -1 {
		t.Errorf("PendingByteOffset = %d, want -1 (all tool_use paired)", st.PendingByteOffset)
	}
}

// TestPendingByteOffset_PointsAtUnpairedToolUseLine verifies that when a tool_use
// is unpaired, PendingByteOffset is the BYTE OFFSET of the assistant line that
// opened it — so a streaming caller can rewind exactly there to re-pair it with a
// later-arriving tool_result. We build [paired-pair, then an open tool_use] and
// assert the offset equals the start of the open line.
func TestPendingByteOffset_PointsAtUnpairedToolUseLine(t *testing.T) {
	use1 := `[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"a"}}]`
	line0 := assistantLine("s1", "2026-05-18T13:00:00.000Z", "claude-opus-4-7", 1, 1, 0, 0, use1)
	line1 := userToolResultLine("s1", "2026-05-18T13:00:01.000Z", "t1", boolPtr(false), "ok")
	// The open tool_use whose result never arrives in this pass.
	use2 := `[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"b"}}]`
	line2 := assistantLine("s1", "2026-05-18T13:00:02.000Z", "claude-opus-4-7", 1, 1, 0, 0, use2)

	// Build the exact byte stream the parser sees (lines joined with '\n', trailing
	// '\n') so we can compute the offset of line2 independently.
	stream := line0 + "\n" + line1 + "\n" + line2 + "\n"
	wantOffset := int64(len(line0) + 1 + len(line1) + 1) // start byte of line2

	b, st, err := Parse(strings.NewReader(stream), Options{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if st.PendingByteOffset != wantOffset {
		t.Fatalf("PendingByteOffset = %d, want %d (start of the unpaired tool_use line)", st.PendingByteOffset, wantOffset)
	}
	// The paired tool_use (t1) still produced a ToolResult; the open one (t2) did not.
	if len(b.ToolResults) != 1 {
		t.Errorf("ToolResults = %d, want 1 (only the paired one)", len(b.ToolResults))
	}
}

// TestEmptyStreamNoSessions verifies an empty transcript yields an empty batch
// (no synthesized empty session) and no error.
func TestEmptyStreamNoSessions(t *testing.T) {
	b, st := parse(t, Options{}, "")
	if len(b.Sessions) != 0 || len(b.APIRequests) != 0 {
		t.Errorf("empty stream should yield empty batch, got %+v", b)
	}
	if st.Lines != 0 {
		t.Errorf("Stats.Lines = %d, want 0", st.Lines)
	}
}

// --- EQ6: dedup_key unification with the OTLP path -------------------------

// TestDedupKeyMatchesOTLP is the EQ6 gate (PLAN v3 §9): the SAME logical
// api_request, ingested via JSONL and via OTLP, must produce the SAME dedup_key
// so the store's UNIQUE constraint collapses them to one row (no double
// counting). We feed identical (session, instant, model, tokens) through both
// internal/jsonl.Parse and internal/otel.ParseLogs and compare the keys.
//
// C1 REGRESSION: the OTLP LogRecord.TimeUnixNano carries FULL nanosecond
// precision, while the transcript (and thus the JSONL path) only has
// millisecond precision. Real Claude Code OTLP api_request timestamps do NOT
// land on exact ms boundaries, so we inject a sub-ms nanosecond remainder
// (extraNanos below) that the JSONL ms instant cannot have. Before the fix the
// OTLP path hashed the raw nanoseconds and this test FAILED (the two keys
// differed → the same api_request double-counted, doubling SUM(cost_usd)).
// After the fix the OTLP path truncates to ms before hashing, so the keys match
// regardless of the sub-ms remainder. Do NOT pre-align tsNanos to a ms boundary
// here — that is exactly what hid the bug.
func TestDedupKeyMatchesOTLP(t *testing.T) {
	const (
		sid     = "s-eq6"
		mdl     = "claude-opus-4-7"
		in      = int64(6)
		out     = int64(29)
		cacheCr = int64(8228)
		cacheRd = int64(48622)
		tsStr   = "2026-05-18T13:43:39.311Z"
		// A non-zero sub-millisecond remainder (microseconds + nanoseconds) that a
		// millisecond-precision transcript instant can never carry. The fix must
		// make the dedup_key insensitive to it.
		extraNanos = uint64(311777)
	)
	// The instant in unix-ms (what the JSONL parser derives) and the same instant
	// in unix-ns (what OTLP carries in LogRecord.TimeUnixNano) PLUS a sub-ms
	// remainder, mimicking a real high-resolution OTLP timestamp.
	tsMillis := parseTimestamp(tsStr)
	if tsMillis == 0 {
		t.Fatalf("timestamp %q failed to parse", tsStr)
	}
	tsNanos := uint64(tsMillis)*1_000_000 + extraNanos
	if tsNanos%1_000_000 == 0 {
		t.Fatalf("test setup error: tsNanos must NOT be ms-aligned (it is %d) or the regression is hidden", tsNanos)
	}

	// JSONL path.
	jb, _ := parse(t, Options{},
		assistantLine(sid, tsStr, mdl, in, out, cacheCr, cacheRd, ""),
	)
	if len(jb.APIRequests) != 1 {
		t.Fatalf("jsonl APIRequests = %d, want 1", len(jb.APIRequests))
	}
	jKey := jb.APIRequests[0].DedupKey

	// OTLP path: an api_request log record at the SAME instant (to ms) but with a
	// full-nanosecond timestamp, plus the same model + token attributes (Claude
	// Code OTLP attribute spellings).
	op := otel.NewParser(otel.Options{})
	req := &collectorlogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			Resource: &resourcepb.Resource{},
			ScopeLogs: []*logspb.ScopeLogs{{
				LogRecords: []*logspb.LogRecord{{
					TimeUnixNano: tsNanos,
					EventName:    "api_request",
					Attributes: []*commonpb.KeyValue{
						strKV("session.id", sid),
						strKV("model", mdl),
						intKV("input_tokens", in),
						intKV("output_tokens", out),
						intKV("cache_creation_tokens", cacheCr),
						intKV("cache_read_tokens", cacheRd),
					},
				}},
			}},
		}},
	}
	ob, _ := op.ParseLogs(req)
	if len(ob.APIRequests) != 1 {
		t.Fatalf("otel APIRequests = %d, want 1", len(ob.APIRequests))
	}
	oKey := ob.APIRequests[0].DedupKey

	if jKey != oKey {
		t.Fatalf("EQ6 dedup_key MISMATCH (sub-ms ns remainder %d):\n jsonl = %q\n otel  = %q\nThe same api_request would double-count and double SUM(cost_usd).", extraNanos, jKey, oKey)
	}

	// Sanity: the key is exactly what the shared constructor produces when fed the
	// ms-TRUNCATED instant rendered as ns (the contract both paths now honor).
	want := model.APIRequestDedupKey(sid, "api_request",
		strconv.FormatInt(tsMillis*1_000_000, 10), mdl,
		strconv.FormatInt(in, 10), strconv.FormatInt(out, 10))
	if jKey != want {
		t.Fatalf("dedup_key = %q, shared constructor (ms-truncated) = %q", jKey, want)
	}
}

// TestToolResultDedupKeyMatchesOTLP covers the tool_result ms-resolution
// invariant (the original P2 fix) AND the new tool_use_id discriminator (the 2nd
// P2 review item).
//
// Cross-source design note: Claude Code's OTLP tool_result carries NO tool_use_id
// (its attributes are session.id/tool_name/success/duration_ms/tool_parameters
// only), so the JSONL parser now discriminates its tool_result dedup_key by
// tool_use_id to prevent intra-line loss (two same-tool results sharing one
// transcript line's timestamp would otherwise collide and the UNIQUE constraint
// would drop one). This is a deliberate JSONL-internal key: a discriminated JSONL
// tool_result no longer collapses with the OTLP record for the same run. That is
// acceptable because tool_result is command-history DISPLAY only and carries no
// cost/token (api_request — the cost-accuracy axis — keeps its cross-source EQ6
// key, see TestAPIRequestDedupKeyMatchesOTLP).
//
// What MUST still hold is the ms-resolution agreement: both paths truncate the
// instant to milliseconds before hashing, so the JSONL FALLBACK key (used when a
// tool_result has no tool_use_id) equals the OTLP key at the same ms instant even
// when the OTLP record carries a sub-ms nanosecond remainder. A sub-ms ns
// remainder is injected to lock in that regression (do NOT ms-align it).
func TestToolResultDedupKeyMatchesOTLP(t *testing.T) {
	const (
		sid      = "s-eq6-tr"
		toolName = "Bash"
		tsStr    = "2026-05-18T13:43:41.512Z"
		// Sub-ms remainder a millisecond-precision transcript can never carry.
		extraNanos = uint64(512613)
	)
	tsMillis := parseTimestamp(tsStr)
	if tsMillis == 0 {
		t.Fatalf("timestamp %q failed to parse", tsStr)
	}
	tsNanos := uint64(tsMillis)*1_000_000 + extraNanos
	if tsNanos%1_000_000 == 0 {
		t.Fatalf("test setup error: tsNanos must NOT be ms-aligned (it is %d)", tsNanos)
	}

	// JSONL path: a Bash tool_use (assistant) joined to its tool_result (user) at
	// the SAME instant the OTLP record uses (to ms). The tool_result carries a
	// tool_use_id, so the JSONL key is the DISCRIMINATED key.
	toolUse := `[{"type":"tool_use","id":"toolu_eq6","name":"Bash","input":{"command":"go test ./..."}}]`
	jb, _ := parse(t, Options{},
		assistantLine(sid, "2026-05-18T13:43:40.000Z", "claude-opus-4-7", 1, 1, 0, 0, toolUse),
		userToolResultLine(sid, tsStr, "toolu_eq6", boolPtr(false), "PASS"),
	)
	if len(jb.ToolResults) != 1 {
		t.Fatalf("jsonl ToolResults = %d, want 1", len(jb.ToolResults))
	}
	jKey := jb.ToolResults[0].DedupKey

	// OTLP path: a tool_result log record at the SAME instant but full-nanosecond.
	op := otel.NewParser(otel.Options{})
	req := &collectorlogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			Resource: &resourcepb.Resource{},
			ScopeLogs: []*logspb.ScopeLogs{{
				LogRecords: []*logspb.LogRecord{{
					TimeUnixNano: tsNanos,
					EventName:    "claude_code.tool_result",
					Attributes: []*commonpb.KeyValue{
						strKV("session.id", sid),
						strKV("tool_name", toolName),
						strKV("success", "true"),
					},
				}},
			}},
		}},
	}
	ob, _ := op.ParseLogs(req)
	if len(ob.ToolResults) != 1 {
		t.Fatalf("otel ToolResults = %d, want 1", len(ob.ToolResults))
	}
	oKey := ob.ToolResults[0].DedupKey

	// The OTLP key equals the shared (non-discriminated) constructor fed the
	// ms-truncated instant: this is the ms-resolution invariant the P2 fix added.
	wantBase := model.ToolResultDedupKey(sid, strconv.FormatInt(tsMillis*1_000_000, 10), toolName)
	if oKey != wantBase {
		t.Fatalf("OTLP tool_result dedup_key not ms-truncated (sub-ms ns remainder %d): got %q, want %q", extraNanos, oKey, wantBase)
	}

	// The JSONL key is the DISCRIMINATED key (tool_use_id folded in). It MUST equal
	// the discriminated constructor at the same ms instant — proving the JSONL path
	// truncates to ms exactly like OTLP (so the discriminator, not a ns mismatch, is
	// the only difference) — and MUST DIFFER from the OTLP base key (the deliberate,
	// documented cross-source divergence for a discriminated tool_result).
	wantDisc := model.ToolResultDedupKeyWithDiscriminator(sid, strconv.FormatInt(tsMillis*1_000_000, 10), toolName, "toolu_eq6")
	if jKey != wantDisc {
		t.Fatalf("JSONL tool_result dedup_key = %q, discriminated constructor (ms-truncated) = %q", jKey, wantDisc)
	}
	if jKey == oKey {
		t.Fatalf("discriminated JSONL key unexpectedly equals OTLP base key (%q): the discriminator must make it distinct", jKey)
	}
}

// TestToolDecisionDedupKeyMsResolution covers tool_decision (P2). The transcript
// carries no permission decisions (EQ5), so there is no JSONL counterpart to
// cross-check; instead we assert the OTLP path now hashes the MILLISECOND-
// truncated instant (rendered ms→ns the same way every other record does), so a
// re-sent tool_decision whose raw ns differ only in the sub-ms remainder still
// collapses to one row and matches what the shared constructor produces for the
// ms instant. Before the fix it hashed raw nanoseconds.
func TestToolDecisionDedupKeyMsResolution(t *testing.T) {
	const (
		sid      = "s-eq6-td"
		toolName = "Edit"
		decision = "accept"
		source   = "config"
		tsStr    = "2026-05-18T13:43:42.777Z"
	)
	tsMillis := parseTimestamp(tsStr)
	tsNanos := uint64(tsMillis)*1_000_000 + 777321 // sub-ms remainder

	op := otel.NewParser(otel.Options{})
	mk := func(ns uint64) string {
		req := &collectorlogspb.ExportLogsServiceRequest{
			ResourceLogs: []*logspb.ResourceLogs{{
				Resource: &resourcepb.Resource{},
				ScopeLogs: []*logspb.ScopeLogs{{
					LogRecords: []*logspb.LogRecord{{
						TimeUnixNano: ns,
						EventName:    "claude_code.tool_decision",
						Attributes: []*commonpb.KeyValue{
							strKV("session.id", sid),
							strKV("tool_name", toolName),
							strKV("decision", decision),
							strKV("source", source),
						},
					}},
				}},
			}},
		}
		b, _ := op.ParseLogs(req)
		if len(b.ToolDecisions) != 1 {
			t.Fatalf("otel ToolDecisions = %d, want 1", len(b.ToolDecisions))
		}
		return b.ToolDecisions[0].DedupKey
	}

	// Two records of the same logical decision whose ns differ only below the ms
	// boundary must hash to the SAME key now (ms-truncated), defeating duplicates.
	if a, b := mk(tsNanos), mk(uint64(tsMillis)*1_000_000); a != b {
		t.Fatalf("tool_decision dedup_key not ms-resolved: sub-ms %q != ms-aligned %q", a, b)
	}
	// And it equals the shared constructor fed the ms instant rendered as ns.
	want := model.ToolDecisionDedupKey(sid, strconv.FormatInt(tsMillis*1_000_000, 10), toolName, decision, source)
	if got := mk(tsNanos); got != want {
		t.Fatalf("tool_decision dedup_key = %q, shared constructor (ms) = %q", got, want)
	}
}

// strKV / intKV build OTLP attributes for the EQ6 cross-check (kept local so the
// jsonl test package does not depend on otel's internal test helpers).
func strKV(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}
func intKV(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

// approx compares two USD figures within a small epsilon.
func approx(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
