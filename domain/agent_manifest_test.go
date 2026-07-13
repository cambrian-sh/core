package domain_test

import (
	"encoding/json"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ── Cycle 4: AgentManifest.Dependencies ──────────────────────────────────────

func TestAgentManifest_Dependencies_JSONRoundTrip(t *testing.T) {
	raw := `{"version":"1.0.0","tools":["sql"],"dependencies":["pandas>=2.0","numpy"]}`
	var m domain.AgentManifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(m.Dependencies) != 2 {
		t.Fatalf("want 2 dependencies, got %d", len(m.Dependencies))
	}
	if m.Dependencies[0] != "pandas>=2.0" {
		t.Errorf("Dependencies[0]: want pandas>=2.0, got %q", m.Dependencies[0])
	}
	if m.Dependencies[1] != "numpy" {
		t.Errorf("Dependencies[1]: want numpy, got %q", m.Dependencies[1])
	}
}

func TestAgentManifest_NoDependencies_IsEmpty(t *testing.T) {
	raw := `{"version":"1.0.0","tools":["sql"]}`
	var m domain.AgentManifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(m.Dependencies) != 0 {
		t.Errorf("want no dependencies, got %v", m.Dependencies)
	}
}

func TestAgentManifest_Dependencies_MarshalOmitsEmpty(t *testing.T) {
	m := domain.AgentManifest{Version: "1.0.0"}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := result["dependencies"]; ok {
		t.Error("empty dependencies should be omitted from JSON")
	}
}

func TestAgentManifest_CostPer1MTokens_RoundTrip(t *testing.T) {
	m := domain.AgentManifest{Version: "1.0.0", CostPer1MTokens: 5.0}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got domain.AgentManifest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.CostPer1MTokens != 5.0 {
		t.Errorf("want CostPer1MTokens=5.0, got %v", got.CostPer1MTokens)
	}
}

func TestAgentManifest_CostPer1MTokens_OmitsWhenZero(t *testing.T) {
	m := domain.AgentManifest{Version: "1.0.0"}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := result["cost_per_1m_tokens"]; ok {
		t.Error("zero cost_per_1m_tokens should be omitted from JSON")
	}
}

func TestAgentManifest_Capabilities_RoundTrip(t *testing.T) {
	m := domain.AgentManifest{Version: "1.0.0", Capabilities: []string{"planning", "text-generation"}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got domain.AgentManifest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Capabilities) != 2 || got.Capabilities[0] != "planning" || got.Capabilities[1] != "text-generation" {
		t.Errorf("Capabilities round-trip failed: got %v", got.Capabilities)
	}
}

func TestAgentManifest_Capabilities_OmitsWhenNil(t *testing.T) {
	m := domain.AgentManifest{Version: "1.0.0"}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := result["capabilities"]; ok {
		t.Error("nil capabilities should be omitted from JSON")
	}
}
