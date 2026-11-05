package config

import "testing"

// TestDefaults_ExperienceArms pins the experiential-record default. The raw capture
// path (`experiential_memory_enabled`) and procedure induction
// (`procedure_induction_interval_hours`) were deleted on 2026-11-05 with scenes
// (decisions/scenes-cancelled.md); TestRemovedExecutionKeys_AbsentFromSchema pins that
// their keys stay gone.
func TestDefaults_ExperienceArms(t *testing.T) {
	cfg := DefaultConfig()

	// ON: the abstraction-only experiential records (ADR-0049 A2.2, ADR-0095): action
	// events and the plan-outcome hook, no raw payload, behind the A2.4 lane guard.
	if !cfg.Execution.Memory.ExperienceRecordsEnabled {
		t.Error("experience_records_enabled must default TRUE (owner decision 2026-07-28)")
	}

	// The gates the enabled arm depends on must be sane, or "on" means something else.
	if cfg.Execution.Memory.ExperienceSurpriseFloor <= 0 || cfg.Execution.Memory.ExperienceSurpriseFloor > 1 {
		t.Errorf("surprise floor must be a usable probability, got %v",
			cfg.Execution.Memory.ExperienceSurpriseFloor)
	}
}
