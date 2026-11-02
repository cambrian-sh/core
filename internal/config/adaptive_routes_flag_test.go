package config

import "testing"

// ADR-0135 D4: adaptive decomposition routes ship default OFF until the gate passes.
func TestDefaultConfig_AgenticAdaptiveRoutesDisabled(t *testing.T) {
	if DefaultConfig().Execution.Retrieval.AgenticAdaptiveRoutesEnabled {
		t.Fatal("agentic_adaptive_routes_enabled must default OFF (ADR-0135 D4)")
	}
}

// The key lives in the Retrieval block (AGENTS.md §3.4) and reaches the field
// from a file and from its CAMBRIAN_* env var.
func TestLoadConfig_AgenticAdaptiveRoutesKey(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"schema_version":2,"execution":{"retrieval":{"agentic_adaptive_routes_enabled":true}}}`)
	cfg, _ := loadFrom(t, dir, nil)
	if !cfg.Execution.Retrieval.AgenticAdaptiveRoutesEnabled {
		t.Fatal("execution.retrieval.agentic_adaptive_routes_enabled in a file did not reach the field")
	}

	dir = baseBundle(t)
	t.Setenv("CAMBRIAN_EXECUTION__RETRIEVAL__AGENTIC_ADAPTIVE_ROUTES_ENABLED", "true")
	cfg, _ = loadFrom(t, dir, nil)
	if !cfg.Execution.Retrieval.AgenticAdaptiveRoutesEnabled {
		t.Fatal("CAMBRIAN_EXECUTION__RETRIEVAL__AGENTIC_ADAPTIVE_ROUTES_ENABLED did not reach the field")
	}
}

// Executing the planner's route changes which passes run, so it changes the
// fingerprint — but only where it can take effect: a flag-off configuration keeps
// the fingerprint it had before the flag existed.
func TestRetrievalFingerprint_AdaptiveRoutes(t *testing.T) {
	// Agentic retrieval is opted into explicitly: it is default OFF since the
	// 2026-11-02 owner decision, and the property under test is the fingerprint,
	// not the default.
	off := DefaultConfig().Execution
	off.Retrieval.AgenticRetrievalEnabled = true
	if !off.Retrieval.AgenticDecomposeEnabled {
		t.Fatal("test assumes decomposition is the default agentic mode")
	}
	on := off
	on.Retrieval.AgenticAdaptiveRoutesEnabled = true
	if on.RetrievalFingerprint("e") == off.RetrievalFingerprint("e") {
		t.Fatal("turning adaptive routes on must change the fingerprint")
	}
	inert := on
	inert.Retrieval.AgenticDecomposeEnabled = false
	noDecomp := off
	noDecomp.Retrieval.AgenticDecomposeEnabled = false
	if inert.RetrievalFingerprint("e") != noDecomp.RetrievalFingerprint("e") {
		t.Fatal("the flag cannot take effect without decomposition and must not change the fingerprint")
	}
}
