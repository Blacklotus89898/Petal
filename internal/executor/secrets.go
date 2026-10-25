package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Blacklotus89898/Petal/internal/pipeline"
)

// Secrets (ADR 0006): granted per repo+ref in Petal's config, requested per
// step, mounted read-only as files, masked in logs, outputs and errors.

// ConfigureSecrets sets where secret files live (the mounted Kubernetes
// Secret — the same path in the dind container) and what the self repo is
// granted. Unset dir = no secrets: any step that requests one fails its run.
func (s *Steps) ConfigureSecrets(dir string, self []string, selfDocker bool) {
	s.secretsDir, s.selfSecrets, s.selfDocker = dir, self, selfDocker
}

// grant is what one run may use (ADR 0006).
type grant struct {
	secrets []string
	docker  bool
	scope   string // for error messages
}

// grantFor resolves a run's privileges. Repo runs get them only at their
// trusted_refs; the self repo (git-synced main) per the self config.
func (s *Steps) grantFor(repo, ref string) grant {
	if repo == "" {
		return grant{secrets: s.selfSecrets, docker: s.selfDocker, scope: "the self repo"}
	}
	r := s.repos[repo]
	if ref == "" {
		ref = r.DefaultRef
	}
	sec, docker := r.GrantsAt(ref)
	return grant{secrets: sec, docker: docker, scope: fmt.Sprintf("repo %q at ref %q", repo, ref)}
}

// loadSecrets checks every step's requests (docker, secrets) are granted and
// reads the secret values (fresh per run, so a rotation applies without a
// restart). Fails before any step starts.
func (s *Steps) loadSecrets(spec *pipeline.Pipeline, g grant) (map[string]string, error) {
	ok := map[string]bool{}
	for _, name := range g.secrets {
		ok[name] = true
	}
	scope := g.scope
	vals := map[string]string{}
	for _, st := range spec.Steps {
		if st.Docker && !g.docker {
			return nil, fmt.Errorf("step %q requests docker, which %s is not granted (Petal config: docker/trusted_refs)", st.Name, scope)
		}
		for _, name := range st.Secrets {
			if !ok[name] {
				return nil, fmt.Errorf("step %q requests secret %q, which %s is not granted (Petal config: secrets/trusted_refs)", st.Name, name, scope)
			}
			if _, done := vals[name]; done {
				continue
			}
			if s.secretsDir == "" {
				return nil, fmt.Errorf("step %q requests secret %q, but no secrets directory is configured", st.Name, name)
			}
			raw, err := os.ReadFile(filepath.Join(s.secretsDir, name))
			if err != nil {
				return nil, fmt.Errorf("secret %q: not readable (missing from the petal-secrets Secret?)", name)
			}
			v := strings.TrimRight(string(raw), "\r\n")
			if v == "" {
				return nil, fmt.Errorf("secret %q is empty", name)
			}
			vals[name] = v
		}
	}
	return vals, nil
}

type secretsKey struct{}

// withMasker scopes a run's masker to its context, so every step log of the
// run (whichever step requested what) is masked with all of its secrets.
func withMasker(ctx context.Context, m *masker) context.Context {
	return context.WithValue(ctx, secretsKey{}, m)
}

func maskerFrom(ctx context.Context) *masker {
	m, _ := ctx.Value(secretsKey{}).(*masker)
	return m
}

const maskText = "***"

// masker replaces secret values with ***. Longest first, so a secret that
// contains another is masked whole; each line of a multi-line secret (PEM
// keys) is masked too, when long enough not to hit ordinary text.
type masker struct {
	vals [][]byte
	max  int
}

func newMasker(secrets map[string]string) *masker {
	set := map[string]bool{}
	for _, v := range secrets {
		set[v] = true
		if strings.Contains(v, "\n") {
			for _, line := range strings.Split(v, "\n") {
				if line = strings.TrimSpace(line); len(line) >= 8 {
					set[line] = true
				}
			}
		}
	}
	m := &masker{}
	for v := range set {
		m.vals = append(m.vals, []byte(v))
		m.max = max(m.max, len(v))
	}
	sort.Slice(m.vals, func(i, j int) bool { return len(m.vals[i]) > len(m.vals[j]) })
	return m
}

func (m *masker) active() bool { return m != nil && len(m.vals) > 0 }

func (m *masker) replace(b []byte) []byte {
	for _, v := range m.vals {
		b = bytes.ReplaceAll(b, v, []byte(maskText))
	}
	return b
}

// String masks s (outputs, errors).
func (m *masker) String(s string) string {
	if !m.active() {
		return s
	}
	return string(m.replace([]byte(s)))
}

// maskWriter masks a stream. It holds back the last max-1 bytes between
// writes: a secret split across two writes is still caught whole.
type maskWriter struct {
	m    *masker
	w    io.Writer
	pend []byte
}

func (m *masker) writer(w io.Writer) *maskWriter { return &maskWriter{m: m, w: w} }

func (mw *maskWriter) Write(p []byte) (int, error) {
	if !mw.m.active() {
		return mw.w.Write(p)
	}
	out := mw.m.replace(append(mw.pend, p...))
	keep := min(mw.m.max-1, len(out))
	emit := out[:len(out)-keep]
	mw.pend = append([]byte{}, out[len(out)-keep:]...)
	if _, err := mw.w.Write(emit); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Flush writes what is held back; call once the stream has ended.
func (mw *maskWriter) Flush() error {
	if len(mw.pend) == 0 {
		return nil
	}
	_, err := mw.w.Write(mw.m.replace(mw.pend))
	mw.pend = nil
	return err
}
