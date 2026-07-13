package domain_test

import (
	"encoding/json"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// Cycle 1: unmarshal without "trait" key → TraitCognitive (backward compat)

func TestAgentDefinition_MissingTrait_DefaultsToTraitCognitive(t *testing.T) {
	raw := `{"ID":"agent-1","Name":"Writer","Runtime":"python"}`
	var def domain.AgentDefinition
	if err := json.Unmarshal([]byte(raw), &def); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if def.Trait != domain.TraitCognitive {
		t.Errorf("want TraitCognitive, got %q", def.Trait)
	}
}

// Cycle 0033-01: TraitDaemon constant, JSON round-trip, and no conflict with existing traits.
func TestAgentDefinition_TraitDaemon_Constant(t *testing.T) {
	if domain.TraitDaemon == domain.TraitCognitive ||
		domain.TraitDaemon == domain.TraitTool ||
		domain.TraitDaemon == domain.TraitModel {
		t.Error("TraitDaemon must be distinct from all other traits")
	}
	def := domain.AgentDefinition{ID: "gold-tracker", Trait: domain.TraitDaemon}
	b, _ := json.Marshal(def)
	var got domain.AgentDefinition
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	if got.Trait != domain.TraitDaemon {
		t.Errorf("want TraitDaemon, got %q", got.Trait)
	}
}

// Cycle 2: marshal TraitTool → JSON contains "trait":"tool"

func TestAgentDefinition_TraitTool_MarshalIncludesKey(t *testing.T) {
	def := domain.AgentDefinition{ID: "file-writer", Trait: domain.TraitTool}
	b, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	trait, ok := result["trait"]
	if !ok {
		t.Fatal("want \"trait\" key in JSON, got none")
	}
	if trait != "tool" {
		t.Errorf("want trait=tool, got %q", trait)
	}
}

// Cycle 3: marshal TraitCognitive (zero value) → "trait" key absent (omitempty)

func TestAgentDefinition_TraitCognitive_MarshalOmitsKey(t *testing.T) {
	def := domain.AgentDefinition{ID: "llm-agent", Trait: domain.TraitCognitive}
	b, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := result["trait"]; ok {
		t.Error("TraitCognitive should be omitted from JSON (omitempty), but \"trait\" key was present")
	}
}

// Cycle 4: TraitModel constant exists and marshals to "trait":"model"

func TestAgentDefinition_TraitModel_ValueAndMarshal(t *testing.T) {
	if domain.TraitModel != "model" {
		t.Fatalf("want TraitModel=\"model\", got %q", domain.TraitModel)
	}
	def := domain.AgentDefinition{ID: "gpt-4o", Trait: domain.TraitModel}
	b, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	trait, ok := result["trait"]
	if !ok {
		t.Fatal("want \"trait\" key in JSON, got none")
	}
	if trait != "model" {
		t.Errorf("want trait=model, got %q", trait)
	}
}
