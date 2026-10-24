package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeLib adds .petal/steps/<name>.yaml files to a repo made by writeRepo.
func writeLib(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, stepsDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const yamllintLib = `
name: yamllint
description: Lint YAML
inputs:
  config: { default: .yamllint }
  strict: { required: true }
image: python:3.12-slim
run: yamllint -c "$INPUT_CONFIG" .
env:
  PIP_QUIET: "1"
timeout: 10m
`

func TestUsesResolvesLibraryStep(t *testing.T) {
	dir := writeRepo(t, map[string]string{"lint.yaml": `
steps:
  - name: lint-a
    uses: yamllint
    with: { strict: "yes" }
  - name: lint-b
    uses: yamllint
    with: { strict: "no", config: other.yaml }
    env: { PIP_QUIET: "0" }
    timeout: 2m
`})
	writeLib(t, dir, map[string]string{"yamllint.yaml": yamllintLib})

	p, err := Get(dir, "lint")
	if err != nil {
		t.Fatal(err)
	}
	a, b := p.Steps[0], p.Steps[1]
	if a.Image != "python:3.12-slim" || !strings.Contains(a.Run, "$INPUT_CONFIG") {
		t.Errorf("lint-a not resolved: %+v", a)
	}
	if a.Env["INPUT_CONFIG"] != ".yamllint" || a.Env["INPUT_STRICT"] != "yes" || a.Env["PIP_QUIET"] != "1" {
		t.Errorf("lint-a env = %v", a.Env)
	}
	if a.TimeoutDur != 10*time.Minute {
		t.Errorf("lint-a should inherit the library timeout, got %v", a.TimeoutDur)
	}
	// Same library step twice: keyed by the pipeline step's name; step env and
	// timeout override the library's.
	if b.Env["INPUT_CONFIG"] != "other.yaml" || b.Env["PIP_QUIET"] != "0" || b.TimeoutDur != 2*time.Minute {
		t.Errorf("lint-b = env %v timeout %v", b.Env, b.TimeoutDur)
	}
	if p.DAG || p.MaxParallel != DefaultMaxParallel || p.TimeoutDur != DefaultPipelineTimeout {
		t.Errorf("defaults: dag=%v max_parallel=%d timeout=%v", p.DAG, p.MaxParallel, p.TimeoutDur)
	}
}

func TestUsesRejects(t *testing.T) {
	cases := map[string]struct{ pipeline, lib string }{
		"unknown step":      {"steps:\n  - {name: a, uses: nope}\n", ""},
		"path traversal":    {"steps:\n  - {name: a, uses: ../../etc}\n", ""},
		"uses + image":      {"steps:\n  - {name: a, uses: yamllint, image: alpine:3.21, with: {strict: x}}\n", yamllintLib},
		"unknown input":     {"steps:\n  - {name: a, uses: yamllint, with: {strict: x, color: red}}\n", yamllintLib},
		"missing required":  {"steps:\n  - {name: a, uses: yamllint}\n", yamllintLib},
		"with without uses": {"steps:\n  - {name: a, image: alpine:3.21, run: 'true', with: {x: y}}\n", ""},
		"nested uses":       {"steps:\n  - {name: a, uses: yamllint, with: {strict: x}}\n", yamllintLib + "uses: other\n"},
		"unpinned library":  {"steps:\n  - {name: a, uses: yamllint, with: {strict: x}}\n", strings.Replace(yamllintLib, "python:3.12-slim", "python:latest", 1)},
		"bad step timeout":  {"steps:\n  - {name: a, image: alpine:3.21, run: 'true', timeout: forever}\n", ""},
	}
	for name, c := range cases {
		dir := writeRepo(t, map[string]string{"p.yaml": c.pipeline})
		if c.lib != "" {
			writeLib(t, dir, map[string]string{"yamllint.yaml": c.lib})
		}
		if _, err := Get(dir, "p"); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: expected ErrInvalid, got %v", name, err)
		}
	}
}

func TestNeedsModes(t *testing.T) {
	dir := writeRepo(t, map[string]string{
		"legacy.yaml": "steps:\n  - {name: a, image: alpine:3.21, run: 'true'}\n  - {name: b, image: alpine:3.21, run: 'true'}\n",
		// Listed out of execution order: only needs decide order.
		"dag.yaml": `
max_parallel: 3
timeout: 2h
steps:
  - {name: deploy, image: alpine:3.21, run: 'true', needs: [lint, test]}
  - {name: lint, image: alpine:3.21, run: 'true', needs: []}
  - {name: test, image: alpine:3.21, run: 'true', needs: []}
`,
	})
	legacy, err := Get(dir, "legacy")
	if err != nil || legacy.DAG {
		t.Fatalf("legacy: (%+v, %v) — no needs anywhere must stay file-order", legacy, err)
	}
	dag, err := Get(dir, "dag")
	if err != nil {
		t.Fatal(err)
	}
	if !dag.DAG || dag.MaxParallel != 3 || dag.TimeoutDur != 2*time.Hour || dag.Timeout != "2h0m0s" {
		t.Errorf("dag = dag:%v max_parallel:%d timeout:%v", dag.DAG, dag.MaxParallel, dag.TimeoutDur)
	}
}

func TestNeedsRejects(t *testing.T) {
	cases := map[string]string{
		"mixed":          "steps:\n  - {name: a, image: alpine:3.21, run: 'true', needs: []}\n  - {name: b, image: alpine:3.21, run: 'true'}\n",
		"unknown":        "steps:\n  - {name: a, image: alpine:3.21, run: 'true', needs: [ghost]}\n",
		"self":           "steps:\n  - {name: a, image: alpine:3.21, run: 'true', needs: [a]}\n",
		"duplicate":      "steps:\n  - {name: a, image: alpine:3.21, run: 'true', needs: []}\n  - {name: b, image: alpine:3.21, run: 'true', needs: [a, a]}\n",
		"cycle":          "steps:\n  - {name: a, image: alpine:3.21, run: 'true', needs: [b]}\n  - {name: b, image: alpine:3.21, run: 'true', needs: [a]}\n",
		"parallel > cap": "max_parallel: 9\nsteps:\n  - {name: a, image: alpine:3.21, run: 'true', needs: []}\n",
	}
	for name, body := range cases {
		dir := writeRepo(t, map[string]string{"p.yaml": body})
		if _, err := Get(dir, "p"); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: expected ErrInvalid, got %v", name, err)
		}
	}
}
