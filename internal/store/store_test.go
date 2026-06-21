package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openwong2kim/wlog/internal/model"
)

// openMem opens a shared in-memory store for a test and registers cleanup.
func openMem(t *testing.T) *Store {
	t.Helper()
	st, err := Open("", true, 5000)
	if err != nil {
		t.Fatalf("Open(memory): %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return st
}

// openFile opens a file-backed store in a temp dir.
func openFile(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wlog.db")
	st, err := Open(path, false, 5000)
	if err != nil {
		t.Fatalf("Open(file): %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return st, path
}

func countRows(t *testing.T, st *Store, table string) int {
	t.Helper()
	var n int
	q := fmt.Sprintf("SELECT COUNT(*) FROM %s", table)
	if err := st.ReadDB().QueryRowContext(context.Background(), q).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// sampleBatch builds a batch that touches every table for session sid.
func sampleBatch(sid string, suffix string) model.Batch {
	return model.Batch{
		Sessions: []model.Session{{
			ID: sid, FirstSeen: 1000, LastSeen: 2000,
			AppVersion: "1.2.3", ModelSet: "claude", UserEmail: "u@x.com",
			OrgID: "org1", TokenSource: "events", HasEvents: true,
			AttrsJSON: `{"k":"v"}`,
		}},
		APIRequests: []model.APIRequest{{
			SessionID: sid, TS: 1500, Model: "claude-3",
			Tokens:  model.Tokens{Input: 10, Output: 20, CacheRead: 5, CacheCreation: 2},
			CostUSD: 0.01, DurationMS: 120, DedupKey: "api-" + suffix,
		}},
		ToolDecisions: []model.ToolDecision{{
			SessionID: sid, TS: 1600, Decision: "accept", Source: "user",
			ToolName: "Bash", DedupKey: "dec-" + suffix,
		}},
		ToolResults: []model.ToolResult{{
			SessionID: sid, TS: 1700, ToolName: "Bash", Success: true,
			DurationMS: 50, BashCommand: "ls", DedupKey: "res-" + suffix,
		}},
		Events: []model.Event{{
			SessionID: sid, TS: 1800, Name: "user_prompt", DedupKey: "evt-" + suffix,
		}},
		Spans: []model.Span{{
			SessionID: sid, TraceID: "trace-" + suffix, SpanID: "span-" + suffix,
			Name: "root", StartTS: 1000, EndTS: 2000, Status: "OK",
		}},
		MetricPoints: []model.MetricPoint{{
			SessionID: sid, TS: 1900, Name: "claude_code.token.usage",
			ValueDelta: 30, ValueKind: 1, SeriesKey: "series-" + suffix,
			StartUnixNano: 100, TimeUnixNano: 200,
		}},
		SeriesStates: []model.SeriesState{{
			Key: "series-" + suffix, LastValue: 30, LastStartUnixNano: 100,
			LastPointUnixNano: 200, Temporality: 2, BaselineKnown: true, Updated: 1900,
		}},
	}
}

func TestWriteBatchBasic(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()

	if err := st.WriteBatch(ctx, sampleBatch("s1", "a")); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	for _, tc := range []struct {
		table string
		want  int
	}{
		{"sessions", 1}, {"api_requests", 1}, {"tool_decisions", 1},
		{"tool_results", 1}, {"events", 1}, {"spans", 1},
		{"metric_points", 1}, {"series_state", 1},
	} {
		if got := countRows(t, st, tc.table); got != tc.want {
			t.Errorf("%s: got %d rows, want %d", tc.table, got, tc.want)
		}
	}
}

// TestIdempotency verifies that re-applying an identical batch does not change
// any row count (PLAN §8 idempotency: dedup_key, series triple, trace/span).
func TestIdempotency(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	b := sampleBatch("s1", "a")

	if err := st.WriteBatch(ctx, b); err != nil {
		t.Fatalf("first WriteBatch: %v", err)
	}
	before := map[string]int{}
	tables := []string{"sessions", "api_requests", "tool_decisions", "tool_results", "events", "spans", "metric_points", "series_state"}
	for _, tbl := range tables {
		before[tbl] = countRows(t, st, tbl)
	}

	// Re-deliver the same batch twice more.
	for i := 0; i < 2; i++ {
		if err := st.WriteBatch(ctx, b); err != nil {
			t.Fatalf("re-deliver WriteBatch: %v", err)
		}
	}
	for _, tbl := range tables {
		if got := countRows(t, st, tbl); got != before[tbl] {
			t.Errorf("%s: row count changed on re-delivery: got %d, want %d", tbl, got, before[tbl])
		}
	}
}

// apiReqRow reads the cost/token/cost_source columns of the single api_request
// at dedup_key for assertions.
func apiReqRow(t *testing.T, st *Store, dedup string) (cost float64, in, out, cr, cc int64, src string) {
	t.Helper()
	var srcNull sql.NullString
	row := st.ReadDB().QueryRowContext(context.Background(), `
		SELECT cost_usd, input_tokens, output_tokens, cache_read, cache_creation,
		       COALESCE(cost_source,'')
		FROM api_requests WHERE dedup_key = ?`, dedup)
	if err := row.Scan(&cost, &in, &out, &cr, &cc, &srcNull); err != nil {
		t.Fatalf("read api_request %q: %v", dedup, err)
	}
	return cost, in, out, cr, cc, srcNull.String
}

// otlpReq / jsonlReq build the two ingestion paths' view of the SAME logical
// api_request (same dedup_key) for the P1 cost-authority tests: OTLP carries the
// real cost and is authoritative; JSONL carries a pricing-table estimate.
func otlpReq(sid, dedup string, cost float64, in, out int64) model.Batch {
	return model.Batch{
		Sessions: []model.Session{{ID: sid, FirstSeen: 1000, LastSeen: 2000}},
		APIRequests: []model.APIRequest{{
			SessionID: sid, TS: 1500, Model: "claude-x",
			Tokens: model.Tokens{Input: in, Output: out}, CostUSD: cost,
			CostSource: model.CostSourceOTLP, DedupKey: dedup,
		}},
	}
}

func jsonlReq(sid, dedup string, cost float64, in, out int64) model.Batch {
	return model.Batch{
		Sessions: []model.Session{{ID: sid, FirstSeen: 1000, LastSeen: 2000}},
		APIRequests: []model.APIRequest{{
			SessionID: sid, TS: 1500, Model: "claude-x",
			Tokens: model.Tokens{Input: in, Output: out}, CostUSD: cost,
			CostSource: model.CostSourceJSONLEstimate, DedupKey: dedup,
		}},
	}
}

// TestAPIRequestCostAuthority is the P1 gate (PLAN v3 §9): when JSONL and OTLP
// describe the same call (same dedup_key), the authoritative OTLP cost+tokens
// must WIN over the JSONL estimate, regardless of arrival order, and a JSONL
// re-scan must never overwrite an OTLP-filled row. The transcript history scan
// typically inserts the estimate BEFORE the live OTLP event lands, so the
// estimate-then-OTLP order is the critical one.
func TestAPIRequestCostAuthority(t *testing.T) {
	ctx := context.Background()
	const sid, dedup = "s-cost", "shared-key-1"

	t.Run("jsonl estimate then OTLP real -> OTLP wins", func(t *testing.T) {
		st := openMem(t)
		// 1) Transcript scan inserts the pricing-table estimate first.
		if err := st.WriteBatch(ctx, jsonlReq(sid, dedup, 0.011, 10, 20)); err != nil {
			t.Fatal(err)
		}
		cost, _, _, _, _, src := apiReqRow(t, st, dedup)
		if cost != 0.011 || src != model.CostSourceJSONLEstimate {
			t.Fatalf("after JSONL insert: cost=%v src=%q, want 0.011/%s", cost, src, model.CostSourceJSONLEstimate)
		}
		// 2) The live OTLP event for the SAME call arrives with the real cost+tokens.
		if err := st.WriteBatch(ctx, otlpReq(sid, dedup, 0.0273, 12, 25)); err != nil {
			t.Fatal(err)
		}
		// Still one row (collapsed by dedup_key), now carrying the OTLP figures.
		if n := countRows(t, st, "api_requests"); n != 1 {
			t.Fatalf("api_requests rows = %d, want 1 (dedup collapse)", n)
		}
		cost, in, out, _, _, src := apiReqRow(t, st, dedup)
		if cost != 0.0273 {
			t.Errorf("cost_usd = %v, want 0.0273 (OTLP authoritative beats estimate)", cost)
		}
		if in != 12 || out != 25 {
			t.Errorf("tokens = %d/%d, want 12/25 (OTLP tokens win)", in, out)
		}
		if src != model.CostSourceOTLP {
			t.Errorf("cost_source = %q, want %s", src, model.CostSourceOTLP)
		}
	})

	t.Run("OTLP real then JSONL re-scan -> estimate does NOT overwrite", func(t *testing.T) {
		st := openMem(t)
		if err := st.WriteBatch(ctx, otlpReq(sid, dedup, 0.0273, 12, 25)); err != nil {
			t.Fatal(err)
		}
		// A later transcript re-scan of the same call must be a no-op for cost/tokens.
		if err := st.WriteBatch(ctx, jsonlReq(sid, dedup, 0.011, 10, 20)); err != nil {
			t.Fatal(err)
		}
		cost, in, out, _, _, src := apiReqRow(t, st, dedup)
		if cost != 0.0273 || in != 12 || out != 25 || src != model.CostSourceOTLP {
			t.Fatalf("OTLP row was clobbered by JSONL re-scan: cost=%v in=%d out=%d src=%q", cost, in, out, src)
		}
	})

	t.Run("JSONL re-scan is idempotent over an OTLP row", func(t *testing.T) {
		st := openMem(t)
		if err := st.WriteBatch(ctx, otlpReq(sid, dedup, 0.0273, 12, 25)); err != nil {
			t.Fatal(err)
		}
		// Re-scan the transcript several times: cost/tokens must never drift.
		for i := 0; i < 3; i++ {
			if err := st.WriteBatch(ctx, jsonlReq(sid, dedup, 0.011, 10, 20)); err != nil {
				t.Fatal(err)
			}
		}
		cost, in, out, _, _, src := apiReqRow(t, st, dedup)
		if cost != 0.0273 || in != 12 || out != 25 || src != model.CostSourceOTLP {
			t.Fatalf("repeated JSONL re-scan drifted the OTLP row: cost=%v in=%d out=%d src=%q", cost, in, out, src)
		}
		if n := countRows(t, st, "api_requests"); n != 1 {
			t.Fatalf("api_requests rows = %d, want 1", n)
		}
	})

	t.Run("unknown-model $0 estimate replaced by OTLP real cost", func(t *testing.T) {
		st := openMem(t)
		// JSONL estimate for an UNKNOWN model is $0 (pricing.Lookup miss) — exactly
		// the failure the gate guards against: a live session showing $0.
		if err := st.WriteBatch(ctx, jsonlReq(sid, dedup, 0, 100, 200)); err != nil {
			t.Fatal(err)
		}
		if cost, _, _, _, _, _ := apiReqRow(t, st, dedup); cost != 0 {
			t.Fatalf("precondition: unknown-model estimate cost = %v, want 0", cost)
		}
		// The OTLP event carries Claude Code's real cost → must replace the $0.
		if err := st.WriteBatch(ctx, otlpReq(sid, dedup, 0.42, 100, 200)); err != nil {
			t.Fatal(err)
		}
		cost, _, _, _, _, src := apiReqRow(t, st, dedup)
		if cost != 0.42 || src != model.CostSourceOTLP {
			t.Fatalf("unknown-model $0 not replaced: cost=%v src=%q, want 0.42/%s", cost, src, model.CostSourceOTLP)
		}
	})
}

// TestSessionUpsertMinMax verifies first_seen=min, last_seen=max independent of
// delivery order, and token_source/has_events merge.
func TestSessionUpsertMinMax(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()

	// First a later window, then an earlier one (out of order on purpose).
	b1 := model.Batch{Sessions: []model.Session{{ID: "s1", FirstSeen: 5000, LastSeen: 9000, TokenSource: "metrics", HasEvents: false}}}
	b2 := model.Batch{Sessions: []model.Session{{ID: "s1", FirstSeen: 1000, LastSeen: 3000, TokenSource: "events", HasEvents: true}}}

	if err := st.WriteBatch(ctx, b1); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteBatch(ctx, b2); err != nil {
		t.Fatal(err)
	}

	var first, last, hasEvents int64
	var src string
	row := st.ReadDB().QueryRowContext(ctx,
		"SELECT first_seen, last_seen, token_source, has_events FROM sessions WHERE id='s1'")
	if err := row.Scan(&first, &last, &src, &hasEvents); err != nil {
		t.Fatal(err)
	}
	if first != 1000 {
		t.Errorf("first_seen: got %d, want 1000 (min)", first)
	}
	if last != 9000 {
		t.Errorf("last_seen: got %d, want 9000 (max)", last)
	}
	if src != "events" {
		t.Errorf("token_source: got %q, want events (non-empty overwrites)", src)
	}
	if hasEvents != 1 {
		t.Errorf("has_events: got %d, want 1 (sticky merge)", hasEvents)
	}

	// Reverse order in a fresh store yields the same min/max.
	st2 := openMem(t)
	if err := st2.WriteBatch(ctx, b2); err != nil {
		t.Fatal(err)
	}
	if err := st2.WriteBatch(ctx, b1); err != nil {
		t.Fatal(err)
	}
	row = st2.ReadDB().QueryRowContext(ctx, "SELECT first_seen, last_seen FROM sessions WHERE id='s1'")
	if err := row.Scan(&first, &last); err != nil {
		t.Fatal(err)
	}
	if first != 1000 || last != 9000 {
		t.Errorf("reverse order: first/last got %d/%d, want 1000/9000", first, last)
	}
}

// TestLoadSeriesStatesRoundTrip writes series states and loads them back.
func TestLoadSeriesStatesRoundTrip(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()

	want := []model.SeriesState{
		{Key: "k1", LastValue: 12.5, LastStartUnixNano: 100, LastPointUnixNano: 200, Temporality: 2, BaselineKnown: true, Updated: 999},
		{Key: "k2", LastValue: 0, LastStartUnixNano: 0, LastPointUnixNano: 0, Temporality: 1, BaselineKnown: false, Updated: 1},
	}
	if err := st.WriteBatch(ctx, model.Batch{SeriesStates: want}); err != nil {
		t.Fatal(err)
	}

	got, err := st.LoadSeriesStates(ctx)
	if err != nil {
		t.Fatalf("LoadSeriesStates: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d states, want %d", len(got), len(want))
	}
	byKey := map[string]model.SeriesState{}
	for _, s := range got {
		byKey[s.Key] = s
	}
	for _, w := range want {
		g, ok := byKey[w.Key]
		if !ok {
			t.Errorf("missing series %q", w.Key)
			continue
		}
		if g != w {
			t.Errorf("series %q: got %+v, want %+v", w.Key, g, w)
		}
	}
}

// TestPragmaPerConn verifies that the required PRAGMAs are applied on each
// connection of both pools (foreign_keys must be 1 everywhere; for file DBs the
// journal_mode must be wal).
func TestPragmaPerConn(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		st := openMem(t)
		assertForeignKeys(t, st)
	})
	t.Run("file", func(t *testing.T) {
		st, _ := openFile(t)
		assertForeignKeys(t, st)
		// journal_mode=wal only meaningful for file DBs.
		var mode string
		if err := st.ReadDB().QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if mode != "wal" {
			t.Errorf("file journal_mode: got %q, want wal", mode)
		}
	})
}

// assertForeignKeys opens several concurrent connections in the read pool and
// confirms foreign_keys==1 on each, proving the DSN PRAGMA is per-conn and not a
// one-time fluke on the first connection.
func assertForeignKeys(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()

	// Writer connection.
	var fk int
	if err := st.write.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Errorf("writer foreign_keys: got %d, want 1", fk)
	}

	// Force the read pool to open multiple distinct connections at once and
	// check each one.
	var wg sync.WaitGroup
	var bad atomic.Int32
	start := make(chan struct{})
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			conn, err := st.ReadDB().Conn(ctx)
			if err != nil {
				bad.Add(1)
				return
			}
			defer conn.Close()
			var v int
			if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&v); err != nil || v != 1 {
				bad.Add(1)
			}
			// Hold the connection briefly so the pool must open siblings.
			time.Sleep(20 * time.Millisecond)
		}()
	}
	close(start)
	wg.Wait()
	if bad.Load() != 0 {
		t.Errorf("%d read connections had foreign_keys != 1", bad.Load())
	}
}

// TestForeignKeyEnforced confirms foreign_keys=ON is actually enforced: a child
// row referencing a missing session must be rejected.
func TestForeignKeyEnforced(t *testing.T) {
	st := openMem(t)
	ctx := context.Background()
	err := st.WriteBatch(ctx, model.Batch{
		Events: []model.Event{{SessionID: "ghost", TS: 1, Name: "x", DedupKey: "e1"}},
	})
	if err == nil {
		t.Fatal("expected FK violation inserting event for missing session, got nil")
	}
}

// TestConcurrentReadDuringWrite runs many writers and readers concurrently and
// asserts there are no SQLITE_BUSY errors (writes serialize through one
// goroutine; reads use the WAL read pool). Run under -race.
func TestConcurrentReadDuringWrite(t *testing.T) {
	st, _ := openFile(t)
	ctx := context.Background()

	const writers = 8
	const perWriter = 25
	var wg sync.WaitGroup
	errCh := make(chan error, writers+8)

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			sid := fmt.Sprintf("sess-%d", w)
			for i := 0; i < perWriter; i++ {
				b := sampleBatch(sid, fmt.Sprintf("%d-%d", w, i))
				if err := st.WriteBatch(ctx, b); err != nil {
					errCh <- fmt.Errorf("writer %d: %w", w, err)
					return
				}
			}
		}(w)
	}

	// Concurrent readers hammering the read pool while writes happen.
	stop := make(chan struct{})
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				var n int
				if err := st.ReadDB().QueryRowContext(ctx,
					"SELECT COUNT(*) FROM api_requests").Scan(&n); err != nil {
					errCh <- fmt.Errorf("reader: %w", err)
					return
				}
			}
		}()
	}

	// Wait for writers, then stop readers.
	go func() {
		// Writers finish; signal readers to stop after writers join.
	}()
	// Join writers first by waiting on a dedicated group is awkward with shared
	// wg, so just sleep-poll until expected rows are present, then stop readers.
	deadline := time.After(30 * time.Second)
	for {
		var n int
		if err := st.ReadDB().QueryRowContext(ctx, "SELECT COUNT(*) FROM api_requests").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= writers*perWriter {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for writes; have %d rows", n)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	close(stop)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Errorf("concurrency error: %v", err)
		}
	}

	if got := countRows(t, st, "api_requests"); got != writers*perWriter {
		t.Errorf("api_requests: got %d, want %d", got, writers*perWriter)
	}
}

// TestCloseIdempotent confirms Close can be called more than once safely.
func TestCloseIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wlog.db")
	st, err := Open(path, false, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteBatch(context.Background(), sampleBatch("s1", "a")); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestFileDBPermissions is the F3 regression: a file-backed DB (and its WAL/SHM
// sidecars, when present) must be chmod'd to 0600 so a driver/umask default of
// 0644 cannot leave telemetry world-readable. Unix-only: Windows os.Chmod has no
// 0600 semantics (file ACLs govern access), so the perm bits are not asserted
// there.
func TestFileDBPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits not meaningful on Windows")
	}
	st, path := openFile(t)
	// Force a write so WAL/SHM sidecars exist, then checkpoint via Close later.
	if err := st.WriteBatch(context.Background(), sampleBatch("s1", "a")); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	// The main DB file must be exactly 0600.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("db file perm = %o, want 600", perm)
	}

	// Sidecars, if present, must also be 0600 (best-effort: skip if absent).
	for _, suffix := range []string{"-wal", "-shm"} {
		fi, err := os.Stat(path + suffix)
		if err != nil {
			continue
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s file perm = %o, want 600", suffix, perm)
		}
	}
}

// TestWriteAfterClose ensures WriteBatch fails cleanly once the store is closed.
func TestWriteAfterClose(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "wlog.db"), false, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteBatch(context.Background(), sampleBatch("s1", "a")); err == nil {
		t.Fatal("expected error writing to closed store, got nil")
	}
}
