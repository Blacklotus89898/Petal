# ADR 0008: Run notifications to ntfy

**Status:** Accepted (2026-11-03)
**Builds on:** [ADR 0004](0004-multi-repo-runs.md) (allowlist config), [ADR 0006](0006-secrets.md) (config owns privileges), [ADR 0007](0007-step-graph-and-step-metrics.md)

## Context

A failed run was only visible in the console, or 5+ minutes later as the
generic `PetalPipelineFailing` alert (Alertmanager → ntfy `homelab-alerts`,
no run link). Some pipelines exist *to tell someone something*: Tulip's
monthly backtest produces a summary the operator should read on their phone,
not dig out of a run drawer.

## Decision

1. **Petal pushes to one ntfy topic** (`-ntfy-url`, `-ntfy-topic`, default
   `petal`), with the run's console link (`-public-url` → `?run=<id>`) as the
   click action.
2. **Failed runs always notify** (priority high, the run's masked error).
   **Canceled runs never do** — someone did that on purpose.
3. **Successful runs notify only when Petal's config opts the pipeline in**:
   `notify_success: [pipeline, …]` per repo (and under `self:`). The message
   is the run's outputs (already secret-masked), as a markdown code block,
   truncated under ntfy's 4096-byte limit. The pipeline file can't opt itself
   in or pick a topic — same rule as secrets: a repo can't grant itself
   attention.
4. **Best-effort, never load-bearing.** Sent after the result is recorded,
   5s timeout, errors logged, no retries: ntfy being down never changes a run.

## Consequences

- `PetalPipelineFailing` stays: it is the aggregate, deduplicated view in
  `homelab-alerts` and survives Petal being unable to send at all.
- Runs marked failed by startup recovery (interrupted) notify like any failure
  — the message carries "interrupted" in its error.
- One topic for everything; per-pipeline topics can be added in config later
  without changing the contract.
