// Package scheduler owns the worker pool: it claims queued runs from the
// registry and drives them through an Executor with timeouts and panic
// isolation.
package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/Blacklotus89898/Petal/internal/executor"
	"github.com/Blacklotus89898/Petal/internal/runs"
)

// safetyCeiling bounds any run, whatever its pipeline declares, so a bug can
// never hold a worker forever. The pipeline's own timeout (default 60m) is
// enforced inside the executor, once the spec is known (ADR 0005).
const safetyCeiling = 2 * time.Hour

// Pool is a fixed-size worker pool over the run queue.
type Pool struct {
	store   *runs.Store
	exec    executor.Executor
	workers int
	poll    time.Duration
	log     *slog.Logger

	mu      sync.Mutex
	cancels map[string]context.CancelCauseFunc // running runs, by ID
}

// Cancel asks a running run to stop (cause: executor.ErrCancelRequested).
// Reports false if the run isn't executing in this process.
func (p *Pool) Cancel(runID string) bool {
	p.mu.Lock()
	cancel, ok := p.cancels[runID]
	p.mu.Unlock()
	if ok {
		cancel(executor.ErrCancelRequested)
	}
	return ok
}

// NewPool creates a pool with n workers using the given executor.
func NewPool(store *runs.Store, exec executor.Executor, n int, log *slog.Logger) *Pool {
	if n < 1 {
		n = 1
	}
	return &Pool{store: store, exec: exec, workers: n, poll: 2 * time.Second, log: log,
		cancels: map[string]context.CancelCauseFunc{}}
}

// Start blocks until ctx is canceled, running workers until the queue drains.
func (p *Pool) Start(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < p.workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			p.worker(ctx, id)
		}(i)
	}
	wg.Wait()
}

func (p *Pool) worker(ctx context.Context, id int) {
	for {
		if ctx.Err() != nil {
			return
		}
		run, err := p.store.ClaimNext(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.log.Error("claim next run", "worker", id, "err", err)
			if !sleep(ctx, p.poll) {
				return
			}
			continue
		}
		if run == nil {
			if !sleep(ctx, p.poll) {
				return
			}
			continue
		}
		p.log.Info("run started", "run", run.ID, "pipeline", run.Pipeline, "worker", id, "executor", p.exec.Name())
		res := p.execute(ctx, *run)
		// WithoutCancel: a shutdown (ctx canceled) must still record the result.
		if err := p.store.Finish(context.WithoutCancel(ctx), run.ID, res.Status, res.Error); err != nil {
			p.log.Error("finish run", "run", run.ID, "err", err)
		}
		if res.Output != "" {
			payload, _ := json.Marshal(map[string]string{"output": res.Output})
			_ = p.store.AppendEvent(ctx, run.ID, "output", string(payload))
		}
		p.log.Info("run finished", "run", run.ID, "status", res.Status)
	}
}

// execute runs one run with a timeout and panic isolation: a panicking
// executor fails that run instead of taking down the worker.
func (p *Pool) execute(ctx context.Context, run runs.Run) (res executor.Result) {
	defer func() {
		if r := recover(); r != nil {
			res = executor.Result{Status: runs.StatusFailed, Error: "executor panic"}
			p.log.Error("executor panic", "run", run.ID, "panic", r)
		}
	}()
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	runCtx, stop := context.WithTimeoutCause(runCtx, safetyCeiling,
		&executor.TimeoutError{What: "run (safety ceiling)", Limit: safetyCeiling})
	defer stop()

	p.mu.Lock()
	p.cancels[run.ID] = cancel
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.cancels, run.ID)
		p.mu.Unlock()
	}()
	return p.exec.Execute(runCtx, run)
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}
