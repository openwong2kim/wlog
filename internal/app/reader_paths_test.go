package app

// reader_paths_test.go covers the read-only open paths for `wlog last` and
// `wlog statusline` (I2 / PLAN OQ3): both commands now open the store via
// store.OpenReader, which never creates or migrates the DB. The graceful states
// exercised here are a MISSING DB file and an OLD-SCHEMA (v1) DB that read-only
// mode cannot migrate — in every case last/statusline must exit 0 (last prints a
// hint, statusline a blank line).

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite" // pure-Go "sqlite" driver, matches the store package

	"github.com/openwong2kim/wlog/internal/config"
)

// writeV1DB fabricates a genuine schema-version-1 database at path: the original
// sessions table WITHOUT the v2 repo/git_branch columns, one seeded row, and
// PRAGMA user_version = 1. It deliberately avoids store.Open (which would create
// the v2 schema and stamp v2), so the app-layer read-only paths see a real
// pre-migration DB. This mirrors store.makeV1DB, duplicated here because that
// helper is unexported in the store package.
func writeV1DB(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	// file: URI with the same PRAGMA shape the store uses; a plain writer here.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open v1 db: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	const v1SessionsDDL = `CREATE TABLE sessions (
		id           TEXT PRIMARY KEY,
		first_seen   INTEGER NOT NULL,
		last_seen    INTEGER NOT NULL,
		app_version  TEXT,
		model_set    TEXT,
		user_email   TEXT,
		org_id       TEXT,
		token_source TEXT,
		has_events   INTEGER NOT NULL DEFAULT 0,
		attrs_json   TEXT
	)`
	if _, err := db.ExecContext(ctx, v1SessionsDDL); err != nil {
		t.Fatalf("create v1 sessions: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO sessions (id, first_seen, last_seen, token_source, has_events)
		 VALUES ('legacy', 1000, 2000, 'events', 1)`); err != nil {
		t.Fatalf("seed legacy session: %v", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		t.Fatalf("stamp v1: %v", err)
	}
}

// fileCfg returns a Config pointed at a file DB path (not memory), with the
// documented defaults so Validate() passes.
func fileCfg(path string) *config.Config {
	c := config.Default()
	c.DBPath = path
	c.Memory = false
	return c
}

// TestLast_MissingDBFileHint: with no DB file at all, Last prints a hint and
// returns nil (exit 0) — it must not error or create the DB.
func TestLast_MissingDBFileHint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")

	var runErr error
	out := captureStdout(t, nil, func() { runErr = Last(context.Background(), fileCfg(path), "", 10) })
	if runErr != nil {
		t.Fatalf("Last(missing db) returned error: %v", runErr)
	}
	if !strings.Contains(out, "no database yet") || !strings.Contains(out, "--print-claude-setup") {
		t.Fatalf("missing-DB hint missing expected text:\n%s", out)
	}
	// The read-only path must not have created the file.
	if _, err := os.Stat(path); err == nil {
		t.Error("Last created the DB file; read-only path must not create it")
	}
}

// TestLast_OldSchemaUpgradeHint: an old (v1) DB cannot be migrated read-only, so
// Last prints an upgrade hint and returns nil (exit 0). It must not migrate the
// DB (no .bak, version stays 1).
func TestLast_OldSchemaUpgradeHint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wlog.db")
	writeV1DB(t, path)

	var runErr error
	out := captureStdout(t, nil, func() { runErr = Last(context.Background(), fileCfg(path), "", 10) })
	if runErr != nil {
		t.Fatalf("Last(old schema) returned error: %v", runErr)
	}
	if !strings.Contains(out, "older version") || !strings.Contains(out, "run `wlog` once") {
		t.Fatalf("old-schema upgrade hint missing expected text:\n%s", out)
	}
	if _, err := os.Stat(path + ".bak-v1"); err == nil {
		t.Error("Last migrated the DB (.bak-v1 created); read-only must not migrate")
	}
	if v := rawUserVersion(t, path); v != 1 {
		t.Errorf("DB user_version after Last: got %d, want 1 (unmigrated)", v)
	}
}

// TestStatusline_MissingDB_EmptyLineExitZero: no DB file -> Statusline still
// prints exactly one (blank) line and returns nil.
func TestStatusline_MissingDB_EmptyLineExitZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")

	var runErr error
	out := captureStdout(t, nil, func() { runErr = Statusline(context.Background(), fileCfg(path), false) })
	if runErr != nil {
		t.Fatalf("Statusline(missing db) returned error: %v", runErr)
	}
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("Statusline must print exactly one line, got %q", out)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("Statusline(missing db) should print a blank line, got %q", out)
	}
}

// TestStatusline_OldSchema_EmptyLineExitZero: an old (v1) DB read-only mode cannot
// migrate -> blank line, exit 0, and no migration side effects.
func TestStatusline_OldSchema_EmptyLineExitZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wlog.db")
	writeV1DB(t, path)

	var runErr error
	out := captureStdout(t, nil, func() { runErr = Statusline(context.Background(), fileCfg(path), false) })
	if runErr != nil {
		t.Fatalf("Statusline(old schema) returned error: %v", runErr)
	}
	if strings.Count(out, "\n") != 1 || strings.TrimSpace(out) != "" {
		t.Fatalf("Statusline(old schema) should print a single blank line, got %q", out)
	}
	if _, err := os.Stat(path + ".bak-v1"); err == nil {
		t.Error("Statusline migrated the DB (.bak-v1 created); read-only must not migrate")
	}
	if v := rawUserVersion(t, path); v != 1 {
		t.Errorf("DB user_version after Statusline: got %d, want 1 (unmigrated)", v)
	}
}

// rawUserVersion reads PRAGMA user_version from path via a fresh connection, for
// asserting the read-only paths did not migrate the DB.
func rawUserVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open for user_version: %v", err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}
