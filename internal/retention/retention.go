// Package retention is Petal's janitor (ADR 0009): it prunes old finished runs
// from the registry, deletes their step logs, removes orphaned log
// directories, and compacts the database — at startup and then periodically,
// so neither the SQLite file nor <data>/logs grows without bound.
package retention

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/Blacklotus89898/Petal/internal/runs"
)

// Janitor applies the retention policy.
type Janitor struct {
	Store   *runs.Store
	LogsDir string        // <data>/logs; "" = registry only
	MaxAge  time.Duration // finished runs older than this are pruned...
	Keep    int           // ...except the newest Keep per (pipeline, repo)
	Every   time.Duration // how often Run sweeps
	Log     *slog.Logger
	Now     func() time.Time // for tests

	pruned    atomic.Int64 // runs deleted since start (for /metrics)
	lastSweep atomic.Int64 // unix seconds of the last completed sweep
}

// Pruned is the number of runs deleted since the process started.
func (j *Janitor) Pruned() int64 { return j.pruned.Load() }

// LastSweep is when the last sweep completed (zero time if none yet).
func (j *Janitor) LastSweep() time.Time {
	if s := j.lastSweep.Load(); s > 0 {
		return time.Unix(s, 0)
	}
	return time.Time{}
}

func (j *Janitor) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

// Run sweeps immediately, then every j.Every until ctx is done.
func (j *Janitor) Run(ctx context.Context) {
	j.Sweep(ctx)
	t := time.NewTicker(j.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.Sweep(ctx)
		}
	}
}

// Sweep runs the policy once. Errors are logged, never fatal: retention is
// housekeeping, and the next sweep retries.
func (j *Janitor) Sweep(ctx context.Context) {
	if j.MaxAge <= 0 {
		return
	}
	cutoff := j.now().Add(-j.MaxAge)
	ids, err := j.Store.PruneFinished(ctx, cutoff, j.Keep)
	if err != nil {
		j.Log.Error("retention: prune runs", "err", err)
		return
	}
	for _, id := range ids {
		j.removeLogs(id)
	}
	orphans := j.removeOrphanLogs(ctx, cutoff)
	if len(ids) > 0 {
		j.pruned.Add(int64(len(ids)))
		if err := j.Store.Compact(ctx); err != nil {
			j.Log.Warn("retention: compact", "err", err)
		}
	}
	j.lastSweep.Store(j.now().Unix())
	j.Log.Info("retention sweep", "pruned_runs", len(ids), "orphan_log_dirs", orphans,
		"cutoff", cutoff.UTC().Format(time.RFC3339), "keep_per_pipeline", j.Keep)
}

func (j *Janitor) removeLogs(runID string) {
	if j.LogsDir == "" || !validRunDir(runID) {
		return
	}
	if err := os.RemoveAll(filepath.Join(j.LogsDir, runID)); err != nil {
		j.Log.Warn("retention: remove logs", "run", runID, "err", err)
	}
}

// removeOrphanLogs deletes log directories whose run is no longer in the
// registry (e.g. pruned before retention also cleaned logs, or a lost DB),
// once they are older than the cutoff — a live run's directory always has a
// registry row, so it is never touched.
func (j *Janitor) removeOrphanLogs(ctx context.Context, cutoff time.Time) int {
	if j.LogsDir == "" {
		return 0
	}
	entries, err := os.ReadDir(j.LogsDir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() || !validRunDir(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if ok, err := j.Store.Exists(ctx, e.Name()); err != nil || ok {
			continue
		}
		if os.RemoveAll(filepath.Join(j.LogsDir, e.Name())) == nil {
			n++
		}
	}
	return n
}

// validRunDir guards RemoveAll: only names shaped like run IDs (run_<hex>).
func validRunDir(name string) bool {
	if len(name) < 5 || len(name) > 64 || name[:4] != "run_" {
		return false
	}
	for _, r := range name[4:] {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}
