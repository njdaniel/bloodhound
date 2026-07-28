package orchestrator

import (
	"strings"
	"testing"
)

func TestV0Walk(t *testing.T) {
	got, err := V0().Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	want := []Phase{PhaseIntake, PhaseInvestigate, PhaseReport}
	if len(got) != len(want) {
		t.Fatalf("walk = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("walk = %v, want %v", got, want)
		}
	}
	for _, reserved := range []Phase{PhasePlan, PhaseVerify} {
		if _, ok := V0().Next[reserved]; ok {
			t.Errorf("v0 visits %q, which is reserved for M2", reserved)
		}
	}
}

// TestPipelineIsData is the property M2 depends on: inserting a phase is a
// change to the table, not to the walk logic.
func TestPipelineIsData(t *testing.T) {
	p := V0()
	p.Version = "v1-test"
	p.Next[PhaseIntake] = PhasePlan
	p.Next[PhasePlan] = PhaseInvestigate

	got, err := p.Walk()
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	want := []Phase{PhaseIntake, PhasePlan, PhaseInvestigate, PhaseReport}
	if len(got) != len(want) {
		t.Fatalf("walk = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("walk = %v, want %v", got, want)
		}
	}
}

func TestPipelineWalkRejectsMalformedTables(t *testing.T) {
	tests := map[string]struct {
		pipeline Pipeline
		wantErr  string
	}{
		"no start": {
			pipeline: Pipeline{Version: "vx", Next: map[Phase]Phase{PhaseIntake: PhaseDone}},
			wantErr:  "no start phase",
		},
		"missing successor": {
			pipeline: Pipeline{Version: "vx", Start: PhaseIntake, Next: map[Phase]Phase{}},
			wantErr:  "no successor",
		},
		"cycle": {
			pipeline: Pipeline{Version: "vx", Start: PhaseIntake, Next: map[Phase]Phase{
				PhaseIntake:      PhaseInvestigate,
				PhaseInvestigate: PhaseIntake,
			}},
			wantErr: "revisited",
		},
		"start is done": {
			pipeline: Pipeline{Version: "vx", Start: PhaseDone},
			wantErr:  "already done",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := tt.pipeline.Walk()
			if err == nil {
				t.Fatal("Walk succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}
