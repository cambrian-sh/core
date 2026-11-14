package config

import "testing"

// TestDefaults_ExperienceArms pins the experiential-record default. The raw capture
// path (`experiential_memory_enabled`) and procedure induction
// (`procedure_induction_interval_hours`) were deleted on 2026-11-05 with scenes
// (decisions/scenes-cancelled.md); TestRemovedExecutionKeys_AbsentFromSchema pins that
// their keys stay gone.
func TestDefaults_ExperienceArms(t *testing.T) {
	cfg := DefaultConfig()

	// ON: the plan-outcome hook (ADR-0049 A2.2, ADR-0095): episode open/close and
	// procedure feedback. The action events and the A2.3 failure record it once also
	// gated were stopped 2026-11-14 (owner decision, ENG-1595), and their surprise
	// floor key retired.
	if !cfg.Execution.Memory.ExperienceRecordsEnabled {
		t.Error("experience_records_enabled must default TRUE (owner decision 2026-07-28)")
	}
}
