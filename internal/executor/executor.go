// Package executor runs pipelines. Phase A ships Stub; the Dagger executor
// (Phase A+) is a drop-in replacement behind this interface.
package executor

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Blacklotus89898/Petal/internal/runs"
)

// Result is the terminal outcome of executing one run.
type Result struct {
	Status string // runs.StatusSucceeded or runs.StatusFailed
	Error  string
}

// Executor executes a single run to completion. Implementations must respect
// ctx cancellation (used for per-run timeouts) and must not panic past the
// scheduler's recover().
type Executor interface {
	Name() string
	Execute(ctx context.Context, run runs.Run) Result
}

// Stub is the Phase A placeholder executor. It simulates a short pipeline run
// and supports {"simulate":"failure"} in run args to exercise failure paths.
type Stub struct{}

// Name implements Executor.
func (Stub) Name() string { return "stub" }

// Execute implements Executor.
func (Stub) Execute(ctx context.Context, run runs.Run) Result {
	var args map[string]any
	_ = json.Unmarshal([]byte(run.ArgsJSON), &args)
	if args["simulate"] == "failure" {
		time.Sleep(300 * time.Millisecond) // simulate a little work before failing
		return Result{Status: runs.StatusFailed, Error: "simulated failure"}
	}
	select {
	case <-time.After(1500 * time.Millisecond): // simulate pipeline work
		return Result{Status: runs.StatusSucceeded}
	case <-ctx.Done():
		return Result{Status: runs.StatusFailed, Error: "canceled: " + ctx.Err().Error()}
	}
}
