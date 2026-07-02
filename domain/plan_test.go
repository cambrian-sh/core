package domain

import (
	"encoding/json"
	"testing"
)

func TestStep_BackwardCompatibility_NilDependsOn(t *testing.T) {
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

func TestStep_RecommendedModel_RoundTrip(t *testing.T) {
	s := Step{Query: "synthesize report", RecommendedModel: "llm:openai:gpt-4o"}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Step
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.RecommendedModel != "llm:openai:gpt-4o" {
		t.Errorf("want RecommendedModel=llm:openai:gpt-4o, got %q", got.RecommendedModel)
	}
}

// Cycle 6 (ADR-0013): Legacy Step JSON without checkpoint fields deserialises
// with zero values — CheckpointAfter=false, CheckpointQuery="".
func TestStep_CheckpointFields_BackwardCompatibility(t *testing.T) {
	raw := `{"query":"do work","depends_on":[0]}`
	var s Step
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if s.CheckpointAfter {
		t.Error("expected CheckpointAfter=false for legacy step, got true")
	}
	if s.CheckpointQuery != "" {
		t.Errorf("expected empty CheckpointQuery for legacy step, got %q", s.CheckpointQuery)
	}
}

// Cycle 7 (ADR-0013): Step with both checkpoint fields set round-trips through JSON.
func TestStep_CheckpointFields_RoundTrip(t *testing.T) {
	s := Step{
		Query:           "convert CSV to JSON schema",
		DependsOn:       []int{0},
		CheckpointAfter: true,
		CheckpointQuery: "Is the output valid JSON schema?",
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
	if got.CheckpointQuery != "Is the output valid JSON schema?" {
		t.Errorf("CheckpointQuery: want %q, got %q", "Is the output valid JSON schema?", got.CheckpointQuery)
	}
}

func TestStep_OptionalFields_OmittedWhenZero(t *testing.T) {
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
	if _, ok := raw["recommended_model"]; ok {
		t.Error("recommended_model should be omitted when empty")
	}
	// Cycle 8 (ADR-0013): checkpoint fields must be absent from JSON when at zero values.
	if _, ok := raw["checkpoint_after"]; ok {
		t.Error("checkpoint_after should be omitted when false")
	}
	if _, ok := raw["checkpoint_query"]; ok {
		t.Error("checkpoint_query should be omitted when empty")
	}
}
