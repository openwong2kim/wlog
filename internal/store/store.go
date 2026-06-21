// Package store is the SQLite persistence layer for wlog (PLAN §8, §8a, §16).
//
// Concurrency model (REVIEWS C, Codex/Eng F4): all writes are serialized through
// a single writer goroutine bound to one dedicated *sql.DB connection
// (MaxOpenConns=1). Reads use a separate connection pool (ReadDB) so WAL readers
// never contend with the writer. Every connection applies the same PRAGMAs
// (WAL, busy_timeout, synchronous=NORMAL, foreign_keys=ON) via the DSN so the
// settings hold per-connection — modernc does not share PRAGMA state across
// connections, so they must be in the DSN (verified by TestPragmaPerConn).
//
// A Batch is applied in exactly one transaction: sessions upsert (first_seen=min,
// last_seen=max, token_source/has_events merge) + idempotent inserts for events
// (ON CONFLICT(dedup_key) DO NOTHING), metric_points (ON CONFLICT on the series
// triple), spans (ON CONFLICT(trace_id,span_id)), and series_state upsert.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver (CGO-free)

	"github.com/openwong2kim/wlog/internal/model"
)

//go:embed schema.sql
var schemaSQL string

// driverName is the database/sql registration name for modernc.org/sqlite.
const driverName = "sqlite"

// Store owns a single-writer connection plus a read pool. It is safe for
// concurrent use: WriteBatch / RunRetention funnel through the writer channel,
// reads go through ReadDB.
//
// A Store may also be opened read-only via OpenReader: in that mode write is nil,
// no writer goroutine runs, and the schema/migration/chmod side effects of Open
// are skipped entirely. The read methods (LatestSessionID / SessionSummary /
// SessionSnapshot / Tool*BySession) work identically; the writer APIs
// (WriteBatch / RunRetention) return ErrReadOnly instead of mutating.
type Store struct {
	path     string // empty for in-memory
	memory   bool
	readOnly bool // true when opened via OpenReader (no writer, no side effects)

	write *sql.DB // serialized writer (MaxOpenConns=1); nil when readOnly
	read  *sql.DB // read pool (WAL concurrent readers)

	reqs chan writeReq // writer goroutine inbox

	stop   chan struct{} // closed by Close to stop accepting new work
	closed chan struct{} // closed when the writer goroutine exits

	closeOnce sync.Once
	stopOnce  sync.Once
}

// ErrReadOnly is returned by writer APIs (WriteBatch / RunRetention) when the
// Store was opened with OpenReader. A read-only Store has no writer connection,
// so a mutation cannot proceed; callers that only read (wlog last / statusline)
// never hit this.
var ErrReadOnly = errors.New("store: read-only store (opened via OpenReader)")

// ErrDBNotFound is returned by OpenReader when the database file does not exist
// (a fresh install that has never run the wlog server / scanned a transcript).
// Read-only callers treat it as "no data yet": wlog last prints a hint, statusline
// prints a blank line — both exit 0.
var ErrDBNotFound = errors.New("store: database file does not exist")

// ErrDBNeedsUpgrade is returned by OpenReader when the on-disk schema is older
// than this binary supports (user_version < schemaVersion). Read-only mode cannot
// run the migration (it would write), so the caller is told to run wlog once to
// upgrade. The current/wanted versions are wrapped via %w-friendly formatting in
// the error text for diagnostics.
var ErrDBNeedsUpgrade = errors.New("store: database schema is older than supported; run `wlog` once to upgrade it")

// writeReq is a unit of work handed to the single writer goroutine. fn runs
// inside one transaction; the result (commit/rollback error) returns on resp.
type writeReq struct {
	ctx  context.Context
	fn   func(ctx context.Context, tx *sql.Tx) error
	resp chan error
}

// Open creates (or opens) the database at path, applies the schema and any
// pending migrations, and starts the single writer goroutine.
//
//   - path: filesystem path to the DB file (ignored when memory is true).
//   - memory: use a shared in-memory database (file::memory:?cache=shared) so the
//     writer and read pool observe the same data.
//   - busyTimeoutMS: SQLite busy_timeout in milliseconds (PLAN uses 5000).
func Open(path string, memory bool, busyTimeoutMS int) (*Store, error) {
	if busyTimeoutMS <= 0 {
		busyTimeoutMS = 5000
	}
	if !memory && path == "" {
		return nil, errors.New("store.Open: empty path with memory=false")
	}

	writeDSN, readDSN, err := buildDSNs(path, memory, busyTimeoutMS)
	if err != nil {
		return nil, err
	}

	write, err := sql.Open(driverName, writeDSN)
	if err != nil {
		return nil, fmt.Errorf("store.Open: open writer: %w", err)
	}
	// Serialize all writes onto a single connection. This is the durability
	// guarantee behind the single-writer model.
	write.SetMaxOpenConns(1)
	write.SetMaxIdleConns(1)
	// Never let the writer connection be reaped: a shared in-memory DB lives only
	// as long as at least one connection is open, and we hold the schema there.
	write.SetConnMaxLifetime(0)
	write.SetConnMaxIdleTime(0)

	read, err := sql.Open(driverName, readDSN)
	if err != nil {
		_ = write.Close()
		return nil, fmt.Errorf("store.Open: open reader: %w", err)
	}
	read.SetMaxOpenConns(8)
	read.SetMaxIdleConns(4)
	read.SetConnMaxLifetime(0)

	ctx := context.Background()
	// Touch both pools so the DSN PRAGMAs are applied at least once and (for the
	// shared in-memory case) the database object exists before the read pool
	// connects to it.
	if err := write.PingContext(ctx); err != nil {
		_ = write.Close()
		_ = read.Close()
		return nil, fmt.Errorf("store.Open: ping writer: %w", err)
	}

	s := &Store{
		path:   path,
		memory: memory,
		write:  write,
		read:   read,
		reqs:   make(chan writeReq),
		stop:   make(chan struct{}),
		closed: make(chan struct{}),
	}

	// Apply schema + migrations on the writer connection (single txn handled
	// inside migrate). Done before starting the goroutine so Open fails fast.
	if err := s.applySchema(ctx); err != nil {
		_ = write.Close()
		_ = read.Close()
		return nil, err
	}
	if err := migrate(ctx, write, path, memory); err != nil {
		_ = write.Close()
		_ = read.Close()
		return nil, err
	}

	if err := read.PingContext(ctx); err != nil {
		_ = write.Close()
		_ = read.Close()
		return nil, fmt.Errorf("store.Open: ping reader: %w", err)
	}

	// Tighten file permissions on the on-disk DB (and its WAL/SHM sidecars) so a
	// driver/umask-created 0644 file is not world-readable: session telemetry can
	// contain prompts and tool arguments (PLAN §13). Memory DBs have no file.
	// Best-effort: on Windows os.Chmod only toggles the read-only bit, so this is
	// a no-op there (file ACLs govern access); a failure to chmod must not block
	// opening the store.
	if !memory && path != "" {
		restrictDBFiles(path)
	}

	go s.writerLoop()
	return s, nil
}

// OpenReader opens the database at path for reading only (PLAN OQ3): it starts no
// writer goroutine and performs NONE of Open's write-side effects — no applySchema
// (CREATE IF NOT EXISTS DDL), no migrate (backup + ALTER), no restrictDBFiles
// (chmod). It is the safe entry point for the hot, frequently-invoked read paths
// (`wlog last`, and especially `wlog statusline`, which Claude Code calls every
// few seconds): re-running schema DDL on every tick is wasteful, and silently
// migrating/creating a DB from a status-line refresh is a surprising side effect,
// the more so if the wlog server is concurrently writing the same file.
//
//   - path: filesystem path to the DB file (ignored when memory is true).
//   - memory: open the shared in-memory database (file::memory:?cache=shared).
//     A read-only memory open is only meaningful when another live Store already
//     created that in-memory DB in this process (the shared cache keeps it alive);
//     it is used by tests. On disk the file must already exist.
//   - busyTimeoutMS: SQLite busy_timeout in milliseconds (PLAN uses 5000).
//
// Errors:
//   - ErrDBNotFound: the on-disk file is absent (nothing has written it yet).
//   - ErrDBNeedsUpgrade: user_version < schemaVersion — read-only mode cannot
//     migrate, so the caller must run wlog once to upgrade.
//   - a downgrade-guard error: user_version > schemaVersion (a newer binary wrote
//     it; this binary must not read it with stale assumptions).
//
// The returned Store exposes the read methods; its writer APIs return ErrReadOnly.
func OpenReader(path string, memory bool, busyTimeoutMS int) (*Store, error) {
	if busyTimeoutMS <= 0 {
		busyTimeoutMS = 5000
	}
	if !memory && path == "" {
		return nil, errors.New("store.OpenReader: empty path with memory=false")
	}

	// Fail fast and clearly when the file is absent: mode=ro would surface a less
	// obvious "unable to open database file" error, and the read callers want to
	// distinguish "no DB yet" (graceful) from a real failure. Memory DBs have no
	// file to stat (they live in the shared cache).
	if !memory {
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				return nil, ErrDBNotFound
			}
			return nil, fmt.Errorf("store.OpenReader: stat %q: %w", path, err)
		}
	}

	readDSN, err := buildReadOnlyDSN(path, memory, busyTimeoutMS)
	if err != nil {
		return nil, err
	}

	read, err := sql.Open(driverName, readDSN)
	if err != nil {
		return nil, fmt.Errorf("store.OpenReader: open reader: %w", err)
	}
	read.SetMaxOpenConns(8)
	read.SetMaxIdleConns(4)
	read.SetConnMaxLifetime(0)

	ctx := context.Background()
	if err := read.PingContext(ctx); err != nil {
		_ = read.Close()
		return nil, fmt.Errorf("store.OpenReader: ping reader: %w", err)
	}

	// Schema-version gate (OQ3 option a): read-only mode cannot migrate, so an old
	// DB whose sessions table lacks repo/git_branch would make read.go fail with a
	// "no such column" error. Detect it up front and return a clear, actionable
	// signal instead. user_version is a read-only PRAGMA (no write).
	cur, err := userVersion(ctx, read)
	if err != nil {
		_ = read.Close()
		return nil, err
	}
	switch {
	case cur == 0:
		// Never-initialized DB: schema.sql/migrate has not run, so there are no
		// tables to read. This is the "no data yet" state (a fresh memory DB with no
		// live writer, or a zero-length/placeholder file). Treat it like a missing
		// DB so callers render the same graceful "nothing yet" path rather than an
		// upgrade prompt (there is nothing to upgrade).
		_ = read.Close()
		return nil, ErrDBNotFound
	case cur > schemaVersion:
		_ = read.Close()
		return nil, fmt.Errorf("store.OpenReader: database schema version %d is newer than "+
			"supported %d; upgrade wlog (safe stop, read-only)", cur, schemaVersion)
	case cur < schemaVersion:
		_ = read.Close()
		return nil, fmt.Errorf("%w (on-disk version %d, supported %d)", ErrDBNeedsUpgrade, cur, schemaVersion)
	}

	s := &Store{
		path:     path,
		memory:   memory,
		readOnly: true,
		write:    nil,
		read:     read,
		// reqs/stop/closed left nil/zero: no writer goroutine. submit() guards on
		// readOnly before touching them, and Close() skips the writer drain.
	}
	return s, nil
}

// restrictDBFiles best-effort-chmods the main DB file and its WAL/SHM sidecars to
// 0600. The sidecars may not exist yet (created lazily by SQLite); a missing file
// or a chmod error is ignored — this is a hardening measure, not a hard
// requirement, and on Windows chmod has limited semantics.
func restrictDBFiles(path string) {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		_ = os.Chmod(p, 0o600)
	}
}

// pragmaValues builds the common per-connection PRAGMA set shared by the writer,
// reader, and read-only DSNs. Encoded as _pragma query params so modernc applies
// them on every new connection (modernc does not share PRAGMA state across
// connections — see the package doc / TestPragmaPerConn).
func pragmaValues(busyTimeoutMS int) url.Values {
	pragmas := url.Values{}
	pragmas.Add("_pragma", "journal_mode(WAL)")
	pragmas.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS))
	pragmas.Add("_pragma", "synchronous(NORMAL)")
	pragmas.Add("_pragma", "foreign_keys(ON)")
	return pragmas
}

// buildDSNs constructs the writer and reader DSNs. PRAGMAs are encoded as
// _pragma query params so modernc applies them on every new connection.
func buildDSNs(path string, memory bool, busyTimeoutMS int) (writeDSN, readDSN string, err error) {
	pragmas := pragmaValues(busyTimeoutMS)

	if memory {
		// Shared cache so the writer DB and read pool see the same in-memory data.
		base := "file::memory:?cache=shared"
		dsn := base + "&" + pragmas.Encode()
		return dsn, dsn, nil
	}

	// modernc accepts a plain path or file: URI. Use file: URI so query params
	// (PRAGMAs) are unambiguous on Windows paths.
	u := &url.URL{Scheme: "file", Opaque: path, RawQuery: pragmas.Encode()}
	dsn := u.String()
	return dsn, dsn, nil
}

// buildReadOnlyDSN constructs the DSN for OpenReader.
//
// File DBs: mode=ro opens the database read-only (SQLite refuses any write/DDL on
// the connection). We deliberately do NOT set immutable=1: the wlog server may be
// concurrently writing this file, and immutable would tell SQLite the DB never
// changes — a WAL reader must still observe committed writes. The cost of mode=ro
// (vs immutable) is that SQLite creates the usual -wal/-shm shared-memory index to
// read a WAL database; that is expected and is not a mutation of the database
// content. PRAGMAs: busy_timeout + foreign_keys are kept (busy_timeout still
// matters — a RO connection can hit a transient lock while the writer commits).
// journal_mode is omitted (it is a write-mode change with no effect read-only),
// and query_only(ON) is added as a belt-and-suspenders guard so any stray
// statement that tried to write would be rejected by SQLite itself.
//
// Memory DBs: the shared-cache DSN is reused as-is. A read-only in-memory open is
// only meaningful when another live Store already created the shared in-memory DB
// in this process (mode=ro on file::memory: would open an empty private DB), so we
// rely on query_only(ON) to enforce read-only semantics on that shared handle.
func buildReadOnlyDSN(path string, memory bool, busyTimeoutMS int) (string, error) {
	pragmas := url.Values{}
	pragmas.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS))
	pragmas.Add("_pragma", "foreign_keys(ON)")
	pragmas.Add("_pragma", "query_only(ON)")

	if memory {
		base := "file::memory:?cache=shared"
		return base + "&" + pragmas.Encode(), nil
	}

	pragmas.Set("mode", "ro")
	u := &url.URL{Scheme: "file", Opaque: path, RawQuery: pragmas.Encode()}
	return u.String(), nil
}

// applySchema runs schema.sql (idempotent CREATE IF NOT EXISTS).
func (s *Store) applySchema(ctx context.Context) error {
	if _, err := s.write.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("store: apply schema: %w", err)
	}
	return nil
}

// writerLoop is the single writer goroutine. It runs each request inside one
// transaction and serializes all mutations through this goroutine. On Close it
// drains any requests already buffered/blocked on send before exiting.
func (s *Store) writerLoop() {
	defer close(s.closed)
	for {
		select {
		case req := <-s.reqs:
			req.resp <- s.runTxn(req.ctx, req.fn)
		case <-s.stop:
			// Drain requests that are already waiting to be sent, then exit. No
			// new sends can start because submit checks s.stop before sending.
			for {
				select {
				case req := <-s.reqs:
					req.resp <- s.runTxn(req.ctx, req.fn)
				default:
					return
				}
			}
		}
	}
}

// runTxn executes fn inside a transaction, committing on success and rolling
// back on any error.
func (s *Store) runTxn(ctx context.Context, fn func(ctx context.Context, tx *sql.Tx) error) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin txn: %w", err)
	}
	if err := fn(ctx, tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// submit hands a unit of work to the writer goroutine and waits for the result.
// It respects ctx cancellation and refuses work once Close has been initiated,
// so it never sends on a channel that the writer loop has stopped reading.
func (s *Store) submit(ctx context.Context, fn func(ctx context.Context, tx *sql.Tx) error) error {
	// A read-only Store has no writer goroutine (and s.reqs/s.stop are nil); refuse
	// the work cleanly instead of dead-locking or panicking on a nil-channel send.
	if s.readOnly {
		return ErrReadOnly
	}
	req := writeReq{ctx: ctx, fn: fn, resp: make(chan error, 1)}
	// Check stop first: once Close runs, the writer loop may stop reading reqs at
	// any moment, so we must not block on a send that could never be received.
	select {
	case <-s.stop:
		return errors.New("store: closed")
	default:
	}
	select {
	case s.reqs <- req:
	case <-s.stop:
		return errors.New("store: closed")
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.resp:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WriteBatch applies a Batch atomically in one transaction via the single
// writer goroutine. Implements model.Writer.
func (s *Store) WriteBatch(ctx context.Context, b model.Batch) error {
	return s.submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return applyBatch(ctx, tx, b)
	})
}

// ReadDB returns the read connection pool for on-the-fly aggregation queries in
// the api package. It must not be used for writes.
func (s *Store) ReadDB() *sql.DB { return s.read }

// LoadSeriesStates returns all persisted series cursors so the otel delta
// normalizer can resume without re-baselining. Implements
// model.SeriesStateStore. It reads through the read pool.
func (s *Store) LoadSeriesStates(ctx context.Context) ([]model.SeriesState, error) {
	rows, err := s.read.QueryContext(ctx, `
		SELECT series_key, last_value, last_start_unixnano, last_point_unixnano,
		       temporality, baseline_known, updated
		FROM series_state`)
	if err != nil {
		return nil, fmt.Errorf("store: load series states: %w", err)
	}
	defer rows.Close()

	var out []model.SeriesState
	for rows.Next() {
		var st model.SeriesState
		var baseline int64
		if err := rows.Scan(&st.Key, &st.LastValue, &st.LastStartUnixNano,
			&st.LastPointUnixNano, &st.Temporality, &baseline, &st.Updated); err != nil {
			return nil, fmt.Errorf("store: scan series state: %w", err)
		}
		st.BaselineKnown = baseline != 0
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate series states: %w", err)
	}
	return out, nil
}

// Close drains the writer goroutine, checkpoints the WAL (TRUNCATE), and closes
// both connection pools. Safe to call multiple times.
//
// For a read-only Store (OpenReader) there is no writer goroutine to drain and no
// write connection to checkpoint/close; Close simply closes the read pool. A
// read-only Close performs no on-disk mutation (no checkpoint), which is part of
// the "no side effects" guarantee of the read path.
func (s *Store) Close() error {
	if s.readOnly {
		var firstErr error
		s.closeOnce.Do(func() {
			if err := s.read.Close(); err != nil {
				firstErr = fmt.Errorf("store: close read pool: %w", err)
			}
		})
		return firstErr
	}

	// Signal the writer to stop accepting new work; it drains already-queued
	// requests before exiting. submit observes s.stop and refuses new sends, so
	// reqs is never closed (a send on a closed channel would panic).
	s.stopOnce.Do(func() { close(s.stop) })
	<-s.closed // wait for writer goroutine to finish

	var firstErr error
	s.closeOnce.Do(func() {
		// Checkpoint the WAL so the main DB file is fully consistent on disk.
		// Best-effort: a memory DB or already-closed conn may error; do not mask
		// a real close error with it.
		if !s.memory {
			if _, err := s.write.ExecContext(context.Background(),
				`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("store: wal checkpoint: %w", err)
			}
		}
		if err := s.read.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("store: close read pool: %w", err)
		}
		if err := s.write.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("store: close write conn: %w", err)
		}
	})
	return firstErr
}
