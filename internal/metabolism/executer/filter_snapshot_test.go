package executer

// Tests for filterSnapshotForStep — the DependsOn-based context filter (ADR-0022 Phase 0).
//
// Design principle: these tests assert on what keys appear in the snapshot
// passed to a step. They do NOT test how the map is iterated internally.
// A test that breaks on rename but not on behaviour change is a bad test.

import (
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// master is a helper that builds a realistic masterContext map for tests.
// Accepts alternating key/value pairs.
func master(kvs ...string) map[string]string {
	m := make(map[string]string, len(kvs)/2)
	for i := 0; i+1 < len(kvs); i += 2 {
		m[kvs[i]] = kvs[i+1]
	}
	return m
}

// ── Tracer bullet ─────────────────────────────────────────────────────────────

// TestFilterSnapshot_EmptyDependsOn: a step with no declared dependencies
// must receive no step_N_result keys — the planner says it needs nothing.
func TestFilterSnapshot_EmptyDependsOn(t *testing.T) {
	m := master(
		"step_0_result", "result of step 0",
		"step_1_result", "result of step 1",
		"initialContext", "user query",
	)
	s := domain.Step{Query: "q", DependsOn: nil}

	got := filterSnapshotForStep(m, s)

	if _, ok := got["step_0_result"]; ok {
		t.Error("step_0_result must not pass when DependsOn is empty")
	}
	if _, ok := got["step_1_result"]; ok {
		t.Error("step_1_result must not pass when DependsOn is empty")
	}
	// Non-step keys must always pass through.
	if got["initialContext"] != "user query" {
		t.Error("initialContext must always pass through")
	}
}

// ── Single declared dependency ─────────────────────────────────────────────

func TestFilterSnapshot_SingleDependency(t *testing.T) {
	m := master(
		"step_0_result", "step 0 output",
		"step_1_result", "step 1 output",
		"step_2_result", "step 2 output",
	)
	// Step 3 declares it only needs step 1.
	s := domain.Step{Query: "q", DependsOn: []int{1}}

	got := filterSnapshotForStep(m, s)

	if got["step_1_result"] != "step 1 output" {
		t.Error("declared dependency step_1_result must pass through")
	}
	if _, ok := got["step_0_result"]; ok {
		t.Error("undeclared step_0_result must not pass through")
	}
	if _, ok := got["step_2_result"]; ok {
		t.Error("undeclared step_2_result must not pass through")
	}
}

// ── Multiple declared dependencies ────────────────────────────────────────

func TestFilterSnapshot_MultipleDependencies(t *testing.T) {
	m := master(
		"step_0_result", "a",
		"step_1_result", "b",
		"step_2_result", "c",
		"step_3_result", "d",
	)
	// Step 4 depends on steps 0 and 2 only.
	s := domain.Step{Query: "q", DependsOn: []int{0, 2}}

	got := filterSnapshotForStep(m, s)

	if got["step_0_result"] != "a" {
		t.Error("step_0_result must pass — declared in DependsOn")
	}
	if got["step_2_result"] != "c" {
		t.Error("step_2_result must pass — declared in DependsOn")
	}
	if _, ok := got["step_1_result"]; ok {
		t.Error("step_1_result must not pass — not in DependsOn")
	}
	if _, ok := got["step_3_result"]; ok {
		t.Error("step_3_result must not pass — not in DependsOn")
	}
}

// ── Agent metadata keys are always stripped ────────────────────────────────

// step_N_{k} keys are added by agents to annotate their output. They must
// never appear in downstream snapshots even when step N is in DependsOn.
// Injecting them caused context pollution (ADR-0022 Q3 decision).
func TestFilterSnapshot_AgentMetadataStripped(t *testing.T) {
	m := master(
		"step_0_result", "main output",
		"step_0_confidence", "0.92",   // agent-added metadata
		"step_0_model", "qwen3:8b",    // agent-added metadata
		"step_0_checkpoint", "passed", // checkpoint key — also stripped
	)
	s := domain.Step{Query: "q", DependsOn: []int{0}}

	got := filterSnapshotForStep(m, s)

	if got["step_0_result"] != "main output" {
		t.Error("step_0_result must pass — declared in DependsOn")
	}
	if _, ok := got["step_0_confidence"]; ok {
		t.Error("step_0_confidence (agent metadata) must be stripped")
	}
	if _, ok := got["step_0_model"]; ok {
		t.Error("step_0_model (agent metadata) must be stripped")
	}
	if _, ok := got["step_0_checkpoint"]; ok {
		t.Error("step_0_checkpoint must be stripped")
	}
}

// ── Non-step keys always pass through ─────────────────────────────────────

func TestFilterSnapshot_NonStepKeysAlwaysPass(t *testing.T) {
	m := master(
		"step_0_result", "step output",
		"initialContext", "user query",
		"ltm_doc_1", "long-term memory doc",
		"ltm_doc_2", "another ltm doc",
		"substrate_session_id", "sess-abc",
	)
	// Step with no dependencies — but non-step keys must still pass.
	s := domain.Step{Query: "q", DependsOn: nil}

	got := filterSnapshotForStep(m, s)

	for _, key := range []string{"initialContext", "ltm_doc_1", "ltm_doc_2", "substrate_session_id"} {
		if got[key] != m[key] {
			t.Errorf("non-step key %q must always pass through, got %q want %q", key, got[key], m[key])
		}
	}
	if _, ok := got["step_0_result"]; ok {
		t.Error("step_0_result must not pass when DependsOn is empty")
	}
}

// ── Diamond dependency ─────────────────────────────────────────────────────

// A → C and B → C: step C must see both A and B.
func TestFilterSnapshot_DiamondDependency(t *testing.T) {
	m := master(
		"step_0_result", "A output",
		"step_1_result", "B output",
	)
	// Step 2 (C) depends on both 0 (A) and 1 (B).
	s := domain.Step{Query: "q", DependsOn: []int{0, 1}}

	got := filterSnapshotForStep(m, s)

	if got["step_0_result"] != "A output" {
		t.Error("step_0_result must pass in diamond — A→C edge declared")
	}
	if got["step_1_result"] != "B output" {
		t.Error("step_1_result must pass in diamond — B→C edge declared")
	}
}

// ── Independent parallel steps ─────────────────────────────────────────────

// Two steps with no dependencies must not see each other's results.
func TestFilterSnapshot_IndependentParallelSteps(t *testing.T) {
	// Both step 0 and step 1 have already completed and are in masterContext.
	m := master(
		"step_0_result", "output of step 0",
		"step_1_result", "output of step 1",
	)

	// Step 2 is independent (DependsOn = nil) — must see neither.
	s := domain.Step{Query: "q", DependsOn: nil}
	got := filterSnapshotForStep(m, s)

	if _, ok := got["step_0_result"]; ok {
		t.Error("independent step must not see step_0_result")
	}
	if _, ok := got["step_1_result"]; ok {
		t.Error("independent step must not see step_1_result")
	}
}

// ── Empty master context ───────────────────────────────────────────────────

func TestFilterSnapshot_EmptyMaster(t *testing.T) {
	got := filterSnapshotForStep(map[string]string{}, domain.Step{Query: "q", DependsOn: []int{0}})
	if len(got) != 0 {
		t.Errorf("empty master should produce empty snapshot, got %v", got)
	}
}

// ── Snapshot is a copy — mutations do not affect masterContext ─────────────

// filterSnapshotForStep must return a new map. The caller owns masterContext;
// the step owns the snapshot. Modifying one must not affect the other.
func TestFilterSnapshot_ReturnedMapIsACopy(t *testing.T) {
	m := master(
		"step_0_result", "original",
		"initialContext", "ctx",
	)
	s := domain.Step{Query: "q", DependsOn: []int{0}}

	got := filterSnapshotForStep(m, s)
	got["step_0_result"] = "mutated"
	got["injected_key"] = "injected"

	if m["step_0_result"] != "original" {
		t.Error("mutating snapshot must not affect masterContext")
	}
	if _, ok := m["injected_key"]; ok {
		t.Error("adding key to snapshot must not affect masterContext")
	}
}
