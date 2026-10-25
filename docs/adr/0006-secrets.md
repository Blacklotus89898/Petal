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
    secret_refs: [main]    # default: [default_ref]
```
A run receives a repo's secrets **only when its ref is one of
`secret_refs`** (the branch names the repo protects). The checkout prefers a
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

## Trust model (unchanged, now stated)
The allowlist is the trust boundary. Steps share a privileged DinD daemon:
a step that drives Docker can start a container that mounts anything the
daemon can see, including `/etc/petal-secrets`. Grants and masking stop
**accidental** exposure and pipelines from **ungranted refs**; they do not
contain a hostile pipeline on a granted ref — that is what branch protection
on `secret_refs` is for.

## Consequences
- Real credentials (registry push tokens) are sealed by the operator into
  `petal-secrets`; nothing in Petal or its config ever holds a value.
- Private-repo checkout can reuse this (a `clone_secret` per repo via
  `GIT_ASKPASS`) — deferred until a private repo exists.
