package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blacklotus89898/Petal/internal/pipeline"
	"github.com/Blacklotus89898/Petal/internal/repos"
	"github.com/Blacklotus89898/Petal/internal/runs"
)

func newRepoMux(t *testing.T) (*http.ServeMux, *runs.Store) {
	t.Helper()
	store, err := runs.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	mux := http.NewServeMux()
	Register(mux, store, Options{
		APIToken: "tok",
		Workdir:  t.TempDir(), // self repo with an empty pipeline catalog
		Repos: repos.Allowlist{
			"homelab": {Alias: "homelab", URL: "https://github.com/Blacklotus89898/Homelab.git", DefaultRef: "main"},
		},
	})
	return mux, store
}

func postRun(mux *http.ServeMux, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestCreateRunForAllowlistedRepo(t *testing.T) {
	mux, _ := newRepoMux(t)

	// No ref → the repo's default_ref; the pipeline isn't checked at enqueue
	// (it lives in that repo's tree, validated at checkout).
	rec := postRun(mux, `{"pipeline":"validate","repo":"homelab","trigger_kind":"backstage"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body)
	}
	var run runs.Run
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.Repo != "homelab" || run.Ref != "main" {
		t.Errorf("got repo=%q ref=%q, want homelab/main", run.Repo, run.Ref)
	}

	list := httptest.NewRecorder()
	mux.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/runs?repo=homelab", nil))
	var got []runs.Run
	_ = json.Unmarshal(list.Body.Bytes(), &got)
	if len(got) != 1 || got[0].ID != run.ID {
		t.Errorf("repo filter: %+v", got)
	}
}

func TestCreateRunRejectsBadSources(t *testing.T) {
	mux, _ := newRepoMux(t)
	cases := map[string]struct {
		body string
		want int
	}{
		"unknown repo":                {`{"pipeline":"p","repo":"evil"}`, http.StatusBadRequest},
		"option-like ref":             {`{"pipeline":"p","repo":"homelab","ref":"--upload-pack=x"}`, http.StatusBadRequest},
		"dotdot ref":                  {`{"pipeline":"p","repo":"homelab","ref":"a..b"}`, http.StatusBadRequest},
		"ref without repo":            {`{"pipeline":"p","ref":"main"}`, http.StatusBadRequest},
		"self repo, unknown pipeline": {`{"pipeline":"p"}`, http.StatusNotFound},
	}
	for name, c := range cases {
		if rec := postRun(mux, c.body); rec.Code != c.want {
			t.Errorf("%s: expected %d, got %d: %s", name, c.want, rec.Code, rec.Body)
		}
	}
}

func TestListRepos(t *testing.T) {
	mux, _ := newRepoMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/repos", nil))
	var got []repos.Repo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || len(got) != 1 || got[0].Alias != "homelab" || got[0].DefaultRef != "main" {
		t.Errorf("GET /api/repos = %d %s", rec.Code, rec.Body)
	}
}

func TestListPipelinesForRepo(t *testing.T) {
	store, err := runs.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	var asked string
	mux := http.NewServeMux()
	Register(mux, store, Options{
		Workdir: t.TempDir(),
		Repos:   repos.Allowlist{"homelab": {Alias: "homelab", URL: "https://x/h.git", DefaultRef: "main"}},
		RepoPipelines: func(_ context.Context, alias string) ([]*pipeline.Pipeline, error) {
			asked = alias
			return []*pipeline.Pipeline{{Name: "validate", Description: "lint"}}, nil
		},
	})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/pipelines?repo=homelab", nil))
	if rec.Code != http.StatusOK || asked != "homelab" || !strings.Contains(rec.Body.String(), `"validate"`) {
		t.Errorf("repo listing = %d %s (asked %q)", rec.Code, rec.Body, asked)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/pipelines?repo=evil", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown repo: expected 400, got %d", rec.Code)
	}
}
