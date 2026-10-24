package executor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Blacklotus89898/Petal/internal/repos"
	"github.com/Blacklotus89898/Petal/internal/runs"
)

// Multi-repo runs (ADR 0004), docker-free: the source is resolved before any
// pipeline spec is read, and checkout failures surface as clear run errors.
func TestExecuteRepoRunWithoutDocker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, err := runs.Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	allow := repos.Allowlist{"homelab": {Alias: "homelab", URL: "https://example.invalid/h.git", DefaultRef: "main"}}
	s := NewSteps(store, t.TempDir(), t.TempDir(), "tcp://127.0.0.1:1", allow, nil)

	// Not allowlisted (e.g. the allowlist shrank after enqueue): no container runs.
	run, err := store.Add(ctx, runs.NewRun{Pipeline: "validate", TriggerKind: "api", Repo: "gone"})
	if err != nil {
		t.Fatal(err)
	}
	if res := s.Execute(ctx, *run); res.Status != runs.StatusFailed || !strings.Contains(res.Error, "not in the allowlist") {
		t.Errorf("unlisted repo: (%q, %q)", res.Status, res.Error)
	}

	// Allowlisted, default ref: the checkout step is attempted and its failure
	// (no docker here) names the repo@ref; the step events are recorded.
	run, err = store.Add(ctx, runs.NewRun{Pipeline: "validate", TriggerKind: "api", Repo: "homelab"})
	if err != nil {
		t.Fatal(err)
	}
	res := s.Execute(ctx, *run)
	if res.Status != runs.StatusFailed || !strings.Contains(res.Error, "checkout homelab@main failed") {
		t.Errorf("checkout failure: (%q, %q)", res.Status, res.Error)
	}
	events, err := store.ListEvents(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var started, finished bool
	for _, e := range events {
		if strings.Contains(e.Payload, `"step":"petal-checkout"`) {
			started = started || e.Kind == "step.started"
			finished = finished || e.Kind == "step.finished"
		}
	}
	if !started || !finished {
		t.Errorf("expected petal-checkout step.started + step.finished events, got %+v", events)
	}
}
