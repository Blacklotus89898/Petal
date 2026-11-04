package notify

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type got struct {
	path, body string
	h          http.Header
}

func fakeNtfy(t *testing.T, status int) (*httptest.Server, *[]got) {
	t.Helper()
	var mu sync.Mutex
	var msgs []got
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		msgs = append(msgs, got{r.URL.Path, string(b), r.Header.Clone()})
		mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &msgs
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestFailedRunNotifiesWithLink(t *testing.T) {
	srv, msgs := fakeNtfy(t, 200)
	n := New(srv.URL, "petal", "https://petal.lab.home", quiet)
	n.RunFinished(context.Background(), Run{ID: "run_1", Pipeline: "validate", Repo: "homelab", Status: "failed", Error: `step "lint" failed (exit 1)`}, false)
	if len(*msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(*msgs))
	}
	m := (*msgs)[0]
	if m.path != "/petal" || m.h.Get("Title") != "Petal: homelab/validate failed" || m.h.Get("Priority") != "high" ||
		m.h.Get("Click") != "https://petal.lab.home/?run=run_1" || !strings.Contains(m.body, `step "lint" failed`) {
		t.Errorf("message = %+v", m)
	}
}

func TestSuccessOnlyWhenOptedIn(t *testing.T) {
	srv, msgs := fakeNtfy(t, 200)
	n := New(srv.URL, "petal", "", quiet)
	ok := Run{ID: "run_2", Pipeline: "backtest", Repo: "tulip", Status: "succeeded", Output: `{"summary":"CAGR table"}`}
	n.RunFinished(context.Background(), ok, false)
	if len(*msgs) != 0 {
		t.Fatalf("success without opt-in must be silent, got %d", len(*msgs))
	}
	n.RunFinished(context.Background(), ok, true)
	if len(*msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(*msgs))
	}
	m := (*msgs)[0]
	if m.h.Get("Markdown") != "yes" || !strings.Contains(m.body, "```\nCAGR table\n```") || m.h.Get("Click") != "" {
		t.Errorf("message = %+v", m)
	}
}

func TestCanceledIsSilentAndNilIsSafe(t *testing.T) {
	srv, msgs := fakeNtfy(t, 200)
	New(srv.URL, "petal", "", quiet).RunFinished(context.Background(), Run{ID: "r", Status: "canceled"}, true)
	if len(*msgs) != 0 {
		t.Errorf("canceled runs must not notify")
	}
	var off *Notifier = New("", "petal", "", quiet) // notifications disabled
	off.RunFinished(context.Background(), Run{ID: "r", Status: "failed"}, false)
}

func TestServerErrorIsSwallowed(t *testing.T) {
	srv, msgs := fakeNtfy(t, 500)
	New(srv.URL, "petal", "", quiet).RunFinished(context.Background(), Run{ID: "r", Pipeline: "p", Status: "failed"}, false)
	if len(*msgs) != 1 {
		t.Errorf("one attempt, no retry storm: %d", len(*msgs))
	}
}

func TestTruncateKeepsFenceAndUTF8(t *testing.T) {
	s := "```\n" + strings.Repeat("é", 3000) + "\n```"
	out := truncate(s, 1000)
	if len(out) > 1000 || strings.Count(out, "```")%2 != 0 || !strings.Contains(out, "truncated") {
		t.Errorf("len=%d fences=%d", len(out), strings.Count(out, "```"))
	}
	if !strings.HasSuffix(strings.TrimSuffix(out, "\n```"), "(truncated)") {
		t.Errorf("tail = %q", out[len(out)-30:])
	}
}

func TestFormatOutput(t *testing.T) {
	for in, want := range map[string]string{
		`{"summary":"x"}`:   "x",
		`{"b":"2","a":"1"}`: "a:\n1\n\nb:\n2",
		"not json":          "not json",
	} {
		if got := FormatOutput(in); got != want {
			t.Errorf("FormatOutput(%s) = %q, want %q", in, got, want)
		}
	}
}
