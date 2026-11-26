# Petal — Architecture Decision Records

The decision log, newest last. Each ADR is the why behind a shape in the code; a change to a
decision is a new ADR, never an edit to an accepted one.

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-architecture.md) | Petal is a control plane over a separate execution layer | Accepted (execution layer superseded by 0002) |
| [0002](0002-step-runner.md) | Replace the Dagger executor with Petal's own step-runner | Accepted 2026-09-07 |
| [0003](0003-backstage-portal-petal-orchestrator.md) | Backstage is the portal, Petal is the orchestrator | Accepted 2026-10-15 |
| [0004](0004-multi-repo-runs.md) | Multi-repo runs: `repo@ref` per run, allowlisted, cloned in a throwaway container | Accepted 2026-10-24 |
| [0005](0005-modular-dag-steps.md) | Modular steps, `needs` DAG with bounded concurrency, timeouts and cancellation | Accepted 2026-10-24 |
| [0006](0006-secrets.md) | Secrets granted per repo and ref, mounted as files, masked in logs | Accepted 2026-10-24 |
| [0007](0007-step-graph-and-step-metrics.md) | Run step graph and per-step metrics | Accepted 2026-11-03 |
| [0008](0008-run-notifications.md) | Run notifications to ntfy | Accepted 2026-11-03 |
| [0009](0009-retention.md) | Retention: bounded registry and step logs | Accepted 2026-11-04 |
