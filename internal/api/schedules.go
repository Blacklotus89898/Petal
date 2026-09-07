package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Blacklotus89898/Petal/internal/runs"
	"github.com/Blacklotus89898/Petal/internal/scheduler"
)

type createScheduleRequest struct {
	Pipeline string         `json:"pipeline"`
	ModuleFn string         `json:"module_fn"`
	Args     map[string]any `json:"args"`
	Cron     string         `json:"cron"`
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	schedules, err := s.store.ListSchedules(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if schedules == nil {
		schedules = []runs.Schedule{}
	}
	writeJSON(w, http.StatusOK, schedules)
}

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) {
	var req createScheduleRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Pipeline == "" {
		writeError(w, http.StatusBadRequest, "pipeline is required")
		return
	}
	if req.Cron == "" {
		writeError(w, http.StatusBadRequest, "cron is required (5-field, e.g. */15 * * * *)")
		return
	}
	next, err := scheduler.NextRun(req.Cron, time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid cron expression: "+err.Error())
		return
	}
	args, _ := json.Marshal(req.Args)
	sch, err := s.store.CreateSchedule(r.Context(), req.Pipeline, req.ModuleFn, string(args), req.Cron, next.UTC().Format(time.RFC3339Nano))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sch)
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteSchedule(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) toggleSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sch, err := s.store.GetSchedule(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sch == nil {
		writeError(w, http.StatusNotFound, "schedule not found")
		return
	}
	if err := s.store.SetScheduleEnabled(r.Context(), id, !sch.Enabled); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	updated, err := s.store.GetSchedule(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated)
}
