# ADR 0007: Run step graph and per-step metrics

**Status:** Accepted (2026-11-03)
**Builds on:** [ADR 0005](0005-modular-dag-steps.md) (needs DAG), [ADR 0004](0004-multi-repo-runs.md) (checkout step)

## Context

Petal orchestrates custom pipelines built from modular steps that can form a
DAG, but a run could only be watched as a flat event timeline plus logs. The
console could not show *which* steps were waiting on which, what was running in
parallel, or what was skipped by fail-fast. And `/metrics` only knew runs, so a
pipeline that fails in one step looked like every other failing pipeline.

A run's graph can't be rebuilt from today's pipeline file: the spec loads from
the run's scratch workspace (deleted after the run), and the file can change
afterwards — an old run would be drawn with someone else's edges.

## Decision

1. **The executor records a `plan` event** after loading the spec and before
   the first step: `{dag, max_parallel, steps: [{name, needs, image, uses, docker}]}`.
   Edges are explicit in both modes — file order becomes a chain, and a
   multi-repo run's `petal-checkout` is the root the pipeline's roots hang off.
   Names and images only: no env, scripts or secret values (ADR 0006).
2. **The server derives the graph** (`runs.BuildGraph`) and returns it as
   `graph` on `GET /api/runs/{id}`: each step with `needs` and a status —
   `pending · running · succeeded · failed · canceled · skipped` — plus
   timing, exit code and error. Once a run is finished nothing stays
   running/pending (stopped steps read `canceled`, unstarted ones `skipped`).
   Server-side so the UI and the Backstage card draw the same thing. Runs
   without a plan (older ones, or a setup failure before the spec loaded) get
   `planned: false` and their started steps in order, without edges.
3. **The console draws it** as a dependency-free SVG (longest-path layering),
   live: the run drawer polls every 2.5s while the run is queued/running and
   refetches the logs of running steps; clicking a node jumps to its log. The
   run list gains a per-pipeline health strip (last status, last 10 outcomes,
   success rate).
4. **Per-step metrics** at scrape time from the audit events:
   `petal_steps_total{pipeline,repo,step,status}` and
   `petal_step_duration_seconds{…}` — like the run metrics, exact and
   restart-proof because they come from SQLite, not in-memory counters.

## Consequences

- No new dependency (no graph-layout library); fine at today's ≤10-step
  pipelines, revisit if pipelines grow wide enough to need edge routing.
- Polling, not push: 2.5s latency and a request per open drawer. SSE can
  replace it without changing the graph contract.
- The step metric query scans step events with `json_extract` on every scrape;
  at homelab volume (hundreds of runs) that is milliseconds. If the events
  table grows large, materialise step rows at write time instead.
