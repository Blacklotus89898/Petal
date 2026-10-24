package runs

import (
	"context"
	"crypto/rand"
	"database/sql"
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
	Repo      string  `json:"repo,omitempty"` // allowlist alias; "" = self repo (ADR 0004)
	Ref       string  `json:"ref,omitempty"`
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

// CreateSchedule inserts a self-repo schedule with its first computed due time.
func (s *Store) CreateSchedule(ctx context.Context, pipeline, moduleFn, argsJSON, cronExpr, nextRunAt string) (*Schedule, error) {
	return s.AddSchedule(ctx, NewSchedule{Pipeline: pipeline, ModuleFn: moduleFn, ArgsJSON: argsJSON, Cron: cronExpr}, nextRunAt)
}

// NewSchedule describes a schedule to create. Repo/Ref are empty for the self repo.
type NewSchedule struct {
	Pipeline, ModuleFn, ArgsJSON, Cron string
	Repo, Ref                          string
}

// AddSchedule inserts a schedule (CreateSchedule's general form, ADR 0004).
func (s *Store) AddSchedule(ctx context.Context, n NewSchedule, nextRunAt string) (*Schedule, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	id := "sched_" + hex.EncodeToString(b)
	if n.ArgsJSON == "" {
		n.ArgsJSON = "{}"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO schedules (id, pipeline, module_fn, args_json, cron, repo, ref, enabled, next_run_at) VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?)`,
		id, n.Pipeline, n.ModuleFn, n.ArgsJSON, n.Cron, n.Repo, n.Ref, nextRunAt)
	if err != nil {
		return nil, err
	}
	return s.GetSchedule(ctx, id)
}

// DeclaredPrefix marks schedules owned by the config file, not the API.
const DeclaredPrefix = "decl-"

// Declared is a schedule from Petal's config file. NextRunAt is the next fire
// time computed by the caller (the store doesn't parse cron).
type Declared struct {
	ID string // without DeclaredPrefix
	NewSchedule
	NextRunAt string
}

// SyncDeclaredSchedules makes the decl-* schedules match the config file:
// new ones are inserted, changed ones updated (next_run_at only recomputed
// when the cron changes, so a restart doesn't re-fire or skip), and decl-*
// rows no longer declared are deleted. API-created schedules are untouched.
// Declared schedules are always (re-)enabled: git is the source of truth.
func (s *Store) SyncDeclaredSchedules(ctx context.Context, decl []Declared) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	keep := map[string]bool{}
	for _, d := range decl {
		id := DeclaredPrefix + d.ID
		keep[id] = true
		args := d.ArgsJSON
		if args == "" {
			args = "{}"
		}
		var cur string
		err := tx.QueryRowContext(ctx, `SELECT cron FROM schedules WHERE id = ?`, id).Scan(&cur)
		switch {
		case err == sql.ErrNoRows:
			_, err = tx.ExecContext(ctx, `INSERT INTO schedules (id, pipeline, module_fn, args_json, cron, repo, ref, enabled, next_run_at) VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?)`,
				id, d.Pipeline, d.ModuleFn, args, d.Cron, d.Repo, d.Ref, d.NextRunAt)
		case err != nil:
			return err
		case cur != d.Cron:
			_, err = tx.ExecContext(ctx, `UPDATE schedules SET pipeline = ?, module_fn = ?, args_json = ?, cron = ?, repo = ?, ref = ?, enabled = 1, next_run_at = ? WHERE id = ?`,
				d.Pipeline, d.ModuleFn, args, d.Cron, d.Repo, d.Ref, d.NextRunAt, id)
		default:
			_, err = tx.ExecContext(ctx, `UPDATE schedules SET pipeline = ?, module_fn = ?, args_json = ?, repo = ?, ref = ?, enabled = 1 WHERE id = ?`,
				d.Pipeline, d.ModuleFn, args, d.Repo, d.Ref, id)
		}
		if err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM schedules WHERE substr(id, 1, ?) = ?`, len(DeclaredPrefix), DeclaredPrefix)
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if !keep[id] {
			stale = append(stale, id)
		}
	}
	rows.Close()
	for _, id := range stale {
		if _, err := tx.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
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

const scheduleColumns = `id, pipeline, module_fn, args_json, cron, repo, ref, enabled, last_run_at, next_run_at`

func scanSchedule(r rowScanner) (*Schedule, error) {
	var sch Schedule
	var enabled int
	if err := r.Scan(&sch.ID, &sch.Pipeline, &sch.ModuleFn, &sch.ArgsJSON, &sch.Cron, &sch.Repo, &sch.Ref, &enabled, &sch.LastRunAt, &sch.NextRunAt); err != nil {
		return nil, err
	}
	sch.Enabled = enabled == 1
	return &sch, nil
}

func nowRFCFrom(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
