package orchestrator

import "fmt"

// PipelineV0 names the M1 pipeline version recorded in checkpoints and case
// files. Bumping it is how M2 introduces the plan/verify walk without
// reinterpreting an in-flight case's checkpoints.
const PipelineV0 = "v0"

// Pipeline is a state-machine walk expressed as data: a start phase plus a
// transition table from each phase to its successor (spec 002 §4.1). The walk
// logic in this package reads the table and knows nothing else about phase
// ordering, so M2 inserts plan and verify by shipping a new table — not by
// editing the walk.
type Pipeline struct {
	// Version identifies the table, e.g. PipelineV0.
	Version string
	// Start is the first phase visited.
	Start Phase
	// Next maps each phase to its successor. The walk ends when it reaches
	// PhaseDone, which must not appear as a key.
	Next map[Phase]Phase
}

// V0 returns the M1 pipeline: intake → investigate → report → done. PhasePlan
// and PhaseVerify stay declared in the Phase enum but unvisited here.
func V0() Pipeline {
	return Pipeline{
		Version: PipelineV0,
		Start:   PhaseIntake,
		Next: map[Phase]Phase{
			PhaseIntake:      PhaseInvestigate,
			PhaseInvestigate: PhaseReport,
			PhaseReport:      PhaseDone,
		},
	}
}

// Walk returns the phases the pipeline visits, in order, excluding PhaseDone.
// It fails on a table that never reaches PhaseDone rather than looping: a
// malformed pipeline is a programming error, and hanging on one is worse than
// refusing it.
func (p Pipeline) Walk() ([]Phase, error) {
	if p.Start == "" {
		return nil, fmt.Errorf("pipeline %q: no start phase", p.Version)
	}
	phases := make([]Phase, 0, len(p.Next))
	seen := make(map[Phase]bool, len(p.Next))
	for phase := p.Start; phase != PhaseDone; {
		if seen[phase] {
			return nil, fmt.Errorf("pipeline %q: phase %q is revisited; the table must be acyclic", p.Version, phase)
		}
		seen[phase] = true
		next, ok := p.Next[phase]
		if !ok {
			return nil, fmt.Errorf("pipeline %q: phase %q has no successor", p.Version, phase)
		}
		phases = append(phases, phase)
		phase = next
	}
	if len(phases) == 0 {
		return nil, fmt.Errorf("pipeline %q: start phase is already done", p.Version)
	}
	return phases, nil
}
