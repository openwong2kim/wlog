package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/openwong2kim/wlog/internal/model"
)

// fileDigest returns the size + mtime of a path (or ok=false if it does not
// exist). Used to assert that an OpenReader open leaves the DB file untouched.
func fileDigest(t *testing.T, path string) (size int64, mod time.Time, ok bool) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, time.Time{}, false
		}
		t.Fatalf("stat %q: %v", path, err)
	}
	return fi.Size(), fi.ModTime(), true
}

// seedFileDB creates a file-backed v2 store, writes a session via WriteBatch, and
// returns the path after a clean Close (so the WAL is checkpointed/truncated and
// the on-disk file is settled before a read-only open).
func seedFileDB(t *testing.T, b model.Batch) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wlog.db")
	st, err := Open(path, false, 5000)
	if err != nil {
		t.Fatalf("Open(seed): %v", err)
	}
	if err := st.WriteBatch(context.Background(), b); err != nil {
		t.Fatalf("WriteBatch(seed): %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close(seed): %v", err)
	}
	return path
}

// TestOpenReaderReadsSeededDB confirms the read methods work against a real
// file DB opened read-only, returning the same data Open would.
func TestOpenReaderReadsSeededDB(t *testing.T) {
	ctx := context.Background()
	const sid = "reader-1"
	path := seedFileDB(t, model.Batch{
		Sessions: []model.Session{{ID: sid, FirstSeen: 1000, LastSeen: 5000, TokenSource: "events", Repo: "wlog", GitBranch: "main"}},
		APIRequests: []model.APIRequest{{
			SessionID: sid, TS: 1500, Model: "m",
			Tokens: model.Tokens{Input: 100, Output: 10, CacheRead: 300}, CostUSD: 0.42, DedupKey: "ar1",
		}},
		ToolDecisions: []model.ToolDecision{
			{SessionID: sid, TS: 1600, Decision: "reject", Source: "user_reject", ToolName: "Bash", DedupKey: "d1"},
		},
		ToolResults: []model.ToolResult{
			{SessionID: sid, TS: 1700, ToolName: "Bash", Success: true, BashCommand: "ls", DedupKey: "r1"},
		},
	})

	st, err := OpenReader(path, false, 5000)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if !st.readOnly {
		t.Error("OpenReader store should have readOnly=true")
	}
	if st.write != nil {
		t.Error("OpenReader store must have a nil writer connection")
	}

	id, err := st.LatestSessionID(ctx)
	if err != nil || id != sid {
		t.Fatalf("LatestSessionID: got %q err=%v, want %q", id, err, sid)
	}
	sm, found, err := st.SessionSummary(ctx, sid)
	if err != nil || !found {
		t.Fatalf("SessionSummary: err=%v found=%v", err, found)
	}
	if sm.Repo != "wlog" || sm.GitBranch != "main" {
		t.Errorf("repo/branch: got %q/%q, want wlog/main", sm.Repo, sm.GitBranch)
	}
	if !floatEq(sm.CostUSD, 0.42) {
		t.Errorf("CostUSD: got %v, want 0.42", sm.CostUSD)
	}
	if sm.ToolReject != 1 {
		t.Errorf("ToolReject: got %d, want 1", sm.ToolReject)
	}
	snap, found, err := st.SessionSnapshot(ctx, sid)
	if err != nil || !found {
		t.Fatalf("SessionSnapshot: err=%v found=%v", err, found)
	}
	if snap.LastTool != "Bash" {
		t.Errorf("LastTool: got %q, want Bash", snap.LastTool)
	}
	results, err := st.ToolResultsBySession(ctx, sid, 10)
	if err != nil {
		t.Fatalf("ToolResultsBySession: %v", err)
	}
	if len(results) != 1 || results[0].BashCommand != "ls" {
		t.Errorf("ToolResultsBySession: got %+v", results)
	}
}

// TestOpenReaderWriteRejected confirms a read-only Store refuses mutations with
// ErrReadOnly instead of panicking on its nil writer goroutine.
func TestOpenReaderWriteRejected(t *testing.T) {
	path := seedFileDB(t, model.Batch{
		Sessions: []model.Session{{ID: "s1", FirstSeen: 1, LastSeen: 2}},
	})
	st, err := OpenReader(path, false, 5000)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	err = st.WriteBatch(context.Background(), model.Batch{
		Sessions: []model.Session{{ID: "s2", FirstSeen: 1, LastSeen: 2}},
	})
	if !errors.Is(err, ErrReadOnly) {
		t.Fatalf("WriteBatch on read-only store: got %v, want ErrReadOnly", err)
	}
	err = st.RunRetention(context.Background(), time.Hour, 0)
	if !errors.Is(err, ErrReadOnly) {
		t.Fatalf("RunRetention on read-only store: got %v, want ErrReadOnly", err)
	}
}

// TestOpenReaderMissingFile: a path with no DB file is ErrDBNotFound (a normal
// "nothing written yet" state), not a generic open failure.
func TestOpenReaderMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.db")
	_, err := OpenReader(path, false, 5000)
	if !errors.Is(err, ErrDBNotFound) {
		t.Fatalf("OpenReader(missing): got %v, want ErrDBNotFound", err)
	}
}

// TestOpenReaderOldSchemaNeedsUpgrade: a genuine v1 DB (no repo/git_branch
// columns) opened read-only must return ErrDBNeedsUpgrade — read-only mode cannot
// migrate, and we must not let read.go hit a "no such column" error. Crucially it
// must NOT migrate or otherwise mutate the file (that is the whole point of the
// read-only path); we assert the v1 file is byte-for-byte unchanged and no backup
// sidecar was written.
func TestOpenReaderOldSchemaNeedsUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wlog.db")
	makeV1DB(t, path)

	beforeSize, beforeMod, ok := fileDigest(t, path)
	if !ok {
		t.Fatal("v1 DB should exist after makeV1DB")
	}

	_, err := OpenReader(path, false, 5000)
	if !errors.Is(err, ErrDBNeedsUpgrade) {
		t.Fatalf("OpenReader(v1): got %v, want ErrDBNeedsUpgrade", err)
	}

	// The read-only open must not have migrated the DB: no backup, version still 1,
	// repo column still absent, and the main file unchanged.
	if _, err := os.Stat(path + ".bak-v1"); err == nil {
		t.Error("OpenReader created a .bak-v1: read-only must never migrate/back up")
	}
	afterSize, afterMod, _ := fileDigest(t, path)
	if afterSize != beforeSize || !afterMod.Equal(beforeMod) {
		t.Errorf("v1 DB file changed across read-only open: size %d->%d, mtime %v->%v",
			beforeSize, afterSize, beforeMod, afterMod)
	}

	// Verify via a fresh writer connection that the schema is still v1.
	writeDSN, _, derr := buildDSNs(path, false, 5000)
	if derr != nil {
		t.Fatal(derr)
	}
	db := openRawDB(t, writeDSN)
	defer db.Close()
	if v := mustUserVersion(t, db); v != 1 {
		t.Errorf("user_version after read-only open: got %d, want 1 (unmigrated)", v)
	}
	if columnExists(t, db, "sessions", "repo") {
		t.Error("repo column appeared: read-only open must not have migrated")
	}
}

// TestOpenReaderDowngradeGuard: a DB whose user_version exceeds the supported
// version is refused (a newer binary wrote it). Read-only is still a refusal —
// stale read assumptions could be wrong against a future schema.
func TestOpenReaderDowngradeGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wlog.db")
	st, err := Open(path, false, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.write.ExecContext(context.Background(), "PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = OpenReader(path, false, 5000)
	if err == nil {
		t.Fatal("OpenReader on a newer-schema DB should error (downgrade guard)")
	}
	if !contains(err.Error(), "newer than") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestOpenReaderNoSideEffectsOnCurrentDB is the core OQ3 guarantee: opening a
// healthy, current-version DB read-only and closing it again must not MUTATE the
// database — no migration, no DDL, no schema bump, no backup. We assert the main
// .db file is byte-for-byte identical (content hash + size + mtime) before and
// after, and that schema_version is unchanged.
//
// Note on sidecars: a mode=ro connection on a WAL database still creates the
// -wal/-shm shared-memory index files SQLite needs to read a WAL DB concurrently
// with a writer. That is by design (it is exactly what lets statusline read while
// the wlog server writes) and is NOT a mutation of the database content — we use
// mode=ro, not immutable, precisely so concurrent reads stay correct. So this test
// checks the database content is untouched, not that zero files were created.
func TestOpenReaderNoSideEffectsOnCurrentDB(t *testing.T) {
	ctx := context.Background()
	path := seedFileDB(t, model.Batch{
		Sessions: []model.Session{{ID: "s1", FirstSeen: 1, LastSeen: 2}},
	})

	// After the seed Close the WAL was checkpoint-truncated, so the entire DB state
	// lives in the main file. Record its content hash + size + mtime.
	beforeHash := fileSHA256(t, path)
	beforeSize, beforeMod, ok := fileDigest(t, path)
	if !ok {
		t.Fatal("seeded DB should exist")
	}

	st, err := OpenReader(path, false, 5000)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	// Exercise a read to force the read pool to actually open a connection.
	if _, err := st.LatestSessionID(ctx); err != nil {
		t.Fatalf("LatestSessionID: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// No mutation of the database content: the main .db file is byte-identical.
	afterHash := fileSHA256(t, path)
	if afterHash != beforeHash {
		t.Errorf("read-only open changed the DB file content: sha256 %s -> %s", beforeHash, afterHash)
	}
	afterSize, afterMod, _ := fileDigest(t, path)
	if afterSize != beforeSize || !afterMod.Equal(beforeMod) {
		t.Errorf("read-only open mutated the DB file: size %d->%d, mtime %v->%v",
			beforeSize, afterSize, beforeMod, afterMod)
	}
	// No migration backup was written.
	if _, err := os.Stat(path + ".bak-v1"); err == nil {
		t.Error("read-only open created a .bak-v1 (must never migrate/back up)")
	}
	if _, err := os.Stat(path + ".bak-v2"); err == nil {
		t.Error("read-only open created a .bak-v2 (must never migrate/back up)")
	}
	// schema_version unchanged (no DDL/migrate ran).
	writeDSN, _, derr := buildDSNs(path, false, 5000)
	if derr != nil {
		t.Fatal(derr)
	}
	db := openRawDB(t, writeDSN)
	defer db.Close()
	if v := mustUserVersion(t, db); v != schemaVersion {
		t.Errorf("user_version after read-only open: got %d, want %d", v, schemaVersion)
	}
}

// TestOpenReaderConcurrentWithWriter confirms a read-only Store reads a DB while a
// separate writer Store actively writes the same file (the real wlog-server +
// statusline scenario). WAL allows the reader to proceed without SQLITE_BUSY.
func TestOpenReaderConcurrentWithWriter(t *testing.T) {
	ctx := context.Background()
	path := seedFileDB(t, model.Batch{
		Sessions: []model.Session{{ID: "s0", FirstSeen: 1, LastSeen: 2}},
	})

	writer, err := Open(path, false, 5000)
	if err != nil {
		t.Fatalf("Open(writer): %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })

	reader, err := OpenReader(path, false, 5000)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	// Interleave writes and reads.
	for i := 0; i < 20; i++ {
		if err := writer.WriteBatch(ctx, sampleBatch("sx", string(rune('a'+i)))); err != nil {
			t.Fatalf("writer.WriteBatch[%d]: %v", i, err)
		}
		if _, err := reader.LatestSessionID(ctx); err != nil {
			t.Fatalf("reader.LatestSessionID[%d]: %v", i, err)
		}
	}
}

// fileSHA256 returns the hex SHA-256 of a file's contents, for asserting a
// read-only open left the main DB byte-for-byte unchanged.
func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q for hashing: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// openRawDB opens a raw single-connection *sql.DB for test assertions against an
// existing file DB (schema_version / column checks). It mirrors the small pattern
// used elsewhere in the package tests (e.g. makeV1DB).
func openRawDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		t.Fatalf("sql.Open(raw): %v", err)
	}
	db.SetMaxOpenConns(1)
	return db
}
