// Package pipeline loads and validates pipeline definitions from a
// repository's .petal/pipelines directory. Pipelines are repo-native YAML —
// any repository gets pipelines by committing files there. See
// docs/adr/0002-step-runner.md for the spec and rationale.
package pipeline

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Errors returned by the loader; test with errors.Is.
var (
	ErrNotFound = errors.New("pipeline not found")
	ErrInvalid  = errors.New("invalid pipeline")
)

// pipelinesDir is the repo-relative directory scanned for pipeline files.
const pipelinesDir = ".petal/pipelines"

// ValidName reports whether s is a valid pipeline/step/param identifier:
// lowercase kebab-case (letters, digits, - _ .), ≤64 chars, no leading
// separator. Exported because log-file serving uses it as its path-safety gate.
func ValidName(s string) bool { return validName(s) }

// nameRe matches valid pipeline/step/param identifiers: lowercase kebab-case.
// (Kept as a plain check to avoid a regexp dep in the hot path.)
func validName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// Param is a typed trigger argument with an optional default.
type Param struct {
	Default  string `yaml:"default,omitempty" json:"default,omitempty"`
	Required bool   `yaml:"required,omitempty" json:"required,omitempty"`
}

// Step is one container execution in a pipeline.
type Step struct {
	Name  string            `yaml:"name" json:"name"`
	Image string            `yaml:"image" json:"image"`
	Run   string            `yaml:"run" json:"run"`
	Env   map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
}

// Pipeline is a validated pipeline definition.
type Pipeline struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Params      map[string]Param `json:"params,omitempty"`
	Steps       []Step           `json:"steps"`
	// Outputs maps output names to workspace-relative file paths the steps
	// must produce; contents become the run's structured output event.
	Outputs map[string]string `json:"outputs,omitempty"`
}

// fileSpec mirrors the YAML on disk; unknown fields are rejected so typos
// fail loudly instead of silently changing behavior.
type fileSpec struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Params      map[string]Param  `yaml:"params"`
	Steps       []Step            `yaml:"steps"`
	Outputs     map[string]string `yaml:"outputs"`
}

func (s *fileSpec) validate(defaultName string) (*Pipeline, error) {
	p := &Pipeline{
		Name:        s.Name,
		Description: s.Description,
		Params:      s.Params,
		Steps:       s.Steps,
		Outputs:     s.Outputs,
	}
	if p.Name == "" {
		p.Name = defaultName
	}
	if !validName(p.Name) {
		return nil, fmt.Errorf("%w: bad pipeline name %q", ErrInvalid, p.Name)
	}
	if len(p.Steps) == 0 {
		return nil, fmt.Errorf("%w: pipeline %q has no steps", ErrInvalid, p.Name)
	}
	seen := map[string]bool{}
	for i, st := range p.Steps {
		if !validName(st.Name) {
			return nil, fmt.Errorf("%w: step %d bad name %q", ErrInvalid, i, st.Name)
		}
		if seen[st.Name] {
			return nil, fmt.Errorf("%w: duplicate step name %q", ErrInvalid, st.Name)
		}
		seen[st.Name] = true
		if st.Image == "" || strings.ContainsAny(st.Image, " \t\n") || strings.EqualFold(filepath.Ext(st.Image), ".latest") || strings.HasSuffix(st.Image, ":latest") {
			return nil, fmt.Errorf("%w: step %q image must be a pinned reference, got %q", ErrInvalid, st.Name, st.Image)
		}
		if strings.TrimSpace(st.Run) == "" {
			return nil, fmt.Errorf("%w: step %q has empty run", ErrInvalid, st.Name)
		}
	}
	for name, path := range p.Outputs {
		if !validName(name) {
			return nil, fmt.Errorf("%w: bad output name %q", ErrInvalid, name)
		}
		// Workspace paths are POSIX (containers run Linux), so check the
		// slash form rather than filepath.IsAbs, which is Windows-host-dependent.
		slash := filepath.ToSlash(path)
		if strings.HasPrefix(slash, "/") || strings.HasPrefix(slash, "../") || slash == ".." {
			return nil, fmt.Errorf("%w: output %q path must be workspace-relative, got %q", ErrInvalid, name, path)
		}
	}
	for name := range p.Params {
		if !validName(name) {
			return nil, fmt.Errorf("%w: bad param name %q", ErrInvalid, name)
		}
	}
	return p, nil
}

// List loads every pipeline in workdir/.petal/pipelines, sorted by name.
// A missing directory is not an error — a repo may simply have no pipelines.
func List(workdir string) ([]*Pipeline, error) {
	entries, err := os.ReadDir(filepath.Join(workdir, pipelinesDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Pipeline
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || (filepath.Ext(name) != ".yaml" && filepath.Ext(name) != ".yml") {
			continue
		}
		p, err := loadFile(filepath.Join(workdir, pipelinesDir, name))
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get loads a single pipeline by name (filename stem or explicit name field).
func Get(workdir, name string) (*Pipeline, error) {
	all, err := List(workdir)
	if err != nil {
		return nil, err
	}
	for _, p := range all {
		if p.Name == name {
			return p, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
}

func loadFile(path string) (*Pipeline, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s fileSpec
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrInvalid, filepath.Base(path), err)
	}
	return s.validate(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
}

// ResolveParams validates trigger args against the pipeline's params: unknown
// args are rejected, defaults are applied, ${PETAL_RUN_ID} is substituted, and
// required params must end up non-empty.
func (p *Pipeline) ResolveParams(args map[string]string, runID string) (map[string]string, error) {
	for k := range args {
		if _, ok := p.Params[k]; !ok {
			return nil, fmt.Errorf("%w: unknown param %q for pipeline %q", ErrInvalid, k, p.Name)
		}
	}
	out := make(map[string]string, len(p.Params))
	for k, pa := range p.Params {
		v, ok := args[k]
		if !ok {
			v = pa.Default
		}
		v = strings.ReplaceAll(v, "${PETAL_RUN_ID}", runID)
		if v == "" && pa.Required {
			return nil, fmt.Errorf("%w: missing required param %q for pipeline %q", ErrInvalid, k, p.Name)
		}
		out[k] = v
	}
	return out, nil
}

// EnvKey is the environment variable name a param is injected as inside steps.
func EnvKey(param string) string {
	return "PARAM_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(param))
}
