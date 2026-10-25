// Steps is the first-party step-runner executor (ADR 0002): it executes each
// pipeline step as a plain Docker container against DOCKER_HOST (the DinD
// sidecar), streaming logs to disk and emitting per-step audit events.
//
// Petal decides and remembers; Docker runs; YAML describes.
package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/Blacklotus89898/Petal/internal/pipeline"
	"github.com/Blacklotus89898/Petal/internal/repos"
	"github.com/Blacklotus89898/Petal/internal/runs"
)

// checkoutImage is the throwaway clone container for multi-repo runs
// (ADR 0004): Petal's own image is distroless and has no git. Pinned.
const checkoutImage = "alpine/git:2.54.0"

// checkoutScript clones PETAL_REPO_URL blob-less into the (empty) workspace and
// checks out PETAL_REF detached — a remote branch first, then a tag or commit.
// URL and ref arrive as env vars, never interpolated into the script; refs are
// pre-validated by repos.ValidRef (no leading "-", no "..").
const checkoutScript = `git clone --quiet --filter=blob:none --no-checkout "$PETAL_REPO_URL" /src
cd /src
sha=$(git rev-parse --verify --quiet "origin/$PETAL_REF^{commit}" || git rev-parse --verify --quiet "$PETAL_REF^{commit}") || {
  echo "ref $PETAL_REF not found in $PETAL_REPO_URL" >&2; exit 1; }
git -c advice.detachedHead=false checkout --quiet --detach "$sha"
printf '%s' "$sha" > .git/petal-resolved-sha
git log -1 --format='checked out %H - %s'`

// Steps configures the step-runner.
type Steps struct {
	store       *runs.Store     // per-step audit events (step.started / step.finished)
	workdir     string          // source tree (git-synced) copied into each run workspace
	dataDir     string          // PVC root: logs/ and work/ live under here
	dockerHost  string          // injected into step containers for image-building steps
	repos       repos.Allowlist // multi-repo sources a run may name (ADR 0004)
	catalogMu   sync.Mutex
	catalog     map[string]catalogEntry // RepoPipelines cache, per alias
	runner      stepRunner              // runStep; swapped for a fake in tests (ADR 0005)
	secretsDir  string                  // mounted Secret, same path in dind (ADR 0006)
	selfSecrets []string                // granted to self-repo runs
	log         *slog.Logger
	cli         *client.Client
	initErr     error
}

// NewSteps creates the executor and its Docker API client. An unreachable
// daemon is not fatal here — runs fail with a clear error instead (same
// behavior the Dagger executor had for engine outages).
func NewSteps(store *runs.Store, workdir, dataDir, dockerHost string, allow repos.Allowlist, log *slog.Logger) *Steps {
	if log == nil {
		log = slog.Default()
	}
	cli, err := client.NewClientWithOpts(
		client.WithHost(dockerHost),
		client.WithAPIVersionNegotiation(),
	)
	s := &Steps{
		store:      store,
		workdir:    workdir,
		dataDir:    dataDir,
		dockerHost: dockerHost,
		repos:      allow,
		log:        log,
		cli:        cli,
		initErr:    err,
	}
	s.runner = s.runStep
	return s
}

// checkoutTimeout bounds the clone: it runs before the pipeline (and its own
// timeout) is known (ADR 0005).
const checkoutTimeout = 5 * time.Minute

// Name implements Executor.
func (s *Steps) Name() string { return "steps" }

// Execute implements Executor: source (snapshot or checkout) → load spec →
// resolve params → run steps (file order, or the needs DAG with bounded
// concurrency; fail-fast) under the pipeline's timeout → collect outputs.
// ctx carries the pool's cancel cause (ErrCancelRequested) and safety ceiling.
func (s *Steps) Execute(ctx context.Context, run runs.Run) Result {
	if s.initErr != nil {
		return Result{Status: runs.StatusFailed, Error: "docker client: " + s.initErr.Error()}
	}

	logDir := filepath.Join(s.dataDir, "logs", run.ID)
	workDir := filepath.Join(s.dataDir, "work", run.ID)
	defer os.RemoveAll(workDir) // logs stay; workspace is scratch
	defer s.sweep(context.WithoutCancel(ctx), run.ID)
	for _, d := range []string{logDir, workDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return Result{Status: runs.StatusFailed, Error: "create " + d + ": " + err.Error()}
		}
	}

	// Source: the git-synced self repo (snapshot, so steps can write freely
	// without racing git-sync), or an allowlisted repo cloned at the run's ref
	// (ADR 0004). Either way the pipeline spec loads from the run's own tree.
	if run.Repo == "" {
		if err := os.CopyFS(workDir, os.DirFS(s.workdir)); err != nil {
			return Result{Status: runs.StatusFailed, Error: "snapshot workspace: " + err.Error()}
		}
	} else if res := s.checkout(ctx, run, workDir, logDir); res != nil {
		if out, ok := runOutcome(ctx); ok {
			return out // canceled / ceiling while cloning
		}
		return *res
	}

	spec, err := pipeline.Get(workDir, run.Pipeline)
	if err != nil {
		return Result{Status: runs.StatusFailed, Error: err.Error()}
	}

	args, err := parseArgs(run.ArgsJSON)
	if err != nil {
		return Result{Status: runs.StatusFailed, Error: "bad run args: " + err.Error()}
	}
	params, err := spec.ResolveParams(args, run.ID)
	if err != nil {
		return Result{Status: runs.StatusFailed, Error: err.Error()}
	}
	granted, scope := s.grantedSecrets(run.Repo, run.Ref)
	secretVals, err := s.loadSecrets(spec, granted, scope)
	if err != nil {
		return Result{Status: runs.StatusFailed, Error: err.Error()}
	}
	mask := newMasker(secretVals)
	ctx = withMasker(ctx, mask)

	baseEnv := map[string]string{
		"PETAL_RUN_ID":    run.ID,
		"PETAL_PIPELINE":  spec.Name,
		"PETAL_TRIGGER":   run.TriggerKind,
		"PETAL_ARGS_JSON": run.ArgsJSON,
	}
	if s.dockerHost != "" {
		baseEnv["DOCKER_HOST"] = s.dockerHost
	}
	for k, v := range params {
		baseEnv[pipeline.EnvKey(k)] = v
	}

	// The run's own budget starts here, once the spec (and its timeout) is
	// known — the pool only holds a safety ceiling.
	runCtx, cancelRun := context.WithTimeoutCause(ctx, spec.TimeoutDur,
		&TimeoutError{What: "run", Limit: spec.TimeoutDur})
	defer cancelRun()
	failure := s.runSteps(runCtx, run.ID, spec, workDir, logDir, baseEnv)
	if out, ok := runOutcome(runCtx); ok {
		return out
	}
	if failure != "" {
		return Result{Status: runs.StatusFailed, Error: mask.String(failure)}
	}

	outputs := map[string]string{}

	for name, rel := range spec.Outputs {
		raw, err := os.ReadFile(filepath.Join(workDir, filepath.FromSlash(rel)))
		if err != nil {
			return Result{Status: runs.StatusFailed, Error: fmt.Sprintf("output %q: read %s: %v", name, rel, err)}
		}
		outputs[name] = mask.String(strings.TrimSpace(string(raw)))
	}
	out := Result{Status: runs.StatusSucceeded}
	if len(outputs) > 0 {
		payload, _ := json.Marshal(outputs)
		out.Output = string(payload)
	}
	return out
}

// checkout clones run.Repo at run.Ref into workDir as the synthetic first step
// (pipeline.CheckoutStep), records the resolved commit on the run, and returns
// a failed Result on any error (nil on success).
func (s *Steps) checkout(ctx context.Context, run runs.Run, workDir, logDir string) *Result {
	fail := func(msg string) *Result { return &Result{Status: runs.StatusFailed, Error: msg} }
	repo, ok := s.repos[run.Repo]
	if !ok {
		return fail(fmt.Sprintf("repo %q is not in the allowlist", run.Repo))
	}
	ref := run.Ref
	if ref == "" {
		ref = repo.DefaultRef
	}
	if !repos.ValidRef(ref) {
		return fail(fmt.Sprintf("invalid ref %q", ref))
	}

	step := pipeline.Step{
		Name:  pipeline.CheckoutStep,
		Image: checkoutImage,
		Run:   checkoutScript,
		Env:   map[string]string{"PETAL_REPO_URL": repo.URL, "PETAL_REF": ref},
	}
	ckCtx, cancel := context.WithTimeoutCause(ctx, checkoutTimeout, &TimeoutError{What: "checkout", Limit: checkoutTimeout})
	defer cancel()
	s.emit(ctx, run.ID, "step.started", map[string]any{"step": step.Name, "index": -1, "image": step.Image})
	exit, err := s.runner(ckCtx, run.ID, step, workDir, logDir, map[string]string{"PETAL_RUN_ID": run.ID})
	if ckCtx.Err() != nil {
		cause := context.Cause(ckCtx)
		s.emit(ctx, run.ID, "step.finished", map[string]any{"step": step.Name, "canceled": true, "cause": cause.Error()})
		return fail(cause.Error())
	}
	if err != nil || exit != 0 {
		msg := fmt.Sprintf("checkout %s@%s failed (exit %d)", run.Repo, ref, exit)
		if err != nil {
			msg += ": " + err.Error()
		}
		s.emit(ctx, run.ID, "step.finished", map[string]any{"step": step.Name, "exit": exit, "error": msg})
		return fail(msg)
	}
	s.emit(ctx, run.ID, "step.finished", map[string]any{"step": step.Name, "exit": 0})

	raw, err := os.ReadFile(filepath.Join(workDir, ".git", "petal-resolved-sha"))
	if err != nil {
		return fail("checkout: read resolved sha: " + err.Error())
	}
	sha := strings.TrimSpace(string(raw))
	if err := s.store.SetGitSHA(context.WithoutCancel(ctx), run.ID, sha); err != nil {
		s.log.Warn("record git sha", "run", run.ID, "err", err)
	}
	s.emit(ctx, run.ID, "source.resolved", map[string]any{"repo": run.Repo, "ref": ref, "sha": sha})
	return nil
}

// sweep force-removes every container labelled with this run — belt and
// braces after cancellation races (ADR 0005): whatever happened mid-run, a
// finished run leaves no containers behind on the DinD daemon.
func (s *Steps) sweep(ctx context.Context, runID string) {
	if s.cli == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	list, err := s.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", "petal.run="+runID)),
	})
	if err != nil {
		return // daemon unreachable: nothing we can clean anyway
	}
	for _, c := range list {
		if err := s.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err == nil {
			s.log.Info("swept leftover step container", "run", runID, "container", c.Names)
		}
	}
}

// stepNetworkMode picks the network for a step container. When DOCKER_HOST is
// a loopback address (the DinD sidecar pattern, tcp://127.0.0.1:2375), steps
// that drive Docker themselves — docker:27-cli builds, for instance — reach
// the daemon over 127.0.0.1, which from a bridge-network container is the
// container's own loopback, not the daemon's. Host networking puts the step
// in the daemon's netns so loopback resolves. With a unix socket or a
// non-loopback DOCKER_HOST, the daemon's default bridge is correct.
func stepNetworkMode(dockerHost string) string {
	u, err := url.Parse(dockerHost)
	if err != nil {
		return ""
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
		return "host"
	default:
		return ""
	}
}

// runStep pulls the image, creates and starts the container, streams
// stdout/stderr into the step's log file, waits, and removes the container.
func (s *Steps) runStep(ctx context.Context, runID string, step pipeline.Step, workDir, logDir string, baseEnv map[string]string) (exit int, err error) {
	name := "petal-" + runID + "-" + step.Name

	logFile, err := os.Create(filepath.Join(logDir, step.Name+".log"))
	if err != nil {
		return -1, fmt.Errorf("open log: %w", err)
	}
	defer logFile.Close()
	if net := stepNetworkMode(s.dockerHost); net != "" {
		fmt.Fprintf(logFile, "$ docker run --network %s --workdir /src --env (see events) %s\n", net, step.Image)
	} else {
		fmt.Fprintf(logFile, "$ docker run --workdir /src --env (see events) %s\n", step.Image)
	}

	// Pull first so create/start failures are unambiguous, with progress in
	// the step log (the dind layer cache makes repeat pulls cheap).
	rc, err := s.cli.ImagePull(ctx, step.Image, image.PullOptions{})
	if err != nil {
		return -1, fmt.Errorf("pull %s: %w", step.Image, err)
	}
	_, _ = io.Copy(logFile, rc)
	rc.Close()

	env := make([]string, 0, len(baseEnv)+len(step.Env))
	for k, v := range baseEnv {
		env = append(env, k+"="+v)
	}
	for k, v := range step.Env {
		env = append(env, k+"="+v)
	}
	env = append(env, "PETAL_STEP="+step.Name) // documented in the README; useful with concurrent steps
	mounts := []mount.Mount{{Type: mount.TypeBind, Source: workDir, Target: "/src"}}
	for _, sec := range step.Secrets { // ADR 0006: files, never env; outside /src
		target := pipeline.SecretMountDir + "/" + sec
		mounts = append(mounts, mount.Mount{Type: mount.TypeBind, Source: filepath.Join(s.secretsDir, sec), Target: target, ReadOnly: true})
		env = append(env, pipeline.SecretEnvKey(sec)+"="+target)
	}

	ctr, err := s.cli.ContainerCreate(ctx, &container.Config{
		Image:      step.Image,
		Env:        env,
		Entrypoint: []string{"sh", "-c"},
		Cmd:        []string{"set -eu\n" + step.Run},
		WorkingDir: "/src",
		Labels:     map[string]string{"petal.run": runID, "petal.step": step.Name},
	}, &container.HostConfig{
		NetworkMode: container.NetworkMode(stepNetworkMode(s.dockerHost)),
		Mounts:      mounts,
		AutoRemove:  false,
	}, nil, nil, name)
	if err != nil {
		// A cancel can land while the daemon is creating the container: the
		// container exists but the client only sees ctx's error, so no remove
		// was ever registered. Remove it by name (no-op if it doesn't exist).
		_ = s.cli.ContainerRemove(context.WithoutCancel(ctx), name, container.RemoveOptions{Force: true})
		return -1, fmt.Errorf("create: %w", err)
	}
	defer func() {
		if rmErr := s.cli.ContainerRemove(context.WithoutCancel(ctx), ctr.ID, container.RemoveOptions{Force: true}); rmErr != nil {
			s.log.Warn("remove step container", "run", runID, "step", step.Name, "err", rmErr)
		}
	}()

	attach, err := s.cli.ContainerAttach(ctx, ctr.ID, container.AttachOptions{
		Stream: true, Stdout: true, Stderr: true,
	})
	if err != nil {
		return -1, fmt.Errorf("attach: %w", err)
	}
	defer attach.Close()

	if err := s.cli.ContainerStart(ctx, ctr.ID, container.StartOptions{}); err != nil {
		return -1, fmt.Errorf("start: %w", err)
	}

	// TTY is off, so streams arrive demultiplexed; stdcopy re-merges them.
	// Secrets of the run are masked on the way to disk (ADR 0006).
	out := maskerFrom(ctx).writer(logFile)
	n, _ := stdcopy.StdCopy(out, out, attach.Reader)
	_ = out.Flush()
	attach.CloseWrite()

	statusCh, errCh := s.cli.ContainerWait(ctx, ctr.ID, container.WaitConditionNotRunning)
	select {
	case status := <-statusCh:
		if status.Error != nil {
			return -1, fmt.Errorf("wait: %s", status.Error.Message)
		}
		s.emit(ctx, runID, "step.log", map[string]any{"step": step.Name, "bytes": n})
		return int(status.StatusCode), nil
	case err := <-errCh:
		if ctx.Err() != nil {
			_ = s.cli.ContainerKill(context.WithoutCancel(ctx), ctr.ID, "SIGKILL")
			return -1, nil // caller maps ctx.Err() to canceled
		}
		return -1, fmt.Errorf("wait: %w", err)
	}
}

func (s *Steps) emit(ctx context.Context, runID, kind string, payload map[string]any) {
	raw, _ := json.Marshal(payload)
	// WithoutCancel: the events that matter most (step.finished canceled,
	// step.skipped) are written after the run's context is canceled — on that
	// context the write itself would fail and the audit trail would lose them.
	if err := s.store.AppendEvent(context.WithoutCancel(ctx), runID, kind, string(raw)); err != nil {
		s.log.Error("append event", "run", runID, "kind", kind, "err", err)
	}
	s.log.Info(kind, "run", runID, "payload", string(raw))
}

func parseArgs(argsJSON string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(argsJSON) == "" {
		return out, nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &raw); err != nil {
		return nil, err
	}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("param %q: %w", k, err)
		}
		out[k] = string(b)
	}
	return out, nil
}
