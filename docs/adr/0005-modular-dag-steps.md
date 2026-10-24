# ADR 0005: Modular steps, `needs` DAG with bounded concurrency, timeouts and cancellation

**Status:** Accepted (2026-10-24, user request)
**Builds on:** [ADR 0002](0002-step-runner.md) (step-runner), [ADR 0004](0004-multi-repo-runs.md) (multi-repo runs)

## Context

A pipeline was a flat list of steps, executed in file order, fail-fast. That
leaves four gaps:

- **No reuse.** The same step (e.g. yamllint) is copy-pasted into every
  pipeline that needs it.
- **No concurrency.** Independent steps (lint vs NodePort check) wait for each
  other; order is position, so reordering the file reorders execution.
- **One hard-coded run timeout.** The worker pool wrapped every run in a fixed
  10 minutes, before the spec (and so any per-pipeline need) was known.
- **No cancellation.** A wrong or runaway run could only be stopped by
  restarting Petal.

## Decision

### 1. Modular steps (`uses`)
A step library lives next to the pipelines, in the same repo:
`.petal/steps/<name>.yaml`:

```yaml
name: yamllint
description: Lint YAML with the repo's config
inputs:
  config: { default: .yamllint }
  version: { default: "1.35.1" }
image: python:3.12-slim
run: |
  pip install --quiet "yamllint==$INPUT_VERSION"
  yamllint -c "$INPUT_CONFIG" .
timeout: 10m
```

A pipeline step uses it with `uses: yamllint` and optional `with:` inputs.
Rules (validated at load time, so a bad reference fails the pipeline, not a run):
- `uses` is a `ValidName` (no path traversal); it is mutually exclusive with
  `image`/`run`; library steps cannot themselves `uses` (no nesting).
- `with` keys must be declared `inputs`; `required` inputs must be given;
  defaults apply. Inputs reach the container as `INPUT_*` env vars (same
  transform as params' `PARAM_*`).
- The **pipeline step's own `name`** keys logs and events, so one library step
  can be used several times in a pipeline. The pipeline step's `env` is merged
  over the library's; its `timeout` overrides the library's.
- The library step's image must be pinned, like any step.

### 2. Order: file order, or an explicit `needs` DAG
- **No step declares `needs`** → steps run in file order, one at a time —
  exactly as before, so every existing pipeline stays valid.
- **Any step declares `needs`** → **every** step must (`needs: []` for roots);
  a mix is a validation error. Execution order is then the DAG alone, so steps
  can be listed — and reordered — freely in the file. Unknown names,
  self-references and cycles are rejected.

### 3. Bounded concurrency
In DAG mode, every step whose `needs` have all succeeded is started, up to
`max_parallel` at a time (pipeline field; **default 2, hard cap 4** — two
workers share one DinD daemon on a single worker node). Concurrent steps share
the `/src` workspace and the daemon's network: they must not write the same
paths or bind the same ports.

**Fail-fast:** the first failing step cancels its running siblings; steps that
never started are recorded as `step.skipped`. The run reports the first
failure's error.

### 4. Timeouts
- `timeout:` per step (Go duration, default **30m**) and per pipeline (default
  **60m**). The run timeout now starts inside the executor, after the spec is
  loaded; the pool keeps a **2h safety ceiling** only.
- Checkout (ADR 0004) has its own fixed **5m** budget — it runs before the
  pipeline (and its timeout) is known.
- Errors name the limit: `step "x" timed out after 10m`, `run timed out after 1h`.

### 5. Cancellation
- New run status **`canceled`**.
- `POST /api/runs/{id}/cancel` (bearer token): a **queued** run is canceled
  atomically (`UPDATE … WHERE status='queued'`); a **running** run has its
  context canceled with a "cancel requested" cause — running step containers
  are killed, unstarted steps skipped; a finished run → `409`.
- The console stays read-only (ADR 0003 — no token in the browser).

### 6. Cancellation must never lose the audit trail
Every registry write inside the executor (events, git SHA) uses
`context.WithoutCancel(ctx)`: once a run's context is canceled, a write on that
context would itself fail and the `step.finished {canceled}` event would vanish.
Cancellation **causes** (`context.WithCancelCause` / `WithTimeoutCause`)
distinguish cancel request → `canceled`, step/run timeout → `failed` with the
limit, sibling failure → that step `canceled` while the run reports the
sibling's error.

## Consequences

- **Positive:** reusable, versioned-with-the-repo steps; real parallelism for
  independent work; per-pipeline time budgets; a way to stop a run.
- **Cost:** the executor is a small DAG scheduler instead of a loop (unit-tested
  with a fake step runner — no Docker needed); concurrent steps can contend for
  the workspace if a pipeline author ignores the shared-`/src` rule.
- **Catalog:** `GET /api/pipelines?repo=` sparse-checks out `.petal/steps` too,
  since listing resolves `uses`.
- **Signal this was wrong:** pipelines start relying on cross-step state that
  only works sequentially, or DinD saturates at `max_parallel` → lower the cap
  or add per-step resource limits.
