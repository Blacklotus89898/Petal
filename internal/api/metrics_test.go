package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Blacklotus89898/Petal/internal/runs"
)

func TestMetrics(t *testing.T) {
	mux, store := newRepoMux(t)
	ctx := context.Background()
	run, _ := store.Add(ctx, runs.NewRun{Pipeline: "validate", TriggerKind: "cron", Repo: "homelab"})
	claimed, _ := store.ClaimNext(ctx)
	if claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim: %+v", claimed)
	}
	_ = store.Finish(ctx, run.ID, runs.StatusSucceeded, "")
	_, _ = store.Enqueue(ctx, "docker-image", "", "{}", "api", "", "") // stays queued, self repo

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("GET /metrics = %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, want := range []string{
		`petal_runs_total{pipeline="validate",repo="homelab",status="succeeded"} 1`,
		`petal_runs_total{pipeline="docker-image",repo="self",status="queued"} 1`,
		`petal_run_duration_seconds_count{pipeline="validate",repo="homelab",status="succeeded"} 1`,
		`petal_run_last_finished_timestamp_seconds{pipeline="validate",repo="homelab",status="succeeded"}`,
		"petal_runs_queued 1",
		"petal_runs_running 0",
		"petal_repos 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}
