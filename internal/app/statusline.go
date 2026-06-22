package app

// statusline.go implements `wlog statusline` (PLAN v3 §2): a single status line
// for Claude Code's configurable status bar. Claude Code invokes the configured
// `statusLine.command` on every refresh and pipes a small JSON object describing
// the current session to the command's stdin; the command's first stdout line
// becomes the status line.
//
// Two invariants drive this file:
//  1. It must NEVER fail the status line. Any error (no stdin, broken JSON, store
//     open failure, no session) results in a benign one-line (or empty) output
//     and exit 0 — the status bar must not show wlog errors or a stack trace.
//  2. It must be fast (≤100ms, PLAN v3 §2): one lightweight snapshot query, then
//     the store is closed immediately. SessionSnapshot is a single indexed read.
//
// `--install` instead merges the statusLine command into ~/.claude/settings.json
// (config.InstallStatusLine) and exits.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/openwong2kim/wlog/internal/config"
	"github.com/openwong2kim/wlog/internal/store"
)

// statuslineInput is the subset of Claude Code's status-line stdin JSON that we
// consume. The schema is parsed leniently: only session_id is required, and both
// snake_case (session_id) and camelCase (sessionId) spellings are accepted to be
// robust to Claude Code key drift (the same principle the otel/jsonl parsers use
// for unknown keys). All other fields (cwd, model, transcript_path, workspace)
// are ignored for now — the snapshot comes from the store, keyed by session id.
type statuslineInput struct {
	SessionID  string `json:"session_id"`
	SessionID2 string `json:"sessionId"`
}

// sessionID returns whichever spelling of the id was present.
func (s statuslineInput) sessionID() string {
	if s.SessionID != "" {
		return s.SessionID
	}
	return s.SessionID2
}

// Statusline emits one status line for Claude Code's status bar (or, with
// install=true, registers the command in settings.json). It always returns nil
// on the render path: a status line must never surface an error to the host, so
// every failure degrades to a minimal / empty line and exit 0.
func Statusline(ctx context.Context, c *config.Config, install bool) error {
	if install {
		return installStatusline()
	}

	// Read whatever Claude Code piped on stdin (it may be empty when invoked by
	// hand). A decode failure is non-fatal — we fall back to the latest session.
	sessionID := readStatuslineSessionID(os.Stdin)

	line := renderStatusline(ctx, c, sessionID)
	// Always print exactly one line (possibly empty); the status bar takes the
	// first line of stdout.
	fmt.Fprintln(os.Stdout, line)
	return nil
}

// readStatuslineSessionID decodes the status-line JSON from r and returns the
// session id, or "" when stdin is empty / not JSON / lacks the field. It never
// errors — an unreadable stdin simply yields "" (the caller then uses the latest
// session). Reads are bounded so a misbehaving host cannot make us block on a
// huge payload.
func readStatuslineSessionID(r io.Reader) string {
	if r == nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(r, 1<<20)) // 1 MiB is far more than the tiny status JSON
	if err != nil || len(strings.TrimSpace(string(data))) == 0 {
		return ""
	}
	var in statuslineInput
	if err := json.Unmarshal(data, &in); err != nil {
		return ""
	}
	return in.sessionID()
}

// renderStatusline builds the one-line snapshot string. It opens the store
// read-only (store.OpenReader: no schema/migration/chmod side effects — critical
// because Claude Code calls statusline every few seconds, and a status-line
// refresh must never create or migrate the DB, nor contend with the wlog server's
// writer), resolves the session (the piped id, falling back to the latest), and
// formats the PLAN v3 §2 line. Every failure — including a missing DB, an
// old-schema DB read-only mode cannot migrate, or any open error — returns "" so
// Statusline can still print a (blank) line and exit 0; the status bar must never
// break.
func renderStatusline(ctx context.Context, c *config.Config, sessionID string) string {
	// Fill the default DB path for the direct-call/test path; never Validate()
	// (statusline binds no port, and a validation error must not break the bar).
	if c.DBPath == "" && !c.Memory {
		c.DBPath = config.DefaultDBPathNoCreate()
	}

	// OpenReader's error cases (ErrDBNotFound / ErrDBNeedsUpgrade / downgrade guard)
	// all degrade to a blank line here: the status bar shows nothing rather than an
	// error, and read-only mode guarantees we did not touch the DB on the way.
	st, err := store.OpenReader(c.DBPath, c.Memory, storeBusyTimeoutMS)
	if err != nil {
		return ""
	}
	defer func() { _ = st.Close() }()

	if sessionID == "" {
		sessionID, err = st.LatestSessionID(ctx)
		if err != nil || sessionID == "" {
			return ""
		}
	}

	snap, found, err := st.SessionSnapshot(ctx, sessionID)
	if err != nil || !found {
		// The piped session id may not be in our DB yet (e.g. OTLP not enabled and
		// no transcript scanned). Fall back to the latest session we do have.
		latest, lerr := st.LatestSessionID(ctx)
		if lerr != nil || latest == "" || latest == sessionID {
			return ""
		}
		snap, found, err = st.SessionSnapshot(ctx, latest)
		if err != nil || !found {
			return ""
		}
	}

	return formatStatusline(snap)
}

// formatStatusline renders a SessionSnapshot as the PLAN v3 §2 one-liner:
//
//	$0.42 · in 128.4K/out 12.1K · cache 71% · ⊘2 reject · Bash
//
// No color is emitted (the status bar host styles the line). The reject segment
// is dropped when there are no rejects, and the tool segment when no tool has
// run yet, keeping the line as short as the data allows (the status bar is
// narrow).
func formatStatusline(s store.SessionSnapshot) string {
	parts := []string{
		"$" + cost2(s.CostUSD),
		fmt.Sprintf("in %s/out %s", tokensHuman(s.Tokens.Input), tokensHuman(s.Tokens.Output)),
		"cache " + pct(s.CacheHitRatio),
	}
	if s.ToolReject > 0 {
		parts = append(parts, fmt.Sprintf("⊘%d reject", s.ToolReject))
	}
	if s.LastTool != "" {
		parts = append(parts, s.LastTool)
	}
	return strings.Join(parts, " · ")
}

// installStatusline merges the wlog status-line command into the Claude Code
// settings.json and prints the result. It is the `--install` path of Statusline.
func installStatusline() error {
	path, err := config.ClaudeSettingsPath()
	if err != nil {
		return err
	}
	changed, err := config.InstallStatusLine(path)
	if err != nil {
		return err
	}
	if changed {
		fmt.Fprintf(os.Stdout, "wlog statusline installed in %s\n", path)
		fmt.Fprintln(os.Stdout, "restart Claude Code (or reload its config) to see the status line.")
	} else {
		fmt.Fprintf(os.Stdout, "wlog statusline already configured in %s (no change)\n", path)
	}
	return nil
}
