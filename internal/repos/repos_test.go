package repos

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "repos.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	a, err := Load(write(t, `
repos:
  homelab:
    url: https://github.com/Blacklotus89898/Homelab.git
    default_ref: main
  petal:
    url: https://github.com/Blacklotus89898/Petal.git
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := a["homelab"]; got.URL != "https://github.com/Blacklotus89898/Homelab.git" || got.DefaultRef != "main" {
		t.Errorf("homelab = %+v", got)
	}
	if a["petal"].DefaultRef != "main" {
		t.Errorf("default_ref should default to main, got %q", a["petal"].DefaultRef)
	}
	if l := a.List(); len(l) != 2 || l[0].Alias != "homelab" || l[1].Alias != "petal" {
		t.Errorf("List not sorted by alias: %+v", l)
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	for _, p := range []string{"", filepath.Join(t.TempDir(), "absent.yaml")} {
		a, err := Load(p)
		if err != nil || len(a) != 0 {
			t.Errorf("Load(%q) = (%v, %v), want empty allowlist", p, a, err)
		}
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]string{
		"ssh url":     "repos:\n  x:\n    url: git@github.com:a/b.git\n",
		"http url":    "repos:\n  x:\n    url: http://example.com/a.git\n",
		"bad alias":   "repos:\n  Bad_Alias:\n    url: https://example.com/a.git\n",
		"bad ref":     "repos:\n  x:\n    url: https://example.com/a.git\n    default_ref: --upload-pack=evil\n",
		"unknown key": "repos:\n  x:\n    url: https://example.com/a.git\n    token: s3cret\n",
	}
	for name, body := range cases {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestValidRef(t *testing.T) {
	good := []string{"main", "v1.2.3", "feature/multi-repo", "4b9f2cc159de2200c4e88ec1c67e864e6cd89c13"}
	bad := []string{"", "-x", "--upload-pack=x", "a..b", "/abs", "trailing/", "has space", "semi;colon", "$(id)"}
	for _, r := range good {
		if !ValidRef(r) {
			t.Errorf("ValidRef(%q) = false, want true", r)
		}
	}
	for _, r := range bad {
		if ValidRef(r) {
			t.Errorf("ValidRef(%q) = true, want false", r)
		}
	}
}
