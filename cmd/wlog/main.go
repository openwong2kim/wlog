// Command wlog is the single-binary OTLP receiver + embedded web UI for
// Claude Code session telemetry. See PLAN.md §12 for the CLI surface.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/openwong2kim/wlog/internal/app"
	"github.com/openwong2kim/wlog/internal/config"
	"github.com/openwong2kim/wlog/internal/version"
)

// flags captures raw flag values that need post-processing (durations, byte
// sizes) before being resolved into a config.Config.
type flags struct {
	retention string // e.g. "90d"
	maxRecv   string // e.g. "16MB"
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "wlog: error: %v\n", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cfg := config.Default()
	f := &flags{
		retention: "90d",
		maxRecv:   "16MB",
	}

	root := &cobra.Command{
		Use:   "wlog",
		Short: "OTLP receiver + embedded UI for Claude Code telemetry",
		Long: "wlog receives Claude Code (and OTLP-compatible) telemetry, normalizes it,\n" +
			"persists to embedded SQLite, and serves an embedded web UI.",
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		// PersistentPreRunE overlays WLOG_* env onto cfg/f for every command so
		// that even --print-claude-setup (which prints before full validation)
		// observes env-configured ports. Explicit flags still win.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			applyEnv(cmd, cfg, f)
			return nil
		},
		// Default command (no subcommand) runs the receiver + UI.
		RunE: func(cmd *cobra.Command, args []string) error {
			if cfg.PrintClaudeSetup {
				fmt.Fprintln(cmd.OutOrStdout(), config.ClaudeSetupSnippet(cfg.OTLPGRPCPort))
				return nil
			}
			if err := resolve(cfg, f); err != nil {
				return err
			}
			ctx, stop := signalContext()
			defer stop()
			return app.Run(ctx, cfg)
		},
	}

	bindGlobalFlags(root, cfg, f)
	root.SetVersionTemplate(version.String() + "\n")

	root.AddCommand(newTailCmd(cfg, f))
	root.AddCommand(newLastCmd(cfg, f))
	root.AddCommand(newStatuslineCmd(cfg, f))
	root.AddCommand(newExportCmd(cfg, f))
	root.AddCommand(newVersionCmd())

	return root
}

// bindGlobalFlags registers every PLAN §12 flag on the root command. These are
// persistent so subcommands (tail/export) share the same configuration knobs.
func bindGlobalFlags(cmd *cobra.Command, cfg *config.Config, f *flags) {
	pf := cmd.PersistentFlags()
	pf.IntVar(&cfg.OTLPGRPCPort, "otlp-grpc-port", cfg.OTLPGRPCPort, "OTLP/gRPC listener port")
	pf.IntVar(&cfg.OTLPHTTPPort, "otlp-http-port", cfg.OTLPHTTPPort, "OTLP/HTTP listener port")
	pf.IntVar(&cfg.UIPort, "ui-port", cfg.UIPort, "web UI port (0 = auto-select free port)")
	pf.StringVar(&cfg.Listen, "listen", cfg.Listen, "bind address")
	pf.BoolVar(&cfg.Unsafe, "unsafe", cfg.Unsafe, "allow non-loopback bind without auth")
	pf.StringVar(&cfg.AuthToken, "auth-token", cfg.AuthToken, "auth token required for non-loopback access")
	pf.StringVar(&cfg.DBPath, "db", cfg.DBPath, "SQLite database path (default: platform data dir)")
	pf.BoolVar(&cfg.Memory, "memory", cfg.Memory, "use an in-memory database")
	pf.StringVar(&f.retention, "retention", f.retention, "data retention window (e.g. 90d, 72h)")
	pf.IntVar(&cfg.MaxSessions, "max-sessions", cfg.MaxSessions, "max retained sessions (oldest pruned first)")
	pf.StringVar(&f.maxRecv, "max-recv-bytes", f.maxRecv, "max size of a single OTLP message (e.g. 16MB)")
	pf.BoolVar(&cfg.NoOpen, "no-open", cfg.NoOpen, "do not auto-open the browser")
	pf.BoolVar(&cfg.NoStorePrompts, "no-store-prompts", cfg.NoStorePrompts, "do not persist prompts / tool arguments")
	pf.BoolVar(&cfg.PrintClaudeSetup, "print-claude-setup", cfg.PrintClaudeSetup, "print the Claude Code OTel env snippet and exit")
	pf.BoolVar(&cfg.NoJSONL, "no-jsonl", cfg.NoJSONL, "disable zero-config Claude Code transcript (JSONL) ingestion")
	pf.StringVar(&cfg.ClaudeDir, "claude-dir", cfg.ClaudeDir, "directory of Claude Code transcripts (default: ~/.claude/projects)")
}

func newTailCmd(cfg *config.Config, f *flags) *cobra.Command {
	var session string
	cmd := &cobra.Command{
		Use:   "tail",
		Short: "Stream live events to the terminal",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := resolve(cfg, f); err != nil {
				return err
			}
			ctx, stop := signalContext()
			defer stop()
			return app.Tail(ctx, cfg, session)
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "session id to tail (default: active session)")
	return cmd
}

// newLastCmd builds `wlog last`: a one-shot terminal summary of the latest (or
// a chosen) session. It is a pure read path that binds no listener and opens the
// store read-only, so — exactly like statusline — the server-oriented
// resolve()/Validate() (bind address, port ranges, --unsafe/--auth-token policy,
// recv limits) must NOT run: in a server-configured environment (e.g. WLOG_LISTEN
// set to a non-loopback address without --unsafe) Validate would reject a config
// that is irrelevant to a read-only summary. Resolve only the minimum the read
// path needs (the DB path), with no validation (resolveReadOnly).
func newLastCmd(cfg *config.Config, _ *flags) *cobra.Command {
	var (
		session string
		n       int
	)
	cmd := &cobra.Command{
		Use:   "last",
		Short: "Print a summary of the latest (or a chosen) session",
		RunE: func(cmd *cobra.Command, args []string) error {
			resolveReadOnly(cfg)
			ctx, stop := signalContext()
			defer stop()
			return app.Last(ctx, cfg, session, n)
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "session id to summarize (default: latest session)")
	cmd.Flags().IntVar(&n, "n", 10, "number of recent tool results (bash) to list")
	return cmd
}

// newStatuslineCmd builds `wlog statusline`: it reads Claude Code's status-line
// JSON from stdin and prints one status line. With --install it instead merges
// the statusLine command into ~/.claude/settings.json. The render path must
// never fail the status bar, so app.Statusline returns nil on any data error.
func newStatuslineCmd(cfg *config.Config, f *flags) *cobra.Command {
	var install bool
	cmd := &cobra.Command{
		Use:   "statusline",
		Short: "Emit one status line for Claude Code's status bar (--install to register)",
		RunE: func(cmd *cobra.Command, args []string) error {
			// statusline is a read-only path: --install only touches settings.json,
			// and the render path opens the store read-only. Neither binds a port, so
			// the server-oriented resolve()/Validate() (bind address, port ranges,
			// --unsafe/--auth-token policy, recv limits) must NOT run — in a
			// server-configured environment (e.g. WLOG_LISTEN set to a non-loopback
			// address without --unsafe) Validate would error and break the status
			// bar. Resolve only the minimum the read path needs (the DB path), with
			// no validation. app.Statusline still swallows any data error and prints
			// a single line, so the bar never shows a wlog failure.
			resolveReadOnly(cfg)
			ctx, stop := signalContext()
			defer stop()
			return app.Statusline(ctx, cfg, install)
		},
	}
	cmd.Flags().BoolVar(&install, "install", false, "register the status line in ~/.claude/settings.json and exit")
	return cmd
}

func newExportCmd(cfg *config.Config, f *flags) *cobra.Command {
	var (
		session string
		format  string
		out     string
	)
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export a session to a file or stdout",
		RunE: func(cmd *cobra.Command, args []string) error {
			if session == "" {
				return fmt.Errorf("--session is required")
			}
			if err := resolve(cfg, f); err != nil {
				return err
			}
			ctx, stop := signalContext()
			defer stop()
			return app.Export(ctx, cfg, session, format, out)
		},
	}
	cmd.Flags().StringVar(&session, "session", "", "session id to export (required)")
	cmd.Flags().StringVar(&format, "format", "json", "export format (json)")
	cmd.Flags().StringVar(&out, "out", "", "output file (default: stdout)")
	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), version.String())
			return nil
		},
	}
}

// resolve finalizes the configuration after env overlay (done in
// PersistentPreRunE): it parses string-form flags (durations, byte sizes),
// fills in the default DB path when needed, and validates.
func resolve(cfg *config.Config, f *flags) error {
	d, err := config.ParseRetention(f.retention)
	if err != nil {
		return err
	}
	cfg.Retention = d

	b, err := config.ParseBytes(f.maxRecv)
	if err != nil {
		return err
	}
	cfg.MaxRecvBytes = b

	if cfg.DBPath == "" && !cfg.Memory {
		cfg.DBPath = config.DefaultDBPath()
	}
	return cfg.Validate()
}

// resolveReadOnly prepares the minimum config a read-only command (statusline,
// last) needs: the default DB path when none was supplied. It deliberately does
// NOT run cfg.Validate() — these commands bind no port and open the store
// read-only, so the server bind policy (non-loopback listen requiring
// --unsafe/--auth-token, port ranges, recv limits) is irrelevant and must not be
// able to fail them. Retention / max-recv-bytes are server-only knobs and are
// left unparsed here.
func resolveReadOnly(cfg *config.Config) {
	if cfg.DBPath == "" && !cfg.Memory {
		cfg.DBPath = config.DefaultDBPath()
	}
}

// changed reports whether the named flag was explicitly set on the command
// line (checking both the command's own and inherited persistent flags).
func changed(cmd *cobra.Command, name string) bool {
	if fl := cmd.Flags().Lookup(name); fl != nil {
		return fl.Changed
	}
	return false
}

// applyEnv overlays WLOG_* environment variables onto cfg/f, but never
// overrides a flag the user set explicitly. Cobra applies flags into cfg
// directly; for those scalar fields we read the env value into a fresh config
// and copy it across only when the corresponding flag is unchanged.
func applyEnv(cmd *cobra.Command, cfg *config.Config, f *flags) {
	env := config.Default()
	env.ApplyEnv()

	if !changed(cmd, "otlp-grpc-port") {
		cfg.OTLPGRPCPort = env.OTLPGRPCPort
	}
	if !changed(cmd, "otlp-http-port") {
		cfg.OTLPHTTPPort = env.OTLPHTTPPort
	}
	if !changed(cmd, "ui-port") {
		cfg.UIPort = env.UIPort
	}
	if !changed(cmd, "listen") {
		cfg.Listen = env.Listen
	}
	if !changed(cmd, "unsafe") {
		cfg.Unsafe = env.Unsafe
	}
	if !changed(cmd, "auth-token") {
		cfg.AuthToken = env.AuthToken
	}
	if !changed(cmd, "db") {
		cfg.DBPath = env.DBPath
	}
	if !changed(cmd, "memory") {
		cfg.Memory = env.Memory
	}
	if !changed(cmd, "max-sessions") {
		cfg.MaxSessions = env.MaxSessions
	}
	if !changed(cmd, "no-open") {
		cfg.NoOpen = env.NoOpen
	}
	if !changed(cmd, "no-store-prompts") {
		cfg.NoStorePrompts = env.NoStorePrompts
	}
	if !changed(cmd, "no-jsonl") {
		cfg.NoJSONL = env.NoJSONL
	}
	if !changed(cmd, "claude-dir") {
		cfg.ClaudeDir = env.ClaudeDir
	}
	if !changed(cmd, "retention") {
		if v := os.Getenv("WLOG_RETENTION"); v != "" {
			f.retention = v
		}
	}
	if !changed(cmd, "max-recv-bytes") {
		if v := os.Getenv("WLOG_MAX_RECV_BYTES"); v != "" {
			f.maxRecv = v
		}
	}
}

// signalContext returns a context cancelled on SIGINT/SIGTERM for graceful
// shutdown, plus a stop function to release the signal handler.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	return ctx, cancel
}
