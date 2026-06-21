package app

// last.go implements `wlog last` (PLAN v3 §1): a one-shot terminal summary of
// the latest (or a chosen) session, printed without standing up the OTLP
// receiver / web UI. It is a pure read path — it opens the store, runs the T2
// read queries (SessionSummary / ToolDecisionsBySession / ToolResultsBySession),
// formats the three DESIGN §6/§7 sections, and returns. ANSI follows the same
// rule as --tail (tail.go): bold by default, yellow for reject only, stripped
// when stdout is not a terminal.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openwong2kim/wlog/internal/config"
	"github.com/openwong2kim/wlog/internal/model"
	"github.com/openwong2kim/wlog/internal/store"
)

// defaultLastN is the fallback for `wlog last --n` (the bash block size).
const defaultLastN = 10

// Last prints the summary of one session (the latest when sessionID is empty)
// in the PLAN v3 §1 format and returns. It does not start any listener; the store
// is opened READ-ONLY (store.OpenReader) so it performs no schema/migration/chmod
// side effects — it never creates or upgrades the DB. The T2 read methods use the
// WAL read pool, so this is safe to run while a wlog server writes the same DB.
//
// Several states are normal, not errors — all exit 0 with a short hint:
//   - no DB file yet (store.ErrDBNotFound),
//   - empty DB (no sessions),
//   - an old-schema DB that read-only mode cannot migrate (store.ErrDBNeedsUpgrade).
func Last(ctx context.Context, c *config.Config, sessionID string, n int) error {
	if n <= 0 {
		n = defaultLastN
	}

	// last binds no port and opens the store read-only, so the server bind policy
	// (non-loopback listen requiring --unsafe/--auth-token, port ranges, recv
	// limits) is irrelevant and must NOT be able to fail it — exactly like
	// statusline. We therefore do NOT call c.Validate() here: a server-configured
	// environment (e.g. WLOG_LISTEN set to a non-loopback address without --unsafe)
	// would otherwise reject a perfectly valid read-only summary. The CLI resolves
	// env + flags via resolveReadOnly (filling the default DB path) before calling
	// Last; we fill the default here too so a direct app-layer call still works,
	// without re-applying WLOG_* (it would clobber explicit flags, C7).
	if c.DBPath == "" && !c.Memory {
		c.DBPath = config.DefaultDBPath()
	}

	st, err := store.OpenReader(c.DBPath, c.Memory, storeBusyTimeoutMS)
	if err != nil {
		// A missing DB or a not-yet-upgraded DB is a normal state for a read-only
		// summary: guide the user and exit 0 rather than failing.
		if errors.Is(err, store.ErrDBNotFound) {
			fmt.Fprintln(os.Stdout, "no database yet. run `wlog --print-claude-setup` to enable Claude Code telemetry, or open a Claude Code session so a transcript is recorded.")
			return nil
		}
		if errors.Is(err, store.ErrDBNeedsUpgrade) {
			fmt.Fprintln(os.Stdout, "the wlog database is from an older version. run `wlog` once to upgrade it, then re-run `wlog last`.")
			return nil
		}
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = st.Close() }()

	if sessionID == "" {
		sessionID, err = st.LatestSessionID(ctx)
		if err != nil {
			return err
		}
		if sessionID == "" {
			// Empty DB: guide the user instead of erroring (exit 0).
			fmt.Fprintln(os.Stdout, "no sessions yet. run `wlog --print-claude-setup` to enable Claude Code telemetry, or open a Claude Code session so a transcript is recorded.")
			return nil
		}
	}

	sm, found, err := st.SessionSummary(ctx, sessionID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	rejects, err := st.ToolDecisionsBySession(ctx, sessionID, true)
	if err != nil {
		return err
	}
	results, err := st.ToolResultsBySession(ctx, sessionID, n)
	if err != nil {
		return err
	}
	prompts, err := st.UserPromptsBySession(ctx, sessionID, n)
	if err != nil {
		return err
	}

	color := isTerminal(os.Stdout)
	out := &strings.Builder{}
	writeLastReport(out, sm, rejects, results, prompts, n, color)
	fmt.Fprint(os.Stdout, out.String())
	return nil
}

// writeLastReport renders the PLAN v3 §1 sections into w. It is split out from
// Last so tests can assert the exact bytes for both the terminal (color) and
// piped (no color) modes without redirecting os.Stdout. The optional prompts
// section (④) lists the session's recent user prompts, each folded to one line.
func writeLastReport(w *strings.Builder, sm store.SessionSummary, rejects []model.ToolDecision, results []model.ToolResult, prompts []store.UserPrompt, n int, color bool) {
	// --- ① session header ----------------------------------------------------
	// session <id8>  HH:MM–HH:MM (dur)  cost=$X.XXXX  in=.. out=..  cache_hit=NN%
	//
	// I3: cost uses the DESIGN §7 sub-$1 4-decimal form (cost4, shared with
	// --tail in tail.go) — NOT the 2-decimal cost2 — so `wlog last` and `--tail`
	// print the SAME figure. A 2-decimal header collapsed distinct sub-cent costs
	// (e.g. $0.4231 and $0.4198 both showed "$0.42"); DESIGN §7 is the authority
	// for the detailed views. (The statusline keeps cost2 by design: PLAN v3 §2
	// is a deliberately narrow one-liner.)
	header := fmt.Sprintf("session %s  %s  cost=$%s  in=%s out=%s  cache_hit=%s",
		shortID(sm.ID),
		timeRange(sm.FirstSeen, sm.LastSeen),
		cost4(sm.CostUSD),
		tokensHuman(sm.Tokens.Input),
		tokensHuman(sm.Tokens.Output),
		pct(sm.CacheHitRatio),
	)
	// When token/cost came from metrics, some per-event fields are absent; be
	// honest about the source instead of implying event-level precision.
	if sm.TokenSource == "metrics" {
		header += "  (metrics-only)"
	}
	writeLine(w, header, color, true)

	// --- ② tool summary + reject list ----------------------------------------
	writeLine(w, fmt.Sprintf("tools  accept %d  reject %d  auto-approved %d",
		sm.ToolAccept, sm.ToolReject, sm.AutoApproved), color, true)
	for _, d := range rejects {
		line := "  ✕ reject  " + str2(d.ToolName)
		if d.Source != "" {
			line += fmt.Sprintf(" (source=%s)", d.Source)
		}
		if bc := bashFromDecision(d); bc != "" {
			line += "   " + bashCell(bc)
		}
		// reject rows are the one place --tail uses yellow; mirror that here.
		writeLine(w, line, color, false)
	}

	// --- ③ bash / tool result list (last n) ----------------------------------
	writeLine(w, fmt.Sprintf("bash (last %d)", n), color, true)
	for _, r := range results {
		ts := time.UnixMilli(r.TS).Format("15:04:05")
		status := "ok  "
		if !r.Success {
			status = "fail"
		}
		cmd := r.BashCommand
		if cmd == "" && r.MCPServer != "" {
			cmd = "mcp=" + r.MCPServer
		}
		if cmd == "" {
			cmd = str2(r.ToolName)
		}
		writeLine(w, fmt.Sprintf("  %s  %s %s", ts, status, bashCell(cmd)), color, false)
	}

	// --- ④ prompts (last n) --------------------------------------------------
	// The session's recent user prompts, each folded to a single line via
	// bashCell (shared singleLine + 80-char truncate) so a long or multi-line
	// prompt never breaks the column layout. A prompt stored with
	// --no-store-prompts has no text — show "(미저장)" with its length so the row
	// is still informative. Omit the section entirely when there are no prompts.
	if len(prompts) > 0 {
		writeLine(w, fmt.Sprintf("prompts (last %d)", n), color, true)
		for _, p := range prompts {
			ts := time.UnixMilli(p.TS).Format("15:04:05")
			var text string
			if p.Stored {
				text = bashCell(p.Text)
			} else {
				text = fmt.Sprintf("(미저장, len=%d)", p.Length)
			}
			writeLine(w, fmt.Sprintf("  %s  %s", ts, text), color, false)
		}
	}
}

// writeLine appends a single report line to w, applying the tail.go ANSI rule
// when color is true: bold for "structural"/header lines, yellow for the rest
// of the report rows except — there is one exception — the reject rows, which
// the caller marks as non-bold so they render yellow like --tail. To keep the
// rule identical to --tail we only ever emit bold (headers) or yellow (reject
// rows); plain detail rows are left uncolored.
//
// bold==true  -> ANSI bold (section headers).
// bold==false -> the line is a reject row (yellow) only when it starts with the
// reject glyph; ordinary bash rows stay uncolored. This keeps the palette to
// the two colors --tail already uses.
func writeLine(w *strings.Builder, line string, color, bold bool) {
	if !color {
		w.WriteString(line)
		w.WriteByte('\n')
		return
	}
	switch {
	case bold:
		w.WriteString(ansiBold + line + ansiReset)
	case strings.Contains(line, "✕ reject"):
		w.WriteString(ansiYellow + line + ansiReset)
	default:
		w.WriteString(line)
	}
	w.WriteByte('\n')
}

// bashFromDecision returns a best-effort command string for a reject row. Reject
// decisions do not carry the bash_command column (that lives on tool_result), so
// there is normally nothing to show beyond the tool + source; this hook exists so
// the format stays a single place if a future field is added. It currently
// returns "" (the §1 example shows only tool + source for rejects, with the
// command shown by the matching tool_result row when present).
func bashFromDecision(_ model.ToolDecision) string { return "" }

// --- numeric/format helpers specific to `wlog last` / statusline -------------
//
// These complement the --tail helpers in tail.go (cost4, dur, bashCell). The
// `wlog last` header now shares cost4 (DESIGN §7 4-decimal) with --tail (I3),
// and the bash list/reject rows share bashCell (single-line + 80-char truncate)
// so both views fold multiline commands onto one row identically (Bug #1). The
// remaining helpers (tokensHuman, pct) give K/M-compacted tokens and integer
// percent, matching the web UI's format.ts (formatTokens / formatPercent) so the
// CLI and UI agree.

// cost2 renders a USD amount with 2 decimals. It is the STATUSLINE cost format
// only (PLAN v3 §2: the status bar is a deliberately narrow one-liner where two
// decimals are intentional); the detailed `wlog last` / --tail views use cost4
// (DESIGN §7) instead. Negative input clamps to 0.
func cost2(v float64) string {
	if v < 0 {
		v = 0
	}
	return fmt.Sprintf("%.2f", v)
}

// tokensHuman compacts a token count the way the UI's formatTokens does:
// < 1000 -> grouped integer ("1,420"); >= 1000 -> "1.4K"; >= 1e6 -> "1.2M".
// One decimal place, trailing ".0" trimmed.
func tokensHuman(n int64) string {
	if n < 0 {
		n = 0
	}
	switch {
	case n < 1000:
		return groupInt(n)
	case n < 1_000_000:
		return trim1(float64(n)/1000) + "K"
	default:
		return trim1(float64(n)/1_000_000) + "M"
	}
}

// trim1 formats f with one decimal and drops a trailing ".0" (so 128.0 -> "128",
// 128.4 -> "128.4"), matching the UI's trim1.
func trim1(f float64) string {
	s := fmt.Sprintf("%.1f", f)
	s = strings.TrimSuffix(s, ".0")
	return s
}

// groupInt renders a non-negative integer with thousands separators ("1,420").
func groupInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
		if len(s) > pre {
			b.WriteByte(',')
		}
	}
	for i := pre; i < len(s); i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < len(s) {
			b.WriteByte(',')
		}
	}
	return b.String()
}

// pct renders a 0..1 ratio as an integer percent ("71%"), matching the UI's
// formatPercent (round, no decimals).
func pct(ratio float64) string {
	if ratio < 0 {
		ratio = 0
	}
	return fmt.Sprintf("%d%%", int(ratio*100+0.5))
}

// shortID returns the first 8 characters of a session id (or the whole id when
// shorter), matching the DESIGN §6 short-id form.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// timeRange renders "HH:MM–HH:MM (dur)" for a session's first/last seen times
// (unix-ms, local clock to match --tail). When first==last (a single instant)
// the duration is shown as 0s; the en dash matches the §1 example.
func timeRange(firstMS, lastMS int64) string {
	start := time.UnixMilli(firstMS)
	end := time.UnixMilli(lastMS)
	d := lastMS - firstMS
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%s–%s (%s)",
		start.Format("15:04"), end.Format("15:04"), dur(d))
}

// str2 returns s unless empty, in which case a dash placeholder so a missing
// tool name does not render as a blank gap.
func str2(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
