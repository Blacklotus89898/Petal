package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blacklotus89898/Petal/internal/runs"
)

func TestMutatingEndpointsRequireToken(t *testing.T) {
	mux := newTestMux(t, "")
	reqs := []*http.Request{
		httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(`{"pipeline":"docker-image"}`)),
		httptest.NewRequest(http.MethodPost, "/api/schedules", strings.NewReader(`{"pipeline":"p","cron":"* * * * *"}`)),
		httptest.NewRequest(http.MethodDelete, "/api/schedules/sch_x", nil),
		httptest.NewRequest(http.MethodPost, "/api/schedules/sch_x/toggle", nil),
	}
	for _, req := range reqs {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token: expected 401, got %d", req.Method, req.URL.Path, rec.Code)
		}
	}

	wrong := httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(`{"pipeline":"docker-image"}`))
	wrong.Header.Set("Authorization", "Bearer nope")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, wrong)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: expected 401, got %d", rec.Code)
	}

	// reads stay open
	read := httptest.NewRecorder()
	mux.ServeHTTP(read, httptest.NewRequest(http.MethodGet, "/api/runs", nil))
	if read.Code != http.StatusOK {
		t.Fatalf("GET /api/runs: expected 200, got %d", read.Code)
	}
}

func TestMutatingEndpointsDisabledWithoutConfiguredToken(t *testing.T) {
	store, err := runs.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	mux := http.NewServeMux()
	Register(mux, store, Options{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(`{"pipeline":"p"}`))
	req.Header.Set("Authorization", "Bearer ") // empty token must never match an empty config
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when PETAL_API_TOKEN unset, got %d", rec.Code)
	}
}

func TestCreateRunTriggerKinds(t *testing.T) {
	mux := newTestMux(t, "")
	cases := []struct {
		body     string
		wantCode int
		wantKind string
	}{
		{`{"pipeline":"p"}`, http.StatusCreated, "api"},
		{`{"pipeline":"p","trigger_kind":"backstage","trigger_source":"backstage:component:default/petal#user:default/jo"}`, http.StatusCreated, "backstage"},
		{`{"pipeline":"p","trigger_kind":"webhook"}`, http.StatusBadRequest, ""}, // reserved for the signed webhook
		{`{"pipeline":"p","trigger_kind":"cron"}`, http.StatusBadRequest, ""},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, authed(httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(tc.body))))
		if rec.Code != tc.wantCode {
			t.Errorf("%s: expected %d, got %d: %s", tc.body, tc.wantCode, rec.Code, rec.Body.String())
			continue
		}
		if tc.wantKind == "" {
			continue
		}
		var run runs.Run
		if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
			t.Fatal(err)
		}
		if run.TriggerKind != tc.wantKind {
			t.Errorf("%s: expected trigger_kind %q, got %q", tc.body, tc.wantKind, run.TriggerKind)
		}
	}
}

func TestCreateRunValidatesPipelineAgainstCatalog(t *testing.T) {
	mux := newCatalogTestMux(t)

	known := httptest.NewRecorder()
	mux.ServeHTTP(known, authed(httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(`{"pipeline":"hello"}`))))
	if known.Code != http.StatusCreated {
		t.Fatalf("known pipeline: expected 201, got %d: %s", known.Code, known.Body.String())
	}

	unknown := httptest.NewRecorder()
	mux.ServeHTTP(unknown, authed(httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(`{"pipeline":"helo"}`))))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown pipeline: expected 404, got %d", unknown.Code)
	}
}

func TestListRunsFilters(t *testing.T) {
	mux := newTestMux(t, "")
	for _, body := range []string{
		`{"pipeline":"a","trigger_kind":"backstage","trigger_source":"backstage:component:default/petal#user:default/jo"}`,
		`{"pipeline":"a"}`,
		`{"pipeline":"b"}`,
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, authed(httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(body))))
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
		}
	}
	cases := map[string]int{
		"/api/runs":                        3,
		"/api/runs?pipeline=a":             2,
		"/api/runs?trigger_kind=backstage": 1,
		"/api/runs?source=backstage:component:default/petal%23": 1,
		"/api/runs?source=backstage:component:default/other%23": 0,
	}
	for url, want := range cases {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		var got []runs.Run
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: %v", url, err)
		}
		if len(got) != want {
			t.Errorf("%s: expected %d runs, got %d", url, want, len(got))
		}
	}
}
