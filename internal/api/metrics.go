// Prometheus exposition (GET /metrics), hand-rolled to keep the binary
// dependency-free. Everything is derived from the run registry at scrape time.
package api

import (
	"fmt"
	"net/http"
	"strings"
)

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func lbl(v string) string { return labelEscaper.Replace(v) }

// repoLabel names the self repo explicitly so dashboards can group on it.
func repoLabel(r string) string {
	if r == "" {
		return "self"
	}
	return r
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.Stats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	enabled, disabled, err := s.store.ScheduleCounts(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var b strings.Builder
	b.WriteString("# HELP petal_runs_total Runs recorded in the registry, by pipeline, repo and current status.\n")
	b.WriteString("# TYPE petal_runs_total counter\n")
	var queued, running int64
	for _, st := range stats {
		fmt.Fprintf(&b, "petal_runs_total{pipeline=\"%s\",repo=\"%s\",status=\"%s\"} %d\n",
			lbl(st.Pipeline), lbl(repoLabel(st.Repo)), lbl(st.Status), st.Count)
		switch st.Status {
		case "queued":
			queued += st.Count
		case "running":
			running += st.Count
		}
	}

	b.WriteString("# HELP petal_run_duration_seconds Wall time of finished runs (started to finished).\n")
	b.WriteString("# TYPE petal_run_duration_seconds summary\n")
	for _, st := range stats {
		if st.Finished == 0 {
			continue
		}
		l := fmt.Sprintf("pipeline=\"%s\",repo=\"%s\",status=\"%s\"", lbl(st.Pipeline), lbl(repoLabel(st.Repo)), lbl(st.Status))
		fmt.Fprintf(&b, "petal_run_duration_seconds_sum{%s} %g\n", l, st.DurationSum)
		fmt.Fprintf(&b, "petal_run_duration_seconds_count{%s} %d\n", l, st.Finished)
	}

	b.WriteString("# HELP petal_run_last_finished_timestamp_seconds When the latest run of each pipeline/repo/status finished.\n")
	b.WriteString("# TYPE petal_run_last_finished_timestamp_seconds gauge\n")
	for _, st := range stats {
		if st.LastFinished == 0 {
			continue
		}
		fmt.Fprintf(&b, "petal_run_last_finished_timestamp_seconds{pipeline=\"%s\",repo=\"%s\",status=\"%s\"} %.3f\n",
			lbl(st.Pipeline), lbl(repoLabel(st.Repo)), lbl(st.Status), st.LastFinished)
	}

	b.WriteString("# HELP petal_runs_queued Runs waiting for a worker.\n# TYPE petal_runs_queued gauge\n")
	fmt.Fprintf(&b, "petal_runs_queued %d\n", queued)
	b.WriteString("# HELP petal_runs_running Runs currently executing.\n# TYPE petal_runs_running gauge\n")
	fmt.Fprintf(&b, "petal_runs_running %d\n", running)
	b.WriteString("# HELP petal_schedules Cron schedules by state.\n# TYPE petal_schedules gauge\n")
	fmt.Fprintf(&b, "petal_schedules{state=\"enabled\"} %d\npetal_schedules{state=\"disabled\"} %d\n", enabled, disabled)
	b.WriteString("# HELP petal_repos Repos in the multi-repo allowlist.\n# TYPE petal_repos gauge\n")
	fmt.Fprintf(&b, "petal_repos %d\n", len(s.repos))

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}
