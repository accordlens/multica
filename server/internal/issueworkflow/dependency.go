package issueworkflow

import (
	"fmt"

	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
)

// DependencyCondition names an authoritative stored fact. RunCompleted is
// deliberately separate from test/review acceptance: a completed agent may
// have delivered a failure report. Script gates belong to the script runner.
type DependencyCondition struct {
	Kind       string `json:"kind"`
	RunID      string `json:"run_id,omitempty"`
	SourceStep string `json:"source_step,omitempty"`
}

func (c DependencyCondition) Validate() error {
	if c.SourceStep != "" && !issuestatus.IsBuiltIn(c.SourceStep) {
		key, err := issuestatus.ValidateKey(c.SourceStep)
		if err != nil || key != c.SourceStep {
			return fmt.Errorf("source_step must be a workflow status key")
		}
	}

	switch c.Kind {
	case "done":
		if c.RunID != "" || c.SourceStep != "" {
			return fmt.Errorf("run fields require a run_completed condition")
		}
	case "in_review", "accepted":
		if c.RunID != "" {
			return fmt.Errorf("run fields require a run_completed condition")
		}
	case "run_completed":
		id, err := util.ParseUUID(c.RunID)
		if err != nil || !id.Valid {
			return fmt.Errorf("run_completed requires a run UUID")
		}
	default:
		return fmt.Errorf("condition kind must be done, in_review, accepted or run_completed")
	}
	return nil
}

type DependencyEdge struct{ Dependent, Prerequisite string }

type DependenciesUnmetError struct{}

func (*DependenciesUnmetError) Error() string {
	return "execution dependencies are not satisfied; inspect the issue's relations for the waiting condition"
}

// DependencyCycle returns the proposed cycle, including its closing vertex.
// Callers must serialize graph mutations while reading and validating edges.
func DependencyCycle(edges []DependencyEdge, proposed DependencyEdge) []string {
	adj := make(map[string][]string)
	for _, e := range edges {
		adj[e.Dependent] = append(adj[e.Dependent], e.Prerequisite)
	}
	queue := []string{proposed.Prerequisite}
	previous := map[string]string{proposed.Prerequisite: ""}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		if v == proposed.Dependent {
			path := []string{v}
			for p := previous[v]; p != ""; p = previous[p] {
				path = append(path, p)
			}
			out := []string{proposed.Dependent}
			for i := len(path) - 1; i >= 0; i-- {
				out = append(out, path[i])
			}
			return out
		}
		for _, next := range adj[v] {
			if _, seen := previous[next]; !seen {
				previous[next] = v
				queue = append(queue, next)
			}
		}
	}
	return nil
}
