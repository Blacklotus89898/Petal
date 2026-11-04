package retention

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Blacklotus89898/Petal/internal/runs"
)

func TestSweepPrunesRunsAndLogsIncludingOrphans(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := runs.Open(ctx, filepath.Join(dir, "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	logs := filepath.Join(dir, "logs")

	// Three finished runs of one pipeline; keep 1 → two get pruned.
	var ids []string
	for i := 0; i < 3; i++ {
		r, _ := store.Add(ctx, runs.NewRun{Pipeline: "p", TriggerKind: "api"})
		if c, _ := store.ClaimNext(ctx); c == nil {
			t.Fatal("claim")
		}
		_ = store.Finish(ctx, r.ID, runs.StatusSucceeded, "")
		_ = os.MkdirAll(filepath.Join(logs, r.ID), 0o750)
		_ = os.WriteFile(filepath.Join(logs, r.ID, "step.log"), []byte("x"), 0o600)
		ids = append(ids, r.ID)
		time.Sleep(5 * time.Millisecond) // distinct queued_at
	}
	orphan := filepath.Join(logs, "run_deadbeef")
	notARun := filepath.Join(logs, "keep-me")
	_ = os.MkdirAll(orphan, 0o750)
	_ = os.MkdirAll(notARun, 0o750)
	past := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(orphan, past, past)
	_ = os.Chtimes(notARun, past, past)

	j := &Janitor{Store: store, LogsDir: logs, MaxAge: 24 * time.Hour, Keep: 1,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return time.Now().Add(72 * time.Hour) }} // everything is "old"
	j.Sweep(ctx)

	if j.Pruned() != 2 || j.LastSweep().IsZero() {
		t.Fatalf("pruned=%d lastSweep=%v", j.Pruned(), j.LastSweep())
	}
	for i, id := range ids {
		_, err := os.Stat(filepath.Join(logs, id))
		kept := i == 2 // newest
		if kept != (err == nil) {
			t.Errorf("run %d (%s): logs kept=%v, want %v", i, id, err == nil, kept)
		}
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("orphan run log dir should be removed")
	}
	if _, err := os.Stat(notARun); err != nil {
		t.Error("a directory not shaped like a run ID must never be removed")
	}
}

func TestDisabledWhenMaxAgeZero(t *testing.T) {
	j := &Janitor{MaxAge: 0}
	j.Sweep(context.Background()) // no store needed: returns before touching it
	if !j.LastSweep().IsZero() {
		t.Error("disabled janitor must not sweep")
	}
}
