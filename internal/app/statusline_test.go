package app

import (
	"context"
	"strings"
	"testing"

	"github.com/openwong2kim/wlog/internal/config"
	"github.com/openwong2kim/wlog/internal/model"
	"github.com/openwong2kim/wlog/internal/store"
)

// --- stdin parsing -----------------------------------------------------------

func TestReadStatuslineSessionID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"snake_case", `{"session_id":"abc","cwd":"/x","model":{"id":"m","display_name":"M"}}`, "abc"},
		{"camelCase", `{"sessionId":"def"}`, "def"},
		{"extra fields ignored", `{"session_id":"ghi","transcript_path":"/t","workspace":{"current_dir":"/w"}}`, "ghi"},
		{"missing field", `{"cwd":"/x"}`, ""},
		{"broken json", `{not json`, ""},
		{"empty", "", ""},
		{"whitespace only", "   \n\t ", ""},
		{"not an object", `"abc"`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := readStatuslineSessionID(strings.NewReader(c.in)); got != c.want {
				t.Errorf("readStatuslineSessionID(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
	// nil reader must not panic.
	if got := readStatuslineSessionID(nil); got != "" {
		t.Errorf("readStatuslineSessionID(nil) = %q, want empty", got)
	}
}

// --- formatStatusline golden -------------------------------------------------

func TestFormatStatusline(t *testing.T) {
	full := store.SessionSnapshot{
		CostUSD:       0.42,
		Tokens:        model.Tokens{Input: 128_400, Output: 12_100, CacheRead: 314_000},
		CacheHitRatio: 0.71,
		ToolReject:    2,
		LastTool:      "Bash",
	}
	want := "$0.42 · in 128.4K/out 12.1K · cache 71% · ⊘2 reject · Bash"
	if got := formatStatusline(full); got != want {
		t.Fatalf("formatStatusline(full):\n got=%q\nwant=%q", got, want)
	}

	// No rejects and no tool: those segments are dropped (narrow status bar).
	min := store.SessionSnapshot{
		CostUSD:       0.01,
		Tokens:        model.Tokens{Input: 10, Output: 2},
		CacheHitRatio: 0,
	}
	wantMin := "$0.01 · in 10/out 2 · cache 0%"
	if got := formatStatusline(min); got != wantMin {
		t.Fatalf("formatStatusline(min):\n got=%q\nwant=%q", got, wantMin)
	}

	// Reject but no tool yet.
	rejNoTool := store.SessionSnapshot{CostUSD: 0.05, ToolReject: 1}
	if got := formatStatusline(rejNoTool); !strings.Contains(got, "⊘1 reject") || strings.HasSuffix(got, "· ") {
		t.Fatalf("formatStatusline(rejNoTool) wrong: %q", got)
	}
}

// --- renderStatusline + Statusline end-to-end --------------------------------

func TestRenderStatusline_ActiveSession(t *testing.T) {
	st, err := store.Open("", true, 5000)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	const sid = "cafebabe00000001"
	seed(t, st, model.Batch{
		Sessions: []model.Session{{ID: sid, FirstSeen: 1000, LastSeen: 5000, TokenSource: "events"}},
		APIRequests: []model.APIRequest{{
			SessionID: sid, TS: 1500, Model: "m",
			Tokens: model.Tokens{Input: 1000, Output: 200, CacheRead: 3000}, CostUSD: 0.42, DedupKey: "ar1",
		}},
		ToolResults: []model.ToolResult{
			{SessionID: sid, TS: 2000, ToolName: "Bash", Success: true, DedupKey: "r1"},
		},
	})

	cfg := config.Default()
	cfg.Memory = true

	got := renderStatusline(context.Background(), cfg, sid)
	if !strings.Contains(got, "$0.42") || !strings.Contains(got, "Bash") {
		t.Fatalf("renderStatusline(active) = %q", got)
	}
}

func TestRenderStatusline_FallbackToLatestWhenSessionMissing(t *testing.T) {
	st, err := store.Open("", true, 5000)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	const sid = "deadbeef00000001"
	seed(t, st, model.Batch{
		Sessions: []model.Session{{ID: sid, FirstSeen: 1000, LastSeen: 5000, TokenSource: "events"}},
		APIRequests: []model.APIRequest{{
			SessionID: sid, TS: 1500, Model: "m",
			Tokens: model.Tokens{Input: 500, Output: 100}, CostUSD: 0.10, DedupKey: "ar1",
		}},
	})

	cfg := config.Default()
	cfg.Memory = true

	// A session id Claude Code piped that we have never seen falls back to the
	// latest session we do have (so the bar still shows something useful).
	got := renderStatusline(context.Background(), cfg, "unknown-session-id")
	if !strings.Contains(got, "$0.10") {
		t.Fatalf("renderStatusline(missing id) should fall back to latest, got %q", got)
	}
}

func TestRenderStatusline_EmptyDBYieldsEmptyLine(t *testing.T) {
	st, err := store.Open("", true, 5000)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	cfg := config.Default()
	cfg.Memory = true

	if got := renderStatusline(context.Background(), cfg, ""); got != "" {
		t.Fatalf("empty DB should render empty line, got %q", got)
	}
}

// TestStatusline_ServerOrientedConfigStillExitsZero is the P2 regression: a
// server-oriented config that cfg.Validate() WOULD reject (a non-loopback Listen
// with neither --unsafe nor --auth-token) must not affect statusline — it binds
// no port and only reads the DB. Statusline must still return nil and print
// exactly one line. The CLI achieves this by NOT running resolve()/Validate() on
// the statusline path (resolveReadOnly); this test guards the app-layer invariant
// that the render path never depends on the server bind policy.
func TestStatusline_ServerOrientedConfigStillExitsZero(t *testing.T) {
	st, err := store.Open("", true, 5000)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	const sid = "feed0001feed0001"
	seed(t, st, model.Batch{
		Sessions: []model.Session{{ID: sid, FirstSeen: 1000, LastSeen: 5000, TokenSource: "events"}},
		APIRequests: []model.APIRequest{{
			SessionID: sid, TS: 1500, Model: "m",
			Tokens: model.Tokens{Input: 1000, Output: 200}, CostUSD: 0.42, DedupKey: "ar1",
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
	out := captureStdout(t, nil, func() { runErr = Statusline(context.Background(), cfg, false) })
	if runErr != nil {
		t.Fatalf("Statusline returned error under server-oriented config (must be nil): %v", runErr)
	}
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("Statusline must print exactly one line, got %q", out)
	}
	// The line is the real snapshot (the read path ran regardless of bind policy).
	if !strings.Contains(out, "$0.42") {
		t.Fatalf("statusline did not render the seeded session: %q", out)
	}
}

func TestStatusline_AlwaysExitsZeroAndPrintsOneLine(t *testing.T) {
	// Even with an empty DB and no stdin session, Statusline must return nil and
	// print exactly one line (the status bar must never break).
	st, err := store.Open("", true, 5000)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	cfg := config.Default()
	cfg.Memory = true

	var runErr error
	out := captureStdout(t, nil, func() { runErr = Statusline(context.Background(), cfg, false) })
	if runErr != nil {
		t.Fatalf("Statusline returned error (must always be nil on render path): %v", runErr)
	}
	// Exactly one line (a single trailing newline, possibly an empty line).
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("Statusline must print exactly one line, got %q", out)
	}
}
