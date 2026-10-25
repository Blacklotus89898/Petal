package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Blacklotus89898/Petal/internal/pipeline"
	"github.com/Blacklotus89898/Petal/internal/runs"
)

// Cancellation causes (ADR 0005). Contexts are canceled with one of these via
// context.WithCancelCause / WithTimeoutCause, so the outcome can say *why*:
// a cancel request ends a run "canceled"; a timeout ends it "failed" naming
// the limit; a sibling's failure cancels running steps but the run reports the
// sibling's error.
var (
	// ErrCancelRequested is the cause when a user cancels a run (API).
	ErrCancelRequested = errors.New("cancel requested")
	// errSiblingFailed cancels running steps after another step failed.
	errSiblingFailed = errors.New("another step failed")
)

// TimeoutError is the cause when a step's, the checkout's or the run's time
// budget runs out.
type TimeoutError struct {
	What  string // `step "lint"`, "checkout", "run"
	Limit time.Duration
}

func (e *TimeoutError) Error() string { return fmt.Sprintf("%s timed out after %s", e.What, e.Limit) }

// stepRunner executes one step container to completion. Swappable so the DAG
// scheduler is unit-testable without Docker (tests inject a fake).
type stepRunner func(ctx context.Context, runID string, step pipeline.Step, workDir, logDir string, baseEnv map[string]string) (exit int, err error)

// runOutcome maps a finished-or-canceled run context to the run's result.
// ok=false means the context wasn't canceled for a run-level reason.
func runOutcome(ctx context.Context) (Result, bool) {
	if ctx.Err() == nil {
		return Result{}, false
	}
	cause := context.Cause(ctx)
	var te *TimeoutError
	switch {
	case errors.Is(cause, ErrCancelRequested):
		return Result{Status: runs.StatusCanceled, Error: "canceled on request"}, true
	case errors.As(cause, &te):
		return Result{Status: runs.StatusFailed, Error: te.Error()}, true
	default:
		return Result{Status: runs.StatusFailed, Error: "stopped: " + cause.Error()}, true
	}
}

type stepDone struct {
	idx    int
	failed string // non-empty: the step failed (exit≠0, error, or its own timeout)
}

// runSteps executes the pipeline's steps: in file order one at a time, or —
// for a DAG pipeline — every step whose needs have succeeded, up to
// MaxParallel at once. Fail-fast: the first failure cancels running siblings
// and leaves the rest unstarted (recorded as step.skipped). Returns the first
// failure's message ("" = all steps succeeded, or the run itself was canceled
// — the caller checks runOutcome first).
func (s *Steps) runSteps(runCtx context.Context, runID string, spec *pipeline.Pipeline, workDir, logDir string, baseEnv map[string]string) string {
	steps := spec.Steps
	index := make(map[string]int, len(steps))
	for i, st := range steps {
		index[st.Name] = i
	}
	needs := make([][]int, len(steps))
	parallel := 1
	if spec.DAG {
		parallel = spec.MaxParallel
		for i, st := range steps {
			for _, n := range st.Needs {
				needs[i] = append(needs[i], index[n])
			}
		}
	} else {
		for i := 1; i < len(steps); i++ {
			needs[i] = []int{i - 1} // file order
		}
	}

	group, cancelGroup := context.WithCancelCause(runCtx)
	defer cancelGroup(nil)

	const (
		pending = iota
		running
		succeeded
		stopped // failed or canceled
	)
	state := make([]int, len(steps))
	results := make(chan stepDone)
	inflight := 0
	firstFailure := ""

	ready := func(i int) bool {
		for _, d := range needs[i] {
			if state[d] != succeeded {
				return false
			}
		}
		return true
	}

	for {
		if firstFailure == "" && group.Err() == nil {
			for i := range steps {
				if inflight >= parallel {
					break
				}
				if state[i] == pending && ready(i) {
					state[i] = running
					inflight++
					go func(i int) { results <- s.execStep(group, runID, i, steps[i], workDir, logDir, baseEnv) }(i)
				}
			}
		}
		if inflight == 0 {
			break
		}
		r := <-results
		inflight--
		if r.failed != "" {
			state[r.idx] = stopped
			if firstFailure == "" {
				firstFailure = r.failed
				cancelGroup(errSiblingFailed)
			}
		} else if group.Err() != nil {
			state[r.idx] = stopped // canceled mid-step
		} else {
			state[r.idx] = succeeded
		}
	}

	for i, st := range steps {
		if state[i] == pending {
			s.emit(runCtx, runID, "step.skipped", map[string]any{"step": st.Name})
		}
	}
	return firstFailure
}

// execStep runs one step under its own timeout and records its events. A
// step's own timeout is a failure; a cancellation from above (cancel request,
// run timeout, sibling failure) is recorded as canceled, not as a failure.
func (s *Steps) execStep(group context.Context, runID string, i int, st pipeline.Step, workDir, logDir string, baseEnv map[string]string) stepDone {
	limit := st.TimeoutDur
	if limit <= 0 {
		limit = pipeline.DefaultStepTimeout
	}
	stepCtx, cancel := context.WithTimeoutCause(group, limit, &TimeoutError{What: fmt.Sprintf("step %q", st.Name), Limit: limit})
	defer cancel()

	started := map[string]any{"step": st.Name, "index": i, "image": st.Image}
	if len(st.Secrets) > 0 {
		started["secrets"] = st.Secrets // names only (ADR 0006)
	}
	s.emit(group, runID, "step.started", started)
	exit, err := s.runner(stepCtx, runID, st, workDir, logDir, baseEnv)

	if stepCtx.Err() != nil {
		var te *TimeoutError
		if cause := context.Cause(stepCtx); errors.As(cause, &te) && group.Err() == nil {
			msg := te.Error()
			s.emit(group, runID, "step.finished", map[string]any{"step": st.Name, "exit": -1, "error": msg, "timed_out": true})
			return stepDone{idx: i, failed: msg}
		}
		s.emit(group, runID, "step.finished", map[string]any{"step": st.Name, "canceled": true, "cause": context.Cause(stepCtx).Error()})
		return stepDone{idx: i}
	}
	if err != nil || exit != 0 {
		msg := fmt.Sprintf("step %q failed (exit %d)", st.Name, exit)
		if err != nil {
			msg += ": " + err.Error()
		}
		s.emit(group, runID, "step.finished", map[string]any{"step": st.Name, "exit": exit, "error": msg})
		return stepDone{idx: i, failed: msg}
	}
	s.emit(group, runID, "step.finished", map[string]any{"step": st.Name, "exit": 0})
	return stepDone{idx: i}
}
