// Package scheduler owns the worker pool: it claims queued runs from the
// registry and drives them through an Executor with timeouts and panic
// isolation.
package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Blacklotus89898/Petal/internal/executor"
	"github.com/Blacklotus89898/Petal/internal/runs"
)

// Pool is a fixed-size worker pool over the run queue.
type Pool struct {
	store   *runs.Store
	exec    executor.Executor
	workers int
	poll    time.Duration
	log     *slog.Logger
}

// NewPool creates a pool with n workers using the given executor.
func NewPool(store *runs.Store, exec executor.Executor, n int, log *slog.Logger) *Pool {
	if n < 1 {
		n = 1
	}
	return &Pool{store: store, exec: exec, workers: n, poll: 2 * time.Second, log: log}
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
		if err := p.store.Finish(ctx, run.ID, res.Status, res.Error); err != nil {
			p.log.Error("finish run", "run", run.ID, "err", err)
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
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
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
