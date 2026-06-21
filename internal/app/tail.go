package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/openwong2kim/wlog/internal/config"
	"github.com/openwong2kim/wlog/internal/ingest"
	"github.com/openwong2kim/wlog/internal/otel"
	"github.com/openwong2kim/wlog/internal/receiver"
	"github.com/openwong2kim/wlog/internal/store"
	"github.com/openwong2kim/wlog/internal/version"
)

// ANSI escape codes used by --tail (DESIGN §6: bold by default, yellow for
// reject only; stripped entirely when output is not a terminal).
const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiYellow = "\033[33m"
)

// maxBashLen is the --tail truncation length for bash_command (DESIGN §6: the
// full value lives in the UI).
const maxBashLen = 80

// Tail streams live events to the terminal in the DESIGN §6 CLI format, without
// the web UI or browser. It stands up the OTLP receiver + ingest pipeline + bus
// (the store is opened per cfg so events still persist), subscribes to the live
// bus, and prints each event as a formatted line until ctx is cancelled.
//
// When sessionID is non-empty only events belonging to that session are
// printed. Because the live bus log event does not itself carry a session id,
// the active session is read from the coalesced "now" snapshot (which the
// pipeline updates atomically just before publishing each batch's events).
func Tail(ctx context.Context, c *config.Config, sessionID string) error {
	// The CLI resolves env + flags (flags winning) before calling Tail, so we do
	// NOT re-apply WLOG_* here (it would clobber explicit flags, C7); we only
	// re-validate the already-resolved config.
	if err := c.Validate(); err != nil {
		return err
	}

	st, err := store.Open(c.DBPath, c.Memory, storeBusyTimeoutMS)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = st.Close() }()

	parser := otel.NewParser(otel.Options{NoStorePrompts: c.NoStorePrompts})
	bus := ingest.NewBus(busRingSize)
	pipeline := ingest.New(parser, st, bus, pipelineQueueSize)
	if states, err := st.LoadSeriesStates(ctx); err == nil {
		pipeline.Seed(states)
	}
	pipeline.Start(ctx)

	recv := receiver.New(c, pipeline)
	if err := recv.Start(ctx); err != nil {
		_ = pipeline.Shutdown(context.Background())
		return err
	}

	color := isTerminal(os.Stdout)
	fmt.Fprintf(os.Stderr, "wlog %s --tail  otlp=grpc://%s,http://%s\n",
		version.Short(), recv.GRPCAddr(), recv.HTTPAddr())

	sub := bus.Subscribe()
	defer sub.Close()

	for {
		select {
		case <-ctx.Done():
			// Graceful shutdown: stop the receiver, drain the pipeline.
			rctx, rcancel := context.WithTimeout(context.Background(), shutdownReceiver)
			_ = recv.Shutdown(rctx)
			rcancel()
			pctx, pcancel := context.WithTimeout(context.Background(), shutdownPipeline)
			_ = pipeline.Shutdown(pctx)
			pcancel()
			return nil

		case msg, ok := <-sub.C():
			if !ok {
				return nil
			}
			if msg.Kind == "_drop" {
				fmt.Fprintf(os.Stdout, "... %d events dropped\n", msg.Dropped)
				continue
			}
			// Session filter: compare against the live snapshot's current session.
			if sessionID != "" && bus.Snapshot().SessionID != sessionID {
				continue
			}
			fmt.Fprintln(os.Stdout, formatTailLine(msg.Event, color))
		}
	}
}

// formatTailLine renders one live event as a DESIGN §6 --tail line:
//
//	[HH:mm:ss] TYPE     SUMMARY
//
// TYPE is left-padded to 8 columns. When color is true ANSI bold wraps the
// whole line and reject decisions are additionally colored yellow; when false
// (piped output) no escapes are emitted.
func formatTailLine(ev ingest.LogEvent, color bool) string {
	ts := time.UnixMilli(ev.TS).Format("15:04:05")
	typ, summary, isReject := tailParts(ev)

	line := fmt.Sprintf("[%s] %-8s %s", ts, typ, summary)

	if !color {
		return line
	}
	if isReject {
		return ansiYellow + line + ansiReset
	}
	return ansiBold + line + ansiReset
}

// tailParts derives the (TYPE, SUMMARY, isReject) triple for an event from its
// kind and fields, matching the DESIGN §6 examples.
func tailParts(ev ingest.LogEvent) (typ, summary string, isReject bool) {
	f := ev.Fields
	switch ev.Kind {
	case "api_request":
		typ = "api"
		summary = fmt.Sprintf("%s in=%d out=%d cost=$%s dur=%s",
			str(f, "model"),
			intField(f, "input"), intField(f, "output"),
			cost4(floatField(f, "cost_usd")),
			dur(intField(f, "duration_ms")))
	case "api_error":
		typ = "error"
		summary = fmt.Sprintf("%s status=%d", str(f, "model"), intField(f, "status_code"))
	case "tool_decision":
		typ = "decision"
		decision := str(f, "decision")
		source := str(f, "source")
		summary = fmt.Sprintf("%s %s", decision, str(f, "tool_name"))
		if source != "" {
			summary += fmt.Sprintf(" (source=%s)", source)
		}
		isReject = strings.EqualFold(decision, "reject")
	case "tool_result":
		typ = "tool"
		ok := "ok"
		if !boolField(f, "success") {
			ok = "fail"
		}
		summary = fmt.Sprintf("%s %s", str(f, "tool_name"), ok)
		if bc := str(f, "bash_command"); bc != "" {
			summary += " " + bashCell(bc)
		} else if mcp := str(f, "mcp_server"); mcp != "" {
			summary += " mcp=" + mcp
		}
	case "user_prompt":
		typ = "prompt"
		if n := intField(f, "prompt_length"); n > 0 {
			summary = fmt.Sprintf("len=%d", n)
		}
	default:
		typ = ev.Kind
		summary = ev.Summary
	}
	if summary == "" {
		summary = ev.Summary
	}
	return typ, summary, isReject
}

// --- small field/format helpers (kept local so tail has no UI dependency) ---

func str(f map[string]any, k string) string {
	if f == nil {
		return ""
	}
	if v, ok := f[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprintf("%v", v)
	}
	return ""
}

func intField(f map[string]any, k string) int64 {
	if f == nil {
		return 0
	}
	switch v := f[k].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case int32:
		return int64(v)
	case float64:
		return int64(v)
	}
	return 0
}

func floatField(f map[string]any, k string) float64 {
	if f == nil {
		return 0
	}
	switch v := f[k].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int64:
		return float64(v)
	case int:
		return float64(v)
	}
	return 0
}

func boolField(f map[string]any, k string) bool {
	if f == nil {
		return false
	}
	b, _ := f[k].(bool)
	return b
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// singleLine collapses s onto a single terminal line: every newline, tab, and
// other control or whitespace rune becomes a single space, runs of whitespace
// fold to one, and leading/trailing whitespace is trimmed. This is what keeps a
// multiline bash_command (heredoc bodies, `&&`-chains, stray \r/\t) from
// spilling across several rows and wrecking the fixed-width `HH:MM:SS  status
// cmd` columns of `wlog last` / --tail. It is length-agnostic — callers apply
// truncate afterwards (see bashCell) so the 80-char boundary and "..." marker
// stay exactly as before, just measured against the cleaned string.
func singleLine(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false   // pending separator: emit one space before the next glyph
	started := false // have we written any non-space rune yet (drops leading ws)
	for _, r := range s {
		if r == '�' {
			// Decoding error / replacement rune: treat as a space so invalid
			// bytes can never smuggle a control glyph onto the line.
			space = started
			continue
		}
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			space = started
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
		started = true
	}
	return b.String()
}

// bashCell renders a possibly multiline command for a single fixed-width cell:
// first single-line it, then truncate to maxBashLen. Order matters — folding
// runs out the hidden newlines/tabs before the length is measured, so the 80
// char budget is spent on visible characters rather than on a "..." that hides
// a newline. last.go and tail.go share this so both views behave identically.
func bashCell(s string) string {
	return truncate(singleLine(s), maxBashLen)
}

// cost4 renders a USD amount with 4 decimals (DESIGN §7 sub-$1 form), good
// enough for the terminal tail (the UI uses the full format module).
func cost4(v float64) string {
	return fmt.Sprintf("%.4f", v)
}

// dur renders a duration given in milliseconds as DESIGN §7: ms / s / m s.
func dur(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	default:
		sec := ms / 1000
		return fmt.Sprintf("%dm %ds", sec/60, sec%60)
	}
}
