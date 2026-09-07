// DaggerExecutor runs pipelines for real through the Dagger Go SDK.
//
// Phase B interim (see ADR 0001 consequences): pipelines are Go functions in
// a registry here, expressed with the host SDK. Standalone Dagger modules
// under pipelines/ remain the target form once the dagger CLI is standard in
// the toolchain; GitHub Actions can already call modules via
// dagger-for-github.
package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"dagger.io/dagger"

	"github.com/Blacklotus89898/Petal/internal/runs"
)

// DaggerFunc is one pipeline implementation: it runs to completion and
// returns a human-readable output (image ref, test summary, ...).
type DaggerFunc func(ctx context.Context, client *dagger.Client, run runs.Run) (string, error)

// Dagger executes pipelines via the Dagger engine. The engine connection is
// established per run and closed after; an unavailable engine fails the run
// with a clear message instead of hanging or panicking.
type Dagger struct {
	workdir   string
	log       *slog.Logger
	pipelines map[string]DaggerFunc
}

// NewDagger creates the executor with the default pipeline registry.
// workdir is the source tree pipelines build from (the Petal repo itself for
// the dogfood path).
func NewDagger(workdir string, log *slog.Logger) *Dagger {
	if log == nil {
		log = slog.Default()
	}
	d := &Dagger{workdir: workdir, log: log, pipelines: map[string]DaggerFunc{}}
	d.pipelines["docker-image"] = d.buildDockerImage
	return d
}

// Name implements Executor.
func (d *Dagger) Name() string { return "dagger" }

// Execute implements Executor.
func (d *Dagger) Execute(ctx context.Context, run runs.Run) Result {
	fn, ok := d.pipelines[run.Pipeline]
	if !ok {
		return Result{
			Status: runs.StatusFailed,
			Error:  fmt.Sprintf("unknown pipeline %q (registered: %s)", run.Pipeline, d.registered()),
		}
	}
	client, err := dagger.Connect(ctx, dagger.WithLogOutput(logWriter{d.log}))
	if err != nil {
		return Result{
			Status: runs.StatusFailed,
			Error:  "dagger engine unavailable (is a container runtime running?): " + err.Error(),
		}
	}
	defer client.Close()

	output, err := fn(ctx, client, run)
	if err != nil {
		return Result{Status: runs.StatusFailed, Error: err.Error()}
	}
	return Result{Status: runs.StatusSucceeded, Output: output}
}

func (d *Dagger) registered() string {
	names := make([]string, 0, len(d.pipelines))
	for name := range d.pipelines {
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

// buildDockerImage builds the container image from the workdir and publishes
// it. Default ref is an ephemeral ttl.sh image (no credentials needed);
// override with args {"ref": "..."}.
func (d *Dagger) buildDockerImage(ctx context.Context, client *dagger.Client, run runs.Run) (string, error) {
	ref := fmt.Sprintf("ttl.sh/petal-%s:1h", strings.TrimPrefix(run.ID, "run_"))
	if custom, ok := argString(run, "ref"); ok && custom != "" {
		ref = custom
	}
	src := client.Host().Directory(d.workdir, dagger.HostDirectoryOpts{
		Exclude: []string{".git", "node_modules", "data", "*.exe", "web/dist"},
	})
	imageRef, err := src.DockerBuild().Publish(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("build/publish %s: %w", ref, err)
	}
	return "published " + imageRef, nil
}

func argString(run runs.Run, key string) (string, bool) {
	var args map[string]any
	if err := json.Unmarshal([]byte(run.ArgsJSON), &args); err != nil {
		return "", false
	}
	v, ok := args[key].(string)
	return v, ok
}

// logWriter adapts Dagger SDK progress output into the structured logger.
type logWriter struct{ log *slog.Logger }

func (w logWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line != "" {
			w.log.Info("dagger", "out", line)
		}
	}
	return len(p), nil
}
