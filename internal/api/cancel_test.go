package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Blacklotus89898/Petal/internal/runs"
)

func TestCancelRun(t *testing.T) {
	store, err := runs.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	asked := map[string]bool{}
	mux := http.NewServeMux()
	Register(mux, store, Options{
		APIToken:      "tok",
		CancelRunning: func(id string) bool { asked[id] = true; return true },
	})
	post := func(id, token string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/runs/"+id+"/cancel", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}

	queued, _ := store.Enqueue(ctx, "p", "", "{}", "api", "", "")
	if code := post(queued.ID, ""); code != http.StatusUnauthorized {
		t.Errorf("no token: %d", code)
	}
	if code := post(queued.ID, "tok"); code != http.StatusOK {
		t.Errorf("queued: expected 200, got %d", code)
	}
	if got, _ := store.Get(ctx, queued.ID); got.Status != runs.StatusCanceled {
		t.Errorf("queued run status = %q", got.Status)
	}
	if asked[queued.ID] {
		t.Error("a queued run must be canceled in the registry, not via the pool")
	}

	running, _ := store.Enqueue(ctx, "p", "", "{}", "api", "", "")
	_, _ = store.ClaimNext(ctx)
	if code := post(running.ID, "tok"); code != http.StatusAccepted || !asked[running.ID] {
		t.Errorf("running: expected 202 via the pool, got %d (asked=%v)", code, asked[running.ID])
	}

	if code := post(queued.ID, "tok"); code != http.StatusConflict {
		t.Errorf("already-canceled run: expected 409, got %d", code)
	}
	if code := post("run_00", "tok"); code != http.StatusNotFound {
		t.Errorf("unknown run: expected 404, got %d", code)
	}
}
