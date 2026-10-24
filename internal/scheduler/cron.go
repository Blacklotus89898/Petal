package scheduler

import (
	"context"
	"time"

	cronlib "github.com/robfig/cron/v3"

	"github.com/Blacklotus89898/Petal/internal/runs"
)

// cronEvery is how often the scheduler polls for due schedules. One-minute
// granularity is the cron standard; the extra few seconds absorb drift.
const cronEvery = 30 * time.Second

// RunCronLoop blocks until ctx is canceled, firing due schedules. It runs
// alongside (and independently of) the worker pool: firing a schedule only
// enqueues a run — the pool executes it.
func (p *Pool) RunCronLoop(ctx context.Context) {
	t := time.NewTicker(cronEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.fireDue(ctx)
		}
	}
}

func (p *Pool) fireDue(ctx context.Context) {
	now := time.Now()
	due, err := p.store.DueSchedules(ctx, now)
	if err != nil {
		p.log.Error("list due schedules", "err", err)
		return
	}
	for _, sch := range due {
		next, err := NextRun(sch.Cron, now)
		if err != nil {
			// Unparseable expression: disable instead of hot-looping.
			p.log.Error("bad cron expression, disabling schedule", "schedule", sch.ID, "cron", sch.Cron, "err", err)
			_ = p.store.SetScheduleEnabled(ctx, sch.ID, false)
			continue
		}
		// Minute-bucket dedup key: restarts and overlapping ticks fire the
		// schedule once per cron minute, never twice.
		dedup := "sched:" + sch.ID + ":" + now.Truncate(time.Minute).UTC().Format(time.RFC3339)
		run, err := p.store.Add(ctx, runs.NewRun{
			Pipeline: sch.Pipeline, ModuleFn: sch.ModuleFn, ArgsJSON: sch.ArgsJSON,
			TriggerKind: "cron", TriggerSource: "schedule:" + sch.ID, DedupKey: dedup,
			Repo: sch.Repo, Ref: sch.Ref,
		})
		if err != nil {
			p.log.Error("enqueue scheduled run", "schedule", sch.ID, "err", err)
			continue
		}
		if run == nil {
			// Dedup hit: this cron minute already fired. Still advance next_run_at.
		} else {
			p.log.Info("schedule fired", "schedule", sch.ID, "run", run.ID, "pipeline", sch.Pipeline)
		}
		if err := p.store.MarkScheduleRan(ctx, sch.ID, now, next); err != nil {
			p.log.Error("mark schedule ran", "schedule", sch.ID, "err", err)
		}
	}
}

// NextRun computes the next fire time after t for a 5-field cron expression.
func NextRun(expr string, after time.Time) (time.Time, error) {
	sched, err := cronlib.ParseStandard(expr)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(after), nil
}

// validateCron reports whether expr is a valid 5-field cron expression.
func validateCron(expr string) error {
	_, err := cronlib.ParseStandard(expr)
	return err
}
