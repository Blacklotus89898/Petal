package runs

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestAddRecordsRepoAndRef(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	run, err := s.Add(ctx, NewRun{Pipeline: "validate", TriggerKind: "api", Repo: "homelab", Ref: "main"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Repo != "homelab" || got.Ref != "main" || got.ArgsJSON != "{}" {
		t.Errorf("got %+v", got)
	}

	self, err := s.Enqueue(ctx, "docker-image", "", "{}", "api", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if self.Repo != "" || self.Ref != "" {
		t.Errorf("Enqueue is the self repo: got repo=%q ref=%q", self.Repo, self.Ref)
	}

	only, err := s.ListFiltered(ctx, Filter{Repo: "homelab"})
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 1 || only[0].ID != run.ID {
		t.Errorf("repo filter returned %d runs: %+v", len(only), only)
	}
}

// A registry created before ADR 0004 has no repo/ref columns; Open must add
// them without touching existing rows.
func TestOpenMigratesPreRepoSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")

	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.ExecContext(ctx, `
		CREATE TABLE runs (
			id TEXT PRIMARY KEY, pipeline TEXT NOT NULL, module_fn TEXT NOT NULL DEFAULT '',
			args_json TEXT NOT NULL DEFAULT '{}', trigger_kind TEXT NOT NULL DEFAULT 'manual',
			trigger_source TEXT NOT NULL DEFAULT '', dedup_key TEXT UNIQUE,
			status TEXT NOT NULL DEFAULT 'queued', git_sha TEXT NOT NULL DEFAULT '',
			trace_id TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
			queued_at TEXT NOT NULL, started_at TEXT, finished_at TEXT);
		INSERT INTO runs (id, pipeline, queued_at) VALUES ('run_old', 'docker-image', '2026-09-07T00:00:00Z');`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	for i := 0; i < 2; i++ { // second Open proves the migration is idempotent
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("open #%d: %v", i+1, err)
		}
		run, err := s.Get(ctx, "run_old")
		if err != nil || run == nil || run.Repo != "" || run.Pipeline != "docker-image" {
			t.Fatalf("open #%d: old row = (%+v, %v)", i+1, run, err)
		}
		s.Close()
	}
}

func TestFailOrphaned(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	run, _ := s.Enqueue(ctx, "docker-image", "", "{}", "api", "", "")
	if _, err := s.ClaimNext(ctx); err != nil { // now "running", as if mid-execution
		t.Fatal(err)
	}
	queued, _ := s.Enqueue(ctx, "docker-image", "", "{}", "api", "", "")

	n, err := s.FailOrphaned(ctx)
	if err != nil || n != 1 {
		t.Fatalf("FailOrphaned = (%d, %v), want (1, nil)", n, err)
	}
	got, _ := s.Get(ctx, run.ID)
	if got.Status != StatusFailed || got.FinishedAt == nil || got.Error == "" {
		t.Errorf("orphaned run = %+v", got)
	}
	if q, _ := s.Get(ctx, queued.ID); q.Status != StatusQueued {
		t.Errorf("queued runs must be left alone (they'll be claimed), got %q", q.Status)
	}
}

func TestCancelQueued(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	queued, _ := s.Enqueue(ctx, "p", "", "{}", "api", "", "")
	running, _ := s.Enqueue(ctx, "p", "", "{}", "api", "", "")
	// Claim the oldest (queued) one so it's "running"; cancel must not touch it.
	claimed, _ := s.ClaimNext(ctx)
	if claimed.ID != queued.ID {
		queued, running = running, queued
	}

	if ok, err := s.CancelQueued(ctx, claimed.ID); err != nil || ok {
		t.Errorf("a running run must not be canceled by CancelQueued: (%v, %v)", ok, err)
	}
	other := queued
	if other.ID == claimed.ID {
		other = running
	}
	if ok, err := s.CancelQueued(ctx, other.ID); err != nil || !ok {
		t.Fatalf("queued run: (%v, %v)", ok, err)
	}
	got, _ := s.Get(ctx, other.ID)
	if got.Status != StatusCanceled || got.FinishedAt == nil {
		t.Errorf("canceled run = %+v", got)
	}
	if ok, _ := s.CancelQueued(ctx, other.ID); ok {
		t.Error("canceling twice must be a no-op")
	}
}
