package domain

import "testing"

// Cycle 1: DocType constants have the expected string values.
func TestDocTypeConstants(t *testing.T) {
	if DocTypeMemory != "memory" {
		t.Errorf("DocTypeMemory = %q, want %q", DocTypeMemory, "memory")
	}
	if DocTypeAgentProfile != "agent_profile" {
		t.Errorf("DocTypeAgentProfile = %q, want %q", DocTypeAgentProfile, "agent_profile")
	}
	if DocTypeJudicialRecord != "judicial_record" {
		t.Errorf("DocTypeJudicialRecord = %q, want %q", DocTypeJudicialRecord, "judicial_record")
	}
	if DocTypeProceduralTemplate != "procedural_template" {
		t.Errorf("DocTypeProceduralTemplate = %q, want %q", DocTypeProceduralTemplate, "procedural_template")
	}
	if DocTypeMnemonicFact != "mnemonic_fact" {
		t.Errorf("DocTypeMnemonicFact = %q, want %q", DocTypeMnemonicFact, "mnemonic_fact")
	}
	if DocTypeMnemonicScene != "mnemonic_scene" {
		t.Errorf("DocTypeMnemonicScene = %q, want %q", DocTypeMnemonicScene, "mnemonic_scene")
	}
}

// Cycle 2: SearchOptions zero-value has empty DocumentType (no filter) and zero TopK.
func TestSearchOptions_ZeroValue(t *testing.T) {
	var opts SearchOptions
	if opts.DocumentType != "" {
		t.Errorf("zero-value DocumentType = %q, want empty string", opts.DocumentType)
	}
	if opts.TopK != 0 {
		t.Errorf("zero-value TopK = %d, want 0", opts.TopK)
	}
}

// Cycle 3: SearchOptions can be constructed with all DocType constants.
func TestSearchOptions_WithDocTypeMemory(t *testing.T) {
	opts := SearchOptions{DocumentType: DocTypeMemory, TopK: 5}
	if opts.DocumentType != "memory" {
		t.Errorf("DocumentType = %q, want %q", opts.DocumentType, "memory")
	}
	if opts.TopK != 5 {
		t.Errorf("TopK = %d, want 5", opts.TopK)
	}
}

// Cycle 4: SearchOptions filter works with new mnemonic DocType constants (ADR-0015).
func TestSearchOptions_WithMnemonicDocTypes(t *testing.T) {
	factOpts := SearchOptions{DocumentType: DocTypeMnemonicFact, TopK: 3}
	if factOpts.DocumentType != "mnemonic_fact" {
		t.Errorf("DocumentType = %q, want %q", factOpts.DocumentType, "mnemonic_fact")
	}
	sceneOpts := SearchOptions{DocumentType: DocTypeMnemonicScene, TopK: 1}
	if sceneOpts.DocumentType != "mnemonic_scene" {
		t.Errorf("DocumentType = %q, want %q", sceneOpts.DocumentType, "mnemonic_scene")
	}
}

// Cycle 5: Document struct has ActivationStrength field and zero-value is valid (ADR-0015).
func TestDocument_ActivationStrength_ZeroValue(t *testing.T) {
	doc := Document{ID: "test-1", Text: "test"}
	if doc.ActivationStrength != 0.0 {
		t.Errorf("zero-value ActivationStrength = %v, want 0.0", doc.ActivationStrength)
	}
}

// Cycle 7: DocTypeEpisodicMemory constant has expected string value (ADR-0029).
func TestDocTypeEpisodicMemory_Value(t *testing.T) {
	if DocTypeEpisodicMemory != "episodic_memory" {
		t.Errorf("DocTypeEpisodicMemory = %q, want %q", DocTypeEpisodicMemory, "episodic_memory")
	}
}

// Cycle 8: LTMEnrichment.Episodes field exists alongside Facts and Negatives (ADR-0029).
func TestLTMEnrichment_EpisodesField(t *testing.T) {
	e := LTMEnrichment{
		Facts:     []SearchResult{{Score: 0.9}},
		Negatives: []SearchResult{{Score: 0.5}},
		Episodes:  []SearchResult{{Score: 0.7}},
	}
	if len(e.Episodes) != 1 {
		t.Errorf("Episodes: want 1 got %d", len(e.Episodes))
	}
	if e.Episodes[0].Score != 0.7 {
		t.Errorf("Episodes[0].Score: want 0.7 got %v", e.Episodes[0].Score)
	}
}

// Cycle 6: Document.ActivationStrength can be set to full lifecycle range [0.0, 1.0] (ADR-0015).
func TestDocument_ActivationStrength_Range(t *testing.T) {
	doc := Document{ActivationStrength: 0.1}
	if doc.ActivationStrength != 0.1 {
		t.Errorf("after set to 0.1, got %v", doc.ActivationStrength)
	}
	doc.ActivationStrength = 0.8
	if doc.ActivationStrength != 0.8 {
		t.Errorf("after set to 0.8, got %v", doc.ActivationStrength)
	}
}
