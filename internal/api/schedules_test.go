package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateScheduleValidatesCron(t *testing.T) {
	mux := newTestMux(t, "")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/schedules",
		strings.NewReader(`{"pipeline":"docker-image","cron":"*/15 * * * *"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	bad := httptest.NewRecorder()
	mux.ServeHTTP(bad, httptest.NewRequest(http.MethodPost, "/api/schedules",
		strings.NewReader(`{"pipeline":"docker-image","cron":"sometimes"}`)))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad cron, got %d", bad.Code)
	}

	noPipeline := httptest.NewRecorder()
	mux.ServeHTTP(noPipeline, httptest.NewRequest(http.MethodPost, "/api/schedules",
		strings.NewReader(`{"cron":"* * * * *"}`)))
	if noPipeline.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing pipeline, got %d", noPipeline.Code)
	}
}

func TestListAndDeleteSchedule(t *testing.T) {
	mux := newTestMux(t, "")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/schedules",
		strings.NewReader(`{"pipeline":"docker-image","module_fn":"build","args":{"ref":"x"},"cron":"0 4 * * *"}`)))
	body := rec.Body.String()
	id := body[strings.Index(body, `"id":"`)+len(`"id":"`):]
	id = id[:strings.Index(id, `"`)]

	list := httptest.NewRecorder()
	mux.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/schedules", nil))
	if !strings.Contains(list.Body.String(), `"cron":"0 4 * * *"`) {
		t.Fatalf("schedule not listed: %s", list.Body.String())
	}

	del := httptest.NewRecorder()
	mux.ServeHTTP(del, httptest.NewRequest(http.MethodDelete, "/api/schedules/"+id, nil))
	if del.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", del.Code)
	}

	after := httptest.NewRecorder()
	mux.ServeHTTP(after, httptest.NewRequest(http.MethodGet, "/api/schedules", nil))
	if strings.Contains(after.Body.String(), id) {
		t.Fatal("schedule should be gone after delete")
	}
}
