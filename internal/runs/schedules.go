package runs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Schedule is a recurring pipeline trigger.
type Schedule struct {
	ID        string  `json:"id"`
	Pipeline  string  `json:"pipeline"`
	ModuleFn  string  `json:"module_fn"`
	ArgsJSON  string  `json:"args"`
	Cron      string  `json:"cron"`
	Enabled   bool    `json:"enabled"`
	LastRunAt *string `json:"last_run_at,omitempty"`
	NextRunAt string  `json:"next_run_at"`
}

const scheduleSchema = `
CREATE TABLE IF NOT EXISTS schedules (
	id         TEXT PRIMARY KEY,
	pipeline   TEXT NOT NULL,
	module_fn  TEXT NOT NULL DEFAULT '',
	args_json  TEXT NOT NULL DEFAULT '{}',
	cron       TEXT NOT NULL,
	enabled    INTEGER NOT NULL DEFAULT 1,
	last_run_at TEXT,
	next_run_at TEXT NOT NULL
);
`

// CreateSchedule inserts a schedule with its first computed due time.
func (s *Store) CreateSchedule(ctx context.Context, pipeline, moduleFn, argsJSON, cronExpr, nextRunAt string) (*Schedule, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	id := "sched_" + hex.EncodeToString(b)
	if argsJSON == "" {
		argsJSON = "{}"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO schedules (id, pipeline, module_fn, args_json, cron, enabled, next_run_at) VALUES (?, ?, ?, ?, ?, 1, ?)`,
		id, pipeline, moduleFn, argsJSON, cronExpr, nextRunAt)
	if err != nil {
		return nil, err
	}
	return s.GetSchedule(ctx, id)
}

// GetSchedule returns one schedule (nil if missing).
func (s *Store) GetSchedule(ctx context.Context, id string) (*Schedule, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM schedules WHERE id = ?`, id)
	return scanSchedule(row)
}

// ListSchedules returns all schedules.
func (s *Store) ListSchedules(ctx context.Context) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+scheduleColumns+` FROM schedules ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		sch, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sch)
	}
	return out, rows.Err()
}

// DueSchedules returns enabled schedules whose next_run_at is at or before now.
func (s *Store) DueSchedules(ctx context.Context, now time.Time) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+scheduleColumns+` FROM schedules WHERE enabled = 1 AND next_run_at <= ? ORDER BY next_run_at ASC`, nowRFCFrom(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		sch, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sch)
	}
	return out, rows.Err()
}

// MarkScheduleRan records a firing and its next due time.
func (s *Store) MarkScheduleRan(ctx context.Context, id string, ranAt, nextRunAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE schedules SET last_run_at = ?, next_run_at = ? WHERE id = ?`, nowRFCFrom(ranAt), nowRFCFrom(nextRunAt), id)
	return err
}

// SetScheduleEnabled flips a schedule on/off.
func (s *Store) SetScheduleEnabled(ctx context.Context, id string, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE schedules SET enabled = ? WHERE id = ?`, enabled, id)
	return err
}

// DeleteSchedule removes a schedule.
func (s *Store) DeleteSchedule(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, id)
	return err
}

const scheduleColumns = `id, pipeline, module_fn, args_json, cron, enabled, last_run_at, next_run_at`

func scanSchedule(r rowScanner) (*Schedule, error) {
	var sch Schedule
	var enabled int
	if err := r.Scan(&sch.ID, &sch.Pipeline, &sch.ModuleFn, &sch.ArgsJSON, &sch.Cron, &enabled, &sch.LastRunAt, &sch.NextRunAt); err != nil {
		return nil, err
	}
	sch.Enabled = enabled == 1
	return &sch, nil
}

func nowRFCFrom(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
