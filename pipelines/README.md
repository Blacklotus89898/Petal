# pipelines/

One directory per pipeline family (target form, per ADR 0001 D3): standalone
Dagger modules callable via `dagger call -m pipelines/<name>` and from GitHub
Actions via dagger-for-github.

## Phase B interim

Until the dagger CLI is standard in the toolchain, pipelines are registered as
Go functions in `internal/executor/dagger.go` using the host SDK
(`DaggerFunc` registry). The two forms produce identical containers — moving a
pipeline into its own module is a refactor, not a rewrite.

Registered today:

| Pipeline | What it does |
|---|---|
| `docker-image` | Builds the workdir Dockerfile and publishes it. Default ref: ephemeral `ttl.sh/petal-<run>:1h`; override with args `{"ref": "..."}` |
