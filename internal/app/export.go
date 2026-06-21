package app

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/openwong2kim/wlog/internal/api"
	"github.com/openwong2kim/wlog/internal/config"
	"github.com/openwong2kim/wlog/internal/store"
)

// Export writes a session export in the requested format to outPath (empty
// outPath means stdout). Only "json" is supported in M2 (CSV is a follow-up,
// PLAN §2). It opens the store read-only-ish (the writer goroutine is started
// but unused) and streams the export through the shared api.ExportSession logic
// so CLI and HTTP export stay byte-identical.
func Export(ctx context.Context, c *config.Config, sessionID, format, outPath string) error {
	if sessionID == "" {
		return fmt.Errorf("--session is required")
	}
	if format != "json" {
		return fmt.Errorf("unsupported export format %q: only json supported", format)
	}

	// The CLI already resolved env + flags before calling Export, so we do NOT
	// re-apply WLOG_* here (re-applying would let a stale env value clobber an
	// explicit --flag, C7). We deliberately skip Validate() — export binds no
	// port, so the bind policy is irrelevant — but still ensure a DB path for the
	// direct-call/test path where Memory is false and DBPath was left empty.
	if c.DBPath == "" && !c.Memory {
		c.DBPath = config.DefaultDBPath()
	}

	st, err := store.Open(c.DBPath, c.Memory, storeBusyTimeoutMS)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = st.Close() }()

	var out io.Writer = os.Stdout
	if outPath != "" {
		f, err := os.Create(outPath)
		if err != nil {
			return fmt.Errorf("create output file %q: %w", outPath, err)
		}
		defer func() { _ = f.Close() }()
		out = f
	}

	if err := api.ExportSession(st.ReadDB(), sessionID, out); err != nil {
		return err
	}
	return nil
}
