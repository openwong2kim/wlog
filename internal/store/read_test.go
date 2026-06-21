package store

import (
	"context"
	"testing"

	"github.com/openwong2kim/wlog/internal/model"
)

// seedSession is a helper to upsert a single session with the given fields.
func seedSession(t *testing.T, st *Store, s model.Session) {
	t.Helper()
	if err := st.WriteBatch(context.Background(), model.Batch{Sessions: []model.Session{s}}); err != nil {
		t.Fatalf("seed session %q: %v", s.ID, err)
	}
}

// TestLatestSessionID covers the empty-DB case and recency ordering.
func TestLatestSessionID(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()

	// Empty DB: "" with no error.
	id, err := st.LatestSessionID(ctx)
	if err != nil {
		t.Fatalf("LatestSessionID(empty): %v", err)
	}
	if id != "" {
		t.Errorf("LatestSessionID(empty): got %q, want \"\"", id)
	}

	// Insert three sessions with distinct last_seen; the max wins.
	seedSession(t, st, model.Session{ID: "old", FirstSeen: 100, LastSeen: 1000})
	seedSession(t, st, model.Session{ID: "newest", FirstSeen: 200, LastSeen: 9000})
	seedSession(t, st, model.Session{ID: "mid", FirstSeen: 300, LastSeen: 5000})

	id, err = st.LatestSessionID(ctx)
	if err != nil {
		t.Fatalf("LatestSessionID: %v", err)
	}
	if id != "newest" {
		t.Errorf("LatestSessionID: got %q, want \"newest\"", id)
	}

	// Tie on last_seen breaks by id ASC (deterministic). A separate file-backed
	// store is required for isolation: every in-memory store in this process shares
	// one cache=shared database, so a second openMem would see the rows above.
	st2, _ := openFile(t)
	seedSession(t, st2, model.Session{ID: "b", FirstSeen: 1, LastSeen: 7000})
	seedSession(t, st2, model.Session{ID: "a", FirstSeen: 1, LastSeen: 7000})
	id, err = st2.LatestSessionID(ctx)
	if err != nil {
		t.Fatalf("LatestSessionID(tie): %v", err)
	}
	if id != "a" {
		t.Errorf("LatestSessionID(tie): got %q, want \"a\" (id ASC tiebreak)", id)
	}
}

// TestSessionSummaryEvents verifies the full header aggregate sourced from
// api_request events: cost sum, four token sums, cache_hit_ratio, accept/reject,
// auto_approved (config|hook accepts), repo/git_branch, model_set, token_source.
func TestSessionSummaryEvents(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()

	const sid = "s-events"
	b := model.Batch{
		Sessions: []model.Session{{
			ID: sid, FirstSeen: 1000, LastSeen: 5000,
			ModelSet: "claude-sonnet", TokenSource: "events",
			Repo: "wlog", GitBranch: "main",
		}},
		APIRequests: []model.APIRequest{
			{SessionID: sid, TS: 1500, Model: "claude-sonnet",
				Tokens:  model.Tokens{Input: 100, Output: 10, CacheRead: 300, CacheCreation: 5},
				CostUSD: 0.20, DedupKey: "ar1"},
			{SessionID: sid, TS: 2500, Model: "claude-sonnet",
				Tokens:  model.Tokens{Input: 28, Output: 2, CacheRead: 100, CacheCreation: 0},
				CostUSD: 0.22, DedupKey: "ar2"},
		},
		ToolDecisions: []model.ToolDecision{
			// 3 accepts (2 auto via config/hook, 1 user) + 2 rejects.
			{SessionID: sid, TS: 1600, Decision: "accept", Source: "config", ToolName: "Read", DedupKey: "d1"},
			{SessionID: sid, TS: 1700, Decision: "accept", Source: "hook", ToolName: "Edit", DedupKey: "d2"},
			{SessionID: sid, TS: 1800, Decision: "accept", Source: "user", ToolName: "Bash", DedupKey: "d3"},
			{SessionID: sid, TS: 1900, Decision: "reject", Source: "user_reject", ToolName: "Bash", DedupKey: "d4"},
			{SessionID: sid, TS: 2000, Decision: "reject", Source: "user_reject", ToolName: "Bash", DedupKey: "d5"},
		},
	}
	if err := st.WriteBatch(ctx, b); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	sm, found, err := st.SessionSummary(ctx, sid)
	if err != nil {
		t.Fatalf("SessionSummary: %v", err)
	}
	if !found {
		t.Fatal("SessionSummary: found=false, want true")
	}

	if sm.ID != sid {
		t.Errorf("ID: got %q, want %q", sm.ID, sid)
	}
	if sm.FirstSeen != 1000 || sm.LastSeen != 5000 {
		t.Errorf("first/last: got %d/%d, want 1000/5000", sm.FirstSeen, sm.LastSeen)
	}
	if !floatEq(sm.CostUSD, 0.42) {
		t.Errorf("CostUSD: got %v, want 0.42", sm.CostUSD)
	}
	wantTok := model.Tokens{Input: 128, Output: 12, CacheRead: 400, CacheCreation: 5}
	if sm.Tokens != wantTok {
		t.Errorf("Tokens: got %+v, want %+v", sm.Tokens, wantTok)
	}
	// cache_hit = cache_read/(input+cache_read) = 400/(128+400) = 0.7575...
	if want := 400.0 / 528.0; !floatEq(sm.CacheHitRatio, want) {
		t.Errorf("CacheHitRatio: got %v, want %v", sm.CacheHitRatio, want)
	}
	if sm.ToolAccept != 3 {
		t.Errorf("ToolAccept: got %d, want 3", sm.ToolAccept)
	}
	if sm.ToolReject != 2 {
		t.Errorf("ToolReject: got %d, want 2", sm.ToolReject)
	}
	if sm.AutoApproved != 2 {
		t.Errorf("AutoApproved: got %d, want 2 (config+hook accepts)", sm.AutoApproved)
	}
	if sm.Repo != "wlog" || sm.GitBranch != "main" {
		t.Errorf("repo/branch: got %q/%q, want wlog/main", sm.Repo, sm.GitBranch)
	}
	if sm.ModelSet != "claude-sonnet" {
		t.Errorf("ModelSet: got %q, want claude-sonnet", sm.ModelSet)
	}
	if sm.TokenSource != "events" {
		t.Errorf("TokenSource: got %q, want events", sm.TokenSource)
	}
}

// TestSessionSummaryMetricsFallback verifies that a session with NO api_request
// falls back to the token.usage / cost.usage metric deltas (axis-of-truth), and
// is labeled token_source="metrics".
func TestSessionSummaryMetricsFallback(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()

	const sid = "s-metrics"
	mp := func(name, attr string, v float64, sk string) model.MetricPoint {
		return model.MetricPoint{
			SessionID: sid, TS: 1500, Name: name, AttrKey: attr, ValueDelta: v,
			ValueKind: 1, SeriesKey: sk, StartUnixNano: 1, TimeUnixNano: 2,
		}
	}
	b := model.Batch{
		Sessions: []model.Session{{ID: sid, FirstSeen: 1000, LastSeen: 3000}},
		MetricPoints: []model.MetricPoint{
			mp("claude_code.cost.usage", "claude-3", 1.50, "sk-cost"),
			mp("claude_code.token.usage", "input", 200, "sk-in"),
			mp("claude_code.token.usage", "output", 40, "sk-out"),
			mp("claude_code.token.usage", "cacheRead", 600, "sk-cr"),
			mp("claude_code.token.usage", "cacheCreation", 10, "sk-cc"),
		},
	}
	if err := st.WriteBatch(ctx, b); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	sm, found, err := st.SessionSummary(ctx, sid)
	if err != nil || !found {
		t.Fatalf("SessionSummary: err=%v found=%v", err, found)
	}
	if !floatEq(sm.CostUSD, 1.50) {
		t.Errorf("CostUSD(metrics): got %v, want 1.50", sm.CostUSD)
	}
	wantTok := model.Tokens{Input: 200, Output: 40, CacheRead: 600, CacheCreation: 10}
	if sm.Tokens != wantTok {
		t.Errorf("Tokens(metrics): got %+v, want %+v", sm.Tokens, wantTok)
	}
	if sm.TokenSource != "metrics" {
		t.Errorf("TokenSource: got %q, want metrics", sm.TokenSource)
	}
	if want := 600.0 / 800.0; !floatEq(sm.CacheHitRatio, want) {
		t.Errorf("CacheHitRatio(metrics): got %v, want %v", sm.CacheHitRatio, want)
	}
}

// TestSessionSummaryNotFound: a missing id is found=false, not an error.
func TestSessionSummaryNotFound(t *testing.T) {
	st := openMem(t)
	_, found, err := st.SessionSummary(context.Background(), "ghost")
	if err != nil {
		t.Fatalf("SessionSummary(ghost): unexpected error %v", err)
	}
	if found {
		t.Error("SessionSummary(ghost): found=true, want false")
	}
}

// TestSessionSummaryAxisNoDoubleCount confirms that when a session has BOTH
// api_requests and token/cost metric points (real Claude Code emits both), the
// summary uses the events axis ONLY — metrics are not added on top.
func TestSessionSummaryAxisNoDoubleCount(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()

	const sid = "s-mixed"
	b := model.Batch{
		Sessions: []model.Session{{ID: sid, FirstSeen: 1000, LastSeen: 2000}},
		APIRequests: []model.APIRequest{{
			SessionID: sid, TS: 1500, Model: "m",
			Tokens: model.Tokens{Input: 100, Output: 10}, CostUSD: 0.5, DedupKey: "ar1",
		}},
		// These metric points would double the totals if (incorrectly) summed.
		MetricPoints: []model.MetricPoint{
			{SessionID: sid, TS: 1500, Name: "claude_code.cost.usage", AttrKey: "m",
				ValueDelta: 99, ValueKind: 1, SeriesKey: "sk1", StartUnixNano: 1, TimeUnixNano: 2},
			{SessionID: sid, TS: 1500, Name: "claude_code.token.usage", AttrKey: "input",
				ValueDelta: 9999, ValueKind: 1, SeriesKey: "sk2", StartUnixNano: 1, TimeUnixNano: 2},
		},
	}
	if err := st.WriteBatch(ctx, b); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}
	sm, _, err := st.SessionSummary(ctx, sid)
	if err != nil {
		t.Fatalf("SessionSummary: %v", err)
	}
	if !floatEq(sm.CostUSD, 0.5) {
		t.Errorf("CostUSD: got %v, want 0.5 (events axis, metrics ignored)", sm.CostUSD)
	}
	if sm.Tokens.Input != 100 {
		t.Errorf("Input: got %d, want 100 (events axis)", sm.Tokens.Input)
	}
	if sm.TokenSource != "events" {
		t.Errorf("TokenSource: got %q, want events", sm.TokenSource)
	}
}

// TestToolDecisionsBySession covers ts-ascending order and the onlyReject filter.
func TestToolDecisionsBySession(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()

	const sid = "s1"
	b := model.Batch{
		Sessions: []model.Session{{ID: sid, FirstSeen: 100, LastSeen: 900}},
		ToolDecisions: []model.ToolDecision{
			// Inserted out of order; query must return ts ASC.
			{SessionID: sid, TS: 300, Decision: "reject", Source: "user_reject", ToolName: "Bash", DedupKey: "d3"},
			{SessionID: sid, TS: 100, Decision: "accept", Source: "config", ToolName: "Read", DedupKey: "d1"},
			{SessionID: sid, TS: 200, Decision: "accept", Source: "user", ToolName: "Edit", DedupKey: "d2"},
			{SessionID: sid, TS: 400, Decision: "reject", Source: "user_reject", ToolName: "Write", DedupKey: "d4"},
		},
	}
	if err := st.WriteBatch(ctx, b); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	all, err := st.ToolDecisionsBySession(ctx, sid, false)
	if err != nil {
		t.Fatalf("ToolDecisionsBySession(all): %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("all decisions: got %d, want 4", len(all))
	}
	wantTS := []int64{100, 200, 300, 400}
	for i, d := range all {
		if d.TS != wantTS[i] {
			t.Errorf("decision[%d].TS: got %d, want %d (ts ASC)", i, d.TS, wantTS[i])
		}
	}

	rejects, err := st.ToolDecisionsBySession(ctx, sid, true)
	if err != nil {
		t.Fatalf("ToolDecisionsBySession(reject): %v", err)
	}
	if len(rejects) != 2 {
		t.Fatalf("rejects: got %d, want 2", len(rejects))
	}
	for _, d := range rejects {
		if d.Decision != "reject" {
			t.Errorf("reject filter leaked decision=%q", d.Decision)
		}
	}
	if rejects[0].TS != 300 || rejects[1].TS != 400 {
		t.Errorf("reject order: got %d,%d want 300,400", rejects[0].TS, rejects[1].TS)
	}
	if rejects[0].ToolName != "Bash" || rejects[0].Source != "user_reject" {
		t.Errorf("reject[0] fields: got tool=%q source=%q", rejects[0].ToolName, rejects[0].Source)
	}

	// Unknown session: empty (non-nil) slice, no error.
	empty, err := st.ToolDecisionsBySession(ctx, "ghost", false)
	if err != nil {
		t.Fatalf("ToolDecisionsBySession(ghost): %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("ghost decisions: got %v, want empty non-nil slice", empty)
	}
}

// TestToolResultsBySession covers the limit (newest N) + ascending output order
// and the inclusion of bash_command / mcp_server.
func TestToolResultsBySession(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()

	const sid = "s1"
	rs := []model.ToolResult{}
	// Five results at ts 100..500.
	for i := 1; i <= 5; i++ {
		rs = append(rs, model.ToolResult{
			SessionID: sid, TS: int64(i * 100), ToolName: "Bash", Success: i%2 == 1,
			DurationMS: int64(i), BashCommand: cmdFor(i), DedupKey: keyFor(i),
		})
	}
	// One MCP result to confirm mcp_server passes through.
	rs = append(rs, model.ToolResult{
		SessionID: sid, TS: 600, ToolName: "mcp__server__tool", Success: true,
		MCPServer: "server", DedupKey: "r-mcp",
	})
	b := model.Batch{
		Sessions:    []model.Session{{ID: sid, FirstSeen: 100, LastSeen: 600}},
		ToolResults: rs,
	}
	if err := st.WriteBatch(ctx, b); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	// limit=3 selects the newest three (ts 400,500,600) but returns them ASC.
	got, err := st.ToolResultsBySession(ctx, sid, 3)
	if err != nil {
		t.Fatalf("ToolResultsBySession(limit=3): %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("limit=3: got %d rows, want 3", len(got))
	}
	wantTS := []int64{400, 500, 600}
	for i, r := range got {
		if r.TS != wantTS[i] {
			t.Errorf("result[%d].TS: got %d, want %d (newest-3, ASC)", i, r.TS, wantTS[i])
		}
	}
	// The MCP row is the last (newest); its mcp_server is preserved and bash empty.
	last := got[len(got)-1]
	if last.MCPServer != "server" {
		t.Errorf("mcp_server: got %q, want server", last.MCPServer)
	}
	if last.BashCommand != "" {
		t.Errorf("mcp row bash_command: got %q, want empty", last.BashCommand)
	}
	// A bash row carries its command + success.
	if got[0].BashCommand != cmdFor(4) {
		t.Errorf("result[0].BashCommand: got %q, want %q", got[0].BashCommand, cmdFor(4))
	}

	// limit<=0 returns all six, ASC.
	all, err := st.ToolResultsBySession(ctx, sid, 0)
	if err != nil {
		t.Fatalf("ToolResultsBySession(0): %v", err)
	}
	if len(all) != 6 {
		t.Fatalf("limit=0: got %d, want 6 (all)", len(all))
	}
	if all[0].TS != 100 || all[5].TS != 600 {
		t.Errorf("all order: first/last TS %d/%d, want 100/600", all[0].TS, all[5].TS)
	}

	// Unknown session: empty non-nil slice.
	empty, err := st.ToolResultsBySession(ctx, "ghost", 10)
	if err != nil {
		t.Fatalf("ToolResultsBySession(ghost): %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("ghost results: got %v, want empty non-nil", empty)
	}
}

// TestUserPromptsBySession covers the limit (newest N) + ascending output order
// and the attrs_json decode of prompt text/length, including the
// --no-store-prompts case (length present, text absent → Stored=false).
func TestUserPromptsBySession(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()

	const sid = "s-prompts"
	ev := func(ts int64, attrs, key string) model.Event {
		return model.Event{
			SessionID: sid, TS: ts, Name: "user_prompt",
			DedupKey: key, AttrsJSON: attrs,
		}
	}
	b := model.Batch{
		Sessions: []model.Session{{ID: sid, FirstSeen: 100, LastSeen: 500}},
		Events: []model.Event{
			// Inserted out of order; query returns ts ASC. Mix stored + not-stored.
			ev(300, `{"prompt_length":11,"prompt":"third prompt"}`, "p3"),
			ev(100, `{"prompt_length":12,"prompt":"first prompt"}`, "p1"),
			ev(200, `{"prompt_length":42}`, "p2"), // --no-store-prompts: no text
			ev(400, `{"prompt_length":13,"prompt":"fourth prompt"}`, "p4"),
			// A non-prompt event must be excluded.
			{SessionID: sid, TS: 250, Name: "api_error", DedupKey: "e1", AttrsJSON: `{"status_code":429}`},
		},
	}
	if err := st.WriteBatch(ctx, b); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	// limit=3 selects the newest three (ts 200,300,400) but returns them ASC.
	got, err := st.UserPromptsBySession(ctx, sid, 3)
	if err != nil {
		t.Fatalf("UserPromptsBySession(limit=3): %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("limit=3: got %d rows, want 3", len(got))
	}
	wantTS := []int64{200, 300, 400}
	for i, p := range got {
		if p.TS != wantTS[i] {
			t.Errorf("prompt[%d].TS: got %d, want %d (newest-3, ASC)", i, p.TS, wantTS[i])
		}
	}
	// The ts=200 row is the --no-store-prompts one: length parsed, text empty.
	if got[0].Stored {
		t.Errorf("prompt[0] should be not-stored (Stored=false): %+v", got[0])
	}
	if got[0].Text != "" {
		t.Errorf("prompt[0].Text: got %q, want empty (not stored)", got[0].Text)
	}
	if got[0].Length != 42 {
		t.Errorf("prompt[0].Length: got %d, want 42", got[0].Length)
	}
	// A stored row carries its text + Stored=true.
	if !got[1].Stored || got[1].Text != "third prompt" || got[1].Length != 11 {
		t.Errorf("prompt[1] stored fields wrong: %+v", got[1])
	}

	// limit<=0 returns all four prompts (api_error excluded), ASC.
	all, err := st.UserPromptsBySession(ctx, sid, 0)
	if err != nil {
		t.Fatalf("UserPromptsBySession(0): %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("limit=0: got %d, want 4 (prompts only)", len(all))
	}
	if all[0].TS != 100 || all[3].TS != 400 {
		t.Errorf("all order: first/last TS %d/%d, want 100/400", all[0].TS, all[3].TS)
	}
	if all[0].Text != "first prompt" {
		t.Errorf("all[0].Text: got %q, want %q", all[0].Text, "first prompt")
	}

	// Unknown session: empty non-nil slice, no error.
	empty, err := st.UserPromptsBySession(ctx, "ghost", 10)
	if err != nil {
		t.Fatalf("UserPromptsBySession(ghost): %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("ghost prompts: got %v, want empty non-nil", empty)
	}
}

// TestSessionSnapshot verifies the lightweight statusline view: cost, tokens,
// cache hit, reject count, and the most-recent tool_result name.
func TestSessionSnapshot(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()

	const sid = "s1"
	b := model.Batch{
		Sessions: []model.Session{{ID: sid, FirstSeen: 1000, LastSeen: 5000}},
		APIRequests: []model.APIRequest{{
			SessionID: sid, TS: 1500, Model: "m",
			Tokens:  model.Tokens{Input: 100, Output: 10, CacheRead: 300},
			CostUSD: 0.42, DedupKey: "ar1",
		}},
		ToolDecisions: []model.ToolDecision{
			{SessionID: sid, TS: 1600, Decision: "reject", Source: "user_reject", ToolName: "Bash", DedupKey: "d1"},
			{SessionID: sid, TS: 1700, Decision: "reject", Source: "user_reject", ToolName: "Bash", DedupKey: "d2"},
		},
		ToolResults: []model.ToolResult{
			{SessionID: sid, TS: 1800, ToolName: "Read", Success: true, DedupKey: "r1"},
			{SessionID: sid, TS: 2000, ToolName: "Bash", Success: true, DedupKey: "r2"}, // most recent
			{SessionID: sid, TS: 1900, ToolName: "Edit", Success: true, DedupKey: "r3"},
		},
	}
	if err := st.WriteBatch(ctx, b); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	snap, found, err := st.SessionSnapshot(ctx, sid)
	if err != nil || !found {
		t.Fatalf("SessionSnapshot: err=%v found=%v", err, found)
	}
	if snap.ID != sid {
		t.Errorf("ID: got %q, want %q", snap.ID, sid)
	}
	if !floatEq(snap.CostUSD, 0.42) {
		t.Errorf("CostUSD: got %v, want 0.42", snap.CostUSD)
	}
	if snap.Tokens.Input != 100 || snap.Tokens.CacheRead != 300 {
		t.Errorf("Tokens: got %+v", snap.Tokens)
	}
	if want := 300.0 / 400.0; !floatEq(snap.CacheHitRatio, want) {
		t.Errorf("CacheHitRatio: got %v, want %v", snap.CacheHitRatio, want)
	}
	if snap.ToolReject != 2 {
		t.Errorf("ToolReject: got %d, want 2", snap.ToolReject)
	}
	if snap.LastTool != "Bash" {
		t.Errorf("LastTool: got %q, want Bash (most recent by ts)", snap.LastTool)
	}
}

// TestSessionSnapshotNoTool: a session with no tool_result has LastTool="".
func TestSessionSnapshotNoTool(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	const sid = "s1"
	seedSession(t, st, model.Session{ID: sid, FirstSeen: 1, LastSeen: 2})

	snap, found, err := st.SessionSnapshot(ctx, sid)
	if err != nil || !found {
		t.Fatalf("SessionSnapshot: err=%v found=%v", err, found)
	}
	if snap.LastTool != "" {
		t.Errorf("LastTool: got %q, want empty", snap.LastTool)
	}

	// Unknown session is found=false, no error.
	_, found, err = st.SessionSnapshot(ctx, "ghost")
	if err != nil {
		t.Fatalf("SessionSnapshot(ghost): %v", err)
	}
	if found {
		t.Error("SessionSnapshot(ghost): found=true, want false")
	}
}

// TestSessionMergeRepoBranch is the batch-upsert golden for the repo/git_branch
// merge: a non-empty value must never be overwritten by a later empty one, and an
// empty initial value must be filled by a later non-empty one, regardless of
// delivery order (PLAN v3 §3, mirrors token_source merge).
func TestSessionMergeRepoBranch(t *testing.T) {
	ctx := context.Background()

	read := func(st *Store, sid string) (repo, branch string) {
		t.Helper()
		row := st.ReadDB().QueryRowContext(ctx,
			"SELECT COALESCE(repo,''), COALESCE(git_branch,'') FROM sessions WHERE id = ?", sid)
		if err := row.Scan(&repo, &branch); err != nil {
			t.Fatalf("read repo/branch: %v", err)
		}
		return repo, branch
	}

	t.Run("non-empty then empty does not clobber", func(t *testing.T) {
		st, _ := openFile(t)
		const sid = "s1"
		// First: JSONL fills repo/branch.
		seedSession(t, st, model.Session{ID: sid, FirstSeen: 1, LastSeen: 2, Repo: "wlog", GitBranch: "main"})
		// Later: an OTLP-sourced upsert with empty repo/branch must not erase them.
		seedSession(t, st, model.Session{ID: sid, FirstSeen: 1, LastSeen: 3, Repo: "", GitBranch: ""})
		repo, branch := read(st, sid)
		if repo != "wlog" || branch != "main" {
			t.Errorf("after empty upsert: repo=%q branch=%q, want wlog/main", repo, branch)
		}
	})

	t.Run("empty then non-empty fills", func(t *testing.T) {
		st, _ := openFile(t)
		const sid = "s2"
		// First: OTLP creates the session with no repo/branch.
		seedSession(t, st, model.Session{ID: sid, FirstSeen: 1, LastSeen: 2})
		// Later: JSONL supplies them.
		seedSession(t, st, model.Session{ID: sid, FirstSeen: 1, LastSeen: 3, Repo: "wlog", GitBranch: "dev"})
		repo, branch := read(st, sid)
		if repo != "wlog" || branch != "dev" {
			t.Errorf("after fill upsert: repo=%q branch=%q, want wlog/dev", repo, branch)
		}
	})

	t.Run("non-empty replaced by different non-empty", func(t *testing.T) {
		st, _ := openFile(t)
		const sid = "s3"
		seedSession(t, st, model.Session{ID: sid, FirstSeen: 1, LastSeen: 2, Repo: "old", GitBranch: "main"})
		seedSession(t, st, model.Session{ID: sid, FirstSeen: 1, LastSeen: 3, Repo: "new", GitBranch: "feature"})
		repo, branch := read(st, sid)
		if repo != "new" || branch != "feature" {
			t.Errorf("non-empty overwrite: repo=%q branch=%q, want new/feature", repo, branch)
		}
	})
}

// --- small local test helpers ---

func cmdFor(i int) string {
	return map[int]string{1: "npm ci", 2: "go test ./...", 3: "go build", 4: "ls -la", 5: "git status"}[i]
}
func keyFor(i int) string {
	return map[int]string{1: "r1", 2: "r2", 3: "r3", 4: "r4", 5: "r5"}[i]
}

// floatEq compares floats with a small tolerance (cost/ratio arithmetic).
func floatEq(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}
