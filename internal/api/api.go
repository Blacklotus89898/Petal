// Package api exposes the control-plane HTTP API. The UI is served same-origin
// from the same binary, so no CORS handling is needed anywhere.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Blacklotus89898/Petal/internal/pipeline"
	"github.com/Blacklotus89898/Petal/internal/repos"
	"github.com/Blacklotus89898/Petal/internal/runs"
)

// Server handles /api/* requests.
type Server struct {
	store        *runs.Store
	triggerToken string
	apiToken     string
	workdir      string // pipeline catalog source (steps executor)
	dataDir      string // step log files live under <dataDir>/logs
	repos        repos.Allowlist
	repoPipes    RepoPipelinesFunc
}

// RepoPipelinesFunc lists the pipelines in an allowlisted repo (the steps
// executor's cached sparse clone). Nil disables GET /api/pipelines?repo=.
type RepoPipelinesFunc func(ctx context.Context, alias string) ([]*pipeline.Pipeline, error)

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
	// Repos is the multi-repo allowlist (ADR 0004): the repos a run may name.
	// Empty means self-repo runs only.
	Repos repos.Allowlist
	// RepoPipelines backs GET /api/pipelines?repo=<alias>.
	RepoPipelines RepoPipelinesFunc
}

// Register wires the API routes onto mux.
func Register(mux *http.ServeMux, store *runs.Store, opts Options) {
	s := &Server{store: store, triggerToken: opts.TriggerToken, apiToken: opts.APIToken, workdir: opts.Workdir, dataDir: opts.DataDir, repos: opts.Repos, repoPipes: opts.RepoPipelines}
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/runs", s.listRuns)
	mux.HandleFunc("POST /api/runs", s.requireToken(s.createRun))
	mux.HandleFunc("GET /api/runs/{id}", s.getRun)
	mux.HandleFunc("GET /api/repos", s.listRepos)
	mux.HandleFunc("GET /metrics", s.metrics) // Prometheus exposition (VMServiceScrape in Homelab)
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
// trigger_kind and repo (exact) and source (trigger_source prefix) — a portal lists one
// entity's runs with source=backstage:<entity-ref>#.
func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	rs, err := s.store.ListFiltered(r.Context(), runs.Filter{
		Limit:        limit,
		Pipeline:     q.Get("pipeline"),
		TriggerKind:  q.Get("trigger_kind"),
		SourcePrefix: q.Get("source"),
		Repo:         q.Get("repo"),
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
	Repo        string         `json:"repo"` // allowlist alias; "" = self repo (ADR 0004)
	Ref         string         `json:"ref"`  // branch, tag or SHA; "" = the repo's default_ref
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
	if req.Repo == "" {
		if req.Ref != "" {
			writeError(w, http.StatusBadRequest, "ref needs a repo (self-repo runs use the git-synced tree)")
			return
		}
		// Self repo: the catalog is local, so an unknown pipeline is a 404 now.
		if ok, err := s.pipelineExists(req.Pipeline); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		} else if !ok {
			writeError(w, http.StatusNotFound, "unknown pipeline "+strconv.Quote(req.Pipeline))
			return
		}
	} else {
		// Other repos: the alias must be allowlisted; the pipeline is checked
		// at checkout (it lives in that repo's tree).
		repo, ok := s.repos[req.Repo]
		if !ok {
			writeError(w, http.StatusBadRequest, "unknown repo "+strconv.Quote(req.Repo)+" (not in the allowlist)")
			return
		}
		if req.Ref == "" {
			req.Ref = repo.DefaultRef
		}
		if !repos.ValidRef(req.Ref) {
			writeError(w, http.StatusBadRequest, "invalid ref "+strconv.Quote(req.Ref))
			return
		}
	}
	if req.Args == nil {
		req.Args = map[string]any{} // record "{}", not "null"
	}
	args, _ := json.Marshal(req.Args)
	run, err := s.store.Add(r.Context(), runs.NewRun{
		Pipeline: req.Pipeline, ModuleFn: req.ModuleFn, ArgsJSON: string(args),
		TriggerKind: req.TriggerKind, TriggerSource: req.TriggerSrc, DedupKey: req.DedupKey,
		Repo: req.Repo, Ref: req.Ref,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

// listRepos returns the multi-repo allowlist (aliases, URLs, default refs —
// nothing secret: v1 repos are public).
func (s *Server) listRepos(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.repos.List())
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
