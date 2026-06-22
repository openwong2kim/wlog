package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/openwong2kim/wlog/internal/config"
	"github.com/openwong2kim/wlog/internal/model"
	"github.com/openwong2kim/wlog/internal/store"
)

// seedSessionDB writes one session (with an api_request, a reject, and a bash
// result) into a fresh file-backed store at path, then closes it.
func seedSessionDB(t *testing.T, path, sid string, lastSeen int64) {
	t.Helper()
	st, err := store.Open(path, false, 5000)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	err = st.WriteBatch(context.Background(), model.Batch{
		Sessions: []model.Session{{
			ID: sid, FirstSeen: lastSeen - 1000, LastSeen: lastSeen,
			ModelSet: "claude-opus-4", TokenSource: "events",
		}},
		APIRequests: []model.APIRequest{{
			SessionID: sid, TS: lastSeen - 500, Model: "claude-opus-4",
			Tokens: model.Tokens{Input: 1000, Output: 500}, CostUSD: 0.42, DedupKey: "a1",
		}},
		ToolDecisions: []model.ToolDecision{{
			SessionID: sid, TS: lastSeen - 400, Decision: "reject",
			Source: "user_reject", ToolName: "Bash", DedupKey: "d1",
		}},
		ToolResults: []model.ToolResult{{
			SessionID: sid, TS: lastSeen - 300, ToolName: "Bash",
			Success: true, BashCommand: "go build ./...", DedupKey: "r1",
		}},
	})
	if err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}
}

// captureOut runs fn with os.Stdout redirected to a pipe and returns what it
// wrote.
func captureOut(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	out := <-done
	_ = r.Close()
	return out
}

// withStdin runs fn with os.Stdin replaced by a reader over content (never
// blocks: EOF is immediate).
func withStdin(t *testing.T, content string, fn func()) {
	t.Helper()
	orig := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdin = r
	// Write in a goroutine so a payload larger than the pipe buffer can't deadlock.
	go func() { _, _ = io.WriteString(w, content); _ = w.Close() }()
	defer func() { os.Stdin = orig; _ = r.Close() }()
	fn()
}

func TestRenderSessionEnd_EmptyStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wlog.db")
	// Create an empty (but valid) store so OpenReader succeeds with no sessions.
	st, err := store.Open(path, false, 5000)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = st.Close()

	c := &config.Config{DBPath: path}
	if out := renderSessionEnd(context.Background(), c, ""); out != "" {
		t.Errorf("empty store should render nothing, got %q", out)
	}
}

func TestRenderSessionEnd_Seeded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wlog.db")
	const sid = "sess-123456789"
	seedSessionDB(t, path, sid, time.Now().UnixMilli())

	c := &config.Config{DBPath: path}
	out := renderSessionEnd(context.Background(), c, sid)
	if out == "" {
		t.Fatal("seeded store should render a report")
	}
	if !strings.Contains(out, "session ") {
		t.Errorf("report missing session header: %q", out)
	}
	if !strings.Contains(out, "reject 1") {
		t.Errorf("report missing reject count: %q", out)
	}
}

func TestHookSessionEnd_MissingDBIsResilient(t *testing.T) {
	// A nonexistent DB path: the hook must exit 0 (nil) and create nothing.
	dbPath := filepath.Join(t.TempDir(), "nope", "wlog.db")
	c := &config.Config{DBPath: dbPath}

	var err error
	withStdin(t, `{"session_id":"whatever"}`, func() {
		_ = captureOut(t, func() {
			err = HookSessionEnd(context.Background(), c)
		})
	})
	if err != nil {
		t.Fatalf("HookSessionEnd should never error: %v", err)
	}
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Errorf("hook must not create the DB (read-only); stat err=%v", statErr)
	}
}

func TestHookSessionEnd_SeededRenders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wlog.db")
	const sid = "abcdef012345"
	seedSessionDB(t, path, sid, time.Now().UnixMilli())
	c := &config.Config{DBPath: path}

	var out string
	withStdin(t, `{"session_id":"`+sid+`"}`, func() {
		out = captureOut(t, func() {
			if err := HookSessionEnd(context.Background(), c); err != nil {
				t.Fatalf("HookSessionEnd: %v", err)
			}
		})
	})
	if !strings.Contains(out, "session ") {
		t.Errorf("hook output missing report: %q", out)
	}
}

func TestSetupStatus_EmptyIsReadOnlyAndExitsZero(t *testing.T) {
	home := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}
	t.Setenv("HOME", home)

	settingsPath := filepath.Join(home, ".claude", "settings.json")
	dbPath := filepath.Join(home, "data", "wlog.db") // nonexistent
	c := &config.Config{OTLPGRPCPort: config.DefaultOTLPGRPCPort, DBPath: dbPath}

	var err error
	out := captureOut(t, func() {
		err = setupStatus(context.Background(), c, settingsPath)
	})
	if err != nil {
		t.Fatalf("setupStatus should exit 0: %v", err)
	}
	if !strings.Contains(out, "wlog setup status") {
		t.Errorf("missing header: %q", out)
	}
	if !strings.Contains(out, "[!]") {
		t.Errorf("expected at least one warning for an unconfigured setup: %q", out)
	}
	// Read-only: never creates the DB.
	if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
		t.Errorf("--status must not create the DB; stat err=%v", statErr)
	}
}

func TestSetupStatus_Configured(t *testing.T) {
	home := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}
	t.Setenv("HOME", home)

	settingsPath := filepath.Join(home, ".claude", "settings.json")
	if _, err := config.ApplySetup(settingsPath, config.SetupOptions{
		GRPCPort: config.DefaultOTLPGRPCPort, StatusLine: true, Hooks: true,
	}); err != nil {
		t.Fatalf("ApplySetup: %v", err)
	}
	dbPath := filepath.Join(home, "wlog.db")
	seedSessionDB(t, dbPath, "sess-abcdef012", time.Now().UnixMilli())
	c := &config.Config{OTLPGRPCPort: config.DefaultOTLPGRPCPort, DBPath: dbPath}

	out := captureOut(t, func() {
		if err := setupStatus(context.Background(), c, settingsPath); err != nil {
			t.Fatalf("setupStatus: %v", err)
		}
	})
	if !strings.Contains(out, "[ok]") {
		t.Errorf("a configured setup should show [ok] checks: %q", out)
	}
	if !strings.Contains(out, "data flowing") {
		t.Errorf("recent seeded data should report 'data flowing': %q", out)
	}
}
