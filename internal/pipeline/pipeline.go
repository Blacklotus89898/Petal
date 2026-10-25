// Package pipeline loads and validates pipeline definitions from a
// repository's .petal/pipelines directory. Pipelines are repo-native YAML —
// any repository gets pipelines by committing files there. Steps may come
// from the repo's step library (.petal/steps, `uses:`) and may form a DAG
// (`needs:`). See docs/adr/0002-step-runner.md and 0005-modular-dag-steps.md.
package pipeline

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Errors returned by the loader; test with errors.Is.
var (
	ErrNotFound = errors.New("pipeline not found")
	ErrInvalid  = errors.New("invalid pipeline")
)

const (
	// pipelinesDir is the repo-relative directory scanned for pipeline files.
	pipelinesDir = ".petal/pipelines"
	// stepsDir holds the repo's reusable step library (ADR 0005).
	stepsDir = ".petal/steps"

	// Defaults and limits (ADR 0005).
	DefaultStepTimeout     = 30 * time.Minute
	DefaultPipelineTimeout = 60 * time.Minute
	maxTimeout             = 24 * time.Hour
	DefaultMaxParallel     = 2
	MaxParallelCap         = 4 // two workers share one DinD daemon
)

// CheckoutStep is the reserved name of the synthetic first step that clones a
// run's repo (ADR 0004); its log is served like any other step's.
const CheckoutStep = "petal-checkout"

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

// Param is a typed trigger argument (or library-step input) with an optional
// default.
type Param struct {
	Default  string `yaml:"default,omitempty" json:"default,omitempty"`
	Required bool   `yaml:"required,omitempty" json:"required,omitempty"`
}

// Step is one container execution in a pipeline. After loading, a `uses`
// step is resolved: Image/Run come from the library, inputs are in Env.
type Step struct {
	Name  string            `yaml:"name" json:"name"`
	Uses  string            `yaml:"uses,omitempty" json:"uses,omitempty"`
	With  map[string]string `yaml:"with,omitempty" json:"with,omitempty"`
	Image string            `yaml:"image,omitempty" json:"image"`
	Run   string            `yaml:"run,omitempty" json:"run"`
	Env   map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	// Needs: nil = not declared (file-order mode); empty = a DAG root.
	Needs   []string `yaml:"needs" json:"needs,omitempty"`
	Timeout string   `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	// Secrets the step requests (names only); granted per repo+ref in
	// Petal's config and mounted as files (ADR 0006).
	Secrets []string `yaml:"secrets,omitempty" json:"secrets,omitempty"`

	TimeoutDur time.Duration `yaml:"-" json:"-"`
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
	// DAG is true when steps declare `needs` (order = dependencies, up to
	// MaxParallel at once); false = file order, one step at a time.
	DAG         bool   `json:"dag"`
	MaxParallel int    `json:"max_parallel"`
	Timeout     string `json:"timeout"`

	TimeoutDur time.Duration `json:"-"`
}

// fileSpec mirrors the YAML on disk; unknown fields are rejected so typos
// fail loudly instead of silently changing behavior.
type fileSpec struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Params      map[string]Param  `yaml:"params"`
	Steps       []Step            `yaml:"steps"`
	Outputs     map[string]string `yaml:"outputs"`
	MaxParallel int               `yaml:"max_parallel"`
	Timeout     string            `yaml:"timeout"`
}

// libStep is a reusable step in .petal/steps/<name>.yaml. It has no `uses`
// field, so nesting is impossible (KnownFields rejects it).
type libStep struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Inputs      map[string]Param  `yaml:"inputs"`
	Image       string            `yaml:"image"`
	Run         string            `yaml:"run"`
	Env         map[string]string `yaml:"env"`
	Timeout     string            `yaml:"timeout"`
	Secrets     []string          `yaml:"secrets"`
}

func pinned(image string) bool {
	return image != "" && !strings.ContainsAny(image, " \t\n") &&
		!strings.EqualFold(filepath.Ext(image), ".latest") && !strings.HasSuffix(image, ":latest")
}

func parseTimeout(raw string, def time.Duration) (time.Duration, error) {
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 || d > maxTimeout {
		return 0, fmt.Errorf("timeout %q must be a duration between 1s and 24h (e.g. 10m)", raw)
	}
	return d, nil
}

// library lazily loads .petal/steps for one List/Get call.
type library struct {
	dir   string
	cache map[string]*libStep
}

func (l *library) get(name string) (*libStep, error) {
	if ls, ok := l.cache[name]; ok {
		return ls, nil
	}
	path := filepath.Join(l.dir, name+".yaml")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: unknown step %q (no %s/%s.yaml)", ErrInvalid, name, stepsDir, name)
	}
	if err != nil {
		return nil, err
	}
	var ls libStep
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&ls); err != nil {
		return nil, fmt.Errorf("%w: step library %s.yaml: %v", ErrInvalid, name, err)
	}
	if ls.Name != "" && ls.Name != name {
		return nil, fmt.Errorf("%w: step library %s.yaml declares name %q", ErrInvalid, name, ls.Name)
	}
	if !pinned(ls.Image) {
		return nil, fmt.Errorf("%w: step library %q image must be a pinned reference, got %q", ErrInvalid, name, ls.Image)
	}
	if strings.TrimSpace(ls.Run) == "" {
		return nil, fmt.Errorf("%w: step library %q has empty run", ErrInvalid, name)
	}
	for in := range ls.Inputs {
		if !validName(in) {
			return nil, fmt.Errorf("%w: step library %q: bad input name %q", ErrInvalid, name, in)
		}
	}
	if _, err := parseTimeout(ls.Timeout, DefaultStepTimeout); err != nil {
		return nil, fmt.Errorf("%w: step library %q: %v", ErrInvalid, name, err)
	}
	l.cache[name] = &ls
	return &ls, nil
}

// resolveUses fills a `uses` step from the library: image/run, merged env
// (library env < step env < INPUT_*), and the timeout (step overrides library).
func resolveUses(st *Step, lib *library) error {
	if !validName(st.Uses) {
		return fmt.Errorf("%w: step %q: bad uses %q", ErrInvalid, st.Name, st.Uses)
	}
	if st.Image != "" || st.Run != "" {
		return fmt.Errorf("%w: step %q: uses is exclusive with image/run", ErrInvalid, st.Name)
	}
	ls, err := lib.get(st.Uses)
	if err != nil {
		return err
	}
	for k := range st.With {
		if _, ok := ls.Inputs[k]; !ok {
			return fmt.Errorf("%w: step %q: %q is not an input of %q", ErrInvalid, st.Name, k, st.Uses)
		}
	}
	env := map[string]string{}
	for k, v := range ls.Env {
		env[k] = v
	}
	for k, v := range st.Env {
		env[k] = v
	}
	for name, in := range ls.Inputs {
		v, ok := st.With[name]
		if !ok {
			v = in.Default
		}
		if v == "" && in.Required {
			return fmt.Errorf("%w: step %q: missing required input %q of %q", ErrInvalid, st.Name, name, st.Uses)
		}
		env[InputEnvKey(name)] = v
	}
	st.Image, st.Run, st.Env = ls.Image, ls.Run, env
	st.Secrets = mergeNames(ls.Secrets, st.Secrets)
	if st.Timeout == "" {
		st.Timeout = ls.Timeout
	}
	return nil
}

func (s *fileSpec) validate(defaultName string, lib *library) (*Pipeline, error) {
	p := &Pipeline{
		Name:        s.Name,
		Description: s.Description,
		Params:      s.Params,
		Steps:       s.Steps,
		Outputs:     s.Outputs,
		MaxParallel: s.MaxParallel,
		Timeout:     s.Timeout,
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
	declared := 0
	for i := range p.Steps {
		st := &p.Steps[i]
		if !validName(st.Name) {
			return nil, fmt.Errorf("%w: step %d bad name %q", ErrInvalid, i, st.Name)
		}
		if seen[st.Name] {
			return nil, fmt.Errorf("%w: duplicate step name %q", ErrInvalid, st.Name)
		}
		if st.Name == CheckoutStep {
			return nil, fmt.Errorf("%w: step name %q is reserved for Petal's source checkout", ErrInvalid, st.Name)
		}
		seen[st.Name] = true
		if st.Uses != "" {
			if err := resolveUses(st, lib); err != nil {
				return nil, err
			}
		} else if len(st.With) > 0 {
			return nil, fmt.Errorf("%w: step %q: with needs uses", ErrInvalid, st.Name)
		}
		if !pinned(st.Image) {
			return nil, fmt.Errorf("%w: step %q image must be a pinned reference, got %q", ErrInvalid, st.Name, st.Image)
		}
		if strings.TrimSpace(st.Run) == "" {
			return nil, fmt.Errorf("%w: step %q has empty run", ErrInvalid, st.Name)
		}
		d, err := parseTimeout(st.Timeout, DefaultStepTimeout)
		if err != nil {
			return nil, fmt.Errorf("%w: step %q: %v", ErrInvalid, st.Name, err)
		}
		st.TimeoutDur = d
		if err := checkSecretNames(st.Secrets); err != nil {
			return nil, fmt.Errorf("%w: step %q: %v", ErrInvalid, st.Name, err)
		}
		if st.Needs != nil {
			declared++
		}
	}

	// Order: file order (no needs anywhere) or a DAG (needs on every step).
	switch {
	case declared == 0:
		p.DAG = false
	case declared == len(p.Steps):
		p.DAG = true
		if err := checkDAG(p.Steps); err != nil {
			return nil, fmt.Errorf("%w: pipeline %q: %v", ErrInvalid, p.Name, err)
		}
	default:
		return nil, fmt.Errorf("%w: pipeline %q: either every step declares needs (needs: [] for roots) or none does — %d of %d do",
			ErrInvalid, p.Name, declared, len(p.Steps))
	}

	switch {
	case p.MaxParallel == 0:
		p.MaxParallel = DefaultMaxParallel
	case p.MaxParallel < 0 || p.MaxParallel > MaxParallelCap:
		return nil, fmt.Errorf("%w: pipeline %q: max_parallel must be 1..%d", ErrInvalid, p.Name, MaxParallelCap)
	}
	d, err := parseTimeout(p.Timeout, DefaultPipelineTimeout)
	if err != nil {
		return nil, fmt.Errorf("%w: pipeline %q: %v", ErrInvalid, p.Name, err)
	}
	p.TimeoutDur, p.Timeout = d, d.String()

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

// mergeNames returns a ∪ b, a's order first, without duplicates.
func mergeNames(a, b []string) []string {
	if len(a) == 0 {
		return b
	}
	out := append([]string{}, a...)
	for _, n := range b {
		dup := false
		for _, m := range out {
			dup = dup || m == n
		}
		if !dup {
			out = append(out, n)
		}
	}
	return out
}

func checkSecretNames(names []string) error {
	seen := map[string]bool{}
	for _, n := range names {
		if !validName(n) {
			return fmt.Errorf("bad secret name %q", n)
		}
		if seen[n] {
			return fmt.Errorf("secret %q requested twice", n)
		}
		seen[n] = true
	}
	return nil
}

// checkDAG rejects unknown, self and duplicate needs, and cycles (Kahn).
func checkDAG(steps []Step) error {
	index := map[string]bool{}
	for _, st := range steps {
		index[st.Name] = true
	}
	indeg := map[string]int{}
	children := map[string][]string{}
	for _, st := range steps {
		dup := map[string]bool{}
		for _, n := range st.Needs {
			switch {
			case n == st.Name:
				return fmt.Errorf("step %q needs itself", st.Name)
			case !index[n]:
				return fmt.Errorf("step %q needs unknown step %q", st.Name, n)
			case dup[n]:
				return fmt.Errorf("step %q lists %q twice in needs", st.Name, n)
			}
			dup[n] = true
			indeg[st.Name]++
			children[n] = append(children[n], st.Name)
		}
	}
	var queue []string
	for _, st := range steps {
		if indeg[st.Name] == 0 {
			queue = append(queue, st.Name)
		}
	}
	visited := 0
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		visited++
		for _, c := range children[n] {
			if indeg[c]--; indeg[c] == 0 {
				queue = append(queue, c)
			}
		}
	}
	if visited != len(steps) {
		return errors.New("needs form a cycle")
	}
	return nil
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
	lib := &library{dir: filepath.Join(workdir, stepsDir), cache: map[string]*libStep{}}
	var out []*Pipeline
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || (filepath.Ext(name) != ".yaml" && filepath.Ext(name) != ".yml") {
			continue
		}
		p, err := loadFile(filepath.Join(workdir, pipelinesDir, name), lib)
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

func loadFile(path string, lib *library) (*Pipeline, error) {
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
	return s.validate(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)), lib)
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

var envKeyReplacer = strings.NewReplacer("-", "_", ".", "_")

// EnvKey is the environment variable name a param is injected as inside steps.
func EnvKey(param string) string {
	return "PARAM_" + strings.ToUpper(envKeyReplacer.Replace(param))
}

// InputEnvKey is the env var a library-step input is injected as (ADR 0005).
func InputEnvKey(input string) string {
	return "INPUT_" + strings.ToUpper(envKeyReplacer.Replace(input))
}

// SecretMountDir is where a step's requested secrets are mounted, one
// read-only file per secret (ADR 0006).
const SecretMountDir = "/run/petal/secrets"

// SecretEnvKey is the env var holding the path of a mounted secret file.
func SecretEnvKey(secret string) string {
	return "PETAL_SECRET_" + strings.ToUpper(envKeyReplacer.Replace(secret)) + "_FILE"
}
