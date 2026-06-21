package store

import (
	"context"
	"testing"
	"time"

	"github.com/openwong2kim/wlog/internal/model"
)

// sessionBatchAt builds a batch for one session with the given last_seen (ms)
// plus a child row in every cascading table so we can assert CASCADE cleanup.
func sessionBatchAt(sid string, lastSeenMS int64) model.Batch {
	return model.Batch{
		Sessions: []model.Session{{
			ID: sid, FirstSeen: lastSeenMS - 1000, LastSeen: lastSeenMS,
			TokenSource: "events", HasEvents: true,
		}},
		APIRequests: []model.APIRequest{{
			SessionID: sid, TS: lastSeenMS, Model: "m", DedupKey: "api-" + sid,
		}},
		ToolDecisions: []model.ToolDecision{{
			SessionID: sid, TS: lastSeenMS, Decision: "accept", DedupKey: "dec-" + sid,
		}},
		ToolResults: []model.ToolResult{{
			SessionID: sid, TS: lastSeenMS, ToolName: "Bash", DedupKey: "res-" + sid,
		}},
		Events: []model.Event{{
			SessionID: sid, TS: lastSeenMS, Name: "user_prompt", DedupKey: "evt-" + sid,
		}},
		Spans: []model.Span{{
			SessionID: sid, TraceID: "tr-" + sid, SpanID: "sp-" + sid, Name: "root",
		}},
		MetricPoints: []model.MetricPoint{{
			SessionID: sid, TS: lastSeenMS, Name: "n", SeriesKey: "sk-" + sid,
			StartUnixNano: 1, TimeUnixNano: 2,
		}},
	}
}

func childTables() []string {
	return []string{"api_requests", "tool_decisions", "tool_results", "events", "spans", "metric_points"}
}

// TestRetentionByAge deletes sessions older than the retention window and
// asserts CASCADE removed all child rows (file-backed DB, WAL on).
func TestRetentionByAge(t *testing.T) {
	st, _ := openFile(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// One old session (10 days), one recent (1 hour).
	old := now - (10 * 24 * time.Hour).Milliseconds()
	recent := now - time.Hour.Milliseconds()
	if err := st.WriteBatch(ctx, sessionBatchAt("old", old)); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteBatch(ctx, sessionBatchAt("recent", recent)); err != nil {
		t.Fatal(err)
	}

	// Retain only the last 7 days.
	if err := st.RunRetention(ctx, 7*24*time.Hour, 0); err != nil {
		t.Fatalf("RunRetention: %v", err)
	}

	if got := countRows(t, st, "sessions"); got != 1 {
		t.Fatalf("sessions after age retention: got %d, want 1", got)
	}
	if !sessionExists(t, st, "recent") {
		t.Error("recent session was deleted")
	}
	if sessionExists(t, st, "old") {
		t.Error("old session survived retention")
	}
	// CASCADE: the old session's child rows must be gone; recent's remain.
	for _, tbl := range childTables() {
		if got := countRows(t, st, tbl); got != 1 {
			t.Errorf("%s after age retention: got %d, want 1 (CASCADE should leave only recent)", tbl, got)
		}
	}
}

// TestRetentionByMaxSessions keeps only the newest maxSessions, deleting the
// oldest first, and verifies CASCADE.
func TestRetentionByMaxSessions(t *testing.T) {
	st, _ := openFile(t)
	ctx := context.Background()
	base := time.Now().UnixMilli()

	// Five sessions with increasing last_seen: s0 oldest .. s4 newest.
	ids := []string{"s0", "s1", "s2", "s3", "s4"}
	for i, id := range ids {
		if err := st.WriteBatch(ctx, sessionBatchAt(id, base+int64(i)*1000)); err != nil {
			t.Fatal(err)
		}
	}

	// Keep only the 2 newest.
	if err := st.RunRetention(ctx, 0, 2); err != nil {
		t.Fatalf("RunRetention: %v", err)
	}

	if got := countRows(t, st, "sessions"); got != 2 {
		t.Fatalf("sessions after max-sessions retention: got %d, want 2", got)
	}
	// s3, s4 are newest and must survive; s0..s2 deleted.
	for _, keep := range []string{"s3", "s4"} {
		if !sessionExists(t, st, keep) {
			t.Errorf("expected %s to survive", keep)
		}
	}
	for _, gone := range []string{"s0", "s1", "s2"} {
		if sessionExists(t, st, gone) {
			t.Errorf("expected %s to be deleted (oldest first)", gone)
		}
	}
	for _, tbl := range childTables() {
		if got := countRows(t, st, tbl); got != 2 {
			t.Errorf("%s after max-sessions retention: got %d, want 2", tbl, got)
		}
	}
}

// sessionBatchWithSeriesState extends sessionBatchAt with a series_state row
// keyed on the same series_key the metric_point uses, so retention's orphan
// sweep (C8) can be observed end-to-end.
func sessionBatchWithSeriesState(sid string, lastSeenMS int64) model.Batch {
	b := sessionBatchAt(sid, lastSeenMS)
	b.SeriesStates = []model.SeriesState{{
		Key: "sk-" + sid, LastValue: 1, LastStartUnixNano: 1, LastPointUnixNano: 2,
		Temporality: 2, BaselineKnown: true, Updated: lastSeenMS,
	}}
	return b
}

// TestRetentionCleansOrphanSeriesState is the C8 regression: series_state has no
// FK to sessions/metric_points, so a session delete cascades to metric_points but
// leaves the series_state cursor behind. RunRetention must sweep cursors whose
// series_key no longer appears in any metric_point.
func TestRetentionCleansOrphanSeriesState(t *testing.T) {
	st, _ := openFile(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	old := now - (10 * 24 * time.Hour).Milliseconds()
	recent := now - time.Hour.Milliseconds()
	if err := st.WriteBatch(ctx, sessionBatchWithSeriesState("old", old)); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteBatch(ctx, sessionBatchWithSeriesState("recent", recent)); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, st, "series_state"); got != 2 {
		t.Fatalf("series_state before retention: got %d, want 2", got)
	}

	// Drop the old session (age rule). Its metric_points cascade away; the
	// matching series_state must be swept; the recent one must remain.
	if err := st.RunRetention(ctx, 7*24*time.Hour, 0); err != nil {
		t.Fatalf("RunRetention: %v", err)
	}

	if got := countRows(t, st, "series_state"); got != 1 {
		t.Fatalf("series_state after retention: got %d, want 1 (orphan swept)", got)
	}
	// The surviving cursor must be the recent session's.
	var key string
	if err := st.ReadDB().QueryRowContext(ctx,
		"SELECT series_key FROM series_state").Scan(&key); err != nil {
		t.Fatal(err)
	}
	if key != "sk-recent" {
		t.Fatalf("surviving series_state = %q, want sk-recent", key)
	}
}

// TestRetentionKeepsLiveSeriesState verifies the sweep does NOT remove cursors
// whose series still has metric_points (no false-positive cleanup).
func TestRetentionKeepsLiveSeriesState(t *testing.T) {
	st, _ := openFile(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if err := st.WriteBatch(ctx, sessionBatchWithSeriesState("live", now)); err != nil {
		t.Fatal(err)
	}
	// Noop retention (no rules) must not touch a live cursor.
	if err := st.RunRetention(ctx, 0, 0); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, st, "series_state"); got != 1 {
		t.Fatalf("live series_state swept by noop retention: got %d, want 1", got)
	}
}

// TestRetentionNoop verifies that disabling both rules deletes nothing.
func TestRetentionNoop(t *testing.T) {
	st, _ := openFile(t)
	ctx := context.Background()
	if err := st.WriteBatch(ctx, sessionBatchAt("s1", time.Now().UnixMilli())); err != nil {
		t.Fatal(err)
	}
	if err := st.RunRetention(ctx, 0, 0); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, st, "sessions"); got != 1 {
		t.Errorf("noop retention deleted rows: sessions=%d, want 1", got)
	}
}

// TestRetentionConcurrentWithWrites runs retention while writes are in flight to
// confirm both serialize through the single writer without BUSY. Run under -race.
func TestRetentionConcurrentWithWrites(t *testing.T) {
	st, _ := openFile(t)
	ctx := context.Background()
	base := time.Now().UnixMilli()

	done := make(chan error, 1)
	go func() {
		for i := 0; i < 50; i++ {
			if err := st.WriteBatch(ctx, sessionBatchAt(
				"w"+itoa(i), base+int64(i)*1000)); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	for i := 0; i < 10; i++ {
		if err := st.RunRetention(ctx, 0, 20); err != nil {
			t.Fatalf("RunRetention during writes: %v", err)
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("concurrent writes: %v", err)
	}

	// Final retention cap holds.
	if err := st.RunRetention(ctx, 0, 20); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, st, "sessions"); got > 20 {
		t.Errorf("sessions after final retention: got %d, want <= 20", got)
	}
}

func sessionExists(t *testing.T, st *Store, id string) bool {
	t.Helper()
	var n int
	if err := st.ReadDB().QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM sessions WHERE id=?", id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [12]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
