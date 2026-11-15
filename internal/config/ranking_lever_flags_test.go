package config

import "testing"

// ENG-484 / ENG-432 (owner decision 2026-11-15): two ranking levers ship dark, each
// behind its own read flag defaulting OFF, measured later in the benchmark phase.
// Each flag must default off, load from env under its documented key, and change
// the ADR-0103 D7 retrieval fingerprint only when it can act — every existing
// fingerprint stays what it was.
func TestRankingLeverFlags_DefaultsEnvFingerprint(t *testing.T) {
	d := DefaultConfig().Execution
	if d.Retrieval.RecallSpreadingKeepAssemblyOrder || d.Facts.LaneChunkCosineMax {
		t.Fatalf("both levers must default OFF: keep_order=%v cosine_max=%v",
			d.Retrieval.RecallSpreadingKeepAssemblyOrder, d.Facts.LaneChunkCosineMax)
	}

	dir := baseBundle(t)
	t.Setenv("CAMBRIAN_EXECUTION__RETRIEVAL__RECALL_SPREADING_KEEP_ASSEMBLY_ORDER", "true")
	t.Setenv("CAMBRIAN_EXECUTION__FACTS__LANE_CHUNK_COSINE_MAX", "true")
	cfg, _ := loadFrom(t, dir, nil)
	if !cfg.Execution.Retrieval.RecallSpreadingKeepAssemblyOrder || !cfg.Execution.Facts.LaneChunkCosineMax {
		t.Fatalf("env did not reach the flags: %+v / %+v", cfg.Execution.Retrieval.RecallSpreadingKeepAssemblyOrder, cfg.Execution.Facts.LaneChunkCosineMax)
	}

	base := DefaultConfig().Execution
	fp := base.RetrievalFingerprint("e")

	keep := DefaultConfig().Execution
	keep.Retrieval.RecallSpreadingKeepAssemblyOrder = true
	if keep.RetrievalFingerprint("e") == fp {
		t.Fatal("keep_assembly_order changes the window order, so it must change the fingerprint")
	}
	keepNoSpread := keep
	keepNoSpread.Retrieval.RecallSpreadingEnabled = false
	noSpread := DefaultConfig().Execution
	noSpread.Retrieval.RecallSpreadingEnabled = false
	if keepNoSpread.RetrievalFingerprint("e") != noSpread.RetrievalFingerprint("e") {
		t.Fatal("without spreading the order flag cannot act and must not change the fingerprint")
	}

	cmax := DefaultConfig().Execution
	cmax.Facts.LaneChunkCosineMax = true
	if !cmax.Facts.LaneEnabled {
		t.Fatal("precondition: the shipped fact lane is on (ENG-1496)")
	}
	if cmax.RetrievalFingerprint("e") == fp {
		t.Fatal("lane_chunk_cosine_max changes Stage A, so it must change the fingerprint")
	}
	cmaxNoLane := cmax
	cmaxNoLane.Facts.LaneEnabled = false
	noLane := DefaultConfig().Execution
	noLane.Facts.LaneEnabled = false
	if cmaxNoLane.RetrievalFingerprint("e") != noLane.RetrievalFingerprint("e") {
		t.Fatal("without the lane the cosine-max flag cannot act and must not change the fingerprint")
	}
}
