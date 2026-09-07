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

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/Blacklotus89898/Petal/internal/pipeline"
	"github.com/Blacklotus89898/Petal/internal/runs"
)

// Steps configures the step-runner.
type Steps struct {
	store      *runs.Store // per-step audit events (step.started / step.finished)
	workdir    string      // source tree (git-synced) copied into each run workspace
	dataDir    string      // PVC root: logs/ and work/ live under here
	dockerHost string      // injected into step containers for image-building steps
	log        *slog.Logger
	cli        *client.Client
	initErr    error
}

// NewSteps creates the executor and its Docker API client. An unreachable
// daemon is not fatal here — runs fail with a clear error instead (same
// behavior the Dagger executor had for engine outages).
func NewSteps(store *runs.Store, workdir, dataDir, dockerHost string, log *slog.Logger) *Steps {
	if log == nil {
		log = slog.Default()
	}
	cli, err := client.NewClientWithOpts(
		client.WithHost(dockerHost),
		client.WithAPIVersionNegotiation(),
	)
	return &Steps{
		store:      store,
		workdir:    workdir,
		dataDir:    dataDir,
		dockerHost: dockerHost,
		log:        log,
		cli:        cli,
		initErr:    err,
	}
}

// Name implements Executor.
func (s *Steps) Name() string { return "steps" }

// Execute implements Executor: load spec → resolve params → copy workspace →
// run steps sequentially (fail-fast) → collect outputs.
func (s *Steps) Execute(ctx context.Context, run runs.Run) Result {
	if s.initErr != nil {
		return Result{Status: runs.StatusFailed, Error: "docker client: " + s.initErr.Error()}
	}

	spec, err := pipeline.Get(s.workdir, run.Pipeline)
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

	logDir := filepath.Join(s.dataDir, "logs", run.ID)
	workDir := filepath.Join(s.dataDir, "work", run.ID)
	defer os.RemoveAll(workDir) // logs stay; workspace is scratch
	for _, d := range []string{logDir, workDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return Result{Status: runs.StatusFailed, Error: "create " + d + ": " + err.Error()}
		}
	}

	// Snapshot the source tree into the per-run workspace so steps can write
	// freely without racing git-sync, and reruns are reproducible per SHA.
	if err := os.CopyFS(workDir, os.DirFS(s.workdir)); err != nil {
		return Result{Status: runs.StatusFailed, Error: "snapshot workspace: " + err.Error()}
	}

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

	outputs := map[string]string{}
	for i, step := range spec.Steps {
		s.emit(ctx, run.ID, "step.started", map[string]any{"step": step.Name, "index": i, "image": step.Image})
		exit, err := s.runStep(ctx, run.ID, step, workDir, logDir, baseEnv)
		if ctx.Err() != nil {
			s.emit(ctx, run.ID, "step.finished", map[string]any{"step": step.Name, "canceled": true})
			return Result{Status: runs.StatusFailed, Error: "canceled"}
		}
		if err != nil || exit != 0 {
			msg := fmt.Sprintf("step %q failed (exit %d)", step.Name, exit)
			if err != nil {
				msg += ": " + err.Error()
			}
			s.emit(ctx, run.ID, "step.finished", map[string]any{"step": step.Name, "exit": exit, "error": msg})
			return Result{Status: runs.StatusFailed, Error: msg}
		}
		s.emit(ctx, run.ID, "step.finished", map[string]any{"step": step.Name, "exit": exit})
	}

	for name, rel := range spec.Outputs {
		raw, err := os.ReadFile(filepath.Join(workDir, filepath.FromSlash(rel)))
		if err != nil {
			return Result{Status: runs.StatusFailed, Error: fmt.Sprintf("output %q: read %s: %v", name, rel, err)}
		}
		outputs[name] = strings.TrimSpace(string(raw))
	}
	out := Result{Status: runs.StatusSucceeded}
	if len(outputs) > 0 {
		payload, _ := json.Marshal(outputs)
		out.Output = string(payload)
	}
	return out
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

	ctr, err := s.cli.ContainerCreate(ctx, &container.Config{
		Image:      step.Image,
		Env:        env,
		Entrypoint: []string{"sh", "-c"},
		Cmd:        []string{"set -eu\n" + step.Run},
		WorkingDir: "/src",
		Labels:     map[string]string{"petal.run": runID, "petal.step": step.Name},
	}, &container.HostConfig{
		NetworkMode: container.NetworkMode(stepNetworkMode(s.dockerHost)),
		Mounts: []mount.Mount{{
			Type:   mount.TypeBind,
			Source: workDir,
			Target: "/src",
		}},
		AutoRemove: false,
	}, nil, nil, name)
	if err != nil {
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
	n, _ := stdcopy.StdCopy(logFile, logFile, attach.Reader)
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
	if err := s.store.AppendEvent(ctx, runID, kind, string(raw)); err != nil {
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
