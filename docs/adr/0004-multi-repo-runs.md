# ADR 0004: Multi-repo runs — `repo@ref` per run, allowlisted, cloned in a throwaway container

**Status:** Proposed (2026-10-24) — for review before implementation
**Builds on:** [ADR 0002](0002-step-runner.md) (step-runner), [ADR 0003](0003-backstage-portal-petal-orchestrator.md) (Petal is the orchestrator)

## Context

Petal runs pipelines from exactly one source tree: the git-sync sidecar keeps
*this* repo checked out at `/git/repo`, the executor snapshots it into each run's
workspace, and pipeline specs load from its `.petal/pipelines/`. So Petal can only
build Petal. Consequences today:

- The Backstage template is restricted to `component:default/petal`; any other
  component would record a Petal self-build as its run.
- Petal can't take over the jobs Jenkins and ARC used to own (Homelab ADR 0010) —
  e.g. Jenkins' only job was YAML validation of the Homelab repo.
- ADR 0003 already named this as "the next increment … needed before Petal can
  orchestrate other services".

Constraints: the Petal image is distroless (no `git` binary); the trust boundary is
already "whoever can trigger a run can run containers next to a privileged DinD
daemon" (ADR 0002/0003); disk on the single worker is the first-class constraint.

## Decision

1. **A run names its source: `repo` + `ref`.** `repo` is an **alias** from an
   allowlist, never a raw URL; `ref` is a branch, tag or full commit SHA (validated
   against `^[A-Za-z0-9._/-]{1,200}$`, no `..`, no leading `-`). Both are optional —
   omitted means today's behaviour (the git-synced self repo), so existing triggers,
   cron schedules and the webhook keep working unchanged.

2. **The allowlist is the trust boundary, and it lives in git.** A YAML file
   (`-repos /etc/petal/repos.yaml`, a ConfigMap in the Homelab repo):

   ```yaml
   repos:
     homelab:
       url: https://github.com/Blacklotus89898/Homelab.git
       default_ref: main
     petal:
       url: https://github.com/Blacklotus89898/Petal.git
       default_ref: main
   ```

   Adding a repo is a reviewed Homelab commit. v1 is **public HTTPS repos only** —
   no clone credentials in Petal. Unknown alias → `400` at enqueue.

3. **Clone in a throwaway container, not in the control plane.** Before the first
   step, the executor runs a pinned `alpine/git` container against the DinD daemon
   (same Docker API path as steps — "Petal decides and remembers; Docker runs"),
   bind-mounting the run workspace:
   `git clone --filter=blob:none --no-checkout <url> /src && git -C /src checkout <ref>`
   (works for branches, tags and SHAs), then `git rev-parse HEAD`. The resolved SHA
   is stored in the existing `runs.git_sha` column and a `source.resolved` event
   (`{repo, ref, sha}`) is appended. Clone logs go to the run's log dir as step
   `_checkout`. The workspace is deleted after the run, as today.

4. **Pipelines load from the run's own tree.** The executor reads
   `.petal/pipelines/<pipeline>.yaml` from the cloned workspace, not the self repo.
   Each repo owns its pipelines.

5. **API.** `POST /api/runs` accepts `repo` and `ref`. For the self repo the
   pipeline-exists check stays at enqueue (`404`); for other repos it can't be
   checked without a clone, so a missing pipeline fails the run at checkout with a
   clear error. `GET /api/repos` lists the allowlist (aliases + default refs, no
   secrets). `GET /api/runs` gains a `repo` filter. `GET /api/pipelines?repo=<alias>`
   is **deferred** (it needs a cached shallow clone — next increment).

6. **Storage.** New columns `runs.repo` and `runs.ref` (default `''` = self repo),
   added by an idempotent `ALTER TABLE … ADD COLUMN` migration at open — the
   schema today is `CREATE TABLE IF NOT EXISTS`, which won't add columns to an
   existing database.

7. **Backstage.** A component opts in with the annotation
   `petal.io/repo: <alias>`. The `petal-run` template's component picker filters on
   that annotation (instead of `metadata.name: petal`), and `petal:run` sends the
   alias from it. Provenance (`backstage:<entity>#<user>`) and the runs card are
   unchanged.

## First use

Port Jenkins' only job — YAML validation of the Homelab repo — to a
`.petal/pipelines/validate.yaml` in the Homelab repo, annotate the
`homelab-platform` component with `petal.io/repo: homelab`, and trigger it from
Backstage. That proves the path end to end on a job the platform actually lost.

## Consequences

- **Positive:** Petal can orchestrate any allowlisted repo; every run records the
  exact commit it ran (`git_sha`); Backstage can trigger runs for real services;
  the remaining reason for ARC (a LAN-side runner) shrinks.
- **Cost:** a clone per run (blob-less, but still network + workspace disk per run
  — workspaces are deleted after the run, logs stay). No clone cache in v1.
- **Trust:** pipeline YAML from any allowlisted repo runs on the worker next to the
  privileged DinD daemon — the allowlist and branch protection on those repos are
  what stand between a commit and the worker. Private repos (credentials) are out
  of scope until there's a need.
- **Signal this was wrong:** clone time dominates short pipelines, or disk churn from
  workspaces shows up on the worker → add a per-repo bare-mirror cache.
