package runs

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneFinishedKeepsNewestPerPipelineAndActiveRuns(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	mk := func(pipeline, repo, status string, ageDays int) string {
		r, err := s.Add(ctx, NewRun{Pipeline: pipeline, Repo: repo, TriggerKind: "api"})
		if err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-time.Duration(ageDays) * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
		var fin any = at
		if status == StatusQueued || status == StatusRunning {
			fin = nil
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE runs SET status=?, queued_at=?, started_at=?, finished_at=? WHERE id=?`,
			status, at, at, fin, r.ID); err != nil {
			t.Fatal(err)
		}
		_ = s.AppendEvent(ctx, r.ID, "queued", `{}`)
		return r.ID
	}
	// validate: 5 old runs + 1 new one → keep newest 2 overall, prune the 4 oldest.
	var old []string
	for d := 60; d >= 56; d-- {
		old = append(old, mk("validate", "homelab", StatusSucceeded, d))
	}
	fresh := mk("validate", "homelab", StatusFailed, 1)
	// backtest: only 2 runs, both ancient → both kept by keep-per-pipeline.
	bt1 := mk("backtest", "tulip", StatusSucceeded, 90)
	bt2 := mk("backtest", "tulip", StatusSucceeded, 60)
	// an ancient *running* run is never touched.
	stuck := mk("validate", "homelab", StatusRunning, 100)

	ids, err := s.PruneFinished(ctx, time.Now().Add(-30*24*time.Hour), 2)
	if err != nil {
		t.Fatal(err)
	}
	// validate ranked by queued_at desc: fresh(1d), stuck(running, excluded from ranking), 56d, 57d, 58d, 59d, 60d
	// → rank 1 fresh, rank 2 = 56d kept; 57..60 pruned.
	want := map[string]bool{old[0]: true, old[1]: true, old[2]: true, old[3]: true}
	if len(ids) != 4 {
		t.Fatalf("pruned %d runs %v, want 4", len(ids), ids)
	}
	for _, id := range ids {
		if !want[id] {
			t.Errorf("pruned unexpected run %s", id)
		}
	}
	for _, id := range []string{old[4], fresh, bt1, bt2, stuck} {
		if ok, _ := s.Exists(ctx, id); !ok {
			t.Errorf("run %s should have been kept", id)
		}
	}
	var orphanEvents int
	_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE run_id NOT IN (SELECT id FROM runs)`).Scan(&orphanEvents)
	if orphanEvents != 0 {
		t.Errorf("%d events left behind for deleted runs", orphanEvents)
	}
	if err := s.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	if st, err := s.Size(ctx); err != nil || st.Runs != 5 || st.Bytes <= 0 {
		t.Errorf("size = %+v, %v", st, err)
	}
}
