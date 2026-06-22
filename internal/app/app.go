// Package app wires together the wlog subsystems (receiver, ingest, store,
// api, web) and manages process lifecycle (start + graceful shutdown).
//
// Run assembles the full single-binary server: it opens the store, runs an
// initial retention pass, builds the OTLP parser + ingest pipeline + live bus,
// starts the OTLP receiver (gRPC/HTTP) and the embedded web UI, prints the
// DESIGN §6 startup line, optionally opens a browser, schedules periodic
// retention, and blocks until ctx is cancelled — at which point it tears the
// stack down in the order receiver → pipeline (drain) → api → store (PLAN §5).
package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/openwong2kim/wlog/internal/api"
	"github.com/openwong2kim/wlog/internal/config"
	"github.com/openwong2kim/wlog/internal/ingest"
	"github.com/openwong2kim/wlog/internal/jsonl"
	"github.com/openwong2kim/wlog/internal/otel"
	"github.com/openwong2kim/wlog/internal/receiver"
	"github.com/openwong2kim/wlog/internal/store"
	"github.com/openwong2kim/wlog/internal/version"
)

// Tunables for the lifecycle. Kept as vars (not consts) so tests can shorten
// them if needed; production uses the defaults.
const (
	busRingSize        = 256
	pipelineQueueSize  = 1024
	storeBusyTimeoutMS = 5000
	retentionInterval  = time.Hour
	shutdownReceiver   = 5 * time.Second
	shutdownPipeline   = 5 * time.Second
	shutdownAPI        = 5 * time.Second
)

// Run starts the OTLP receiver and embedded web UI and blocks until ctx is
// cancelled, then performs a graceful shutdown. It is the entry point for the
// default `wlog` command.
func Run(ctx context.Context, c *config.Config) error {
	// The CLI resolves env + flags (with flags winning) before calling Run, so we
	// do NOT re-apply WLOG_* here: doing so would let a stale env value clobber an
	// explicit --flag (C7). We only re-validate the already-resolved config (Run
	// is also callable directly from tests/embeds, which pass a finished config).
	if err := c.Validate(); err != nil {
		return err
	}

	// --- Store ---------------------------------------------------------------
	st, err := store.Open(c.DBPath, c.Memory, storeBusyTimeoutMS)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	// From here on, any error path must Close the store.

	// Initial retention pass: prune stale data before serving.
	if err := st.RunRetention(ctx, c.Retention, c.MaxSessions); err != nil {
		warnf("initial retention pass failed: %v", err)
	}

	// --- Ingest pipeline + live bus ------------------------------------------
	parser := otel.NewParser(otel.Options{NoStorePrompts: c.NoStorePrompts})
	bus := ingest.NewBus(busRingSize)
	pipeline := ingest.New(parser, st, bus, pipelineQueueSize)

	// Resume delta normalization from persisted series cursors before any
	// ingestion begins (PLAN §7).
	states, err := st.LoadSeriesStates(ctx)
	if err != nil {
		warnf("load series state failed (delta normalization restarts from baseline): %v", err)
	} else {
		pipeline.Seed(states)
	}
	pipeline.Start(ctx)

	// --- OTLP receiver -------------------------------------------------------
	recv := receiver.New(c, pipeline)
	if err := recv.Start(ctx); err != nil {
		// receiver.Start already formats a DESIGN §6 hint; the single "wlog: error:"
		// prefix is added once by main (F4).
		_ = st.Close()
		return err
	}

	// --- Zero-config JSONL transcript source ---------------------------------
	// Scan + watch ~/.claude/projects so installed-immediately history shows up
	// without enabling OTLP (PLAN v3 feature 0). It feeds the SAME pipeline/store
	// and dedups against OTLP via shared dedup_keys (EQ6). A missing directory is
	// skipped silently inside the source (OTLP-only stays fully functional).
	if !c.NoJSONL {
		dir := c.ClaudeDir
		if dir == "" {
			dir = config.DefaultClaudeDir()
		}
		if dir != "" {
			src := ingest.NewJSONLSource(dir, pipeline, jsonl.Options{NoStorePrompts: c.NoStorePrompts}, 0)
			go func() { _ = src.Run(ctx) }()
		}
	}

	// --- Web UI / JSON API ---------------------------------------------------
	dbPath := c.DBPath
	if c.Memory {
		dbPath = ":memory:"
	}
	apiSrv := api.New(c, st.ReadDB(), bus, pipeline.Counters, dbPath)

	uiAddr := net.JoinHostPort(c.Listen, fmt.Sprintf("%d", c.UIPort))
	uiLn, err := net.Listen("tcp", uiAddr)
	if err != nil {
		_ = recv.Shutdown(context.Background())
		_ = pipeline.Shutdown(context.Background())
		_ = st.Close()
		return uiPortErr(uiAddr, c.UIPort, err)
	}
	httpSrv := &http.Server{
		Handler:           apiSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		if err := httpSrv.Serve(uiLn); err != nil && err != http.ErrServerClosed {
			warnf("web UI server error: %v", err)
		}
	}()

	// --- Startup banner (DESIGN §6) ------------------------------------------
	uiURL := fmt.Sprintf("http://%s", displayHost(c.Listen, uiLn.Addr()))
	fmt.Fprintln(os.Stdout, startupLine(c, recv, uiURL))

	// --- Browser auto-open ---------------------------------------------------
	if !c.NoOpen {
		if err := openBrowser(uiURL); err != nil {
			warnf("could not open browser automatically (%v); visit %s", err, uiURL)
		}
	}

	// --- First-run setup nudge -----------------------------------------------
	// One-time hint to run `wlog setup` when telemetry isn't wired to us yet.
	maybeSetupNudge(c)

	// --- Periodic retention --------------------------------------------------
	ticker := time.NewTicker(retentionInterval)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := st.RunRetention(ctx, c.Retention, c.MaxSessions); err != nil {
					warnf("periodic retention failed: %v", err)
				}
			}
		}
	}()

	// --- Block until shutdown signalled --------------------------------------
	<-ctx.Done()

	return shutdown(recv, pipeline, apiSrv, httpSrv, st)
}

// shutdown tears the stack down in the order required for a clean drain
// (PLAN §5): stop accepting OTLP (receiver listeners) → drain the ingest queue
// (commit in-flight) → stop the UI/API server → close the store. Each step gets
// its own deadline so a wedged stage cannot block the whole shutdown.
func shutdown(recv *receiver.Server, pipeline *ingest.Pipeline, apiSrv *api.Server, httpSrv *http.Server, st *store.Store) error {
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// 1. Receiver: stop accepting new OTLP so the pipeline queue can settle.
	rctx, rcancel := context.WithTimeout(context.Background(), shutdownReceiver)
	if err := recv.Shutdown(rctx); err != nil {
		warnf("receiver shutdown: %v", err)
		note(err)
	}
	rcancel()

	// 2. Pipeline: drain the bounded queue, committing buffered batches.
	pctx, pcancel := context.WithTimeout(context.Background(), shutdownPipeline)
	if err := pipeline.Shutdown(pctx); err != nil {
		warnf("pipeline drain: %v", err)
		note(err)
	}
	pcancel()

	// 3. UI/API: stop serving (no in-flight writes depend on it).
	actx, acancel := context.WithTimeout(context.Background(), shutdownAPI)
	if err := httpSrv.Shutdown(actx); err != nil {
		warnf("web UI shutdown: %v", err)
		note(err)
	}
	_ = apiSrv.Shutdown(actx) // no-op unless apiSrv.Start was used; harmless.
	acancel()

	// 4. Store: flush the writer and close connections.
	if err := st.Close(); err != nil {
		warnf("store close: %v", err)
		note(err)
	}

	return firstErr
}

// startupLine builds the single DESIGN §6 startup summary line:
//
//	wlog v0.1.0  otlp=grpc://127.0.0.1:4317,http://127.0.0.1:4318  ui=http://127.0.0.1:7890
func startupLine(c *config.Config, recv *receiver.Server, uiURL string) string {
	grpc := recv.GRPCAddr()
	httpA := recv.HTTPAddr()
	return fmt.Sprintf("wlog %s  otlp=grpc://%s,http://%s  ui=%s",
		version.Short(), grpc, httpA, uiURL)
}

// displayHost renders a user-facing host:port for the UI URL. When the bind
// host is loopback we prefer 127.0.0.1 for a clickable URL; otherwise we use
// the actual bound address (which resolves the auto-selected port).
func displayHost(listen string, addr net.Addr) string {
	if ta, ok := addr.(*net.TCPAddr); ok {
		host := listen
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		return net.JoinHostPort(host, fmt.Sprintf("%d", ta.Port))
	}
	return addr.String()
}

// uiPortErr formats a UI bind failure with a DESIGN §6 actionable hint. The
// single "wlog: error:" prefix is added once by main (F4).
func uiPortErr(addr string, port int, err error) error {
	return fmt.Errorf("cannot bind web UI on %s: %w\n  hint: another process may be using port %d; pass a different --ui-port (or 0 to auto-select)", addr, err, port)
}

// warnf prints a DESIGN §6 warning line to stderr.
func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "wlog: warn: "+format+"\n", args...)
}
