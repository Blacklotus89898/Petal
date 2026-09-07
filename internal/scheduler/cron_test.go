package scheduler

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Blacklotus89898/Petal/internal/executor"
	"github.com/Blacklotus89898/Petal/internal/runs"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNextRunValidAndInvalid(t *testing.T) {
	if _, err := NextRun("*/15 * * * *", time.Now()); err != nil {
		t.Fatalf("valid cron rejected: %v", err)
	}
	if _, err := NextRun("not a cron", time.Now()); err == nil {
		t.Fatal("invalid cron accepted")
	}
}

func TestFireDueEnqueuesOncePerMinute(t *testing.T) {
	store, err := runs.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	pool := NewPool(store, executor.Stub{}, 1, discardLogger())

	// Schedule already due (next_run_at in the past).
	if _, err := store.CreateSchedule(context.Background(), "docker-image", "build", "{}", "* * * * *", "2000-01-01T00:00:00Z"); err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	pool.fireDue(context.Background())
	pool.fireDue(context.Background()) // same minute: dedup must hold

	runsList, err := store.List(context.Background(), 10)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runsList) != 1 {
		t.Fatalf("expected exactly 1 run after two fire ticks in the same minute, got %d", len(runsList))
	}
	if runsList[0].TriggerKind != "cron" {
		t.Fatalf("expected cron trigger, got %s", runsList[0].TriggerKind)
	}

	// next_run_at must have advanced past now.
	sch, _ := store.ListSchedules(context.Background())
	if sch[0].LastRunAt == nil {
		t.Fatal("last_run_at not recorded")
	}
	next, err := time.Parse(time.RFC3339Nano, sch[0].NextRunAt)
	if err != nil {
		t.Fatalf("parse next_run_at: %v", err)
	}
	if !next.After(time.Now().Add(-time.Minute)) {
		t.Fatalf("next_run_at should be in the future, got %s", sch[0].NextRunAt)
	}
}

func TestFireDueDisablesBadCron(t *testing.T) {
	store, err := runs.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	// Insert a schedule with a bad expression directly (API validates).
	if _, err := store.CreateSchedule(context.Background(), "p", "", "{}", "garbage", "2000-01-01T00:00:00Z"); err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	pool := NewPool(store, executor.Stub{}, 1, discardLogger())
	pool.fireDue(context.Background())

	sch, _ := store.ListSchedules(context.Background())
	if sch[0].Enabled {
		t.Fatal("schedule with unparseable cron should be disabled, not hot-looped")
	}
}
