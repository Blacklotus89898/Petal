// Package repos is the multi-repo allowlist (ADR 0004): the set of git
// repositories a run may name, by alias. The allowlist is the trust boundary —
// pipeline YAML from any listed repo runs next to the DinD daemon — so it is
// loaded from a file that lives in git (a ConfigMap), never from a request.
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
}

// Allowlist maps alias → repo. The zero value allows nothing (self repo only).
type Allowlist map[string]Repo

type fileSpec struct {
	Repos map[string]struct {
		URL        string `yaml:"url"`
		DefaultRef string `yaml:"default_ref"`
	} `yaml:"repos"`
}

var (
	aliasRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	refRe   = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
)

// ValidRef reports whether ref is a safe branch, tag or commit SHA to hand to
// git: a conservative charset, no "..", and no leading "-" (option injection).
func ValidRef(ref string) bool {
	return refRe.MatchString(ref) && !strings.Contains(ref, "..") &&
		!strings.HasPrefix(ref, "-") && !strings.HasPrefix(ref, "/") && !strings.HasSuffix(ref, "/")
}

// Load reads the allowlist file. A missing file is not an error: it means no
// extra repos (the git-synced self repo still works). v1 accepts public HTTPS
// URLs only — Petal holds no clone credentials.
func Load(path string) (Allowlist, error) {
	if path == "" {
		return Allowlist{}, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Allowlist{}, nil
	}
	if err != nil {
		return nil, err
	}
	var spec fileSpec
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := Allowlist{}
	for alias, r := range spec.Repos {
		if !aliasRe.MatchString(alias) {
			return nil, fmt.Errorf("%s: invalid repo alias %q (lowercase, digits, dashes)", path, alias)
		}
		if !strings.HasPrefix(r.URL, "https://") {
			return nil, fmt.Errorf("%s: repo %q: url must be https:// (public repos only in v1)", path, alias)
		}
		ref := r.DefaultRef
		if ref == "" {
			ref = "main"
		}
		if !ValidRef(ref) {
			return nil, fmt.Errorf("%s: repo %q: invalid default_ref %q", path, alias, ref)
		}
		out[alias] = Repo{Alias: alias, URL: r.URL, DefaultRef: ref}
	}
	return out, nil
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
