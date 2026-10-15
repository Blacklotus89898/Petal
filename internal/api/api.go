// Package api exposes the control-plane HTTP API. The UI is served same-origin
// from the same binary, so no CORS handling is needed anywhere.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Blacklotus89898/Petal/internal/runs"
)

// Server handles /api/* requests.
type Server struct {
	store        *runs.Store
	triggerToken string
	apiToken     string
	workdir      string // pipeline catalog source (steps executor)
	dataDir      string // step log files live under <dataDir>/logs
}

// Options configures optional API features.
type Options struct {
	// TriggerToken enables the signed webhook trigger endpoint (POST
	// /api/triggers/github). Empty disables it.
	TriggerToken string
	// APIToken guards every mutating endpoint (create run, schedule CRUD)
	// with "Authorization: Bearer <token>". Empty disables those endpoints:
	// an open port never means an open trigger (ADR 0003).
	APIToken string
	// Workdir enables the pipeline catalog endpoints (GET /api/pipelines)
	// and create-run validation against the discovered specs.
	Workdir string
	// DataDir enables step log serving (GET /api/runs/{id}/logs/{step}).
	DataDir string
}

// Register wires the API routes onto mux.
func Register(mux *http.ServeMux, store *runs.Store, opts Options) {
	s := &Server{store: store, triggerToken: opts.TriggerToken, apiToken: opts.APIToken, workdir: opts.Workdir, dataDir: opts.DataDir}
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/runs", s.listRuns)
	mux.HandleFunc("POST /api/runs", s.requireToken(s.createRun))
	mux.HandleFunc("GET /api/runs/{id}", s.getRun)
	if s.workdir != "" {
		mux.HandleFunc("GET /api/pipelines", s.listPipelines)
	}
	if s.dataDir != "" {
		mux.HandleFunc("GET /api/runs/{id}/logs/{step}", s.stepLogs)
	}
	mux.HandleFunc("POST /api/triggers/github", s.handleGitHubTrigger)
	mux.HandleFunc("GET /api/schedules", s.listSchedules)
	mux.HandleFunc("POST /api/schedules", s.requireToken(s.createSchedule))
	mux.HandleFunc("DELETE /api/schedules/{id}", s.requireToken(s.deleteSchedule))
	mux.HandleFunc("POST /api/schedules/{id}/toggle", s.requireToken(s.toggleSchedule))
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// listRuns returns recent runs. Optional query filters: pipeline,
// trigger_kind (exact) and source (trigger_source prefix) — a portal lists one
// entity's runs with source=backstage:<entity-ref>#.
func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	rs, err := s.store.ListFiltered(r.Context(), runs.Filter{
		Limit:        limit,
		Pipeline:     q.Get("pipeline"),
		TriggerKind:  q.Get("trigger_kind"),
		SourcePrefix: q.Get("source"),
	})
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
	Pipeline    string         `json:"pipeline"`
	ModuleFn    string         `json:"module_fn"`
	Args        map[string]any `json:"args"`
	DedupKey    string         `json:"dedup_key"`
	TriggerKind string         `json:"trigger_kind"`
	TriggerSrc  string         `json:"trigger_source"`
}

// apiTriggerKinds are the trigger kinds a token-holding API caller may claim.
// "webhook" and "cron" stay reserved for the signed webhook and the scheduler.
var apiTriggerKinds = map[string]bool{"api": true, "backstage": true}

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
	if req.TriggerKind == "" {
		req.TriggerKind = "api"
	}
	if !apiTriggerKinds[req.TriggerKind] {
		writeError(w, http.StatusBadRequest, `trigger_kind must be "api" or "backstage"`)
		return
	}
	if ok, err := s.pipelineExists(req.Pipeline); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	} else if !ok {
		writeError(w, http.StatusNotFound, "unknown pipeline "+strconv.Quote(req.Pipeline))
		return
	}
	if req.Args == nil {
		req.Args = map[string]any{} // record "{}", not "null"
	}
	args, _ := json.Marshal(req.Args)
	run, err := s.store.Enqueue(r.Context(), req.Pipeline, req.ModuleFn, string(args), req.TriggerKind, req.TriggerSrc, req.DedupKey)
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

// requireToken guards a mutating handler with the API bearer token.
func (s *Server) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiToken == "" {
			writeError(w, http.StatusServiceUnavailable, "API token not configured (set PETAL_API_TOKEN)")
			return
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.apiToken)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="petal"`)
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
