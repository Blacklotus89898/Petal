# AGENTS.md — Petal

Guide for any coding agent (Claude Code, Codex, Cursor, Copilot, Gemini…) and human contributor.

## What this is

The homelab's pipeline orchestrator. **Petal decides and remembers; Docker runs; YAML describes.**
Pipelines live in the target repo (`.petal/pipelines/*.yaml`) and run as plain Docker
containers; Petal schedules, dedups, records (SQLite) and reports. Backstage is the portal,
Petal the orchestrator (ADR 0003). One Go binary with an embedded React UI.

Read first: `docs/adr/` in order (0001 architecture … 0009 retention). The ADRs are the
design source of truth; the README is the operator view.

## Commands

```bash
gofmt -l .                       # must print nothing
go vet ./...
go test ./...
go run .                         # API + UI on :8080 (stub executor)
cd web && npm install && npm run dev   # UI hot reload on :5173, proxies /api
npm run build --prefix web       # required before a production `go build` (go:embed web/dist)
docker compose -f deploy/compose.yaml up --build -d   # real step execution on Windows
go run . -executor steps -workdir .                   # real execution on Linux/WSL
```

Trigger a run locally: `PETAL_API_TOKEN=dev go run .` then
`curl -X POST localhost:8080/api/runs -H "Authorization: Bearer dev" -d '{"pipeline":"docker-image"}'`.

## Layout

```
main.go              entrypoint; go:embed of web/dist
internal/api         HTTP handlers (same-origin UI, no CORS)
internal/pipeline    spec loader/validator for .petal/pipelines + .petal/steps
internal/runs        SQLite run registry (runs, steps, events) — the traceability spine
internal/scheduler   worker pool: claim → execute → record; cron schedules
internal/executor    Executor interface: stub, steps (Docker API)
internal/repos       multi-repo checkout (repo@ref, allowlist) — ADR 0004
internal/notify      ntfy run notifications — ADR 0008
internal/retention   janitor — ADR 0009
web/                 Vite + React + Mantine console (read-only)
.petal/pipelines     Petal's own pipelines (dogfood: Petal builds Petal)
deploy/compose.yaml  local container run
docs/adr/            decision records
```

## Non-negotiables

- **Docker access is a grant:** only steps with `docker: true` get `DOCKER_HOST`/host network.
- **Secrets** are granted per repo and ref, mounted read-only as files and masked in logs (ADR 0006).
  Never pass them as env, never log them, never add a secret to this public repo.
- No step container may outlive a canceled run.
- The console is read-only; state changes go through the bearer-token API.
- Every behaviour change that alters a decision → new ADR (`docs/adr/NNNN-title.md`);
  never rewrite an accepted ADR.

## Adding a feature

1. Branch from `main`. If it changes a contract (pipeline spec, API, executor), write the ADR first.
2. Pipeline-spec changes: update the loader + validator in `internal/pipeline` with tests for
   both valid and rejected specs, and an example in the README "Pipelines" section.
3. Registry changes: additive SQLite migrations only; keep old runs readable.
4. UI changes: `npm run build --prefix web` must pass (tsc + vite).
5. gofmt/vet/test green → PR (or a commit to `main` for small fixes), conventional-commit title
   that cites the ADR when relevant: `feat: retention janitor … (ADR 0009)`.

## Shipping

`build-petal.yaml` on `main` → `ghcr.io/blacklotus89898/petal:<sha>` (Trivy-scanned) → bump the
SHA in Homelab `services/petal/` → ArgoCD. Never `:latest` in Homelab.

## Knowledge base

If the `knowledge` MCP is connected, search it before debugging pipelines/runner issues and
write back non-obvious fixes (rules: DaVinci `AGENTS.md`).
