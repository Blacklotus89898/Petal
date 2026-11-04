package runs

import "encoding/json"

// Step statuses in a run's step graph.
const (
	StepPending   = "pending"
	StepRunning   = "running"
	StepSucceeded = "succeeded"
	StepFailed    = "failed"
	StepCanceled  = "canceled"
	StepSkipped   = "skipped"
)

// StepState is one node of a run's step graph: the plan's edges plus what the
// audit events say happened to the step.
type StepState struct {
	Name       string   `json:"name"`
	Needs      []string `json:"needs"`
	Image      string   `json:"image,omitempty"`
	Uses       string   `json:"uses,omitempty"`
	Docker     bool     `json:"docker,omitempty"`
	Status     string   `json:"status"`
	StartedAt  string   `json:"started_at,omitempty"`
	FinishedAt string   `json:"finished_at,omitempty"`
	Exit       *int     `json:"exit,omitempty"`
	Error      string   `json:"error,omitempty"`
	TimedOut   bool     `json:"timed_out,omitempty"`
}

// Graph is a run's step graph. Planned=false means the run has no "plan" event
// (runs from before it existed, or a run that failed before its spec loaded):
// the steps are the ones that started, in order, with no edges.
type Graph struct {
	Planned     bool        `json:"planned"`
	DAG         bool        `json:"dag"`
	MaxParallel int         `json:"max_parallel,omitempty"`
	Steps       []StepState `json:"steps"`
}

type stepPayload struct {
	Step     string `json:"step"`
	Image    string `json:"image"`
	Exit     *int   `json:"exit"`
	Error    string `json:"error"`
	Canceled bool   `json:"canceled"`
	TimedOut bool   `json:"timed_out"`
}

type planPayload struct {
	DAG         bool        `json:"dag"`
	MaxParallel int         `json:"max_parallel"`
	Steps       []StepState `json:"steps"`
}

// BuildGraph folds a run's events (in append order) into its step graph. Once
// the run is finished nothing is left "running" or "pending": a step still
// running when the run ended was stopped (canceled), and one never started was
// skipped — whatever the reason (fail-fast, cancel, restart, a setup error).
func BuildGraph(run Run, events []Event) Graph {
	g := Graph{Steps: []StepState{}}
	byName := map[string]int{}
	get := func(name string) *StepState {
		if i, ok := byName[name]; ok {
			return &g.Steps[i]
		}
		byName[name] = len(g.Steps)
		g.Steps = append(g.Steps, StepState{Name: name, Needs: []string{}, Status: StepPending})
		return &g.Steps[len(g.Steps)-1]
	}

	for _, ev := range events {
		switch ev.Kind {
		case "plan":
			var p planPayload
			if json.Unmarshal([]byte(ev.Payload), &p) != nil {
				continue
			}
			g.Planned, g.DAG, g.MaxParallel = true, p.DAG, p.MaxParallel
			// Plan order first; steps seen before the plan (the checkout) keep
			// their state and move to their planned position.
			prev, prevIdx := g.Steps, byName
			g.Steps, byName = make([]StepState, 0, len(p.Steps)+len(prev)), map[string]int{}
			for _, ps := range p.Steps {
				st := ps
				if i, ok := prevIdx[ps.Name]; ok {
					st = prev[i]
					st.Needs, st.Uses, st.Docker = ps.Needs, ps.Uses, ps.Docker
					if st.Image == "" {
						st.Image = ps.Image
					}
				} else {
					st.Status = StepPending
				}
				if st.Needs == nil {
					st.Needs = []string{}
				}
				byName[st.Name] = len(g.Steps)
				g.Steps = append(g.Steps, st)
			}
			for _, st := range prev {
				if _, ok := byName[st.Name]; !ok {
					byName[st.Name] = len(g.Steps)
					g.Steps = append(g.Steps, st)
				}
			}
		case "step.started", "step.finished", "step.skipped":
			var p stepPayload
			if json.Unmarshal([]byte(ev.Payload), &p) != nil || p.Step == "" {
				continue
			}
			st := get(p.Step)
			switch ev.Kind {
			case "step.started":
				st.Status, st.StartedAt = StepRunning, ev.TS
				if p.Image != "" {
					st.Image = p.Image
				}
			case "step.finished":
				st.FinishedAt, st.Exit, st.Error, st.TimedOut = ev.TS, p.Exit, p.Error, p.TimedOut
				switch {
				case p.Canceled:
					st.Status = StepCanceled
				case p.Error != "" || (p.Exit != nil && *p.Exit != 0):
					st.Status = StepFailed
				default:
					st.Status = StepSucceeded
				}
			case "step.skipped":
				st.Status = StepSkipped
			}
		}
	}

	if run.Status == StatusSucceeded || run.Status == StatusFailed || run.Status == StatusCanceled {
		for i := range g.Steps {
			switch g.Steps[i].Status {
			case StepRunning:
				g.Steps[i].Status = StepCanceled
			case StepPending:
				g.Steps[i].Status = StepSkipped
			}
		}
	}
	return g
}
