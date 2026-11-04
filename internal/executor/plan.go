package executor

import "github.com/Blacklotus89898/Petal/internal/pipeline"

// planStep is one node of a run's step graph as recorded in the "plan" event.
// Edges are explicit (needs) in both modes so readers never re-derive them:
// file-order pipelines become a chain, and a multi-repo run's checkout is the
// root every other root hangs off.
type planStep struct {
	Name   string   `json:"name"`
	Needs  []string `json:"needs"`
	Image  string   `json:"image,omitempty"`
	Uses   string   `json:"uses,omitempty"`
	Docker bool     `json:"docker,omitempty"`
}

// planPayload is the "plan" event: the resolved step graph of this run. No env,
// run scripts or secret values — names and images only (ADR 0006).
func planPayload(spec *pipeline.Pipeline, checkout bool) map[string]any {
	steps := make([]planStep, 0, len(spec.Steps)+1)
	var rootNeeds []string
	if checkout {
		steps = append(steps, planStep{Name: pipeline.CheckoutStep, Needs: []string{}, Image: checkoutImage})
		rootNeeds = []string{pipeline.CheckoutStep}
	}
	for i, st := range spec.Steps {
		var needs []string
		switch {
		case spec.DAG && len(st.Needs) > 0:
			needs = append([]string{}, st.Needs...)
		case !spec.DAG && i > 0:
			needs = []string{spec.Steps[i-1].Name}
		default: // a root
			needs = append([]string{}, rootNeeds...)
		}
		steps = append(steps, planStep{Name: st.Name, Needs: needs, Image: st.Image, Uses: st.Uses, Docker: st.Docker})
	}
	parallel := 1 // file order runs one step at a time whatever max_parallel says
	if spec.DAG {
		parallel = spec.MaxParallel
	}
	return map[string]any{"dag": spec.DAG, "max_parallel": parallel, "steps": steps}
}
