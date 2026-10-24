package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Blacklotus89898/Petal/internal/runs"
)

func TestStepNetworkMode(t *testing.T) {
	cases := []struct {
		host string
		want string
	}{
		{"tcp://127.0.0.1:2375", "host"},    // DinD sidecar: loopback must resolve
		{"tcp://localhost:2375", "host"},    // same, by name
		{"unix:///var/run/docker.sock", ""}, // socket daemon: default bridge is right
		{"tcp://10.0.0.5:2375", ""},         // remote daemon: default bridge is right
		{"", ""},                            // unset: default bridge
		{"not a url %", ""},                 // unparseable: fail open to default
	}
	for _, c := range cases {
		if got := stepNetworkMode(c.host); got != c.want {
			t.Errorf("stepNetworkMode(%q) = %q, want %q", c.host, got, c.want)
		}
	}
}

func TestParseArgs(t *testing.T) {
	got, err := parseArgs(`{"ref":"ghcr.io/x:1","count":3,"flag":true}`)
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if got["ref"] != "ghcr.io/x:1" {
		t.Errorf("ref = %q", got["ref"])
	}
	if got["count"] != "3" || got["flag"] != "true" {
		t.Errorf("non-string params should JSON-encode: %v", got)
	}

	empty, err := parseArgs("")
	if err != nil || len(empty) != 0 {
		t.Errorf("empty args: (%v, %v)", empty, err)
	}
	if _, err := parseArgs("{nope"); err == nil {
		t.Error("bad JSON should error")
	}
}

func TestExecuteWithoutDocker(t *testing.T) {
	// Exercises the docker-free portion of Execute end to end: with an
	// unreachable docker host the run must fail cleanly and quickly, never
	// hang, and record step events in the registry along the way.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	store, err := runs.Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	workdir := t.TempDir()
	pipelinesDir := filepath.Join(workdir, ".petal", "pipelines")
	if err := os.MkdirAll(pipelinesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := "steps:\n  - name: hello\n    image: alpine:3.21\n    run: echo hi\n"
	if err := os.WriteFile(filepath.Join(pipelinesDir, "hello.yaml"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()

	// tcp://127.0.0.1:1 refuses immediately — no long connect timeouts.
	s := NewSteps(store, workdir, dataDir, "tcp://127.0.0.1:1", nil, nil)

	run, err := store.Enqueue(ctx, "hello", "", `{}`, "manual", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	res := s.Execute(ctx, *run)
	if res.Status != runs.StatusFailed {
		t.Errorf("status = %q (want failed), err=%q", res.Status, res.Error)
	}
	if !strings.Contains(res.Error, "pull") && !strings.Contains(res.Error, "connect") {
		t.Errorf("error should mention the docker failure, got %q", res.Error)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "work")); statErr != nil {
		t.Errorf("run workspace should exist under dataDir/work: %v", statErr)
	}

	run, err = store.Enqueue(ctx, "missing", "", `{}`, "manual", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	res = s.Execute(ctx, *run)
	if res.Status != runs.StatusFailed || !strings.Contains(res.Error, "not found") {
		t.Errorf("unknown pipeline: (%q, %q)", res.Status, res.Error)
	}
}
