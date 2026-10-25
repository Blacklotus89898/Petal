# ADR 0006: Secrets — granted per repo and ref, mounted as files, masked in logs

**Status:** Accepted (2026-10-24)
**Builds on:** [ADR 0004](0004-multi-repo-runs.md) (multi-repo allowlist), [ADR 0005](0005-modular-dag-steps.md) (steps)

## Context

A step cannot hold a credential: there is no way to push an image to a real
registry, call an authenticated API, or (later) clone a private repo. Every
pipeline so far is "read public things, write to ttl.sh".

Pipeline YAML comes from the repos it runs, so **a pipeline must never be able
to grant itself a secret**. Any branch of an allowlisted repo can be run
(`repo@ref`), so a grant to a repo would otherwise let anyone who can push a
branch print the secret from a pipeline of their own.

## Decision

### 1. Grants live in Petal's config (git, reviewed), per repo and ref
```yaml
self:                      # the git-synced self repo (always its main)
  secrets: [ghcr-push]
repos:
  homelab:
    url: https://github.com/Blacklotus89898/Homelab.git
    secrets: [e2e-canary]
    trusted_refs: [main]    # default: [default_ref]
```
A run receives a repo's secrets **only when its ref is one of
`trusted_refs`** (the branch names the repo protects). The checkout prefers a
branch over a tag or SHA of the same name, so `main` always means the branch.
Any other ref — a feature branch, a tag, even main's own SHA — gets no secrets.

### 2. Steps request, they don't grant
`secrets: [name, …]` on a step (or on a library step — the two lists are
merged for a `uses` step). Names are checked at load time; grants are checked
**when the run starts**, after the source is resolved: a step requesting a
secret the run isn't granted fails the run before any step starts, naming the
secret, repo and ref. Listing pipelines (the Backstage picker) never touches
secrets.

### 3. Delivery: read-only files, not env
The Kubernetes Secret `petal-secrets` is mounted at `/etc/petal-secrets` in
**both** the petal and the dind container (dind resolves bind-mount sources
in its own mount namespace), `optional: true`, without `subPath` so a
rotation reaches the pod without a restart. A step gets each requested
secret bind-mounted read-only at `/run/petal/secrets/<name>` and
`PETAL_SECRET_<NAME>_FILE` pointing at it. Never env (visible in
`docker inspect` to any docker-capable sibling), never under `/src` (shared
workspace; `outputs:` reads from it).

### 4. Masking (best-effort)
Every value of the run's secrets is replaced by `***` in all of the run's
step logs (streaming, including a value split across writes), in the run's
outputs and in its error. Multi-line secrets are masked line by line too.
Masking is a seatbelt, not a boundary: base64'd or reversed values pass.

### 5. Daemon access is a grant too (amended 2026-10-25, same day)
The first cut gated only secret *requests*. A live probe proved that
insufficient: every step had `DOCKER_HOST` and host networking, and the
daemon can see `/etc/petal-secrets`, so a pipeline on an **ungranted branch**
ran `docker run -v /etc/petal-secrets:/s alpine cat /s/…` and read the value
— unmasked, since that run had no secrets loaded. Ref grants on secrets alone
were theatre. Now:
- `docker: true` on a step (or library step) requests the daemon; it is
  granted like secrets (`docker: true` on the repo / `self`, at
  `trusted_refs` only — the key was `secret_refs` for a day, renamed because
  it gates every privilege). Ungranted → the run fails before any step.
- Only a granted docker step gets `DOCKER_HOST` and host networking (needed
  for the loopback daemon). Every other step — and the checkout — runs on
  the daemon's default bridge with no `DOCKER_HOST`; a `DOCKER_HOST` in a
  step's own `env` is dropped.
- dind listens on `127.0.0.1:2375` only, so a bridge container cannot reach
  it through the gateway IP.

## Trust model
- **Any ref of an allowlisted repo** runs as unprivileged containers on a
  bridge: no daemon, no secrets. That is what a branch push can do.
- **A trusted ref granted docker** can reach everything the privileged DinD
  daemon can: every secret file, other runs' workspaces under `/data`, and
  the node beyond it. Grant docker only where you would grant everything,
  and only to refs with branch protection that is actually enforced (the
  Homelab and Petal repos' `main` currently accept admin-bypassed pushes).
- **A trusted ref granted only secrets** gets just its requested files —
  but its pipeline is still trusted code that could print them.
- Masking stops accidental exposure, not a hostile pipeline.
- Values are read once at run start and masked from then on; a re-seal that
  lands mid-run is mounted into later steps but not masked for that run.

## Consequences
- Real credentials (registry push tokens) are sealed by the operator into
  `petal-secrets`; nothing in Petal or its config ever holds a value.
- Private-repo checkout can reuse this (a `clone_secret` per repo via
  `GIT_ASKPASS`) — deferred until a private repo exists.
