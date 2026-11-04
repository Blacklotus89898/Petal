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

	steps, err := s.store.StepStats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	b.WriteString("# HELP petal_steps_total Step outcomes recorded in the registry (succeeded, failed, canceled, skipped).\n")
	b.WriteString("# TYPE petal_steps_total counter\n")
	for _, st := range steps {
		fmt.Fprintf(&b, "petal_steps_total{pipeline=\"%s\",repo=\"%s\",step=\"%s\",status=\"%s\"} %d\n",
			lbl(st.Pipeline), lbl(repoLabel(st.Repo)), lbl(st.Step), lbl(st.Status), st.Count)
	}
	b.WriteString("# HELP petal_step_duration_seconds Wall time of finished steps (started to finished).\n")
	b.WriteString("# TYPE petal_step_duration_seconds summary\n")
	for _, st := range steps {
		if st.Status == "skipped" {
			continue
		}
		l := fmt.Sprintf("pipeline=\"%s\",repo=\"%s\",step=\"%s\",status=\"%s\"", lbl(st.Pipeline), lbl(repoLabel(st.Repo)), lbl(st.Step), lbl(st.Status))
		fmt.Fprintf(&b, "petal_step_duration_seconds_sum{%s} %g\n", l, st.DurationSum)
		fmt.Fprintf(&b, "petal_step_duration_seconds_count{%s} %d\n", l, st.Count)
	}

	b.WriteString("# HELP petal_runs_queued Runs waiting for a worker.\n# TYPE petal_runs_queued gauge\n")
	fmt.Fprintf(&b, "petal_runs_queued %d\n", queued)
	b.WriteString("# HELP petal_runs_running Runs currently executing.\n# TYPE petal_runs_running gauge\n")
	fmt.Fprintf(&b, "petal_runs_running %d\n", running)
	b.WriteString("# HELP petal_schedules Cron schedules by state.\n# TYPE petal_schedules gauge\n")
	fmt.Fprintf(&b, "petal_schedules{state=\"enabled\"} %d\npetal_schedules{state=\"disabled\"} %d\n", enabled, disabled)
	if sz, err := s.store.Size(r.Context()); err == nil {
		b.WriteString("# HELP petal_db_bytes Size of the SQLite registry file (pages x page size; WAL excluded).\n# TYPE petal_db_bytes gauge\n")
		fmt.Fprintf(&b, "petal_db_bytes %d\n", sz.Bytes)
		b.WriteString("# HELP petal_db_rows Rows in the registry, by table.\n# TYPE petal_db_rows gauge\n")
		fmt.Fprintf(&b, "petal_db_rows{table=\"runs\"} %d\npetal_db_rows{table=\"events\"} %d\n", sz.Runs, sz.Events)
	}
	if s.retention != nil {
		b.WriteString("# HELP petal_retention_pruned_runs_total Runs deleted by the retention janitor since start (ADR 0009).\n# TYPE petal_retention_pruned_runs_total counter\n")
		fmt.Fprintf(&b, "petal_retention_pruned_runs_total %d\n", s.retention.Pruned())
		if t := s.retention.LastSweep(); !t.IsZero() {
			b.WriteString("# HELP petal_retention_last_sweep_timestamp_seconds When the janitor last completed a sweep.\n# TYPE petal_retention_last_sweep_timestamp_seconds gauge\n")
			fmt.Fprintf(&b, "petal_retention_last_sweep_timestamp_seconds %d\n", t.Unix())
		}
	}
	b.WriteString("# HELP petal_repos Repos in the multi-repo allowlist.\n# TYPE petal_repos gauge\n")
	fmt.Fprintf(&b, "petal_repos %d\n", len(s.repos))

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}
