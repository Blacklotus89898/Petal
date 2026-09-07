package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blacklotus89898/Petal/internal/runs"
)

func newCatalogTestMux(t *testing.T) *http.ServeMux {
	t.Helper()
	store, err := runs.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	workdir := t.TempDir()
	specDir := filepath.Join(workdir, ".petal", "pipelines")
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := "steps:\n  - name: hello\n    image: alpine:3.21\n    run: echo hi\n"
	if err := os.WriteFile(filepath.Join(specDir, "hello.yaml"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "logs", "run_beef0001"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "logs", "run_beef0001", "hello.log"), []byte("hi from hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	Register(mux, store, Options{Workdir: workdir, DataDir: dataDir})
	return mux
}

func TestListPipelines(t *testing.T) {
	mux := newCatalogTestMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/pipelines", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"name":"hello"`) {
		t.Errorf("catalog should contain hello pipeline: %s", rec.Body.String())
	}
}

func TestStepLogs(t *testing.T) {
	mux := newCatalogTestMux(t)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs/run_beef0001/logs/hello", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "hi from hello\n" {
		t.Errorf("log fetch: %d %q", rec.Code, rec.Body.String())
	}

	missing := httptest.NewRecorder()
	mux.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/api/runs/run_beef0001/logs/nope", nil))
	if missing.Code != http.StatusNotFound {
		t.Errorf("missing step log: %d", missing.Code)
	}

	traversal := httptest.NewRecorder()
	mux.ServeHTTP(traversal, httptest.NewRequest(http.MethodGet, "/api/runs/run_beef0001/logs/..%2f..%2fpetal.db", nil))
	if traversal.Code != http.StatusBadRequest {
		t.Errorf("traversal attempt: %d", traversal.Code)
	}

	emptyHex := httptest.NewRecorder()
	mux.ServeHTTP(emptyHex, httptest.NewRequest(http.MethodGet, "/api/runs/run_/logs/hello", nil))
	if emptyHex.Code != http.StatusBadRequest {
		t.Errorf("empty-hex run id: %d", emptyHex.Code)
	}
}

func TestValidRunID(t *testing.T) {
	for id, want := range map[string]bool{
		"run_0123456789abcdef": true,
		"run_":                 false,
		"run_ABC":              false,
		"../etc":               false,
		"run_0123/../../x":     false,
	} {
		if got := validRunID(id); got != want {
			t.Errorf("validRunID(%q) = %v, want %v", id, got, want)
		}
	}
}
