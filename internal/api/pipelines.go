// Pipeline catalog and step-log endpoints (ADR 0002).
package api

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Blacklotus89898/Petal/internal/pipeline"
)

// listPipelines returns the pipeline catalog discovered in the workdir.
func (s *Server) listPipelines(w http.ResponseWriter, r *http.Request) {
	specs, err := pipeline.List(s.workdir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if specs == nil {
		specs = []*pipeline.Pipeline{}
	}
	writeJSON(w, http.StatusOK, specs)
}

// pipelineExists reports whether name is in the workdir catalog. Without a
// workdir (stub-only setups) every name is accepted, as before.
func (s *Server) pipelineExists(name string) (bool, error) {
	if s.workdir == "" {
		return true, nil
	}
	specs, err := pipeline.List(s.workdir)
	if err != nil {
		return false, err
	}
	for _, p := range specs {
		if p.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// stepLogs streams a step's log file (text/plain, partial logs included
// while the step is still running).
func (s *Server) stepLogs(w http.ResponseWriter, r *http.Request) {
	runID, step := r.PathValue("id"), r.PathValue("step")
	// run IDs are "run_" + hex; step names are kebab-case identifiers. Both
	// checks together guarantee the joined path stays inside the logs dir.
	if !validRunID(runID) {
		writeError(w, http.StatusBadRequest, "bad run id")
		return
	}
	if !pipeline.ValidName(step) {
		writeError(w, http.StatusBadRequest, "bad step name")
		return
	}
	f, err := os.Open(filepath.Join(s.dataDir, "logs", runID, step+".log"))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "no log for this step (yet)")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.Copy(w, f)
}

func validRunID(id string) bool {
	const prefix = "run_"
	if len(id) <= len(prefix) || len(id) > len(prefix)+64 {
		return false
	}
	if !strings.HasPrefix(id, prefix) {
		return false
	}
	for _, r := range id[len(prefix):] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
