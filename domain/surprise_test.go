package domain

import "testing"

// TestSurpriseFrom_IsSymmetricAboutExpectation is the property that distinguishes a
// SURPRISE gate from a FAILURE gate: a confident agent that fails and a distrusted one
// that succeeds are equally informative, and both must score high.
func TestSurpriseFrom_IsSymmetricAboutExpectation(t *testing.T) {
	confidentButFailed := SurpriseFrom(0.9, false) // expected to succeed, didn't
	distrustedButWorked := SurpriseFrom(0.1, true) // expected to fail, did fine
	if confidentButFailed != distrustedButWorked {
		t.Errorf("surprise must be symmetric: %.2f vs %.2f", confidentButFailed, distrustedButWorked)
	}
	if confidentButFailed < 0.8 {
		t.Errorf("a badly mispredicted outcome must score high, got %.2f", confidentButFailed)
	}
}

// A routine failure by an agent already known to fail teaches nothing, and is exactly
// what a naive `1 - quality` failure gate would over-record.
func TestSurpriseFrom_RoutineOutcomesScoreLow(t *testing.T) {
	routineFailure := SurpriseFrom(0.05, false)
	routineSuccess := SurpriseFrom(0.95, true)
	for name, got := range map[string]float64{"failure": routineFailure, "success": routineSuccess} {
		if got > 0.2 {
			t.Errorf("routine %s must score low, got %.2f", name, got)
		}
	}
	if routineFailure > SurpriseFrom(0.9, false) {
		t.Error("a routine failure must be LESS surprising than an unexpected one — " +
			"otherwise this is a failure gate wearing a surprise gate's name")
	}
}

func TestSurpriseFrom_StaysInUnitRange(t *testing.T) {
	for _, e := range []float64{-5, 0, 0.5, 1, 7} {
		for _, ok := range []bool{true, false} {
			if got := SurpriseFrom(e, ok); got < 0 || got > 1 {
				t.Errorf("SurpriseFrom(%v,%v)=%v out of [0,1]", e, ok, got)
			}
		}
	}
}
