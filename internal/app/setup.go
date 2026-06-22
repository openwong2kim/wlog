package app

// setup.go implements `wlog setup` (auto-configure Claude Code) and
// `wlog hook session-end` (the passive end-of-session summary hook).
//
// `wlog setup` writes the OTel env block + status line + SessionEnd hook into
// ~/.claude/settings.json (config.ApplySetup), turning the old copy-paste
// onboarding into one command. Sub-modes: --print (dry run), --uninstall
// (value-matched teardown), --status (read-only doctor). None of them bind a
// port; --status and the hook open the store READ-ONLY (store.OpenReader), so
// they never create or migrate the DB.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openwong2kim/wlog/internal/config"
	"github.com/openwong2kim/wlog/internal/store"
)

// recentWindow is how fresh the latest session must be for the doctor to report
// telemetry as "flowing".
const recentWindow = 24 * time.Hour

// SetupOptions are the resolved `wlog setup` flags.
type SetupOptions struct {
	Print        bool
	Uninstall    bool
	Status       bool
	Prompts      bool
	NoStatusLine bool
	NoHooks      bool
}

// Setup runs the `wlog setup` workflow selected by opts. It binds no port; the
// install/uninstall/print paths only touch settings.json, and --status opens the
// store read-only.
func Setup(ctx context.Context, c *config.Config, o SetupOptions) error {
	path, err := config.ClaudeSettingsPath()
	if err != nil {
		return err
	}
	opts := config.SetupOptions{
		GRPCPort:   c.OTLPGRPCPort,
		Prompts:    o.Prompts,
		StatusLine: !o.NoStatusLine,
		Hooks:      !o.NoHooks,
	}

	switch {
	case o.Status:
		return setupStatus(ctx, c, path)
	case o.Print:
		return setupPrint(opts)
	case o.Uninstall:
		ch, err := config.RemoveSetup(path, opts)
		if err != nil {
			return err
		}
		printRemoveResult(path, ch)
		return nil
	default:
		ch, err := config.ApplySetup(path, opts)
		if err != nil {
			return err
		}
		printApplyResult(path, opts, ch)
		return nil
	}
}

// setupPrint shows exactly what `wlog setup` would write, and writes nothing.
func setupPrint(opts config.SetupOptions) error {
	out := os.Stdout
	fmt.Fprintf(out, "# wlog setup would merge the following into ~/.claude/settings.json\n# (dry run — nothing written; run `wlog setup` to apply)\n\n")
	fmt.Fprintln(out, config.ClaudeSetupSnippetJSON(opts.GRPCPort, opts.Prompts))
	if opts.StatusLine {
		fmt.Fprintf(out, "\nstatusLine        -> %s\n", config.StatuslineCommand)
	}
	if opts.Hooks {
		fmt.Fprintf(out, "hooks.SessionEnd  -> %s\n", config.SessionEndHookCommand)
	}
	if !opts.Prompts {
		fmt.Fprintln(out, "\nprompt-text capture is OFF (privacy default); add --prompts to include it.")
	}
	return nil
}

func printApplyResult(path string, opts config.SetupOptions, ch config.SetupChange) {
	out := os.Stdout
	if !ch.Any() {
		fmt.Fprintf(out, "wlog is already configured in %s (no change).\n", path)
		fmt.Fprintln(out, "verify with: wlog setup --status")
		return
	}
	fmt.Fprintf(out, "wlog setup updated %s:\n", path)
	if ch.Env {
		if opts.Prompts {
			fmt.Fprintln(out, "  + telemetry env (OTel -> wlog, incl. prompt text)")
		} else {
			fmt.Fprintln(out, "  + telemetry env (OTel -> wlog)")
		}
	}
	if ch.StatusLine {
		fmt.Fprintln(out, "  + status line")
	}
	if ch.Hooks {
		fmt.Fprintln(out, "  + SessionEnd summary hook")
	}
	fmt.Fprintln(out, "\nrestart Claude Code (or reload its config) for changes to take effect.")
	fmt.Fprintln(out, "verify with: wlog setup --status   ·   undo with: wlog setup --uninstall")
}

func printRemoveResult(path string, ch config.SetupChange) {
	out := os.Stdout
	if !ch.Any() {
		fmt.Fprintf(out, "no wlog-owned settings found in %s (nothing to remove).\n", path)
		return
	}
	fmt.Fprintf(out, "wlog setup removed from %s:\n", path)
	if ch.Env {
		fmt.Fprintln(out, "  - telemetry env")
	}
	if ch.StatusLine {
		fmt.Fprintln(out, "  - status line")
	}
	if ch.Hooks {
		fmt.Fprintln(out, "  - SessionEnd summary hook")
	}
	fmt.Fprintln(out, "\nrestart Claude Code (or reload its config) to apply.")
}

// --- doctor (--status) -----------------------------------------------------

type checkState int

const (
	checkOK checkState = iota
	checkWarn
	checkInfo
)

func (s checkState) tag() string {
	switch s {
	case checkOK:
		return "[ok]  "
	case checkWarn:
		return "[!]   "
	default:
		return "[info]"
	}
}

// setupStatus prints a read-only onboarding checklist and always exits 0. It
// never opens the store read-write (no DB is created) and binds no port.
func setupStatus(ctx context.Context, c *config.Config, path string) error {
	out := os.Stdout
	grpc := c.OTLPGRPCPort
	if grpc == 0 {
		grpc = config.DefaultOTLPGRPCPort
	}
	fmt.Fprintf(out, "wlog setup status (settings: %s, gRPC port: %d)\n\n", path, grpc)

	line := func(s checkState, label, detail string) {
		if detail != "" {
			fmt.Fprintf(out, "%s %s — %s\n", s.tag(), label, detail)
		} else {
			fmt.Fprintf(out, "%s %s\n", s.tag(), label)
		}
	}

	ins, err := config.InspectSetup(path, grpc)
	if err != nil {
		// Malformed settings.json: report and skip the settings-derived checks.
		line(checkWarn, "settings.json", "unreadable: "+err.Error())
	} else if !ins.SettingsExist {
		line(checkWarn, "settings.json", "not found — run `wlog setup`")
	} else {
		if ins.OTelConfigured {
			line(checkOK, "OTel telemetry env", "configured")
		} else {
			line(checkWarn, "OTel telemetry env", "missing — run `wlog setup` (live + tool-decision data needs it)")
		}
		if ins.OTelConfigured {
			if ins.PortMatches {
				line(checkOK, "endpoint port", fmt.Sprintf("matches (%d)", grpc))
			} else {
				line(checkWarn, "endpoint port", fmt.Sprintf("settings point at :%d but wlog uses :%d — re-run `wlog setup`", ins.EndpointPort, grpc))
			}
			if ins.PortMatches && !ins.ProtocolGRPC {
				line(checkWarn, "OTLP protocol", "endpoint is the gRPC port but OTEL_EXPORTER_OTLP_PROTOCOL isn't grpc — re-run `wlog setup`")
			}
		}
		if ins.StatusLine {
			line(checkOK, "status line", "registered")
		} else {
			line(checkWarn, "status line", "not registered — run `wlog setup`")
		}
		if ins.SessionEndHook {
			line(checkOK, "SessionEnd hook", "registered")
		} else {
			line(checkWarn, "SessionEnd hook", "not registered — run `wlog setup`")
		}
		if ins.PromptsEnabled {
			line(checkInfo, "prompt-text capture", "ON (OTEL_LOG_USER_PROMPTS=1)")
		} else {
			line(checkInfo, "prompt-text capture", "off (privacy default; `wlog setup --prompts` to enable)")
		}
	}

	// Transcript dir (zero-config JSONL source).
	dir := c.ClaudeDir
	if dir == "" {
		dir = config.DefaultClaudeDir()
	}
	if dir != "" {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			line(checkOK, "transcript dir", dir)
		} else {
			line(checkWarn, "transcript dir", dir+" — not found (zero-config history unavailable)")
		}
	}

	// DB readable + data freshness (read-only; never creates/migrates the DB).
	if c.DBPath == "" && !c.Memory {
		c.DBPath = config.DefaultDBPathNoCreate()
	}
	checkData(ctx, c, line)

	fmt.Fprintln(out, "\nafter `wlog setup`, restart Claude Code so it picks up the env.")
	return nil
}

// checkData reports DB readability and whether telemetry has arrived recently,
// strictly read-only. Missing/old-schema DBs degrade to WARN (exit 0).
func checkData(ctx context.Context, c *config.Config, line func(checkState, string, string)) {
	st, err := store.OpenReader(c.DBPath, c.Memory, storeBusyTimeoutMS)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrDBNotFound):
			line(checkWarn, "database", "no data yet — run a Claude Code session (or `wlog` to ingest transcripts)")
		case errors.Is(err, store.ErrDBNeedsUpgrade):
			line(checkWarn, "database", "from an older wlog version — run `wlog` once to upgrade")
		default:
			line(checkWarn, "database", "not readable: "+err.Error())
		}
		return
	}
	defer func() { _ = st.Close() }()

	id, err := st.LatestSessionID(ctx)
	if err != nil || id == "" {
		line(checkWarn, "data flowing", "no sessions recorded yet")
		return
	}
	sm, found, err := st.SessionSummary(ctx, id)
	if err != nil || !found {
		line(checkOK, "database", "readable")
		return
	}
	age := time.Since(time.UnixMilli(sm.LastSeen))
	if age <= recentWindow {
		line(checkOK, "data flowing", fmt.Sprintf("latest session %s ago", shortDur(age)))
	} else {
		line(checkWarn, "data flowing", fmt.Sprintf("latest session %s ago — telemetry may be off; restart Claude Code after `wlog setup`", shortDur(age)))
	}
}

// shortDur renders a coarse human duration for the doctor ("3h", "2d", "5m").
func shortDur(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return "just now"
	}
}

// --- SessionEnd hook -------------------------------------------------------

// HookSessionEnd renders a one-shot summary of the just-ended session for
// Claude Code's SessionEnd hook. It reads the hook JSON (session_id) from stdin,
// opens the store READ-ONLY, prints the same report as `wlog last`, and ALWAYS
// returns nil and exits 0 — a hook must never wedge Claude Code's session
// teardown, and never makes a network call.
func HookSessionEnd(ctx context.Context, c *config.Config) error {
	if c.DBPath == "" && !c.Memory {
		c.DBPath = config.DefaultDBPathNoCreate()
	}
	sessionID := readStatuslineSessionID(os.Stdin)
	if report := renderSessionEnd(ctx, c, sessionID); report != "" {
		fmt.Fprint(os.Stdout, report)
	}
	return nil
}

// renderSessionEnd builds the session summary string (empty on any failure).
func renderSessionEnd(ctx context.Context, c *config.Config, sessionID string) string {
	st, err := store.OpenReader(c.DBPath, c.Memory, storeBusyTimeoutMS)
	if err != nil {
		return ""
	}
	defer func() { _ = st.Close() }()

	// An explicit session_id from the hook must NOT be substituted: if it hasn't
	// been ingested yet (OTLP/JSONL lag) we print nothing rather than an unrelated
	// older session's summary. The latest-session fallback applies only when no id
	// was piped (manual/test invocation).
	if sessionID == "" {
		if sessionID, err = st.LatestSessionID(ctx); err != nil || sessionID == "" {
			return ""
		}
	}
	sm, found, err := st.SessionSummary(ctx, sessionID)
	if err != nil || !found {
		return ""
	}

	rejects, _ := st.ToolDecisionsBySession(ctx, sessionID, true)
	results, _ := st.ToolResultsBySession(ctx, sessionID, defaultLastN)
	prompts, _ := st.UserPromptsBySession(ctx, sessionID, defaultLastN)

	b := &strings.Builder{}
	// No color: hook output is captured by Claude Code, not a terminal.
	writeLastReport(b, sm, rejects, results, prompts, defaultLastN, false)
	return b.String()
}

// --- first-run nudge -------------------------------------------------------

// maybeSetupNudge prints a one-time hint to run `wlog setup` when the default
// `wlog` server starts and telemetry is not wired to it. It only nudges an
// interactive human (tty), once per machine (a sentinel beside the DB), and
// never on in-memory runs. Best-effort: it never fails the run path.
func maybeSetupNudge(c *config.Config) {
	if c.Memory || c.DBPath == "" || !isTerminal(os.Stdout) {
		return
	}
	if config.OTelConfiguredForPort(c.OTLPGRPCPort) {
		return
	}
	sentinel := filepath.Join(filepath.Dir(c.DBPath), ".setup-nudge-seen")
	if _, err := os.Stat(sentinel); err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "wlog: tip: live + tool-decision data is OFF. Run `wlog setup` to enable it, then restart Claude Code.")
	_ = os.WriteFile(sentinel, []byte("1\n"), 0o600)
}
