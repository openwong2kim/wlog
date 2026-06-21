package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// RunRetention enforces the retention policy (PLAN §8 / REVIEWS F / CEO F6:
// default 90 days OR 1000 sessions, whichever comes first). It runs through the
// single writer goroutine so it serializes with ingest writes; child rows are
// removed by ON DELETE CASCADE (foreign_keys=ON per connection).
//
//   - retention: max age; sessions whose last_seen < now-retention are deleted.
//     A non-positive retention disables the age rule.
//   - maxSessions: cap on retained sessions; the oldest (by last_seen) beyond the
//     cap are deleted. A non-positive maxSessions disables the count rule.
//
// Both rules apply in the same transaction so the result is consistent.
func (s *Store) RunRetention(ctx context.Context, retention time.Duration, maxSessions int) error {
	now := time.Now()
	return s.submit(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if retention > 0 {
			cutoff := now.Add(-retention).UnixMilli()
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM sessions WHERE last_seen < ?`, cutoff); err != nil {
				return fmt.Errorf("store: retention age delete: %w", err)
			}
		}

		if maxSessions > 0 {
			// Delete sessions ranked beyond the cap, keeping the most recent
			// maxSessions by last_seen (then id as a stable tiebreaker).
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM sessions
				WHERE id IN (
					SELECT id FROM sessions
					ORDER BY last_seen DESC, id DESC
					LIMIT -1 OFFSET ?
				)`, maxSessions); err != nil {
				return fmt.Errorf("store: retention count delete: %w", err)
			}
		}

		// series_state has no FK to sessions/metric_points, so cascade deletes
		// never reach it: without this sweep it grows unbounded and seeds stale
		// delta cursors on restart. After the metric_points for a series are gone
		// (CASCADE from the session deletes above), drop the orphaned cursor in the
		// same transaction so the cleanup stays consistent (C8). Runs in the writer
		// goroutine, no schema/model change.
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM series_state
			WHERE series_key NOT IN (SELECT DISTINCT series_key FROM metric_points)`); err != nil {
			return fmt.Errorf("store: retention series_state cleanup: %w", err)
		}
		return nil
	})
}
