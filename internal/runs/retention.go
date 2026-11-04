package runs

import (
	"context"
	"time"
)

// PruneFinished deletes finished runs (succeeded / failed / canceled) that
// finished before olderThan, together with their events — except the newest
// keepPerPipeline runs of each (pipeline, repo), which are always kept so a
// rarely-run pipeline (a monthly backtest) never loses its history or its
// "last succeeded" timestamp, which the dead-man alerts read (ADR 0009).
// Queued and running runs are never touched. Returns the deleted run IDs so
// the caller can remove their log directories.
func (s *Store) PruneFinished(ctx context.Context, olderThan time.Time, keepPerPipeline int) ([]string, error) {
	if keepPerPipeline < 0 {
		keepPerPipeline = 0
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH ranked AS (
			SELECT id, finished_at,
			       ROW_NUMBER() OVER (PARTITION BY pipeline, repo ORDER BY queued_at DESC, id DESC) AS rn
			FROM runs
			WHERE status IN ('succeeded', 'failed', 'canceled') AND finished_at IS NOT NULL
		)
		SELECT id FROM ranked
		WHERE rn > ? AND julianday(finished_at) < julianday(?)`,
		keepPerPipeline, olderThan.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE run_id = ?`, id); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE id = ?`, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// Exists reports whether a run with this ID is in the registry.
func (s *Store) Exists(ctx context.Context, id string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE id = ?`, id).Scan(&n)
	return n > 0, err
}

// Compact returns freed pages to the filesystem: VACUUM rewrites the file
// without the deleted rows, then the WAL is checkpointed and truncated. Cheap
// at Petal's size (a few MB); call it after a prune that deleted something.
func (s *Store) Compact(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// DBStats is the registry's size, for /metrics.
type DBStats struct {
	Runs, Events int64
	Bytes        int64 // page_count * page_size of the main file
}

// Size reports row counts and the main database file size.
func (s *Store) Size(ctx context.Context) (DBStats, error) {
	var st DBStats
	if err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM runs), (SELECT count(*) FROM events)`).Scan(&st.Runs, &st.Events); err != nil {
		return st, err
	}
	var pages, size int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&size); err != nil {
		return st, err
	}
	st.Bytes = pages * size
	return st, nil
}
