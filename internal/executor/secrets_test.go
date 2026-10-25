package executor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blacklotus89898/Petal/internal/pipeline"
	"github.com/Blacklotus89898/Petal/internal/repos"
	"github.com/Blacklotus89898/Petal/internal/runs"
)

const canary = "s3cr3t-canary-value"

func TestMaskWriterMasksWholeAndSplitValues(t *testing.T) {
	m := newMasker(map[string]string{"a": canary, "short": "tok"})
	var buf bytes.Buffer
	w := m.writer(&buf)
	in := "start " + canary + " mid tok " + canary + "\nend"
	for i := 0; i < len(in); i++ { // byte by byte: every split point
		if _, err := w.Write([]byte{in[i]}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), "start *** mid *** ***\nend"; got != want {
		t.Errorf("masked = %q, want %q", got, want)
	}
}

func TestMaskerLongestFirstAndMultiline(t *testing.T) {
	pem := "-----BEGIN KEY-----\nAAAABBBBCCCCDDDD\n-----END KEY-----"
	m := newMasker(map[string]string{"outer": "abc-" + canary, "inner": canary, "pem": pem})
	if got := m.String("x abc-" + canary + " y"); got != "x *** y" {
		t.Errorf("longest-first: %q", got)
	}
	if got := m.String("leaked line AAAABBBBCCCCDDDD"); strings.Contains(got, "AAAABBBB") {
		t.Errorf("multi-line secret's line leaked: %q", got)
	}
	var none *masker
	if none.String("plain") != "plain" {
		t.Error("nil masker must pass through")
	}
}

// secretsHarness: a repo run whose fake checkout lays down the pipeline, and
// a step that writes the secret into an output file (outputs are masked).
type secretsHarness struct {
	store *runs.Store
	steps *Steps
	ran   []pipeline.Step
}

func newSecretsHarness(t *testing.T, pipelineYAML string) *secretsHarness {
	t.Helper()
	store, err := runs.Open(context.Background(), filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	secDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(secDir, "e2e-canary"), []byte(canary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	allow := repos.Allowlist{"homelab": {Alias: "homelab", URL: "https://example.invalid/h.git", DefaultRef: "main",
		Secrets: []string{"e2e-canary", "absent"}, SecretRefs: []string{"main"}}}
	s := NewSteps(store, t.TempDir(), t.TempDir(), "tcp://127.0.0.1:1", allow, nil)
	s.ConfigureSecrets(secDir, nil)
	h := &secretsHarness{store: store, steps: s}
	s.runner = func(_ context.Context, _ string, st pipeline.Step, workDir, _ string, _ map[string]string) (int, error) {
		if st.Name == pipeline.CheckoutStep {
			dir := filepath.Join(workDir, ".petal", "pipelines")
			_ = os.MkdirAll(dir, 0o755)
			_ = os.MkdirAll(filepath.Join(workDir, ".git"), 0o755)
			_ = os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(pipelineYAML), 0o644)
			_ = os.WriteFile(filepath.Join(workDir, ".git", "petal-resolved-sha"), []byte("abc123"), 0o644)
			return 0, nil
		}
		h.ran = append(h.ran, st)
		_ = os.WriteFile(filepath.Join(workDir, "out.txt"), []byte("token="+canary), 0o644)
		return 0, nil
	}
	return h
}

func (h *secretsHarness) run(t *testing.T, ref string) Result {
	t.Helper()
	run, err := h.store.Add(context.Background(), runs.NewRun{Pipeline: "p", TriggerKind: "api", Repo: "homelab", Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	return h.steps.Execute(context.Background(), *run)
}

const secretPipeline = `steps:
  - name: push
    image: alpine:3.21
    run: cat "$PETAL_SECRET_E2E_CANARY_FILE"
    secrets: [e2e-canary]
outputs:
  out: out.txt
`

func TestGrantedSecretAtProtectedRef(t *testing.T) {
	h := newSecretsHarness(t, secretPipeline)
	res := h.run(t, "") // default ref = main
	if res.Status != runs.StatusSucceeded {
		t.Fatalf("run = %+v", res)
	}
	if len(h.ran) != 1 || len(h.ran[0].Secrets) != 1 || h.ran[0].Secrets[0] != "e2e-canary" {
		t.Errorf("step should carry its requested secret, got %+v", h.ran)
	}
	if strings.Contains(res.Output, canary) || !strings.Contains(res.Output, "token=***") {
		t.Errorf("output must be masked, got %q", res.Output)
	}
}

func TestSecretsRefusedOffProtectedRefs(t *testing.T) {
	for _, ref := range []string{"feature/x", "v1.0.0", "0123456789abcdef0123456789abcdef01234567"} {
		h := newSecretsHarness(t, secretPipeline)
		res := h.run(t, ref)
		if res.Status != runs.StatusFailed || !strings.Contains(res.Error, `not granted`) || !strings.Contains(res.Error, ref) {
			t.Errorf("ref %s: want refusal naming the ref, got %+v", ref, res)
		}
		if len(h.ran) != 0 {
			t.Errorf("ref %s: no step may start when a grant is missing, ran %d", ref, len(h.ran))
		}
	}
}

func TestUngrantedAndMissingSecrets(t *testing.T) {
	h := newSecretsHarness(t, strings.Replace(secretPipeline, "[e2e-canary]", "[ghcr-push]", 1))
	if res := h.run(t, "main"); res.Status != runs.StatusFailed || !strings.Contains(res.Error, `secret "ghcr-push"`) {
		t.Errorf("ungranted: %+v", res)
	}
	h = newSecretsHarness(t, strings.Replace(secretPipeline, "[e2e-canary]", "[absent]", 1))
	if res := h.run(t, "main"); res.Status != runs.StatusFailed || !strings.Contains(res.Error, "not readable") {
		t.Errorf("granted but missing file: %+v", res)
	}
	if len(h.ran) != 0 {
		t.Error("no step may start when a secret is missing")
	}
}

func TestStepsWithoutSecretsNeedNoGrant(t *testing.T) {
	h := newSecretsHarness(t, "steps:\n  - name: plain\n    image: alpine:3.21\n    run: true\n")
	if res := h.run(t, "feature/x"); res.Status != runs.StatusSucceeded {
		t.Errorf("a pipeline without secrets runs at any ref: %+v", res)
	}
}
