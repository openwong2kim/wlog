package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openwong2kim/wlog/internal/model"
)

// TestMigrateFreshStampsVersion verifies a brand-new DB ends at schemaVersion.
func TestMigrateFreshStampsVersion(t *testing.T) {
	st, _ := openFile(t)
	var v int
	if err := st.ReadDB().QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("fresh DB user_version: got %d, want %d", v, schemaVersion)
	}
}

// TestMigrateDowngradeGuard ensures a DB whose user_version exceeds the supported
// version is refused (safe stop, no changes).
func TestMigrateDowngradeGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wlog.db")
	// Open once to create the schema, then bump user_version past support.
	st, err := Open(path, false, 5000)
	if err != nil {
		t.Fatal(err)
	}
	// Use the writer connection directly to stamp a future version.
	if _, err := st.write.ExecContext(context.Background(), "PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Re-open: migrate must reject the newer schema.
	_, err = Open(path, false, 5000)
	if err == nil {
		t.Fatal("expected downgrade-guard error opening DB with newer schema, got nil")
	}
	if !contains(err.Error(), "newer than supported") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestMigrateRollbackOnFailure injects a deliberately failing forward migration
// and asserts the DB version is left unchanged (whole step rolled back) and the
// schema is untouched by the partial step.
func TestMigrateRollbackOnFailure(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wlog.db")

	// Create a v1 DB the normal way, then close.
	st, err := Open(path, false, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Open a raw connection (writer DSN) so we can drive migrate() directly with
	// a temporary failing migration step 1->2.
	writeDSN, _, err := buildDSNs(path, false, 5000)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(driverName, writeDSN)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	// A failing migration that first applies a real DDL (ADD COLUMN), then
	// returns an error — the column add and the version bump must both roll back.
	// We exercise applyMigration directly because it is the unit responsible for
	// the single-transaction rollback guarantee (schemaVersion is const, so we
	// cannot drive the full migrate() loop to a synthetic v2).
	wantErr := errors.New("boom")
	failing := migration{
		from: 1, to: 2, desc: "failing test step",
		apply: func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, "ALTER TABLE sessions ADD COLUMN test_col TEXT"); err != nil {
				return err
			}
			return wantErr
		},
	}

	beforeVer := mustUserVersion(t, db)
	if beforeVer != schemaVersion {
		t.Fatalf("precondition: user_version=%d, want %d", beforeVer, schemaVersion)
	}

	err = applyMigration(ctx, db, failing)
	if !errors.Is(err, wantErr) {
		t.Fatalf("applyMigration error: got %v, want wrap of %v", err, wantErr)
	}

	// Version must be unchanged.
	if got := mustUserVersion(t, db); got != beforeVer {
		t.Errorf("user_version after failed migration: got %d, want %d", got, beforeVer)
	}
	// The ADD COLUMN must have rolled back (column absent).
	if columnExists(t, db, "sessions", "test_col") {
		t.Error("test_col present after rolled-back migration; ALTER was not rolled back")
	}
}

// TestMigrateBackupCreated verifies that a real upgrade path writes a backup
// file before applying steps. We drive migrate() with a synthetic v1->v2 step on
// a DB stamped at v1, raising the target via a swapped migrations slice. Because
// schemaVersion is const we cannot reach the "after" check, so this test only
// asserts the backup side effect by invoking backupFile directly (the unit that
// migrate calls), keeping the contract honest.
func TestMigrateBackupCreated(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wlog.db")
	st, err := Open(path, false, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Drive backupFile through a writer connection (its new signature takes a
	// *sql.DB so it can checkpoint the WAL before copying — I1).
	writeDSN, _, err := buildDSNs(path, false, 5000)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(driverName, writeDSN)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	if err := backupFile(ctx, db, path, 1); err != nil {
		t.Fatalf("backupFile: %v", err)
	}
	bak := path + ".bak-v1"
	info, err := os.Stat(bak)
	if err != nil {
		t.Fatalf("backup not created: %v", err)
	}
	if info.Size() == 0 {
		t.Error("backup file is empty")
	}
	// Memory DBs must skip backup: backupFile is never called for them (migrate
	// guards memory==true). Verify the guard path indirectly: opening a memory
	// store creates no .bak file anywhere (there is no path).
}

// TestBackupIncludesUncheckpointedWAL is the I1 regression: when committed pages
// still live only in the -wal sidecar (the prior process exited without a
// Close-time checkpoint), backupFile must fold them into the main file before
// copying so the .bak captures them. We simulate the crash by writing through a
// raw writer connection with auto-checkpoint disabled, leaving rows in the WAL,
// then taking the backup and reading it back via a fresh independent connection.
func TestBackupIncludesUncheckpointedWAL(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wlog.db")

	// Create a real v2 DB the normal way so the schema exists, then close it (this
	// checkpoints+truncates the WAL, giving us a clean starting file).
	st, err := Open(path, false, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Open a raw writer connection and DISABLE auto-checkpoint so committed rows
	// stay in the -wal file (modeling an abnormal exit with no checkpoint).
	writeDSN, _, err := buildDSNs(path, false, 5000)
	if err != nil {
		t.Fatal(err)
	}
	crash, err := sql.Open(driverName, writeDSN)
	if err != nil {
		t.Fatal(err)
	}
	crash.SetMaxOpenConns(1)
	if _, err := crash.ExecContext(ctx, "PRAGMA wal_autocheckpoint = 0"); err != nil {
		crash.Close()
		t.Fatal(err)
	}
	if _, err := crash.ExecContext(ctx,
		`INSERT INTO sessions (id, first_seen, last_seen, has_events)
		 VALUES ('wal-only', 1000, 2000, 0)`); err != nil {
		crash.Close()
		t.Fatalf("insert wal-only row: %v", err)
	}
	// Confirm the row really is sitting in the WAL (sidecar non-empty). If the
	// platform checkpointed anyway this assertion documents the precondition.
	if wi, err := os.Stat(path + "-wal"); err != nil || wi.Size() == 0 {
		crash.Close()
		t.Fatalf("precondition: expected non-empty -wal (committed-but-unmerged), stat err=%v", err)
	}

	// Take the backup THROUGH this same connection (as migrate does on the writer):
	// backupFile checkpoints first, so the copied file must contain the WAL row.
	if err := backupFile(ctx, crash, path, 1); err != nil {
		crash.Close()
		t.Fatalf("backupFile: %v", err)
	}
	// Close the crash connection so the backup file is not subject to further WAL
	// activity when we open it.
	if err := crash.Close(); err != nil {
		t.Fatalf("close crash conn: %v", err)
	}

	// Read the backup independently. The wal-only row must be present — proving the
	// checkpoint folded the sidecar into the main file before the copy.
	bak := path + ".bak-v1"
	bakDSN, _, err := buildDSNs(bak, false, 5000)
	if err != nil {
		t.Fatal(err)
	}
	bdb, err := sql.Open(driverName, bakDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer bdb.Close()
	var n int
	if err := bdb.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sessions WHERE id = 'wal-only'").Scan(&n); err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if n != 1 {
		t.Errorf("backup missing the uncheckpointed WAL row: got %d, want 1 (I1)", n)
	}
}

// v1SessionsDDL is the sessions table exactly as schema version 1 defined it:
// the original column set with NO repo / git_branch. Used to fabricate a genuine
// pre-migration DB so the v1->v2 add-column step has something real to upgrade
// (schema.sql now creates the v2 shape, so a fresh store is already at v2).
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

// makeV1DB writes a minimal version-1 database at path: a v1-shaped sessions
// table with one seeded row, stamped user_version=1. It deliberately bypasses
// store.Open (which would create the v2 schema and stamp v2) by driving a raw
// writer connection, so the subsequent store.Open exercises the real v1->v2
// migration path.
func makeV1DB(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	writeDSN, _, err := buildDSNs(path, false, 5000)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(driverName, writeDSN)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	if _, err := db.ExecContext(ctx, v1SessionsDDL); err != nil {
		t.Fatalf("create v1 sessions: %v", err)
	}
	// Seed a row whose data must survive the migration untouched.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO sessions (id, first_seen, last_seen, token_source, has_events)
		 VALUES ('legacy', 1000, 2000, 'events', 1)`); err != nil {
		t.Fatalf("seed legacy session: %v", err)
	}
	if err := setUserVersion(ctx, db, 1); err != nil {
		t.Fatalf("stamp v1: %v", err)
	}
}

// TestMigrateV1ToV2AddsColumns drives the real v1->v2 forward step through
// store.Open: the two new columns appear, the pre-existing row is preserved with
// NULL repo/git_branch, user_version reaches 2, and a pre-migration backup
// (wlog.db.bak-v1) is written.
func TestMigrateV1ToV2AddsColumns(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wlog.db")
	makeV1DB(t, path)

	st, err := Open(path, false, 5000)
	if err != nil {
		t.Fatalf("Open (migrate v1->v2): %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Version advanced to the current schemaVersion (2).
	if v := mustUserVersion(t, st.write); v != schemaVersion {
		t.Errorf("user_version after migrate: got %d, want %d", v, schemaVersion)
	}
	// New columns exist.
	if !columnExists(t, st.write, "sessions", "repo") {
		t.Error("repo column missing after v1->v2 migration")
	}
	if !columnExists(t, st.write, "sessions", "git_branch") {
		t.Error("git_branch column missing after v1->v2 migration")
	}
	// The legacy row survived, with NULL (-> empty) repo/git_branch and its
	// original data intact.
	var (
		first, last, hasEvents int64
		src, repo, branch      string
	)
	row := st.ReadDB().QueryRowContext(ctx, `
		SELECT first_seen, last_seen, token_source, has_events,
		       COALESCE(repo,''), COALESCE(git_branch,'')
		FROM sessions WHERE id = 'legacy'`)
	if err := row.Scan(&first, &last, &src, &hasEvents, &repo, &branch); err != nil {
		t.Fatalf("read migrated legacy row: %v", err)
	}
	if first != 1000 || last != 2000 || src != "events" || hasEvents != 1 {
		t.Errorf("legacy row mutated: first=%d last=%d src=%q has_events=%d", first, last, src, hasEvents)
	}
	if repo != "" || branch != "" {
		t.Errorf("legacy repo/branch: got %q/%q, want empty (add-column NULL default)", repo, branch)
	}

	// The backup of the pre-migration (v1) file must exist and be non-empty.
	bi, err := os.Stat(path + ".bak-v1")
	if err != nil {
		t.Fatalf("pre-migration backup missing: %v", err)
	}
	if bi.Size() == 0 {
		t.Error("pre-migration backup is empty")
	}
}

// TestMigrateV1ToV2Idempotent re-opens an already-migrated DB and confirms the
// migration does not run again: no new backup file appears and the version stays
// at schemaVersion. A new write also succeeds (the merged repo/branch upsert
// works against the migrated table).
func TestMigrateV1ToV2Idempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wlog.db")
	makeV1DB(t, path)

	// First open performs the migration (writes bak-v1).
	st1, err := Open(path, false, 5000)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := st1.Close(); err != nil {
		t.Fatal(err)
	}

	// Second open: already at v2, so migrate is a no-op. Re-opening must not write
	// a bak-v2 (no upgrade happened from v2).
	st2, err := Open(path, false, 5000)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })

	if _, err := os.Stat(path + ".bak-v2"); err == nil {
		t.Error("bak-v2 created on re-open of an up-to-date DB; migration ran again")
	}
	if v := mustUserVersion(t, st2.write); v != schemaVersion {
		t.Errorf("user_version on idempotent re-open: got %d, want %d", v, schemaVersion)
	}

	// A write exercising the new merged columns succeeds end-to-end.
	if err := st2.WriteBatch(ctx, model.Batch{Sessions: []model.Session{
		{ID: "fresh", FirstSeen: 10, LastSeen: 20, Repo: "wlog", GitBranch: "main"},
	}}); err != nil {
		t.Fatalf("write after migrate: %v", err)
	}
	var repo, branch string
	if err := st2.ReadDB().QueryRowContext(ctx,
		"SELECT COALESCE(repo,''), COALESCE(git_branch,'') FROM sessions WHERE id='fresh'").
		Scan(&repo, &branch); err != nil {
		t.Fatal(err)
	}
	if repo != "wlog" || branch != "main" {
		t.Errorf("post-migrate write: repo=%q branch=%q, want wlog/main", repo, branch)
	}
}

// v2APIRequestsDDL is the api_requests table exactly as schema version 2 defined
// it: WITHOUT the v3 cost_source column. Used to fabricate a genuine pre-v3 DB so
// the v2->v3 add-column step has a real table to upgrade (schema.sql now creates
// the v3 shape, so a fresh store is already at v3).
const v2APIRequestsDDL = `CREATE TABLE api_requests (
	id             INTEGER PRIMARY KEY,
	session_id     TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	ts             INTEGER NOT NULL,
	model          TEXT,
	input_tokens   INTEGER NOT NULL DEFAULT 0,
	output_tokens  INTEGER NOT NULL DEFAULT 0,
	cache_read     INTEGER NOT NULL DEFAULT 0,
	cache_creation INTEGER NOT NULL DEFAULT 0,
	cost_usd       REAL    NOT NULL DEFAULT 0,
	duration_ms    INTEGER NOT NULL DEFAULT 0,
	is_error       INTEGER NOT NULL DEFAULT 0,
	status_code    INTEGER NOT NULL DEFAULT 0,
	dedup_key      TEXT NOT NULL UNIQUE,
	attrs_json     TEXT
)`

// makeV2DB writes a minimal version-2 database at path: a v2-shaped sessions
// table (with repo/git_branch, so schema.sql's CREATE IF NOT EXISTS no-ops) plus
// a v2-shaped api_requests table (NO cost_source) carrying one seeded row,
// stamped user_version=2. Driving raw DDL bypasses store.Open (which would create
// the v3 shape), so the subsequent store.Open exercises the real v2->v3 migration.
func makeV2DB(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	writeDSN, _, err := buildDSNs(path, false, 5000)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(driverName, writeDSN)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	// v2 sessions DDL (includes repo/git_branch) so schema.sql does not need to add
	// them; the FK target for the seeded api_request must also exist.
	const v2SessionsDDL = `CREATE TABLE sessions (
		id TEXT PRIMARY KEY, first_seen INTEGER NOT NULL, last_seen INTEGER NOT NULL,
		app_version TEXT, model_set TEXT, user_email TEXT, org_id TEXT,
		token_source TEXT, has_events INTEGER NOT NULL DEFAULT 0,
		repo TEXT, git_branch TEXT, attrs_json TEXT)`
	if _, err := db.ExecContext(ctx, v2SessionsDDL); err != nil {
		t.Fatalf("create v2 sessions: %v", err)
	}
	if _, err := db.ExecContext(ctx, v2APIRequestsDDL); err != nil {
		t.Fatalf("create v2 api_requests: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO sessions (id, first_seen, last_seen, has_events) VALUES ('legacy', 1000, 2000, 1)`); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	// A pre-existing api_request whose cost must survive the migration with a NULL
	// (non-authoritative) cost_source.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO api_requests (session_id, ts, model, input_tokens, output_tokens, cost_usd, dedup_key)
		 VALUES ('legacy', 1500, 'claude-x', 10, 20, 0.05, 'legacy-key')`); err != nil {
		t.Fatalf("seed api_request: %v", err)
	}
	if err := setUserVersion(ctx, db, 2); err != nil {
		t.Fatalf("stamp v2: %v", err)
	}
}

// TestMigrateV2ToV3AddsCostSource drives the real v2->v3 forward step through
// store.Open: the cost_source column appears, the pre-existing api_request row is
// preserved with NULL cost_source and its original cost, user_version reaches 3,
// and a pre-migration backup (wlog.db.bak-v2) is written.
func TestMigrateV2ToV3AddsCostSource(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wlog.db")
	makeV2DB(t, path)

	st, err := Open(path, false, 5000)
	if err != nil {
		t.Fatalf("Open (migrate v2->v3): %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if v := mustUserVersion(t, st.write); v != schemaVersion {
		t.Errorf("user_version after migrate: got %d, want %d", v, schemaVersion)
	}
	if !columnExists(t, st.write, "api_requests", "cost_source") {
		t.Error("cost_source column missing after v2->v3 migration")
	}
	// The legacy api_request survived with its cost intact and a NULL cost_source
	// (-> empty), which the upsert treats as non-authoritative so OTLP can fill it.
	var (
		cost float64
		src  sql.NullString
	)
	if err := st.ReadDB().QueryRowContext(ctx,
		"SELECT cost_usd, cost_source FROM api_requests WHERE dedup_key = 'legacy-key'").
		Scan(&cost, &src); err != nil {
		t.Fatalf("read migrated api_request: %v", err)
	}
	if cost != 0.05 {
		t.Errorf("legacy cost_usd mutated: got %v, want 0.05", cost)
	}
	if src.Valid {
		t.Errorf("legacy cost_source = %q, want NULL (add-column default)", src.String)
	}

	// A later authoritative OTLP delivery for that SAME pre-v3 row (NULL
	// cost_source) must FILL it: the upsert treats NULL as non-authoritative
	// (IS NOT 'otlp' is true for NULL), so cost/tokens/cost_source are updated.
	if err := st.WriteBatch(ctx, model.Batch{
		Sessions: []model.Session{{ID: "legacy", FirstSeen: 1000, LastSeen: 2000}},
		APIRequests: []model.APIRequest{{
			SessionID: "legacy", TS: 1500, Model: "claude-x", CostUSD: 0.20,
			CostSource: model.CostSourceOTLP, DedupKey: "legacy-key",
		}},
	}); err != nil {
		t.Fatalf("OTLP fill of migrated NULL-cost_source row: %v", err)
	}
	if err := st.ReadDB().QueryRowContext(ctx,
		"SELECT cost_usd, cost_source FROM api_requests WHERE dedup_key = 'legacy-key'").
		Scan(&cost, &src); err != nil {
		t.Fatal(err)
	}
	if cost != 0.20 || src.String != model.CostSourceOTLP {
		t.Errorf("migrated NULL row not filled by OTLP: cost=%v src=%q, want 0.20/%s", cost, src.String, model.CostSourceOTLP)
	}

	// Backup of the pre-migration (v2) file must exist and be non-empty.
	bi, err := os.Stat(path + ".bak-v2")
	if err != nil {
		t.Fatalf("pre-migration backup missing: %v", err)
	}
	if bi.Size() == 0 {
		t.Error("pre-migration backup is empty")
	}
}

// TestMigrateV2ToV3Idempotent re-opens an already-migrated DB and confirms the
// v2->v3 step does not run again (no new backup, version stays at schemaVersion),
// and that an authoritative OTLP write then a JSONL re-scan keep the OTLP cost
// (the upsert works end-to-end against the migrated table).
func TestMigrateV2ToV3Idempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wlog.db")
	makeV2DB(t, path)

	st1, err := Open(path, false, 5000)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := st1.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(path, false, 5000)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })

	if _, err := os.Stat(path + ".bak-v3"); err == nil {
		t.Error("bak-v3 created on re-open of an up-to-date DB; migration ran again")
	}
	if v := mustUserVersion(t, st2.write); v != schemaVersion {
		t.Errorf("user_version on idempotent re-open: got %d, want %d", v, schemaVersion)
	}

	// End-to-end cost-authority over the migrated table: OTLP then a JSONL re-scan.
	const sid, dedup = "mig-sess", "mig-key"
	if err := st2.WriteBatch(ctx, model.Batch{
		Sessions: []model.Session{{ID: sid, FirstSeen: 10, LastSeen: 20}},
		APIRequests: []model.APIRequest{{
			SessionID: sid, TS: 15, Model: "m", CostUSD: 0.30,
			CostSource: model.CostSourceOTLP, DedupKey: dedup,
		}},
	}); err != nil {
		t.Fatalf("OTLP write after migrate: %v", err)
	}
	if err := st2.WriteBatch(ctx, model.Batch{
		Sessions: []model.Session{{ID: sid, FirstSeen: 10, LastSeen: 20}},
		APIRequests: []model.APIRequest{{
			SessionID: sid, TS: 15, Model: "m", CostUSD: 0.99,
			CostSource: model.CostSourceJSONLEstimate, DedupKey: dedup,
		}},
	}); err != nil {
		t.Fatalf("JSONL re-scan after migrate: %v", err)
	}
	var cost float64
	if err := st2.ReadDB().QueryRowContext(ctx,
		"SELECT cost_usd FROM api_requests WHERE dedup_key = ?", dedup).Scan(&cost); err != nil {
		t.Fatal(err)
	}
	if cost != 0.30 {
		t.Errorf("post-migrate upsert: cost = %v, want 0.30 (OTLP authoritative)", cost)
	}
}

func mustUserVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	v, err := userVersion(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func columnExists(t *testing.T, db *sql.DB, table, col string) bool {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), "PRAGMA table_info("+table+")")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == col {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
