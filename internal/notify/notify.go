// Package notify pushes run outcomes to ntfy (ADR 0008): every failed run,
// and the outputs of successful runs that Petal's config opts in. Delivery is
// best-effort and never affects a run — errors are logged, nothing retried.
package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// maxBody keeps a message under ntfy's 4096-byte limit (larger bodies would be
// turned into attachments, which the homelab ntfy has disabled).
const maxBody = 3800

// Run is what a notification needs to know about a finished run.
type Run struct {
	ID, Pipeline, Repo, Status, Error, Output string
}

// Notifier posts to one ntfy topic. The zero value / nil is a no-op.
type Notifier struct {
	url, topic, publicURL string
	client                *http.Client
	log                   *slog.Logger
}

// New returns a notifier, or nil when baseURL is empty (notifications off).
func New(baseURL, topic, publicURL string, log *slog.Logger) *Notifier {
	if baseURL == "" || topic == "" {
		return nil
	}
	return &Notifier{
		url: strings.TrimRight(baseURL, "/"), topic: topic, publicURL: strings.TrimRight(publicURL, "/"),
		client: &http.Client{Timeout: 5 * time.Second}, log: log,
	}
}

// RunFinished notifies on a failed run, and on a successful one when
// notifySuccess is set (the run's outputs are the message). Canceled runs
// are someone's deliberate action: no message.
func (n *Notifier) RunFinished(ctx context.Context, r Run, notifySuccess bool) {
	if n == nil {
		return
	}
	where := r.Pipeline
	if r.Repo != "" {
		where = r.Repo + "/" + r.Pipeline
	}
	switch {
	case r.Status == "failed":
		body := r.Error
		if body == "" {
			body = "run failed"
		}
		n.post(ctx, "Petal: "+where+" failed", "high", "x,petal", body+"\n\nrun "+r.ID, false, r.ID)
	case r.Status == "succeeded" && notifySuccess:
		body := "run " + r.ID + " succeeded"
		if out := strings.TrimSpace(FormatOutput(r.Output)); out != "" {
			body = "```\n" + out + "\n```"
		}
		n.post(ctx, "Petal: "+where+" succeeded", "default", "white_check_mark,petal", body, true, r.ID)
	}
}

func (n *Notifier) post(ctx context.Context, title, priority, tags, body string, markdown bool, runID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	body = truncate(body, maxBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url+"/"+n.topic, strings.NewReader(body))
	if err != nil {
		n.log.Warn("ntfy request", "err", err)
		return
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("Title", asciiOnly(title))
	req.Header.Set("Priority", priority)
	req.Header.Set("Tags", tags)
	if markdown {
		req.Header.Set("Markdown", "yes")
	}
	if n.publicURL != "" {
		req.Header.Set("Click", n.publicURL+"/?run="+runID)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		n.log.Warn("ntfy post", "run", runID, "err", err)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		n.log.Warn("ntfy post", "run", runID, "status", resp.StatusCode)
		return
	}
	n.log.Info("ntfy sent", "run", runID, "title", title)
}

// truncate cuts s to at most max bytes on a rune boundary, closing an open
// code fence so the markdown still renders.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max - 32
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	out := s[:cut] + "\n… (truncated)"
	if strings.Count(out, "```")%2 == 1 {
		out += "\n```"
	}
	return out
}

// asciiOnly keeps HTTP header values safe (ntfy titles go in a header).
func asciiOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}

// FormatOutput renders a run's output event (a JSON map of output name →
// contents, ADR 0002) for humans: one output is shown bare, several get a
// "name:" line each in name order. Anything else is shown as-is.
func FormatOutput(raw string) string {
	var m map[string]string
	if json.Unmarshal([]byte(raw), &m) != nil || len(m) == 0 {
		return raw
	}
	if len(m) == 1 {
		for _, v := range m {
			return v
		}
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		b.WriteString(k + ":\n" + m[k] + "\n\n")
	}
	return strings.TrimSpace(b.String())
}
