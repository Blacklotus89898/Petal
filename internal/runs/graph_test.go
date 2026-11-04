package runs

import (
	"strings"
	"testing"
)

func ev(kind, payload string) Event {
	return Event{Kind: kind, Payload: payload, TS: "2026-11-03T12:00:00Z"}
}

// summary renders "name:status<-needs" per step, in graph order.
func summary(g Graph) string {
	var parts []string
	for _, s := range g.Steps {
		p := s.Name + ":" + s.Status
		if len(s.Needs) > 0 {
			p += "<-" + strings.Join(s.Needs, "+")
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " ")
}

const planABC = `{"dag":true,"max_parallel":2,"steps":[{"name":"a","needs":[]},{"name":"b","needs":[]},{"name":"c","needs":["a","b"]}]}`

func TestBuildGraph(t *testing.T) {
	cases := []struct {
		name   string
		status string
		events []Event
		want   string
	}{
		{"planned, nothing started yet", StatusRunning,
			[]Event{ev("plan", planABC)},
			"a:pending b:pending c:pending<-a+b"},
		{"roots running in parallel", StatusRunning,
			[]Event{ev("plan", planABC), ev("step.started", `{"step":"a"}`), ev("step.started", `{"step":"b"}`)},
			"a:running b:running c:pending<-a+b"},
		{"all green", StatusSucceeded,
			[]Event{ev("plan", planABC),
				ev("step.started", `{"step":"a"}`), ev("step.started", `{"step":"b"}`),
				ev("step.finished", `{"step":"a","exit":0}`), ev("step.finished", `{"step":"b","exit":0}`),
				ev("step.started", `{"step":"c"}`), ev("step.finished", `{"step":"c","exit":0}`)},
			"a:succeeded b:succeeded c:succeeded<-a+b"},
		{"fail-fast: sibling canceled, dependent skipped", StatusFailed,
			[]Event{ev("plan", planABC),
				ev("step.started", `{"step":"a"}`), ev("step.started", `{"step":"b"}`),
				ev("step.finished", `{"step":"a","exit":3,"error":"step \"a\" failed (exit 3)"}`),
				ev("step.finished", `{"step":"b","canceled":true,"cause":"another step failed"}`),
				ev("step.skipped", `{"step":"c"}`)},
			"a:failed b:canceled c:skipped<-a+b"},
		{"step timeout is a failure", StatusFailed,
			[]Event{ev("plan", planABC), ev("step.started", `{"step":"a"}`),
				ev("step.finished", `{"step":"a","exit":-1,"error":"step \"a\" timed out after 1s","timed_out":true}`)},
			"a:failed b:skipped c:skipped<-a+b"},
		{"interrupted by a restart: running step ends canceled", StatusFailed,
			[]Event{ev("plan", planABC), ev("step.started", `{"step":"a"}`), ev("interrupted", `{}`)},
			"a:canceled b:skipped c:skipped<-a+b"},
		{"canceled while queued-ish: pending steps skipped", StatusCanceled,
			[]Event{ev("plan", planABC)},
			"a:skipped b:skipped c:skipped<-a+b"},
		{"checkout before the plan keeps its state and position", StatusRunning,
			[]Event{ev("step.started", `{"step":"petal-checkout","image":"alpine/git:x"}`),
				ev("step.finished", `{"step":"petal-checkout","exit":0}`),
				ev("plan", `{"dag":false,"max_parallel":1,"steps":[{"name":"petal-checkout","needs":[]},{"name":"x","needs":["petal-checkout"]}]}`)},
			"petal-checkout:succeeded x:pending<-petal-checkout"},
		{"no plan (old run): started steps in order, no edges", StatusSucceeded,
			[]Event{ev("step.started", `{"step":"one"}`), ev("step.finished", `{"step":"one","exit":0}`),
				ev("step.started", `{"step":"two"}`), ev("step.finished", `{"step":"two","exit":0}`)},
			"one:succeeded two:succeeded"},
		{"checkout failed before any plan", StatusFailed,
			[]Event{ev("step.started", `{"step":"petal-checkout"}`), ev("step.finished", `{"step":"petal-checkout","exit":128,"error":"checkout failed"}`)},
			"petal-checkout:failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := BuildGraph(Run{Status: tc.status}, tc.events)
			if got := summary(g); got != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestBuildGraphPlannedFlagAndTiming(t *testing.T) {
	g := BuildGraph(Run{Status: StatusRunning}, []Event{
		ev("plan", planABC),
		{Kind: "step.started", Payload: `{"step":"a","image":"alpine:3.21"}`, TS: "t1"},
		{Kind: "step.finished", Payload: `{"step":"a","exit":0}`, TS: "t2"},
	})
	if !g.Planned || !g.DAG || g.MaxParallel != 2 {
		t.Fatalf("graph header = %+v", g)
	}
	a := g.Steps[0]
	if a.StartedAt != "t1" || a.FinishedAt != "t2" || a.Exit == nil || *a.Exit != 0 || a.Image != "alpine:3.21" {
		t.Errorf("step a = %+v", a)
	}
	if empty := BuildGraph(Run{Status: StatusQueued}, nil); empty.Planned || empty.Steps == nil {
		t.Errorf("no events: want unplanned graph with a non-nil (JSON []) step list, got %+v", empty)
	}
}
