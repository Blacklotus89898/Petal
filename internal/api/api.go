// Package api exposes the control-plane HTTP API. The UI is served same-origin
// from the same binary, so no CORS handling is needed anywhere.
package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/Blacklotus89898/Petal/internal/runs"
)

// Server handles /api/* requests.
type Server struct {
	store        *runs.Store
	triggerToken string
}

// Options configures optional API features.
type Options struct {
	// TriggerToken enables the signed webhook trigger endpoint (POST
	// /api/triggers/github). Empty disables it.
	TriggerToken string
}

// Register wires the API routes onto mux.
func Register(mux *http.ServeMux, store *runs.Store, opts Options) {
	s := &Server{store: store, triggerToken: opts.TriggerToken}
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/runs", s.listRuns)
	mux.HandleFunc("POST /api/runs", s.createRun)
	mux.HandleFunc("GET /api/runs/{id}", s.getRun)
	mux.HandleFunc("POST /api/triggers/github", s.handleGitHubTrigger)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rs, err := s.store.List(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rs == nil {
		rs = []runs.Run{}
	}
	writeJSON(w, http.StatusOK, rs)
}

type createRunRequest struct {
	Pipeline   string         `json:"pipeline"`
	ModuleFn   string         `json:"module_fn"`
	Args       map[string]any `json:"args"`
	DedupKey   string         `json:"dedup_key"`
	TriggerSrc string         `json:"trigger_source"`
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	var req createRunRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Pipeline == "" {
		writeError(w, http.StatusBadRequest, "pipeline is required")
		return
	}
	args, _ := json.Marshal(req.Args)
	run, err := s.store.Enqueue(r.Context(), req.Pipeline, req.ModuleFn, string(args), "manual", req.TriggerSrc, req.DedupKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := s.store.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	events, err := s.store.ListEvents(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if events == nil {
		events = []runs.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "events": events})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
