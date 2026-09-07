package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, pipelinesDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const validYAML = `
name: docker-image
description: Build and publish the image
params:
  ref:
    default: ttl.sh/petal-${PETAL_RUN_ID}:1h
  channel:
    required: true
steps:
  - name: build
    image: docker:27-cli
    run: |
      docker buildx build --push -t "$PARAM_REF" /src
  - name: report
    image: alpine:3.21
    run: echo done
outputs:
  image-ref: .petal-out/image-ref.txt
`

func TestListAndGet(t *testing.T) {
	dir := writeRepo(t, map[string]string{
		"docker-image.yaml": validYAML,
		"test.yml":          "steps:\n  - name: s\n    image: golang:1.26\n    run: go test\n",
		"ignored.txt":       "steps: []",
	})

	all, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 pipelines, got %d: %v", len(all), all)
	}
	if all[0].Name != "docker-image" || all[1].Name != "test" {
		t.Fatalf("want sorted [docker-image test], got [%s %s]", all[0].Name, all[1].Name)
	}

	p, err := Get(dir, "docker-image")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if p.Description != "Build and publish the image" {
		t.Errorf("description = %q", p.Description)
	}
	if len(p.Steps) != 2 || p.Steps[0].Name != "build" || p.Steps[0].Image != "docker:27-cli" {
		t.Errorf("steps = %+v", p.Steps)
	}

	if _, err := Get(dir, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestListMissingDir(t *testing.T) {
	all, err := List(t.TempDir())
	if err != nil || all != nil {
		t.Errorf("want (nil, nil), got (%v, %v)", all, err)
	}
}

func TestInvalidSpecs(t *testing.T) {
	cases := map[string]string{
		"unknown field": "steeps:\n  - name: a\n    image: alpine:3\n    run: x\n",
		"no steps":      "name: empty\n",
		"bad name":      "name: Bad_Name\nsteps:\n  - name: a\n    image: alpine:3\n    run: x\n",
		"dup step":      "steps:\n  - name: a\n    image: alpine:3\n    run: x\n  - name: a\n    image: alpine:3\n    run: y\n",
		"step bad name": "steps:\n  - name: \"\"\n    image: alpine:3\n    run: x\n",
		"latest image":  "steps:\n  - name: a\n    image: alpine:latest\n    run: x\n",
		"empty image":   "steps:\n  - name: a\n    run: x\n",
		"image w/ args": "steps:\n  - name: a\n    image: \"alpine:3 sh -c x\"\n    run: x\n",
		"empty run":     "steps:\n  - name: a\n    image: alpine:3\n    run: \"  \"\n",
		"abs output":    "steps:\n  - name: a\n    image: alpine:3\n    run: x\noutputs:\n  o: /etc/passwd\n",
		"escape output": "steps:\n  - name: a\n    image: alpine:3\n    run: x\noutputs:\n  o: ../secrets.txt\n",
		"bad param":     "params:\n  \"Bad Param\": {}\nsteps:\n  - name: a\n    image: alpine:3\n    run: x\n",
		"bad yaml":      "steps: [",
		"bad output id": "steps:\n  - name: a\n    image: alpine:3\n    run: x\noutputs:\n  \"!!\": o.txt\n",
	}

	for label, yaml := range cases {
		dir := writeRepo(t, map[string]string{"broken.yaml": yaml})
		_, err := List(dir)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", label, err)
		}
	}
}

func TestResolveParams(t *testing.T) {
	dir := writeRepo(t, map[string]string{"p.yaml": validYAML})
	p, err := Get(dir, "docker-image")
	if err != nil {
		t.Fatal(err)
	}

	res, err := p.ResolveParams(map[string]string{"channel": "dev"}, "run123")
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if res["ref"] != "ttl.sh/petal-run123:1h" {
		t.Errorf("default ref not substituted: %q", res["ref"])
	}
	if res["channel"] != "dev" {
		t.Errorf("required param not passed through: %q", res["channel"])
	}

	res, err = p.ResolveParams(map[string]string{"ref": "ghcr.io/x:1", "channel": "dev"}, "r")
	if err != nil {
		t.Fatalf("explicit: %v", err)
	}
	if res["ref"] != "ghcr.io/x:1" || res["channel"] != "dev" {
		t.Errorf("explicit args lost: %v", res)
	}

	if _, err := p.ResolveParams(map[string]string{"bogus": "1"}, "r"); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown param: want ErrInvalid, got %v", err)
	}
}

func TestResolveRequiredParam(t *testing.T) {
	dir := writeRepo(t, map[string]string{
		"p.yaml": "params:\n  tag:\n    required: true\nsteps:\n  - name: a\n    image: alpine:3\n    run: x\n",
	})
	p, err := Get(dir, "p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ResolveParams(nil, "r"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "required") {
		t.Errorf("missing required: want ErrInvalid w/ message, got %v", err)
	}
	if _, err := p.ResolveParams(map[string]string{"tag": "v1"}, "r"); err != nil {
		t.Errorf("provided required: %v", err)
	}
}

func TestEnvKey(t *testing.T) {
	for in, want := range map[string]string{
		"ref":         "PARAM_REF",
		"image-tag":   "PARAM_IMAGE_TAG",
		"build.flags": "PARAM_BUILD_FLAGS",
	} {
		if got := EnvKey(in); got != want {
			t.Errorf("EnvKey(%q) = %q, want %q", in, got, want)
		}
	}
}
