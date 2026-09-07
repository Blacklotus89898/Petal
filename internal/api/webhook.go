package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
)

// maxWebhookBody caps webhook payload size.
const maxWebhookBody = 1 << 20

// githubTriggerRequest is the payload GitHub Actions sends to Petal.
type githubTriggerRequest struct {
	Pipeline string         `json:"pipeline"`
	ModuleFn string         `json:"module_fn"`
	Args     map[string]any `json:"args"`
	GitSHA   string         `json:"git_sha"`
	DedupKey string         `json:"dedup_key"`
	Source   string         `json:"source"` // e.g. "gha:<run html url>"
}

// handleGitHubTrigger accepts HMAC-SHA256-signed trigger payloads and enqueues
// a run with webhook semantics: dedup defaults to git_sha:pipeline so the same
// commit never double-runs (at-least-once triggers, exactly-once execution).
//
// Enabled only when a trigger token is configured; without one the endpoint
// returns 503 so an open port never means an open trigger.
func (s *Server) handleGitHubTrigger(w http.ResponseWriter, r *http.Request) {
	if s.triggerToken == "" {
		writeError(w, http.StatusServiceUnavailable, "trigger token not configured (set PETAL_TRIGGER_TOKEN)")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	sig := r.Header.Get("X-Petal-Signature")
	if !verifyHMAC(sig, s.triggerToken, body) {
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	var req githubTriggerRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Pipeline == "" {
		writeError(w, http.StatusBadRequest, "pipeline is required")
		return
	}

	dedup := req.DedupKey
	if dedup == "" && req.GitSHA != "" {
		dedup = req.GitSHA + ":" + req.Pipeline
	}
	args, _ := json.Marshal(req.Args)
	run, err := s.store.Enqueue(r.Context(), req.Pipeline, req.ModuleFn, string(args), "webhook", req.Source, dedup)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.GitSHA != "" {
		if err := s.store.SetGitSHA(r.Context(), run.ID, req.GitSHA); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		run.GitSHA = req.GitSHA
	}
	writeJSON(w, http.StatusCreated, run)
}

func verifyHMAC(header, token string, body []byte) bool {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(header), []byte(want))
}
