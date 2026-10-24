package api

import (
	"net/http"

	"github.com/Blacklotus89898/Petal/internal/runs"
)

// cancelRun stops a run (ADR 0005). Queued → canceled atomically (200);
// running → its context is canceled with "cancel requested" and the worker
// records it as canceled once the step containers are killed (202); already
// finished → 409.
func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validRunID(id) {
		writeError(w, http.StatusBadRequest, "bad run id")
		return
	}
	run, err := s.store.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	switch run.Status {
	case runs.StatusSucceeded, runs.StatusFailed, runs.StatusCanceled:
		writeError(w, http.StatusConflict, "run is "+run.Status+", nothing to cancel")
		return
	}
	if run.Status == runs.StatusQueued {
		ok, err := s.store.CancelQueued(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if ok {
			writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": runs.StatusCanceled})
			return
		}
		// Claimed by a worker between Get and the update: cancel it as running.
	}
	if s.cancelRunning != nil && s.cancelRunning(id) {
		writeJSON(w, http.StatusAccepted, map[string]string{"id": id, "status": "canceling"})
		return
	}
	if latest, _ := s.store.Get(r.Context(), id); latest != nil {
		run = latest
	}
	writeError(w, http.StatusConflict, "run is "+run.Status+", nothing to cancel")
}
