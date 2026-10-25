// Package repos loads Petal's config file (a ConfigMap in git, never a
// request): the multi-repo allowlist (ADR 0004) — the git repositories a run
// may name, by alias; the trust boundary, since pipeline YAML from any listed
// repo runs next to the DinD daemon — and declarative cron schedules.
package repos

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Repo is one allowlisted source repository.
type Repo struct {
	Alias      string `json:"alias"`
	URL        string `json:"url"`
	DefaultRef string `json:"default_ref"`
	// Privileges granted to runs of this repo, only at TrustedRefs (ADR 0006):
	// secrets by name, and Docker = steps may drive the DinD daemon.
	Secrets     []string `json:"secrets,omitempty"`
	Docker      bool     `json:"docker,omitempty"`
	TrustedRefs []string `json:"trusted_refs,omitempty"`
}

// GrantsAt returns what a run of this repo at ref is granted: everything when
// ref is one of TrustedRefs, nothing otherwise (ADR 0006).
func (r Repo) GrantsAt(ref string) (secrets []string, docker bool) {
	for _, tr := range r.TrustedRefs {
		if tr == ref {
			return r.Secrets, r.Docker
		}
	}
	return nil, false
}

// Allowlist maps alias → repo. The zero value allows nothing (self repo only).
type Allowlist map[string]Repo

// Schedule is a cron schedule declared in the config file. Petal syncs these
// at startup (insert / update / delete), so git owns them; API-created
// schedules are separate and untouched.
type Schedule struct {
	ID       string         `yaml:"id"`
	Pipeline string         `yaml:"pipeline"`
	Cron     string         `yaml:"cron"`
	Repo     string         `yaml:"repo"` // "" = self repo
	Ref      string         `yaml:"ref"`
	Args     map[string]any `yaml:"args"`
}

// Config is the whole file.
type Config struct {
	Repos     Allowlist
	Schedules []Schedule
	// Granted to runs of the git-synced self repo (ADR 0006).
	SelfSecrets []string
	SelfDocker  bool
}

type fileSpec struct {
	Self struct {
		Secrets []string `yaml:"secrets"`
		Docker  bool     `yaml:"docker"`
	} `yaml:"self"`
	Repos map[string]struct {
		URL         string   `yaml:"url"`
		DefaultRef  string   `yaml:"default_ref"`
		Secrets     []string `yaml:"secrets"`
		Docker      bool     `yaml:"docker"`
		TrustedRefs []string `yaml:"trusted_refs"`
	} `yaml:"repos"`
	Schedules []Schedule `yaml:"schedules"`
}

var (
	aliasRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	refRe    = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	secretRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`) // = a Secret key and a pipeline name
)

func checkSecretNames(names []string) error {
	seen := map[string]bool{}
	for _, n := range names {
		if !secretRe.MatchString(n) {
			return fmt.Errorf("invalid secret name %q (lowercase, digits, . _ -)", n)
		}
		if seen[n] {
			return fmt.Errorf("secret %q listed twice", n)
		}
		seen[n] = true
	}
	return nil
}

// ValidRef reports whether ref is a safe branch, tag or commit SHA to hand to
// git: a conservative charset, no "..", and no leading "-" (option injection).
func ValidRef(ref string) bool {
	return refRe.MatchString(ref) && !strings.Contains(ref, "..") &&
		!strings.HasPrefix(ref, "-") && !strings.HasPrefix(ref, "/") && !strings.HasSuffix(ref, "/")
}

// Load reads the allowlist from the config file (schedules ignored).
func Load(path string) (Allowlist, error) {
	c, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	return c.Repos, nil
}

// LoadConfig reads the config file. A missing file is not an error: it means
// no extra repos and no declared schedules (the git-synced self repo still
// works). v1 accepts public HTTPS URLs only — Petal holds no clone credentials.
// Cron expressions are checked by the caller (the scheduler owns cron parsing).
func LoadConfig(path string) (Config, error) {
	empty := Config{Repos: Allowlist{}}
	if path == "" {
		return empty, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return Config{}, err
	}
	var spec fileSpec
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	out := Allowlist{}
	for alias, r := range spec.Repos {
		if !aliasRe.MatchString(alias) {
			return Config{}, fmt.Errorf("%s: invalid repo alias %q (lowercase, digits, dashes)", path, alias)
		}
		if !strings.HasPrefix(r.URL, "https://") {
			return Config{}, fmt.Errorf("%s: repo %q: url must be https:// (public repos only in v1)", path, alias)
		}
		ref := r.DefaultRef
		if ref == "" {
			ref = "main"
		}
		if !ValidRef(ref) {
			return Config{}, fmt.Errorf("%s: repo %q: invalid default_ref %q", path, alias, ref)
		}
		if err := checkSecretNames(r.Secrets); err != nil {
			return Config{}, fmt.Errorf("%s: repo %q: %w", path, alias, err)
		}
		privileged := len(r.Secrets) > 0 || r.Docker
		trusted := r.TrustedRefs
		if len(trusted) == 0 && privileged {
			trusted = []string{ref}
		}
		for _, tr := range trusted {
			if !ValidRef(tr) {
				return Config{}, fmt.Errorf("%s: repo %q: invalid trusted_ref %q", path, alias, tr)
			}
		}
		if !privileged && len(r.TrustedRefs) > 0 {
			return Config{}, fmt.Errorf("%s: repo %q: trusted_refs without secrets or docker", path, alias)
		}
		out[alias] = Repo{Alias: alias, URL: r.URL, DefaultRef: ref, Secrets: r.Secrets, Docker: r.Docker, TrustedRefs: trusted}
	}
	if err := checkSecretNames(spec.Self.Secrets); err != nil {
		return Config{}, fmt.Errorf("%s: self: %w", path, err)
	}
	seen := map[string]bool{}
	for i, sch := range spec.Schedules {
		switch {
		case !aliasRe.MatchString(sch.ID):
			return Config{}, fmt.Errorf("%s: schedule %d: invalid id %q (lowercase, digits, dashes)", path, i, sch.ID)
		case seen[sch.ID]:
			return Config{}, fmt.Errorf("%s: duplicate schedule id %q", path, sch.ID)
		case sch.Pipeline == "" || sch.Cron == "":
			return Config{}, fmt.Errorf("%s: schedule %q needs pipeline and cron", path, sch.ID)
		case sch.Repo != "" && out[sch.Repo].URL == "":
			return Config{}, fmt.Errorf("%s: schedule %q: repo %q is not in the allowlist", path, sch.ID, sch.Repo)
		case sch.Repo == "" && sch.Ref != "":
			return Config{}, fmt.Errorf("%s: schedule %q: ref needs a repo", path, sch.ID)
		case sch.Ref != "" && !ValidRef(sch.Ref):
			return Config{}, fmt.Errorf("%s: schedule %q: invalid ref %q", path, sch.ID, sch.Ref)
		}
		seen[sch.ID] = true
	}
	return Config{Repos: out, Schedules: spec.Schedules, SelfSecrets: spec.Self.Secrets, SelfDocker: spec.Self.Docker}, nil
}

// List returns the repos sorted by alias (stable API output).
func (a Allowlist) List() []Repo {
	out := make([]Repo, 0, len(a))
	for _, r := range a {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out
}
