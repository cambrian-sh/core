package gatekeeper

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ROUTE-04 / ADR-0067: under canonical=true, a format-variant declared capability
// satisfies a required one; under canonical=false (ROUTE-03 verbatim) it does not.
func TestPassesDeclaration_Canonical_FoldsFormatVariance(t *testing.T) {
	manifest := &domain.AgentManifest{Capabilities: []string{"Web_Navigation"}}
	task := &domain.AuctionTask{RequiredCapabilities: []string{"web-navigation"}}

	if PassesDeclaration(manifest, task, false) {
		t.Fatal("verbatim (canonical=false): Web_Navigation must NOT satisfy web-navigation")
	}
	if !PassesDeclaration(manifest, task, true) {
		t.Fatal("canonical=true: Web_Navigation must satisfy web-navigation (format fold)")
	}
}

// Canonicalization must not create wrong merges: a distinct-word capability still
// fails the gate.
func TestPassesDeclaration_Canonical_NoWrongMerge(t *testing.T) {
	manifest := &domain.AgentManifest{Capabilities: []string{"file-write"}}
	task := &domain.AuctionTask{RequiredCapabilities: []string{"file-read"}}
	if PassesDeclaration(manifest, task, true) {
		t.Fatal("file-write must NOT satisfy file-read even under canonicalization")
	}
}
