package config

import "testing"

// TestDefaults_ExperienceArms pins the three-way distinction that "turn experiential
// memory on" could otherwise blur. These are three different levers with three different
// risk profiles, and only one of them is on.
func TestDefaults_ExperienceArms(t *testing.T) {
	cfg := DefaultConfig()

	// ON: the abstraction-only outcome record (ADR-0049 A2.2). One structured record per
	// plan, no raw payload, behind the A2.4 lane guard.
	if !cfg.Execution.Memory.ExperienceRecordsEnabled {
		t.Error("experience_records_enabled must default TRUE (owner decision 2026-07-28)")
	}

	// OFF, PERMANENTLY: the raw path removed on 2026-07-18 — whole tool payloads
	// embedded as single vectors, which overflowed the embedder and polluted recall.
	// If this test ever fails, the July removal has been silently undone.
	if cfg.Execution.Memory.ExperientialMemoryEnabled {
		t.Error("experiential_memory_enabled (the RAW path) must stay false — it is the " +
			"design that was removed, not the one that was rebuilt")
	}

	// OFF: the procedural induction scheduler. It cannot produce anything until plans
	// accumulate under the capability_contract arm, so running it is pure cost.
	if cfg.Execution.Procedure.ProcedureInductionIntervalHours != 0 {
		t.Errorf("procedure induction must stay disabled until there is something to "+
			"induce from, got interval %d", cfg.Execution.Procedure.ProcedureInductionIntervalHours)
	}

	// The gates the enabled arm depends on must be sane, or "on" means something else.
	if cfg.Execution.Memory.ExperienceSurpriseFloor <= 0 || cfg.Execution.Memory.ExperienceSurpriseFloor > 1 {
		t.Errorf("surprise floor must be a usable probability, got %v",
			cfg.Execution.Memory.ExperienceSurpriseFloor)
	}
}
