package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStep_BackwardCompatibility_NilDependsOn(t *testing.T) {
	t.Parallel()
	raw := `{"required_tools":["search"],"query":"find results"}`
	var s Step
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if s.DependsOn != nil {
		t.Errorf("expected nil DependsOn for legacy step, got %v", s.DependsOn)
	}
}

func TestStep_DependsOn_RoundTrip(t *testing.T) {
	t.Parallel()
	s := Step{Query: "do work", DependsOn: []int{0, 2}}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var got Step
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(got.DependsOn) != 2 || got.DependsOn[0] != 0 || got.DependsOn[1] != 2 {
		t.Errorf("DependsOn round-trip failed: got %v", got.DependsOn)
	}
}

func TestStep_EmptyDependsOn_OmittedFromJSON(t *testing.T) {
	t.Parallel()
	s := Step{Query: "do work"}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if _, ok := raw["depends_on"]; ok {
		t.Error("expected depends_on to be omitted from JSON when nil, but it was present")
	}
}

func TestStep_MaxEnergy_RoundTrip(t *testing.T) {
	t.Parallel()
	s := Step{Query: "do work", MaxEnergy: 0.05}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Step
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.MaxEnergy != 0.05 {
		t.Errorf("want MaxEnergy=0.05, got %v", got.MaxEnergy)
	}
}

// Cycle 6 (ADR-0013): Legacy Step JSON without checkpoint fields deserialises
// with zero values — CheckpointAfter=false.
func TestStep_CheckpointFields_BackwardCompatibility(t *testing.T) {
	t.Parallel()
	raw := `{"query":"do work","depends_on":[0]}`
	var s Step
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if s.CheckpointAfter {
		t.Error("expected CheckpointAfter=false for legacy step, got true")
	}
}

// ENG-1313: a stored plan written before Step.CheckpointQuery was deleted still
// decodes; the retired key is ignored and is not written back.
func TestStep_RetiredCheckpointQueryIsIgnored(t *testing.T) {
	t.Parallel()
	raw := `{"query":"do work","checkpoint_after":true,"checkpoint_query":"is it valid?"}`
	var s Step
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if !s.CheckpointAfter || s.Query != "do work" {
		t.Errorf("legacy step lost its live fields: %+v", s)
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "checkpoint_query") {
		t.Errorf("retired checkpoint_query written back: %s", out)
	}
}

// Cycle 7 (ADR-0013): Step with CheckpointAfter set round-trips through JSON.
func TestStep_CheckpointFields_RoundTrip(t *testing.T) {
	t.Parallel()
	s := Step{
		Query:           "convert CSV to JSON schema",
		DependsOn:       []int{0},
		CheckpointAfter: true,
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Step
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.CheckpointAfter {
		t.Error("CheckpointAfter: want true, got false")
	}
}

func TestStep_OptionalFields_OmittedWhenZero(t *testing.T) {
	t.Parallel()
	s := Step{Query: "simple task"}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["max_energy"]; ok {
		t.Error("max_energy should be omitted when zero")
	}
	// Cycle 8 (ADR-0013): checkpoint fields must be absent from JSON when at zero values.
	if _, ok := raw["checkpoint_after"]; ok {
		t.Error("checkpoint_after should be omitted when false")
	}
	// ROUTE-03: required_capabilities is omitted when empty (backward compatible).
	if _, ok := raw["required_capabilities"]; ok {
		t.Error("required_capabilities should be omitted when empty")
	}
}

// ROUTE-03: required_capabilities round-trips and Clone deep-copies it.
func TestStep_RequiredCapabilities_RoundTripAndClone(t *testing.T) {
	t.Parallel()
	s := Step{Query: "read file", DependsOn: []int{0}, RequiredCapabilities: []string{"file_read", "code_search"}}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Step
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.RequiredCapabilities) != 2 || got.RequiredCapabilities[0] != "file_read" {
		t.Errorf("RequiredCapabilities round-trip failed: got %v", got.RequiredCapabilities)
	}

	plan := &ExecutionPlan{Subject: "s", Steps: []Step{s}}
	clone := plan.Clone()
	// Mutating the clone must not affect the original (deep copy).
	clone.Steps[0].RequiredCapabilities[0] = "MUTATED"
	if plan.Steps[0].RequiredCapabilities[0] != "file_read" {
		t.Error("Clone did not deep-copy RequiredCapabilities (aliasing detected)")
	}
}
