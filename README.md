# Petal 🌸

Homelab pipeline orchestrator: a control plane for repo-native CI/CD
pipelines. **Backstage is the portal; Petal is the orchestrator** — Backstage
triggers Petal runs, Petal executes and remembers them
([ADR 0003](docs/adr/0003-backstage-portal-petal-orchestrator.md)). The portal
role moves into Petal incrementally.

**Principle:** *Petal decides and remembers; Docker runs; YAML describes.*
Petal schedules, deduplicates, records, and reports pipeline runs; pipelines
are declarative YAML files committed to the repository being built
(`.petal/pipelines/*.yaml`) and execute as plain Docker containers;
GitHub Actions stays the trigger layer; ArgoCD stays the deploy layer;
OpenObserve (via the existing OTel collector) is the telemetry sink.

## Architecture

```mermaid
flowchart LR
  subgraph Triggers["Triggers"]
    GHA["GitHub Actions webhook"]
    CRON["Petal scheduler (cron)"]
    BS["Backstage (petal:run action)"]
  end

  subgraph Petal["Petal control plane — one Go binary"]
    API["API server<br/>(REST + embedded React UI)"]
    SCHED["Scheduler<br/>(worker pool, dedup, timeouts)"]
    REG[("SQLite run registry<br/>runs · steps · events")]
    EXEC["Step-runner executor"]
  end

  subgraph Execution["Execution"]
    SPEC[".petal/pipelines/*.yaml<br/>(in the target repo)"]
    DC["Step containers<br/>via Docker API / DOCKER_HOST"]
  end

  subgraph DataPlane["Existing data plane"]
    OTEL["OTel collector"]
    OO["OpenObserve"]
    ARGO["ArgoCD"]
    K3S["k3s"]
  end

  GHA --> API
  CRON --> SCHED
  BS --> API
  API --> REG
  SCHED --> REG
  SCHED --> EXEC
  SPEC --> EXEC
  EXEC --> DC
  DC --> ARGO --> K3S
  API -. "read-only views" .-> ARGO
```

See [ADR 0001](docs/adr/0001-architecture.md),
[ADR 0002](docs/adr/0002-step-runner.md) and
[ADR 0003](docs/adr/0003-backstage-portal-petal-orchestrator.md) for the
decision log.

## Pipelines

A repository gets pipelines by committing YAML files — zero Petal-side config:

```yaml
# .petal/pipelines/docker-image.yaml
name: docker-image
description: Build and publish the project container image

params:
  ref:
    default: ttl.sh/petal-${PETAL_RUN_ID}:1h

steps:
  - name: build
    image: docker:27-cli
    run: |
      docker buildx build --tag "$PARAM_REF" --push /src

outputs:
  image-ref: .petal-out/image-ref.txt
```

Rules (full spec in ADR 0002, 0005):

- Steps run **fail-fast**, each in a fresh container from
  `image` (pinned tags required, `latest` rejected), workspace bind-mounted
  at `/src` (a per-run snapshot of the repo — steps never race git-sync).
- `run` executes with `sh -c "set -eu"`; logs stream to
  `<data>/logs/<run_id>/<step>.log`.
- Every step gets `PARAM_*` for each param, plus `PETAL_RUN_ID`,
  `PETAL_PIPELINE`, `PETAL_STEP`, `PETAL_TRIGGER`, `PETAL_ARGS_JSON`, and
  `DOCKER_HOST` (for image-building steps).
- `outputs` files are read after success and recorded as the run's output
  event.
- The executor talks the Docker Engine API (`DOCKER_HOST` — the DinD sidecar
  on k3s, the mounted socket under compose). No engine, no SDK, no docker CLI.

### Modular steps, DAG order, concurrency, timeouts (ADR 0005)

```yaml
# .petal/steps/yamllint.yaml — a reusable step in the same repo
inputs:
  config: { default: .yamllint }
image: python:3.12-slim
run: yamllint -c "$INPUT_CONFIG" .
timeout: 10m
```

```yaml
# .petal/pipelines/validate.yaml
max_parallel: 2          # default 2, cap 4
timeout: 30m             # whole run (default 60m)
steps:
  - name: deploy         # listed first — only `needs` decides order
    image: alpine:3.21
    run: ./deploy.sh
    needs: [lint, test]
  - name: lint
    uses: yamllint       # resolved from .petal/steps/yamllint.yaml
    with: { config: .yamllint }
    needs: []            # a root
  - name: test
    image: golang:1.26-alpine
    run: go test ./...
    timeout: 15m         # per step (default 30m)
    needs: []
```

- **Order:** no step declares `needs` → file order, one at a time (the
  original behaviour). Any step declares `needs` → every step must
  (`needs: []` for roots); the DAG alone decides order, so steps can be
  reordered freely. Unknown/self/duplicate needs and cycles are rejected.
- **Concurrency:** ready steps run in parallel up to `max_parallel`. They share
  `/src` and the daemon's network — don't write the same paths or bind the same
  ports. The first failure cancels running siblings; never-started steps are
  recorded as `step.skipped`.
- **`uses`:** library steps live in `.petal/steps/<name>.yaml` (no nesting).
  `with` keys must be declared `inputs` (`default`, `required`); they arrive as
  `INPUT_*` env. The pipeline step's `name` keys its logs, so one library step
  can be used several times; its `env`/`timeout` override the library's.
- **Timeouts:** step (default 30m) and run (default 60m) budgets; errors name
  the limit (`step "lint" timed out after 10m`). Checkout has its own 5m.
- **Cancel:** `POST /api/runs/{id}/cancel` — see the API table.

The `stub` executor (`-executor stub`, the default) simulates runs without
Docker — handy for UI development; `{"simulate":"failure"}` exercises failure
paths.

### Secrets (ADR 0006)

```yaml
steps:
  - name: push
    image: docker:27-cli
    secrets: [ghcr-push]       # request by name; library steps may too
    run: docker login ghcr.io -u x --password-stdin < "$PETAL_SECRET_GHCR_PUSH_FILE"
```

- **Granted in Petal's config, never by a pipeline**: `self.secrets` for the
  git-synced self repo, `repos.<alias>.secrets` + `secret_refs` (default:
  `[default_ref]`) for allowlisted repos. A run at any other ref — a branch,
  a tag, a SHA — gets none; a step requesting an ungranted secret fails the
  run before any step starts.
- Values are files in `-secrets <dir>` (a mounted Kubernetes Secret, the same
  path in the dind container), re-read per run. Each requested secret is
  mounted read-only at `/run/petal/secrets/<name>`, path in
  `PETAL_SECRET_<NAME>_FILE` — never env, never under `/src`.
- Every value of the run's secrets is masked as `***` in step logs, outputs
  and errors. Best-effort: encoded values pass. The allowlist and branch
  protection on `secret_refs` are the real boundary.

## Quickstart (dev on Windows)

Two terminals — Go API on `:8080`, Vite dev server on `:5173` (proxies `/api`):

```powershell
# Terminal 1 — control plane
go run .

# Terminal 2 — frontend with hot reload
cd web
npm install   # first time only
npm run dev
```

Open http://localhost:5173. The UI is a read-only console; trigger runs through
the API (set `PETAL_API_TOKEN` first):

```powershell
$env:PETAL_API_TOKEN = "dev"; go run .
curl -X POST localhost:8080/api/runs -H "Authorization: Bearer dev" -d '{"pipeline":"docker-image"}'
```

### Real pipelines

Steps are Linux containers, so on a Windows dev box run Petal itself as a
Linux container — same image as production, sibling step containers via the
mounted daemon socket:

```powershell
docker compose -f deploy/compose.yaml up --build -d
# UI on http://localhost:8080 — executor is steps, workdir mounted at /src
```

On Linux/WSL2 (and the k3s deployment), the binary runs directly against a
daemon:

```powershell
go run . -executor steps -workdir .
```

Pipeline `docker-image` (in this repo's `.petal/pipelines/`) builds the workdir
Dockerfile and publishes it — by default to an ephemeral
[ttl.sh](https://ttl.sh) ref (no credentials needed), overridable with args
`{"ref": "ghcr.io/you/petal:dev"}`. **Dogfood: Petal builds Petal.**

### Production build (single container)

```powershell
npm run build --prefix web   # writes web/dist, embedded into the binary
go build -trimpath -ldflags "-s -w" -o petal.exe .
docker build -t petal .      # or: docker compose -f deploy/compose.yaml up --build
```

The container is a distroless static image (~20MB) — no OS packages, no shell.

## API

| Method | Path | Description |
|---|---|---|
| GET | `/api/health` | Liveness probe |
| GET | `/api/pipelines` | Pipeline catalog discovered in the workdir; `?repo=<alias>` lists an allowlisted repo's default ref instead (sparse clone, cached 2 min) |
| GET | `/api/runs?limit=50` | Recent runs; filters `pipeline=`, `trigger_kind=`, `repo=`, `source=` (trigger_source prefix, e.g. `backstage:component:default/petal%23`) |
| POST | `/api/runs` | 🔒 Trigger a run: `{"pipeline":"docker-image","args":{}}` — optional `trigger_kind` (`api`\|`backstage`), `trigger_source`, `dedup_key`, and `repo` (allowlist alias) + `ref` (branch/tag/SHA, default the repo's `default_ref`) for a multi-repo run (ADR 0004). Self-repo unknown pipeline → 404; unknown repo / bad ref → 400 |
| GET | `/api/repos` | Multi-repo allowlist (`-repos` file): alias, URL, default ref |
| GET | `/api/runs/{id}` | Run detail + audit events (step.started / step.finished / output) |
| POST | `/api/runs/{id}/cancel` | 🔒 Cancel a run: queued → `canceled` at once (200); running → step containers killed, unstarted steps skipped, run ends `canceled` (202); finished → 409 |
| GET | `/api/runs/{id}/logs/{step}` | Step log (text/plain, live while the step runs) |
| POST | `/api/triggers/github` | Signed webhook (HMAC-SHA256, header `X-Petal-Signature`) for CI triggers; enables with `PETAL_TRIGGER_TOKEN` env; dedups on `git_sha:pipeline` |
| GET | `/api/schedules` | List cron schedules (`decl-*` ones come from the config file and are re-synced at startup) |
| POST | `/api/schedules` | 🔒 Create a schedule: `{"pipeline":"docker-image","cron":"*/15 * * * *","args":{}}` — optional `repo` + `ref` |
| GET | `/metrics` | Prometheus exposition: `petal_runs_total`, `petal_run_duration_seconds`, `petal_run_last_finished_timestamp_seconds` (by pipeline/repo/status), `petal_runs_queued`, `petal_runs_running`, `petal_schedules`, `petal_repos` — computed from the registry at scrape time |
| DELETE | `/api/schedules/{id}` | 🔒 Delete a schedule |
| POST | `/api/schedules/{id}/toggle` | 🔒 Enable/disable a schedule |

🔒 = requires `Authorization: Bearer $PETAL_API_TOKEN`. With no token
configured these endpoints return 503 — an open port never means an open
trigger. Reads stay open on the LAN (ADR 0003).

### Wiring GitHub Actions → Petal

`.github/workflows/petal-trigger.yaml` sends a signed trigger on every push to
`main`. To activate:

1. Generate a token: `openssl rand -hex 32`, run Petal with
   `PETAL_TRIGGER_TOKEN=<token>`
2. In the GitHub repo: **Settings → Secrets and variables → Actions** — add
   secrets `PETAL_URL` (where Petal is reachable from runners) and
   `PETAL_TRIGGER_TOKEN`, and set variable `PETAL_ENABLED=true`
3. Push — the run appears in Petal with trigger `webhook` and the GHA run URL
   as its source

## Layout

```
main.go                       # entrypoint; go:embed of web/dist
internal/
  api/                        # HTTP handlers (same-origin UI — no CORS anywhere)
  pipeline/                   # spec loader: .petal/pipelines/*.yaml → validated Pipeline
  runs/                       # SQLite run registry (traceability spine)
  scheduler/                  # worker pool: claim → execute → record
  executor/                   # Executor interface; stub, steps (Docker API)
web/                          # Vite + React + Mantine frontend
.petal/pipelines/             # this repo's own pipelines (dogfood)
deploy/compose.yaml           # local container run
docs/adr/                     # architecture decision records
```

## Phases

- [x] **A — walking skeleton**: binary + UI + registry + manual trigger + stub executor
- [x] **B — brains**: ~~GHA webhook receiver~~ ✅ · ~~cron scheduler~~ ✅ · repo-native pipeline spec + step-runner executor (ADR 0002) ✅
- [x] **Orchestrator contract** (ADR 0003): bearer-token API, trigger provenance, run filters, read-only console
- [x] **Backstage integration**: `petal:run` scaffolder action, "Run a Petal pipeline" template with a live pipeline picker, "Petal runs" entity card (Homelab repo, 2026-10-23)
- [x] **Multi-repo runs** (ADR 0004): `repo@ref` per run, cloned in a throwaway `alpine/git` container, allowlist file (`-repos`), resolved commit recorded
- [x] **C — senses (metrics)**: `/metrics` from the registry + Homelab scrape + alerts (OTel traces still open)
- [x] **Declarative schedules**: `schedules:` in the config file, synced at startup (git owns them)
- [ ] **D — catalog**: ArgoCD/K8s views, scorecards, notifications
- [x] **Modular + DAG steps** (ADR 0005): `uses:` step library, `needs` DAG with bounded concurrency, step/run timeouts, cancel API
- [ ] petal CLI scaffolding
