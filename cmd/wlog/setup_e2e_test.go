package main

// End-to-end tests for the `wlog setup` / `wlog hook` CLI surface. They drive
// the real cobra command tree (newRootCmd().Execute) against a temp HOME so the
// whole path — flag parsing, env overlay, settings.json merge, read-only doctor,
// hook stdin — is exercised exactly as a user would, with no real files touched.

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/openwong2kim/wlog/internal/config"
)

// run executes the wlog CLI with args, silencing stdout/stderr, and returns the
// command error. runStdin additionally feeds stdin.
func run(t *testing.T, args ...string) error {
	t.Helper()
	return runStdin(t, "", args...)
}

func runStdin(t *testing.T, stdin string, args ...string) error {
	t.Helper()
	origOut, origErr, origIn := os.Stdout, os.Stderr, os.Stdin
	if devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		os.Stdout, os.Stderr = devnull, devnull
		defer func() { _ = devnull.Close() }()
	}
	if stdin != "" {
		r, w, err := os.Pipe()
		if err == nil {
			_, _ = w.WriteString(stdin)
			_ = w.Close()
			os.Stdin = r
		}
	}
	defer func() { os.Stdout, os.Stderr, os.Stdin = origOut, origErr, origIn }()

	root := newRootCmd()
	root.SetArgs(args)
	return root.Execute()
}

// hermeticHome points HOME/USERPROFILE + WLOG_DB + XDG at a temp dir so the CLI
// never touches the developer's real settings or data.
func hermeticHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
		t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "xdg"))
	t.Setenv("WLOG_DB", filepath.Join(home, "wlog.db"))
	return home
}

func TestE2E_SetupLifecycle(t *testing.T) {
	home := hermeticHome(t)
	settings := filepath.Join(home, ".claude", "settings.json")

	// setup: writes env + statusLine + SessionEnd hook.
	if err := run(t, "setup"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	data, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("read settings after setup: %v", err)
	}
	s := string(data)
	for _, want := range []string{
		"CLAUDE_CODE_ENABLE_TELEMETRY",
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		config.StatuslineCommand,
		config.SessionEndHookCommand,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("settings.json missing %q after setup:\n%s", want, s)
		}
	}

	// idempotent second run.
	if err := run(t, "setup"); err != nil {
		t.Fatalf("setup (2nd): %v", err)
	}

	// --print is a dry run: settings.json byte-identical before/after.
	before, _ := os.ReadFile(settings)
	if err := run(t, "setup", "--print"); err != nil {
		t.Fatalf("setup --print: %v", err)
	}
	after, _ := os.ReadFile(settings)
	if string(before) != string(after) {
		t.Error("setup --print must not modify settings.json")
	}

	// --status is read-only and exits 0.
	if err := run(t, "setup", "--status"); err != nil {
		t.Fatalf("setup --status: %v", err)
	}

	// --uninstall removes wlog-owned keys.
	if err := run(t, "setup", "--uninstall"); err != nil {
		t.Fatalf("setup --uninstall: %v", err)
	}
	data, _ = os.ReadFile(settings)
	s = string(data)
	if strings.Contains(s, "CLAUDE_CODE_ENABLE_TELEMETRY") || strings.Contains(s, config.SessionEndHookCommand) {
		t.Errorf("uninstall left wlog keys behind:\n%s", s)
	}
}

func TestE2E_SetupPrompts(t *testing.T) {
	home := hermeticHome(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := run(t, "setup", "--prompts"); err != nil {
		t.Fatalf("setup --prompts: %v", err)
	}
	data, _ := os.ReadFile(settings)
	if !strings.Contains(string(data), "OTEL_LOG_USER_PROMPTS") {
		t.Errorf("--prompts should enable OTEL_LOG_USER_PROMPTS:\n%s", data)
	}
}

func TestE2E_HookSessionEndResilient(t *testing.T) {
	hermeticHome(t)
	// Point at a missing DB; the hook must still exit 0 (never wedge teardown).
	t.Setenv("WLOG_DB", filepath.Join(t.TempDir(), "missing", "wlog.db"))
	if err := runStdin(t, `{"session_id":"abc"}`, "hook", "session-end"); err != nil {
		t.Fatalf("hook session-end should exit 0 with a missing DB: %v", err)
	}
}

func TestE2E_PrintClaudeSetupCrossPlatform(t *testing.T) {
	hermeticHome(t)
	// --print-claude-setup must emit the JSON env block (cross-platform), not only
	// the bash form. Capture stdout to assert.
	orig := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	root := newRootCmd()
	root.SetArgs([]string{"--print-claude-setup"})
	err := root.Execute()
	_ = w.Close()
	os.Stdout = orig
	out := <-done
	_ = r.Close()
	if err != nil {
		t.Fatalf("--print-claude-setup: %v", err)
	}
	if !strings.Contains(out, "OTEL_EXPORTER_OTLP_ENDPOINT") {
		t.Errorf("expected OTel keys in output: %q", out)
	}
}
