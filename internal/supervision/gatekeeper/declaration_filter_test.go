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
