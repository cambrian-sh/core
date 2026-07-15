package gatekeeper

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func TestPassesDeclaration_ExactFormatMatch(t *testing.T) {
	manifest := &domain.AgentManifest{
		Version:          "1.0.0",
		Tools:            []string{"sql"},
		SupportedFormats: []string{"json"},
	}
	task := &domain.AuctionTask{RequiredFormats: []string{"json"}}
	if !PassesDeclaration(manifest, task) {
		t.Error("expected agent with exact format match to pass Declaration")
	}
}

func TestPassesDeclaration_MissingFormatEliminated(t *testing.T) {
	manifest := &domain.AgentManifest{
		Version:          "1.0.0",
		Tools:            []string{"sql"},
		SupportedFormats: []string{"csv"},
	}
	task := &domain.AuctionTask{RequiredFormats: []string{"json"}}
	if PassesDeclaration(manifest, task) {
		t.Error("expected agent missing required format to be eliminated")
	}
}

func TestPassesDeclaration_NilManifestPasses(t *testing.T) {
	task := &domain.AuctionTask{RequiredFormats: []string{"json"}}
	if !PassesDeclaration(nil, task) {
		t.Error("expected nil manifest to pass Declaration unconditionally")
	}
}

func TestPassesDeclaration_EmptyManifestPasses(t *testing.T) {
	manifest := &domain.AgentManifest{}
	task := &domain.AuctionTask{RequiredFormats: []string{"json"}}
	if !PassesDeclaration(manifest, task) {
		t.Error("expected empty manifest to pass Declaration unconditionally")
	}
}

func TestPassesDeclaration_NoRequirementsAllPass(t *testing.T) {
	manifest := &domain.AgentManifest{Version: "1.0.0", Tools: []string{"sql"}}
	task := &domain.AuctionTask{}
	if !PassesDeclaration(manifest, task) {
		t.Error("expected all agents to pass when task has no requirements")
	}
}

func TestPassesDeclaration_SupersetFormatsPasses(t *testing.T) {
	manifest := &domain.AgentManifest{
		Version:          "1.0.0",
		Tools:            []string{"sql"},
		SupportedFormats: []string{"json", "csv"},
	}
	task := &domain.AuctionTask{RequiredFormats: []string{"json"}}
	if !PassesDeclaration(manifest, task) {
		t.Error("expected agent with superset of supported formats to pass Declaration")
	}
}

func TestPassesDeclaration_MultipleFormats_MissingOneEliminated(t *testing.T) {
	manifest := &domain.AgentManifest{
		Version:          "1.0.0",
		Tools:            []string{"sql"},
		SupportedFormats: []string{"json"},
	}
	task := &domain.AuctionTask{RequiredFormats: []string{"json", "xml"}}
	if PassesDeclaration(manifest, task) {
		t.Error("expected agent missing required format to be eliminated")
	}
}

// ── ROUTE-03 capability contract ─────────────────────────────────────────────

func TestPassesDeclaration_Capabilities_SubsetPasses(t *testing.T) {
	manifest := &domain.AgentManifest{Capabilities: []string{"file_read", "code_search", "planning"}}
	task := &domain.AuctionTask{RequiredCapabilities: []string{"file_read", "code_search"}}
	if !PassesDeclaration(manifest, task) {
		t.Error("expected agent declaring a superset of required capabilities to pass")
	}
}

func TestPassesDeclaration_Capabilities_MissingOneEliminated(t *testing.T) {
	manifest := &domain.AgentManifest{Capabilities: []string{"file_read"}}
	task := &domain.AuctionTask{RequiredCapabilities: []string{"file_read", "test_execution"}}
	if PassesDeclaration(manifest, task) {
		t.Error("expected agent missing a required capability to be eliminated")
	}
}

// The empty-manifest free pass MUST NOT apply when the step declares a capability
// requirement — an agent that declares nothing can satisfy no capability (D2 fix).
func TestPassesDeclaration_Capabilities_EmptyManifestEliminated(t *testing.T) {
	task := &domain.AuctionTask{RequiredCapabilities: []string{"browser"}}
	if PassesDeclaration(&domain.AgentManifest{}, task) {
		t.Error("empty manifest must NOT pass when the step requires a capability")
	}
	if PassesDeclaration(nil, task) {
		t.Error("nil manifest must NOT pass when the step requires a capability")
	}
}

// No capability requirement ⇒ control-arm behavior is unchanged (empty/nil pass).
func TestPassesDeclaration_NoCapabilityRequirement_UnchangedBehavior(t *testing.T) {
	task := &domain.AuctionTask{}
	if !PassesDeclaration(&domain.AgentManifest{}, task) {
		t.Error("empty manifest must still pass when no capability is required")
	}
	if !PassesDeclaration(nil, task) {
		t.Error("nil manifest must still pass when no capability is required")
	}
}

// Capability gate composes with the format gate: caps satisfied but a required
// format missing ⇒ still eliminated.
func TestPassesDeclaration_Capabilities_ComposeWithFormatGate(t *testing.T) {
	manifest := &domain.AgentManifest{
		Capabilities:     []string{"file_read"},
		SupportedFormats: []string{"csv"},
		Tools:            []string{"fs"},
	}
	task := &domain.AuctionTask{
		RequiredCapabilities: []string{"file_read"},
		RequiredFormats:      []string{"json"},
	}
	if PassesDeclaration(manifest, task) {
		t.Error("capabilities satisfied but format missing ⇒ must be eliminated")
	}
}
