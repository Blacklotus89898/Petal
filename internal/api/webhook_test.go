package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Blacklotus89898/Petal/internal/runs"
)

func newTestMux(t *testing.T, token string) *http.ServeMux {
	t.Helper()
	store, err := runs.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	mux := http.NewServeMux()
	Register(mux, store, Options{TriggerToken: token})
	return mux
}

func signedRequest(t *testing.T, token, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/triggers/github", bytes.NewBufferString(body))
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(body))
	req.Header.Set("X-Petal-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestWebhookTriggerCreatesRun(t *testing.T) {
	mux := newTestMux(t, "secret")
	body := `{"pipeline":"docker-image","module_fn":"build","args":{},"git_sha":"abc123","source":"gha:http://x/1"}`

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, signedRequest(t, "secret", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var run runs.Run
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.TriggerKind != "webhook" || run.GitSHA != "abc123" {
		t.Fatalf("wrong run recorded: %+v", run)
	}
	if run.DedupKey == nil || *run.DedupKey != "abc123:docker-image" {
		t.Fatalf("expected default dedup key git_sha:pipeline, got %+v", run.DedupKey)
	}
}

func TestWebhookTriggerDedupesSameSHA(t *testing.T) {
	mux := newTestMux(t, "secret")
	body := `{"pipeline":"docker-image","args":{},"git_sha":"deadbee"}`

	first := httptest.NewRecorder()
	mux.ServeHTTP(first, signedRequest(t, "secret", body))
	second := httptest.NewRecorder()
	mux.ServeHTTP(second, signedRequest(t, "secret", body))

	var r1, r2 runs.Run
	_ = json.Unmarshal(first.Body.Bytes(), &r1)
	_ = json.Unmarshal(second.Body.Bytes(), &r2)
	if r1.ID != r2.ID {
		t.Fatalf("same git sha should dedup to one run: %s vs %s", r1.ID, r2.ID)
	}
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	mux := newTestMux(t, "secret")
	req := signedRequest(t, "WRONG-token", `{"pipeline":"docker-image"}`)
	req.Header.Set("X-Petal-Signature", "sha256=deadbeef")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestWebhookDisabledWithoutToken(t *testing.T) {
	mux := newTestMux(t, "")
	req := signedRequest(t, "", `{"pipeline":"docker-image"}`)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when no token configured, got %d", rec.Code)
	}
}
