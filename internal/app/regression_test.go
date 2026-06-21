package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openwong2kim/wlog/internal/config"
	"github.com/openwong2kim/wlog/internal/model"
	"github.com/openwong2kim/wlog/internal/store"
)

// TestExport_NoErrorPrefix is the F4 regression: internal entry points must
// return plain errors WITHOUT the "wlog: error:" prefix, so main can add it
// exactly once (no doubled "wlog: error: wlog: error: ...").
func TestExport_NoErrorPrefix(t *testing.T) {
	cfg := config.Default()
	cfg.Memory = true

	// Missing --session.
	err := Export(context.Background(), cfg, "", "json", "")
	if err == nil {
		t.Fatal("expected error for empty session")
	}
	if strings.Contains(err.Error(), "wlog: error:") {
		t.Fatalf("internal error must not carry the prefix (F4): %q", err.Error())
	}

	// Unsupported format.
	err = Export(context.Background(), cfg, "s1", "csv", "")
	if err == nil {
		t.Fatal("expected error for unsupported format")
	}
	if strings.Contains(err.Error(), "wlog: error:") {
		t.Fatalf("format error must not carry the prefix (F4): %q", err.Error())
	}

	// Missing session in an (empty) DB also returns a plain error.
	err = Export(context.Background(), cfg, "no-such", "json", "")
	if err == nil {
		t.Fatal("expected error exporting a missing session")
	}
	if strings.Contains(err.Error(), "wlog: error:") {
		t.Fatalf("not-found error must not carry the prefix (F4): %q", err.Error())
	}
}

// TestRun_NoErrorPrefix confirms the receiver/UI bind-conflict path also returns
// a plain (prefix-free) error with the hint intact (F4).
func TestRun_NoErrorPrefix(t *testing.T) {
	occupied := mustListen(t)
	defer occupied.Close()
	_, portStr, _ := splitHostPort(occupied.Addr().String())

	cfg := config.Default()
	cfg.Memory = true
	cfg.NoOpen = true
	cfg.OTLPGRPCPort = 0
	cfg.OTLPHTTPPort = 0
	cfg.UIPort = atoi(t, portStr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := captureStdoutErr(t, func() error { return Run(ctx, cfg) })
	if err == nil {
		t.Fatal("expected a bind error for an occupied UI port")
	}
	if strings.Contains(err.Error(), "wlog: error:") {
		t.Fatalf("bind error must not carry the prefix (F4): %q", err.Error())
	}
	if !strings.Contains(err.Error(), "hint:") {
		t.Fatalf("bind error should still carry the hint: %q", err.Error())
	}
}

// TestExport_EnvDoesNotOverrideConfig is the C7 regression: app entry points must
// NOT re-apply WLOG_* env (the CLI already resolved env + flags with flags
// winning). Here a WLOG_DB env var pointing at a bogus path must be ignored;
// Export must use the DBPath already set on the config.
func TestExport_EnvDoesNotOverrideConfig(t *testing.T) {
	// Seed a real file DB at the path the config will carry.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "configured.db")
	st, err := store.Open(dbPath, false, 5000)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	const sid = "sess-c7"
	batch := model.Batch{
		Sessions: []model.Session{{
			ID: sid, FirstSeen: 1000, LastSeen: 2000, HasEvents: true,
		}},
	}
	if err := st.WriteBatch(context.Background(), batch); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	// Point WLOG_DB at a path that has no such session. If Export wrongly
	// re-applied env, it would open this DB and fail to find sid.
	bogus := filepath.Join(dir, "from-env.db")
	t.Setenv("WLOG_DB", bogus)

	cfg := config.Default()
	cfg.DBPath = dbPath // resolved by the CLI; must win over WLOG_DB

	if err := Export(context.Background(), cfg, sid, "json", ""); err != nil {
		t.Fatalf("Export should read the CONFIGURED db (flag wins over env), got: %v", err)
	}
	// The config DBPath must be untouched by Export (no ApplyEnv side effect).
	if cfg.DBPath != dbPath {
		t.Fatalf("Export mutated DBPath from env: got %q, want %q", cfg.DBPath, dbPath)
	}
}
