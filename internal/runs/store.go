// Package runs is the run registry: the SQLite-backed source of truth for
// every pipeline run (the traceability spine).
package runs

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// Run statuses.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// Run is a single pipeline execution record.
type Run struct {
	ID            string  `json:"id"`
	Pipeline      string  `json:"pipeline"`
	ModuleFn      string  `json:"module_fn"`
	ArgsJSON      string  `json:"args"`
	TriggerKind   string  `json:"trigger_kind"`
	TriggerSource string  `json:"trigger_source"`
	DedupKey      *string `json:"dedup_key,omitempty"`
	Status        string  `json:"status"`
	GitSHA        string  `json:"git_sha,omitempty"`
	TraceID       string  `json:"trace_id,omitempty"`
	Error         string  `json:"error,omitempty"`
	QueuedAt      string  `json:"queued_at"`
	StartedAt     *string `json:"started_at,omitempty"`
	FinishedAt    *string `json:"finished_at,omitempty"`
}

// Event is an append-only registry entry (audit trail for a run).
type Event struct {
	ID      int64  `json:"id"`
	RunID   string `json:"run_id"`
	Kind    string `json:"kind"`
	Payload string `json:"payload"`
	TS      string `json:"ts"`
}

// Store wraps the SQLite run registry.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS runs (
	id             TEXT PRIMARY KEY,
	pipeline       TEXT NOT NULL,
	module_fn      TEXT NOT NULL DEFAULT '',
	args_json      TEXT NOT NULL DEFAULT '{}',
	trigger_kind   TEXT NOT NULL DEFAULT 'manual',
	trigger_source TEXT NOT NULL DEFAULT '',
	dedup_key      TEXT UNIQUE,
	status         TEXT NOT NULL DEFAULT 'queued',
	git_sha        TEXT NOT NULL DEFAULT '',
	trace_id       TEXT NOT NULL DEFAULT '',
	error          TEXT NOT NULL DEFAULT '',
	queued_at      TEXT NOT NULL,
	started_at     TEXT,
	finished_at    TEXT
);
CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status);
CREATE INDEX IF NOT EXISTS idx_runs_queued ON runs(queued_at DESC);

CREATE TABLE IF NOT EXISTS events (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id  TEXT NOT NULL,
	kind    TEXT NOT NULL,
	payload TEXT NOT NULL DEFAULT '{}',
	ts      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_run ON events(run_id);
` + scheduleSchema

// Open opens (creating if needed) the registry at path and applies the schema.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// Single connection: SQLite has one writer; this avoids SQLITE_BUSY entirely.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Enqueue records a new queued run. If dedupKey is non-empty and a run with
// the same key already exists, that run is returned instead (at-least-once
// triggers, deduplicated).
func (s *Store) Enqueue(ctx context.Context, pipeline, moduleFn, argsJSON, triggerKind, triggerSource, dedupKey string) (*Run, error) {
	if argsJSON == "" {
		argsJSON = "{}"
	}
	if dedupKey != "" {
		if existing, err := s.byDedupKey(ctx, dedupKey); err != nil || existing != nil {
			return existing, err
		}
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	var dedup any
	if dedupKey != "" {
		dedup = dedupKey
	}
	now := nowRFC()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO runs (id, pipeline, module_fn, args_json, trigger_kind, trigger_source, dedup_key, status, queued_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'queued', ?)`,
		id, pipeline, moduleFn, argsJSON, triggerKind, triggerSource, dedup, now)
	if err != nil {
		return nil, err
	}
	s.appendEvent(ctx, id, "queued", `{"trigger":"`+triggerKind+`"}`)
	return s.Get(ctx, id)
}

// ClaimNext atomically moves the oldest queued run to running and returns it.
// Returns (nil, nil) when the queue is empty.
func (s *Store) ClaimNext(ctx context.Context) (*Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE status = 'queued' ORDER BY queued_at ASC LIMIT 1`)
	run, err := scanRun(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	started := nowRFC()
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status = 'running', started_at = ? WHERE id = ? AND status = 'queued'`, started, run.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	run.Status = StatusRunning
	run.StartedAt = &started
	return run, nil
}

// Finish records the terminal state of a run.
func (s *Store) Finish(ctx context.Context, id, status, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET status = ?, finished_at = ?, error = ? WHERE id = ?`, status, nowRFC(), errMsg, id)
	if err == nil {
		s.appendEvent(ctx, id, status, `{}`)
	}
	return err
}

// SetGitSHA records the commit a run belongs to (traceability spine).
func (s *Store) SetGitSHA(ctx context.Context, id, sha string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET git_sha = ? WHERE id = ?`, sha, id)
	return err
}

// Get returns a run by ID.
func (s *Store) Get(ctx context.Context, id string) (*Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, id)
	run, err := scanRun(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return run, err
}

// List returns the most recent runs.
func (s *Store) List(ctx context.Context, limit int) ([]Run, error) {
	return s.ListFiltered(ctx, Filter{Limit: limit})
}

// Filter narrows ListFiltered. Zero-value fields match everything.
type Filter struct {
	Limit        int
	Pipeline     string // exact match
	TriggerKind  string // exact match
	SourcePrefix string // prefix match on trigger_source (e.g. "backstage:component:default/petal")
}

// ListFiltered returns the most recent runs matching f — how a portal shows
// the runs belonging to one catalog entity.
func (s *Store) ListFiltered(ctx context.Context, f Filter) ([]Run, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	q := `SELECT ` + runColumns + ` FROM runs WHERE 1=1`
	var args []any
	if f.Pipeline != "" {
		q += ` AND pipeline = ?`
		args = append(args, f.Pipeline)
	}
	if f.TriggerKind != "" {
		q += ` AND trigger_kind = ?`
		args = append(args, f.TriggerKind)
	}
	if f.SourcePrefix != "" {
		q += ` AND substr(trigger_source, 1, ?) = ?`
		// substr counts characters, not bytes
		args = append(args, utf8.RuneCountInString(f.SourcePrefix), f.SourcePrefix)
	}
	q += ` ORDER BY queued_at DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *run)
	}
	return out, rows.Err()
}

// ListEvents returns the audit events for a run.
func (s *Store) ListEvents(ctx context.Context, runID string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, kind, payload, ts FROM events WHERE run_id = ? ORDER BY id ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.RunID, &e.Kind, &e.Payload, &e.TS); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AppendEvent adds an audit event for a run.
func (s *Store) AppendEvent(ctx context.Context, runID, kind, payload string) error {
	s.appendEvent(ctx, runID, kind, payload)
	return nil
}

func (s *Store) appendEvent(ctx context.Context, runID, kind, payload string) {
	if payload == "" {
		payload = "{}"
	}
	_, _ = s.db.ExecContext(ctx, `INSERT INTO events (run_id, kind, payload, ts) VALUES (?, ?, ?, ?)`, runID, kind, payload, nowRFC())
}

func (s *Store) byDedupKey(ctx context.Context, key string) (*Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE dedup_key = ? ORDER BY queued_at DESC LIMIT 1`, key)
	run, err := scanRun(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return run, err
}

const runColumns = `id, pipeline, module_fn, args_json, trigger_kind, trigger_source, dedup_key, status, git_sha, trace_id, error, queued_at, started_at, finished_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanRun(r rowScanner) (*Run, error) {
	var run Run
	err := r.Scan(&run.ID, &run.Pipeline, &run.ModuleFn, &run.ArgsJSON, &run.TriggerKind, &run.TriggerSource,
		&run.DedupKey, &run.Status, &run.GitSHA, &run.TraceID, &run.Error,
		&run.QueuedAt, &run.StartedAt, &run.FinishedAt)
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "run_" + hex.EncodeToString(b), nil
}

func nowRFC() string { return time.Now().UTC().Format(time.RFC3339Nano) }
