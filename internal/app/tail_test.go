package app

import (
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openwong2kim/wlog/internal/api"
	"github.com/openwong2kim/wlog/internal/ingest"
	"github.com/openwong2kim/wlog/internal/store"
)

// ts builds a unix-ms timestamp whose local-time clock reads hhmmss, so that
// formatTailLine (which renders TS in local time) round-trips to the same
// "HH:MM:SS" string regardless of the test machine's timezone.
func ts(hhmmss string) int64 {
	parts := strings.Split(hhmmss, ":")
	if len(parts) != 3 {
		panic("ts: want HH:MM:SS, got " + hhmmss)
	}
	h, _ := strconv.Atoi(parts[0])
	m, _ := strconv.Atoi(parts[1])
	s, _ := strconv.Atoi(parts[2])
	d := time.Date(2026, 6, 16, h, m, s, 0, time.Local)
	return d.UnixMilli()
}

// --- shared test helpers -----------------------------------------------------

func mustListen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

func splitHostPort(addr string) (host, port string, err error) {
	return net.SplitHostPort(addr) // host, port, err
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("atoi %q: %v", s, err)
	}
	return n
}

// captureStdoutErr runs fn while discarding its stdout (so the startup banner
// does not pollute test output) and returns fn's error.
func captureStdoutErr(t *testing.T, fn func() error) error {
	t.Helper()
	var err error
	_ = captureStdout(t, nil, func() { err = fn() })
	return err
}

// exportToString runs the shared export path (api.ExportSession, exactly what
// app.Export wraps) against an open store and returns the JSON document.
func exportToString(t *testing.T, st *store.Store, sessionID string) string {
	t.Helper()
	var b strings.Builder
	if err := api.ExportSession(st.ReadDB(), sessionID, &b); err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	return b.String()
}

// --- formatTailLine tests ----------------------------------------------------

func TestFormatTailLine_APIRequestNoColor(t *testing.T) {
	ev := ingest.LogEvent{
		TS:   ts("14:03:22"),
		Kind: "api_request",
		Fields: map[string]any{
			"model":       "gpt-4o",
			"input":       int64(1420),
			"output":      int64(382),
			"cost_usd":    0.0031,
			"duration_ms": int64(2100),
		},
	}
	got := formatTailLine(ev, false)
	want := "[14:03:22] api      gpt-4o in=1420 out=382 cost=$0.0031 dur=2.1s"
	if got != want {
		t.Fatalf("api line:\n got=%q\nwant=%q", got, want)
	}
	if strings.Contains(got, "\033") {
		t.Fatalf("no-color line contains ANSI escapes: %q", got)
	}
}

func TestFormatTailLine_DecisionReject(t *testing.T) {
	ev := ingest.LogEvent{
		TS:   ts("14:03:25"),
		Kind: "tool_decision",
		Fields: map[string]any{
			"decision":  "reject",
			"source":    "user_reject",
			"tool_name": "Bash",
		},
	}

	// No color: plain text, no ANSI.
	plain := formatTailLine(ev, false)
	want := "[14:03:25] decision reject Bash (source=user_reject)"
	if plain != want {
		t.Fatalf("decision line:\n got=%q\nwant=%q", plain, want)
	}

	// Color: reject must be wrapped in ANSI yellow, not bold.
	colored := formatTailLine(ev, true)
	if !strings.HasPrefix(colored, ansiYellow) || !strings.HasSuffix(colored, ansiReset) {
		t.Fatalf("reject color line not yellow-wrapped: %q", colored)
	}
	if strings.Contains(colored, ansiBold) {
		t.Fatalf("reject line should use yellow, not bold: %q", colored)
	}
	if !strings.Contains(colored, want) {
		t.Fatalf("colored line missing payload: %q", colored)
	}
}

func TestFormatTailLine_AcceptDecisionUsesBoldNotYellow(t *testing.T) {
	ev := ingest.LogEvent{
		TS:   ts("09:00:00"),
		Kind: "tool_decision",
		Fields: map[string]any{
			"decision":  "accept",
			"source":    "config",
			"tool_name": "Read",
		},
	}
	colored := formatTailLine(ev, true)
	if !strings.HasPrefix(colored, ansiBold) {
		t.Fatalf("accept decision should be bold: %q", colored)
	}
	if strings.Contains(colored, ansiYellow) {
		t.Fatalf("accept decision must not be yellow: %q", colored)
	}
}

func TestFormatTailLine_ToolResultBashTruncated(t *testing.T) {
	longCmd := strings.Repeat("x", 200)
	ev := ingest.LogEvent{
		TS:   ts("01:02:03"),
		Kind: "tool_result",
		Fields: map[string]any{
			"tool_name":    "Bash",
			"success":      true,
			"duration_ms":  int64(50),
			"bash_command": longCmd,
		},
	}
	got := formatTailLine(ev, false)
	if !strings.HasPrefix(got, "[01:02:03] tool     Bash ok ") {
		t.Fatalf("tool result prefix wrong: %q", got)
	}
	if !strings.Contains(got, "...") {
		t.Fatalf("long bash command should be truncated with ...: %q", got)
	}
	// The visible command portion must not exceed maxBashLen.
	idx := strings.Index(got, "Bash ok ")
	cmdPart := got[idx+len("Bash ok "):]
	if len(cmdPart) > maxBashLen {
		t.Fatalf("truncated command too long (%d > %d): %q", len(cmdPart), maxBashLen, cmdPart)
	}
}

// --- singleLine / bashCell unit tests (Bug #1) -------------------------------

func TestSingleLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "go build ./...", "go build ./..."},
		{"empty", "", ""},
		{"newline_andchain", "npm ci\n&& go build", "npm ci && go build"},
		{"crlf", "a\r\nb", "a b"},
		{"tabs", "go\ttest\t./...", "go test ./..."},
		{"collapse_runs", "a   \t\n  b", "a b"},
		{"trim_edges", "  \n hello \t ", "hello"},
		{"heredoc", "cat <<EOF\nline1\nline2\nEOF", "cat <<EOF line1 line2 EOF"},
		{"control_chars", "echo\x00a\x1bb\x07c", "echo a b c"},
		{"only_whitespace", " \t\n\r ", ""},
		{"unicode_kept", "echo 日本語 done", "echo 日本語 done"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := singleLine(c.in)
			if got != c.want {
				t.Errorf("singleLine(%q) = %q, want %q", c.in, got, c.want)
			}
			if strings.ContainsAny(got, "\n\r\t") {
				t.Errorf("singleLine(%q) still has a line break / tab: %q", c.in, got)
			}
		})
	}
}

func TestBashCell_SingleLinesThenTruncates(t *testing.T) {
	// A multiline command longer than maxBashLen must end up on one line AND be
	// truncated against the *cleaned* length (single-line first, then 80-char).
	cmd := "npm ci\n" + strings.Repeat("&& go build ", 20)
	got := bashCell(cmd)

	if strings.ContainsAny(got, "\n\r\t") {
		t.Fatalf("bashCell left a line break / tab in: %q", got)
	}
	if len(got) > maxBashLen {
		t.Fatalf("bashCell not truncated to %d (got %d): %q", maxBashLen, len(got), got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("over-length bashCell should end with ellipsis: %q", got)
	}
	if !strings.HasPrefix(got, "npm ci && go build") {
		t.Fatalf("bashCell should fold the newline to a space: %q", got)
	}
}

func TestBashCell_ShortMultilineNoEllipsis(t *testing.T) {
	// Short heredoc: folded to one line, no truncation, no ellipsis.
	got := bashCell("cat <<EOF\nhi\nEOF")
	want := "cat <<EOF hi EOF"
	if got != want {
		t.Fatalf("bashCell short multiline = %q, want %q", got, want)
	}
}

func TestFormatTailLine_ToolResultMultilineBashSingleLine(t *testing.T) {
	ev := ingest.LogEvent{
		TS:   ts("01:02:03"),
		Kind: "tool_result",
		Fields: map[string]any{
			"tool_name":    "Bash",
			"success":      true,
			"bash_command": "npm ci\n&& go build",
		},
	}
	got := formatTailLine(ev, false)
	want := "[01:02:03] tool     Bash ok npm ci && go build"
	if got != want {
		t.Fatalf("multiline tail line:\n got=%q\nwant=%q", got, want)
	}
	// The whole rendered line is exactly one terminal row.
	if strings.Contains(got, "\n") {
		t.Fatalf("tail line spilled across rows: %q", got)
	}
}

func TestFormatTailLine_TypePadding(t *testing.T) {
	ev := ingest.LogEvent{TS: ts("00:00:00"), Kind: "api_request", Fields: map[string]any{"model": "m"}}
	got := formatTailLine(ev, false)
	// "[HH:MM:SS] " is 11 chars; the next 8 are the padded TYPE field.
	const prefix = "[00:00:00] "
	field := got[len(prefix) : len(prefix)+8]
	if field != "api     " {
		t.Fatalf("TYPE field not 8-padded: %q", field)
	}
}
