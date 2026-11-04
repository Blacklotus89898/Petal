# ADR 0009: Retention — bounded registry and step logs

**Status:** Accepted (2026-11-04)
**Builds on:** [ADR 0007](0007-step-graph-and-step-metrics.md) (metrics from the registry), [ADR 0008](0008-run-notifications.md)

## Context

Every run adds a registry row, its audit events and a log directory under
`<data>/logs/<run>`, forever. Disk is the homelab's first-class constraint
(the worker's root fs is ~78% used), and the step metrics scan the events
table on every scrape, so an unbounded registry costs twice.

## Decision

1. **A janitor sweeps at startup and every `-retention-every` (6h)**: finished
   runs (succeeded / failed / canceled) older than `-retention` (30 days) are
   deleted with their events and their log directory. `-retention 0` keeps
   everything.
2. **The newest `-retention-keep` (20) finished runs of each pipeline+repo are
   always kept**, whatever their age. A monthly pipeline (Tulip's backtest)
   never loses its history, and the "last succeeded" timestamps the dead-man
   alerts read (`PetalNightlyValidateMissing`, `PetalMonthlyBacktestMissing`)
   never disappear.
3. **Queued and running runs are never touched.**
4. **Orphaned log directories** (no registry row, older than the cutoff) are
   removed; only names shaped like run IDs are ever deleted.
5. After a sweep that deleted something, **`VACUUM` + `wal_checkpoint(TRUNCATE)`**
   return the space to the filesystem (the registry is a few MB; it's quick).
6. Observability: `petal_db_bytes`, `petal_db_rows{table}`,
   `petal_retention_pruned_runs_total`, `petal_retention_last_sweep_timestamp_seconds`.

## Consequences

- `petal_runs_total` / `petal_steps_total` are computed from the registry, so
  they *drop* when old runs are pruned. Use them for "what happened recently"
  (`increase(...[1d])` tolerates the drop as a counter reset), not as
  all-time totals.
- `GET /api/runs` is capped at 500 rows; the console can load 50 / 200 / 500.
- A run older than the window is gone, logs included — fine for a CI
  orchestrator; anything worth keeping should be a pipeline output that goes
  somewhere durable (e.g. Tulip's dated backtest reports in git).
