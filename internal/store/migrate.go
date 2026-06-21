package store

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
)

// schemaVersion is the current schema version this binary supports. schema.sql
// represents the latest version (it is applied with CREATE IF NOT EXISTS before
// migrate runs, so a fresh DB is born at schemaVersion). Each forward step in
// migrations advances user_version by one and runs inside a single transaction
// (failure rolls the whole step back).
//
// Version history:
//
//	1: initial schema (sessions, api_requests, tool_*, events, spans,
//	   metric_points, series_state, meta).
//	2: sessions.repo + sessions.git_branch + idx_sessions_repo (PLAN v3 §3
//	   cross-repo aggregation dimensions, sourced from the JSONL transcript).
//	3: api_requests.cost_source — provenance of cost_usd ('otlp' authoritative |
//	   'jsonl-estimate') so a JSONL+OTLP dedup_key collision keeps the real OTLP
//	   cost and a JSONL re-scan never overwrites it with an estimate (PLAN v3 §9
//	   cost-accuracy gate).
const schemaVersion = 3

// migration is one forward step. from is the user_version it upgrades, to is the
// resulting version (must be from+1). apply runs inside the migration txn and
// should prefer additive changes (ADD COLUMN) per PLAN §8.
type migration struct {
	from  int
	to    int
	desc  string
	apply func(ctx context.Context, tx *sql.Tx) error
}

// createRepoIndexSQL creates the v2 repo index. It is intentionally idempotent
// (IF NOT EXISTS) and lives here rather than in schema.sql because schema.sql is
// applied before migrations on a pre-v2 DB, where the repo column does not yet
// exist (see schema.sql NOTE). Both the v1->v2 migration (existing DBs) and the
// fresh-DB stamp path (new DBs) run it, so the index exists on every v2 DB.
const createRepoIndexSQL = "CREATE INDEX IF NOT EXISTS idx_sessions_repo ON sessions(repo)"

// migrations is the ordered list of forward steps. The base version is
// established by schema.sql (applied in store.Open before migrate runs), so there
// is no 0->base migration body here; instead migrate stamps user_version to
// schemaVersion on a fresh DB. Forward steps for existing DBs are appended here.
//
// Each step must leave an existing DB structurally identical to a fresh DB at the
// same version (schema.sql is the source of truth for the CREATE shape; a step
// reproduces the same additive change via ADD COLUMN / CREATE INDEX IF NOT
// EXISTS). add-column on SQLite appends a column with a NULL default, matching the
// nullable repo/git_branch declarations in schema.sql.
var migrations = []migration{
	{
		from: 1, to: 2,
		desc: "add sessions.repo, sessions.git_branch + idx_sessions_repo",
		apply: func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, "ALTER TABLE sessions ADD COLUMN repo TEXT"); err != nil {
				return fmt.Errorf("add column repo: %w", err)
			}
			if _, err := tx.ExecContext(ctx, "ALTER TABLE sessions ADD COLUMN git_branch TEXT"); err != nil {
				return fmt.Errorf("add column git_branch: %w", err)
			}
			if _, err := tx.ExecContext(ctx, createRepoIndexSQL); err != nil {
				return fmt.Errorf("create idx_sessions_repo: %w", err)
			}
			return nil
		},
	},
	{
		from: 2, to: 3,
		desc: "add api_requests.cost_source (cost provenance for JSONL+OTLP dedup)",
		apply: func(ctx context.Context, tx *sql.Tx) error {
			// Additive nullable column (matches schema.sql's cost_source TEXT). Pre-v3
			// rows get NULL, which the store upsert treats as "not authoritative" so an
			// arriving OTLP cost can fill them; the column is never back-filled here.
			//
			// Guard with a column-existence check so the step is idempotent: schema.sql
			// (applied on every Open BEFORE migrate) uses CREATE TABLE IF NOT EXISTS, so
			// on a DB whose api_requests table did not pre-exist (it gets created fresh
			// at the latest v3 shape, already carrying cost_source) the ALTER would fail
			// with "duplicate column". SQLite's ADD COLUMN has no IF NOT EXISTS, so we
			// check PRAGMA table_info first. A genuine pre-v3 api_requests table lacks
			// the column and is altered normally.
			has, err := columnExistsTx(ctx, tx, "api_requests", "cost_source")
			if err != nil {
				return fmt.Errorf("inspect api_requests columns: %w", err)
			}
			if has {
				return nil // already present (fresh-shape table from schema.sql)
			}
			if _, err := tx.ExecContext(ctx, "ALTER TABLE api_requests ADD COLUMN cost_source TEXT"); err != nil {
				return fmt.Errorf("add column cost_source: %w", err)
			}
			return nil
		},
	},
}

// columnExistsTx reports whether table has a column named col, inside the
// migration transaction. It reads PRAGMA table_info (no bound parameters — the
// table name is a trusted constant from the migrations table, not user input).
func columnExistsTx(ctx context.Context, tx *sql.Tx, table, col string) (bool, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid         int
			name, ctype string
			notnull, pk int
			dflt        sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

// migrate brings the database schema up to schemaVersion using PRAGMA
// user_version. Behaviour (PLAN §8 / REVIEWS F11):
//   - Fresh DB (user_version==0): schema.sql already created the v1 tables, so we
//     stamp user_version to schemaVersion.
//   - user_version < schemaVersion: back up the file (file DBs only) then apply
//     each forward step in its own transaction (rollback on failure).
//   - user_version > schemaVersion: safe stop with an error (downgrade guard) —
//     a newer binary wrote this DB; an older binary must not touch it.
func migrate(ctx context.Context, db *sql.DB, path string, memory bool) error {
	cur, err := userVersion(ctx, db)
	if err != nil {
		return err
	}

	if cur > schemaVersion {
		return fmt.Errorf("store: database schema version %d is newer than supported %d; "+
			"upgrade wlog or use a different --db path (safe stop, no changes made)",
			cur, schemaVersion)
	}

	if cur == schemaVersion {
		return nil // up to date
	}

	// Fresh database: schema.sql created the current tables already (including the
	// v2 repo/git_branch columns). The repo index is NOT in schema.sql (it would
	// break pre-v2 DBs there), so create it here for fresh DBs — the column exists,
	// so this is safe — then stamp the version. No backup needed for a new DB.
	if cur == 0 {
		if _, err := db.ExecContext(ctx, createRepoIndexSQL); err != nil {
			return fmt.Errorf("store: create idx_sessions_repo (fresh): %w", err)
		}
		return setUserVersion(ctx, db, schemaVersion)
	}

	// cur in [1, schemaVersion): real upgrade. Back up first (file DBs only). The
	// backup runs through the writer connection so it can checkpoint the WAL into
	// the main file before copying (I1) — see backupFile.
	if !memory && path != "" {
		if err := backupFile(ctx, db, path, cur); err != nil {
			return err
		}
	}

	for _, m := range migrations {
		if m.from < cur {
			continue
		}
		if m.from != cur {
			return fmt.Errorf("store: migration gap: at version %d, next step expects %d", cur, m.from)
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return fmt.Errorf("store: migration %d->%d (%s) failed (rolled back): %w",
				m.from, m.to, m.desc, err)
		}
		cur = m.to
	}

	if cur != schemaVersion {
		return fmt.Errorf("store: after migrations reached version %d, expected %d", cur, schemaVersion)
	}
	return nil
}

// applyMigration runs one step plus its user_version bump in a single
// transaction so a failure leaves the DB at its prior version.
func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	if m.to != m.from+1 {
		return fmt.Errorf("store: malformed migration %d->%d (must advance by 1)", m.from, m.to)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if err := m.apply(ctx, tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	// PRAGMA user_version does not accept bound parameters; the value is a
	// trusted integer constant from our migrations table, so formatting is safe.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.to)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("stamp user_version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func userVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("store: read user_version: %w", err)
	}
	return v, nil
}

func setUserVersion(ctx context.Context, db *sql.DB, v int) error {
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
		return fmt.Errorf("store: set user_version=%d: %w", v, err)
	}
	return nil
}

// backupFile copies the database file to <path>.bak-v<cur> before a migration so
// a failed/aborted upgrade can be recovered. In-memory DBs skip this (no file).
//
// I1: migrate runs inside store.Open right after applySchema and BEFORE the writer
// goroutine starts, so there is no Close-time checkpoint on this path. If the
// previous process exited abnormally, committed pages may still live only in the
// -wal sidecar; a plain copy of the main .db would silently drop them, leaving an
// incomplete recovery asset. We therefore checkpoint the WAL into the main file
// (PRAGMA wal_checkpoint(TRUNCATE)) on the writer connection first, so the single
// copied file is self-consistent and complete. The checkpoint is best-effort: on
// a brand-new DB there is no WAL to fold, and a checkpoint hiccup must not block
// the (still useful) backup — but the TRUNCATE is the common, durable case.
func backupFile(ctx context.Context, db *sql.DB, path string, cur int) error {
	// Fold any committed-but-unmerged WAL pages into the main DB file so the copy
	// below captures them. Best-effort (see doc): ignore the error.
	_, _ = db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")

	dst := fmt.Sprintf("%s.bak-v%d", path, cur)
	in, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("store: open db for backup: %w", err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("store: create backup %q: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("store: write backup %q: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("store: close backup %q: %w", dst, err)
	}
	return nil
}
