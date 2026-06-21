package app

import (
	"context"
	"strings"
	"testing"

	"github.com/openwong2kim/wlog/internal/config"
	"github.com/openwong2kim/wlog/internal/model"
	"github.com/openwong2kim/wlog/internal/store"
)

// --- format helper unit tests ------------------------------------------------

func TestTokensHuman(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{42, "42"},
		{999, "999"},
		{1420, "1.4K"},
		{1000, "1K"},
		{128_400, "128.4K"},
		{12_100, "12.1K"},
		{1_000_000, "1M"},
		{1_200_000, "1.2M"},
		{-5, "0"}, // negative clamps to 0
	}
	for _, c := range cases {
		if got := tokensHuman(c.in); got != c.want {
			t.Errorf("tokensHuman(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestGroupInt(t *testing.T) {
	cases := map[int64]string{0: "0", 1: "1", 12: "12", 123: "123", 1234: "1,234", 12345: "12,345", 123456: "123,456", 1234567: "1,234,567"}
	for in, want := range cases {
		if got := groupInt(in); got != want {
			t.Errorf("groupInt(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestPctAndCost2(t *testing.T) {
	if got := pct(0.71); got != "71%" {
		t.Errorf("pct(0.71) = %q, want 71%%", got)
	}
	if got := pct(0); got != "0%" {
		t.Errorf("pct(0) = %q, want 0%%", got)
	}
	if got := pct(1); got != "100%" {
		t.Errorf("pct(1) = %q, want 100%%", got)
	}
	if got := pct(0.715); got != "72%" { // rounds
		t.Errorf("pct(0.715) = %q, want 72%%", got)
	}
	if got := cost2(0.42); got != "0.42" {
		t.Errorf("cost2(0.42) = %q, want 0.42", got)
	}
	if got := cost2(-1); got != "0.00" {
		t.Errorf("cost2(-1) = %q, want 0.00", got)
	}
}

func TestShortID(t *testing.T) {
	if got := shortID("0a1b2c3d4e5f"); got != "0a1b2c3d" {
		t.Errorf("shortID long = %q, want 0a1b2c3d", got)
	}
	if got := shortID("short"); got != "short" {
		t.Errorf("shortID short = %q, want short", got)
	}
}

// --- writeLastReport golden (pure, no os.Stdout) -----------------------------

// summaryFixture builds a representative SessionSummary for the golden tests
// using the same numbers as the PLAN v3 §1 example (cost $0.42, in 128.4K, out
// 12.1K, cache 71%).
func summaryFixture() store.SessionSummary {
	return store.SessionSummary{
		ID:            "0a1b2c3d4e5f6789",
		FirstSeen:     ts("18:42:00"),
		LastSeen:      ts("19:05:00"),
		CostUSD:       0.42,
		Tokens:        model.Tokens{Input: 128_400, Output: 12_100, CacheRead: 314_000},
		CacheHitRatio: 0.71,
		ToolAccept:    34,
		ToolReject:    2,
		AutoApproved:  28,
		TokenSource:   "events",
	}
}

func TestWriteLastReport_NoColorGolden(t *testing.T) {
	sm := summaryFixture()
	rejects := []model.ToolDecision{
		{SessionID: sm.ID, TS: ts("18:45:00"), Decision: "reject", Source: "user_reject", ToolName: "Bash", DedupKey: "d1"},
		{SessionID: sm.ID, TS: ts("18:50:00"), Decision: "reject", Source: "user_reject", ToolName: "Bash", DedupKey: "d2"},
	}
	results := []model.ToolResult{
		{SessionID: sm.ID, TS: ts("18:43:00"), ToolName: "Bash", Success: true, BashCommand: "npm ci", DedupKey: "r1"},
		{SessionID: sm.ID, TS: ts("18:51:00"), ToolName: "Bash", Success: true, BashCommand: "go test ./...", DedupKey: "r2"},
		{SessionID: sm.ID, TS: ts("19:02:00"), ToolName: "Bash", Success: false, BashCommand: "go build ./cmd/wlog", DedupKey: "r3"},
	}

	var b strings.Builder
	writeLastReport(&b, sm, rejects, results, nil, 10, false)
	got := b.String()

	want := strings.Join([]string{
		"session 0a1b2c3d  18:42–19:05 (23m 0s)  cost=$0.4200  in=128.4K out=12.1K  cache_hit=71%",
		"tools  accept 34  reject 2  auto-approved 28",
		"  ✕ reject  Bash (source=user_reject)",
		"  ✕ reject  Bash (source=user_reject)",
		"bash (last 10)",
		"  18:43:00  ok   npm ci",
		"  18:51:00  ok   go test ./...",
		"  19:02:00  fail go build ./cmd/wlog",
		"",
	}, "\n")

	if got != want {
		t.Fatalf("no-color report mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if strings.Contains(got, "\033") {
		t.Fatalf("no-color report contains ANSI escapes:\n%s", got)
	}
}

func TestWriteLastReport_ColorMode(t *testing.T) {
	sm := summaryFixture()
	rejects := []model.ToolDecision{
		{SessionID: sm.ID, TS: ts("18:45:00"), Decision: "reject", Source: "user_reject", ToolName: "Bash", DedupKey: "d1"},
	}
	results := []model.ToolResult{
		{SessionID: sm.ID, TS: ts("18:43:00"), ToolName: "Bash", Success: true, BashCommand: "npm ci", DedupKey: "r1"},
	}

	var b strings.Builder
	writeLastReport(&b, sm, rejects, results, nil, 10, true)
	got := b.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")

	// Header line: bold-wrapped.
	if !strings.HasPrefix(lines[0], ansiBold) || !strings.HasSuffix(lines[0], ansiReset) {
		t.Errorf("header not bold-wrapped: %q", lines[0])
	}
	// tools summary line: bold.
	if !strings.HasPrefix(lines[1], ansiBold) {
		t.Errorf("tools summary not bold: %q", lines[1])
	}
	// reject row: yellow, not bold.
	rejectLine := lines[2]
	if !strings.HasPrefix(rejectLine, ansiYellow) || !strings.HasSuffix(rejectLine, ansiReset) {
		t.Errorf("reject row not yellow-wrapped: %q", rejectLine)
	}
	if strings.Contains(rejectLine, ansiBold) {
		t.Errorf("reject row must be yellow, not bold: %q", rejectLine)
	}
	// "bash (last N)" header: bold.
	if !strings.HasPrefix(lines[3], ansiBold) {
		t.Errorf("bash header not bold: %q", lines[3])
	}
	// bash detail row: uncolored (no ANSI) per the two-color rule.
	if strings.Contains(lines[4], "\033") {
		t.Errorf("bash detail row should be uncolored: %q", lines[4])
	}
}

func TestWriteLastReport_MetricsOnlyBadgeAndBashTruncation(t *testing.T) {
	sm := summaryFixture()
	sm.TokenSource = "metrics"
	longCmd := strings.Repeat("y", 200)
	results := []model.ToolResult{
		{SessionID: sm.ID, TS: ts("10:00:00"), ToolName: "Bash", Success: true, BashCommand: longCmd, DedupKey: "r1"},
	}

	var b strings.Builder
	writeLastReport(&b, sm, nil, results, nil, 10, false)
	got := b.String()

	if !strings.Contains(got, "(metrics-only)") {
		t.Errorf("metrics-only session should carry the badge:\n%s", got)
	}
	// Bash command truncated to maxBashLen.
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "yyy") {
			idx := strings.Index(line, "ok   ")
			cmdPart := line[idx+len("ok   "):]
			if len(cmdPart) > maxBashLen {
				t.Errorf("bash command not truncated (%d > %d): %q", len(cmdPart), maxBashLen, cmdPart)
			}
			if !strings.Contains(cmdPart, "...") {
				t.Errorf("truncated command missing ellipsis: %q", cmdPart)
			}
		}
	}
}

func TestWriteLastReport_MultilineBashSingleLineGolden(t *testing.T) {
	// Bug #1: heredoc / &&-chain / tab commands in the bash list must each render
	// as exactly one column-aligned row, with newlines/tabs/control chars folded
	// to single spaces (then 80-char truncation as usual).
	sm := summaryFixture()
	long := "npm ci\n" + strings.Repeat("&& go build ", 20) // > 80 chars after folding
	results := []model.ToolResult{
		{SessionID: sm.ID, TS: ts("09:00:00"), ToolName: "Bash", Success: true, BashCommand: "npm ci\n&& go build", DedupKey: "r1"},
		{SessionID: sm.ID, TS: ts("09:01:00"), ToolName: "Bash", Success: false, BashCommand: "cat <<EOF\nline1\nline2\nEOF", DedupKey: "r2"},
		{SessionID: sm.ID, TS: ts("09:02:00"), ToolName: "Bash", Success: true, BashCommand: "go\ttest\t./...", DedupKey: "r3"},
		{SessionID: sm.ID, TS: ts("09:03:00"), ToolName: "Bash", Success: true, BashCommand: long, DedupKey: "r4"},
	}

	var b strings.Builder
	writeLastReport(&b, sm, nil, results, nil, 10, false)
	got := b.String()

	// Every output line must be a single row; no bash row may carry a raw newline
	// (the report's own line breaks separate rows). With nil rejects the rows are:
	// header(0), tools(1), bash-header(2), then the 4 result rows = 7 lines total.
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 7 {
		t.Fatalf("expected 7 report rows, got %d:\n%s", len(lines), got)
	}
	for _, ln := range lines {
		if strings.ContainsAny(ln, "\t") {
			t.Errorf("row still contains a tab: %q", ln)
		}
	}

	// Golden for the three short folded commands (result rows index 3,4,5).
	wantRows := []string{
		"  09:00:00  ok   npm ci && go build",
		"  09:01:00  fail cat <<EOF line1 line2 EOF",
		"  09:02:00  ok   go test ./...",
	}
	for i, want := range wantRows {
		if lines[3+i] != want {
			t.Errorf("bash row %d:\n got=%q\nwant=%q", i, lines[3+i], want)
		}
	}

	// The long folded command row: single line, command portion truncated to 80
	// with an ellipsis, starting from the folded text.
	longRow := lines[len(lines)-1]
	idx := strings.Index(longRow, "ok   ")
	if idx < 0 {
		t.Fatalf("long row missing status: %q", longRow)
	}
	cmdPart := longRow[idx+len("ok   "):]
	if len(cmdPart) > maxBashLen {
		t.Errorf("long command not truncated (%d > %d): %q", len(cmdPart), maxBashLen, cmdPart)
	}
	if !strings.HasSuffix(cmdPart, "...") {
		t.Errorf("long command missing ellipsis: %q", cmdPart)
	}
	if !strings.HasPrefix(cmdPart, "npm ci && go build") {
		t.Errorf("long command should fold newline to space: %q", cmdPart)
	}
}

// TestWriteLastReport_PromptsSection covers the prompts (④) section: a stored
// multi-line prompt folds to a single line (bashCell), a long prompt truncates
// with an ellipsis, and a prompt recorded with --no-store-prompts (Stored=false)
// renders "(미저장, len=N)" instead of text. With no prompts the section is
// omitted entirely.
func TestWriteLastReport_PromptsSection(t *testing.T) {
	sm := summaryFixture()
	long := "explain " + strings.Repeat("the architecture ", 20) // > 80 chars

	prompts := []store.UserPrompt{
		{TS: ts("18:42:30"), Text: "fix the bug\nin parser.go", Length: 24, Stored: true},
		{TS: ts("18:44:00"), Length: 99, Stored: false}, // --no-store-prompts
		{TS: ts("18:46:00"), Text: long, Length: int64(len(long)), Stored: true},
	}

	var b strings.Builder
	writeLastReport(&b, sm, nil, nil, prompts, 10, false)
	got := b.String()

	if !strings.Contains(got, "prompts (last 10)") {
		t.Fatalf("missing prompts header:\n%s", got)
	}
	// Multi-line prompt folded to one line (no raw newline in any row).
	if !strings.Contains(got, "  18:42:30  fix the bug in parser.go") {
		t.Errorf("multi-line prompt not folded to one line:\n%s", got)
	}
	// Not-stored prompt shows the marker + length, never any text.
	if !strings.Contains(got, "  18:44:00  (미저장, len=99)") {
		t.Errorf("not-stored prompt marker missing:\n%s", got)
	}
	// Long prompt: its row is single-line and truncated to maxBashLen.
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	for _, ln := range lines {
		if strings.ContainsAny(ln, "\n\t") {
			t.Errorf("prompt row contains a raw control char: %q", ln)
		}
	}
	longRow := lines[len(lines)-1]
	idx := strings.Index(longRow, "18:46:00  ")
	if idx < 0 {
		t.Fatalf("long prompt row missing: %q", longRow)
	}
	cmdPart := longRow[idx+len("18:46:00  "):]
	if len(cmdPart) > maxBashLen {
		t.Errorf("long prompt not truncated (%d > %d): %q", len(cmdPart), maxBashLen, cmdPart)
	}
	if !strings.HasSuffix(cmdPart, "...") {
		t.Errorf("long prompt missing ellipsis: %q", cmdPart)
	}
}

func TestWriteLastReport_NoPromptsOmitsSection(t *testing.T) {
	sm := summaryFixture()
	var b strings.Builder
	writeLastReport(&b, sm, nil, nil, nil, 10, false)
	if strings.Contains(b.String(), "prompts (last") {
		t.Errorf("empty prompts must omit the section:\n%s", b.String())
	}
}

// --- end-to-end Last via the store + captured stdout -------------------------

func TestLast_EmptyDBHint(t *testing.T) {
	// An empty in-memory DB: Last must print a hint and return nil (exit 0).
	st, err := store.Open("", true, 5000)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	cfg := config.Default()
	cfg.Memory = true

	var runErr error
	out := captureStdout(t, nil, func() { runErr = Last(context.Background(), cfg, "", 10) })
	if runErr != nil {
		t.Fatalf("Last(empty) returned error: %v", runErr)
	}
	if !strings.Contains(out, "no sessions yet") || !strings.Contains(out, "--print-claude-setup") {
		t.Fatalf("empty-DB hint missing expected text:\n%s", out)
	}
}

func TestLast_NotFound(t *testing.T) {
	st, err := store.Open("", true, 5000)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	// Seed one session so the DB is non-empty, then ask for a different id.
	seed(t, st, model.Batch{Sessions: []model.Session{{ID: "real", FirstSeen: 1, LastSeen: 2}}})

	cfg := config.Default()
	cfg.Memory = true
	err = captureStdoutErr(t, func() error { return Last(context.Background(), cfg, "ghost", 10) })
	if err == nil {
		t.Fatal("Last(ghost) should error for a missing explicit session")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("not-found error should mention 'not found': %q", err.Error())
	}
	if strings.Contains(err.Error(), "wlog: error:") {
		t.Fatalf("internal error must not carry the prefix (F4): %q", err.Error())
	}
}

func TestLast_LatestSessionEndToEnd(t *testing.T) {
	st, err := store.Open("", true, 5000)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	const sid = "feedface00000001"
	seed(t, st, model.Batch{
		Sessions: []model.Session{{ID: sid, FirstSeen: ts("08:00:00"), LastSeen: ts("08:30:00"), TokenSource: "events"}},
		APIRequests: []model.APIRequest{{
			SessionID: sid, TS: ts("08:05:00"), Model: "claude",
			Tokens: model.Tokens{Input: 1000, Output: 200, CacheRead: 3000}, CostUSD: 0.42, DedupKey: "ar1",
		}},
		ToolDecisions: []model.ToolDecision{
			{SessionID: sid, TS: ts("08:10:00"), Decision: "reject", Source: "user_reject", ToolName: "Bash", DedupKey: "d1"},
		},
		ToolResults: []model.ToolResult{
			{SessionID: sid, TS: ts("08:12:00"), ToolName: "Bash", Success: true, BashCommand: "ls -la", DedupKey: "r1"},
		},
	})

	cfg := config.Default()
	cfg.Memory = true

	var runErr error
	out := captureStdout(t, nil, func() { runErr = Last(context.Background(), cfg, "", 10) })
	if runErr != nil {
		t.Fatalf("Last returned error: %v", runErr)
	}
	// Latest session resolved (no --session given) and the three sections present.
	if !strings.Contains(out, "session feedface") {
		t.Errorf("header missing short id:\n%s", out)
	}
	if !strings.Contains(out, "cost=$0.4200") {
		t.Errorf("header missing cost:\n%s", out)
	}
	if !strings.Contains(out, "tools  accept 0  reject 1  auto-approved 0") {
		t.Errorf("tools summary wrong:\n%s", out)
	}
	if !strings.Contains(out, "✕ reject  Bash (source=user_reject)") {
		t.Errorf("reject list missing:\n%s", out)
	}
	if !strings.Contains(out, "ls -la") {
		t.Errorf("bash list missing command:\n%s", out)
	}
}

// TestLast_ServerOrientedConfigStillExitsZero is the P2 regression: `wlog last`
// is a read-only command that binds no port and opens the store read-only, so a
// server-oriented config that cfg.Validate() WOULD reject (a non-loopback Listen
// with neither --unsafe nor --auth-token) must not affect it. Before the fix the
// CLI ran resolve()/Validate() for `last` (and Last itself re-called Validate),
// so a WLOG_LISTEN=0.0.0.0 environment broke a plain `wlog last`. The CLI now uses
// resolveReadOnly and Last no longer validates; this test guards the app-layer
// invariant that the summary never depends on the server bind policy.
func TestLast_ServerOrientedConfigStillExitsZero(t *testing.T) {
	st, err := store.Open("", true, 5000)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	const sid = "feedface00000002"
	seed(t, st, model.Batch{
		Sessions: []model.Session{{ID: sid, FirstSeen: ts("08:00:00"), LastSeen: ts("08:30:00"), TokenSource: "events"}},
		APIRequests: []model.APIRequest{{
			SessionID: sid, TS: ts("08:05:00"), Model: "claude",
			Tokens: model.Tokens{Input: 1000, Output: 200}, CostUSD: 0.42, DedupKey: "ar-srv",
		}},
	})

	// A config the server would reject: non-loopback bind, no unsafe/auth.
	cfg := config.Default()
	cfg.Memory = true
	cfg.Listen = "0.0.0.0"
	cfg.Unsafe = false
	cfg.AuthToken = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("precondition: this config must be rejected by Validate (else the test proves nothing)")
	}

	var runErr error
	out := captureStdout(t, nil, func() { runErr = Last(context.Background(), cfg, "", 10) })
	if runErr != nil {
		t.Fatalf("Last returned error under server-oriented config (must be nil; bind policy is irrelevant to a read-only summary): %v", runErr)
	}
	// The read path ran regardless of the bind policy: the seeded session renders.
	if !strings.Contains(out, "session feedface") || !strings.Contains(out, "cost=$0.4200") {
		t.Fatalf("Last did not render the seeded session under server-oriented config:\n%s", out)
	}
}

// seed writes a batch into st, failing the test on error.
func seed(t *testing.T, st *store.Store, b model.Batch) {
	t.Helper()
	if err := st.WriteBatch(context.Background(), b); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}
}
