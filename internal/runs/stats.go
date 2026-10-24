package runs

import "context"

// RunStat aggregates the registry per (pipeline, repo, status) — the source of
// the /metrics endpoint. Computed from SQLite on demand, so the numbers are
// exact and survive restarts (no in-memory counters to lose).
type RunStat struct {
	Pipeline, Repo, Status string
	Count                  int64
	Finished               int64   // runs with started_at and finished_at
	DurationSum            float64 // seconds, over Finished runs
	LastFinished           float64 // unix seconds; 0 if none finished
}

// Stats returns one RunStat per (pipeline, repo, status) group.
func (s *Store) Stats(ctx context.Context) ([]RunStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT pipeline, repo, status, count(*),
		       count(CASE WHEN started_at IS NOT NULL AND finished_at IS NOT NULL THEN 1 END),
		       coalesce(sum(CASE WHEN started_at IS NOT NULL AND finished_at IS NOT NULL
		                         THEN (julianday(finished_at) - julianday(started_at)) * 86400 END), 0),
		       coalesce(max(CASE WHEN finished_at IS NOT NULL
		                         THEN (julianday(finished_at) - 2440587.5) * 86400 END), 0)
		FROM runs GROUP BY pipeline, repo, status ORDER BY pipeline, repo, status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunStat
	for rows.Next() {
		var st RunStat
		if err := rows.Scan(&st.Pipeline, &st.Repo, &st.Status, &st.Count, &st.Finished, &st.DurationSum, &st.LastFinished); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ScheduleCounts returns how many schedules are enabled and disabled.
func (s *Store) ScheduleCounts(ctx context.Context) (enabled, disabled int64, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT count(CASE WHEN enabled = 1 THEN 1 END), count(CASE WHEN enabled = 0 THEN 1 END) FROM schedules`).
		Scan(&enabled, &disabled)
	return enabled, disabled, err
}
