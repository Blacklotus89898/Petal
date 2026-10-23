# ADR 0003: Backstage is the portal, Petal is the orchestrator

**Status:** Accepted (2026-10-15) — trigger path live 2026-10-23 (see Implementation)
**Amends:** [ADR 0001](0001-architecture.md) — Petal no longer replaces Backstage
in one step; the portal role moves to Petal incrementally, if and when it earns it.

## Context

Petal was started (2026-09-06) to replace Backstage. Since then the Homelab
platform invested in Backstage as the portal: the scaffolder golden path,
TechDocs for every service, catalog scorecards, and a canary rollout gated by
the front-door SLO. Meanwhile Petal's executor proved out (ADR 0002) but its
portal phases (C, D) never started, and its API had no authentication — any
LAN client could trigger privileged builds.

Running two half-portals costs memory on the single worker and splits the
story. The two systems are good at different things.

## Decision

1. **Backstage is the portal** — catalog, docs, scaffolding, and the place a
   human starts an action.
2. **Petal is the orchestrator** — API-first. It decides, executes, and
   remembers runs. Backstage triggers it through a scaffolder action
   (`petal:run`, in the Homelab repo's Backstage backend).
3. **Petal's UI is a read-only ops console** for now: runs, timelines, step
   logs. Triggers come from Backstage, the signed GitHub webhook, or cron.
4. **The portal grows in Petal slowly** (catalog read, then scorecards), each
   step justified on its own; Backstage is retired only when Petal covers what
   is actually used.

## API contract (v1)

| Endpoint | Auth |
|---|---|
| `GET /api/health`, `/api/runs`, `/api/runs/{id}`, `/api/runs/{id}/logs/{step}`, `/api/pipelines`, `/api/schedules` | open (LAN) |
| `POST /api/runs`, `POST/DELETE /api/schedules…` | `Authorization: Bearer $PETAL_API_TOKEN` |
| `POST /api/triggers/github` | HMAC (`PETAL_TRIGGER_TOKEN`, unchanged) |

- **No token configured → mutating endpoints return 503.** An open port never
  means an open trigger (same rule as the webhook).
- `POST /api/runs` accepts `trigger_kind` ∈ {`api` (default), `backstage`};
  `webhook` and `cron` stay reserved for their own paths. The pipeline must
  exist in the workdir catalog (404 otherwise).
- **Trigger source convention for Backstage:**
  `backstage:<entity-ref>#<user-ref>`, e.g.
  `backstage:component:default/petal#user:default/jo`. The `#` terminates the
  entity ref, so `GET /api/runs?source=backstage:component:default/petal%23`
  lists exactly one entity's runs (no `petal-docs` bleed) — the hook for a
  future "Petal runs" tab on Backstage entity pages.
- Reads are unauthenticated on purpose: run history is not secret on the LAN,
  and the console must work without credentials. Revisit when the gateway +
  authentik front door lands.

## Consequences

- **Positive:** one portal, one orchestrator, no duplicate UI work; direct
  unauthenticated calls to Petal's write API are closed; every run records
  who/what started it; `/?run=<id>` deep-links a run in the console.
- **Not yet closed — the trigger surface moves to Backstage:** Backstage runs
  the allow-all permission policy, so every *signed-in* user can run the Petal
  template and start a privileged build. (Corrected 2026-10-23: sign-in is
  GitHub OAuth with the `usernameMatchingUserEntityName` resolver, so only
  GitHub users with a User entity in `catalog/users.yaml` — today one — can
  sign in at all; the guest provider was removed earlier.) The token only stops callers that bypass
  Backstage. What actually closes it: authentik sign-in for Backstage plus a
  real permission policy on the `petal:run` action (Homelab roadmap, OIDC item).
- **Negative / accepted:** a shared bearer token (sealed in both namespaces)
  rather than per-user auth — Backstage is the identity boundary. Petal still
  executes pipelines from a single git-synced repo; multi-repo runs with an
  allowlist are the next increment (needed before Petal can orchestrate other
  services).
- **Trust boundary unchanged from ADR 0002:** whoever holds the token can run
  containers on the worker via the privileged DinD sidecar.

## Implementation (2026-10-23)

- `petal:run` is an inline backend module in the Homelab repo
  (`backstage/packages/backend/src/modules/petal.ts`), template
  `templates/petal-run`. It sends `dedup_key: backstage-task:<task id>` so a
  scaffolder retry cannot queue a second build, and returns the run id plus a
  `https://petal.lab.home/?run=<id>` console link.
- Scripted callers use a scoped Backstage `externalAccess` token (subject
  `automation`, scaffolder + catalog only); their runs carry no user, so the
  source reads `backstage:<entity>#unknown`. Scope note: scaffolder access
  means *every* template, including `homelab-service` (publishes to GitHub
  with the integration token) — the token is as strong as the templates.
- **Single-repo limit:** Petal runs pipelines only from its own git-synced
  repo, so the template's component picker is restricted to
  `component:default/petal` — another component would record a Petal
  self-build as its run. Multi-repo runs (`repo@ref` + allowlist) lift this.
- Verified end to end by `tools/petal-e2e/run.sh` (Homelab): first run
  `run_7bd4f3989cb49f1a`, docker-image, succeeded; the per-entity source
  filter returns it.
