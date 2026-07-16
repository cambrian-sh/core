package gatekeeper

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ROUTE-06 / ADR-0069: with the arm on, merit reads the tag-scoped success/trust for the
// step's required capability; a global star with poor per-capability history scores low.
func TestComputeMeritBreakdown_PerCapabilityScoping(t *testing.T) {
	agent := domain.AgentDefinition{ID: "versatile"}
	// Globally excellent, but poor specifically at "pdf"; no "browser" history.
	profile := &domain.AgentProfile{
		SuccessRate: 1.0, TrustScore: 1.0,
		NetworkLatencyMedianMs: 10, ComputationLatencyMedianMs: 10,
		CapabilityStats: map[string]domain.CapabilityStat{
			"pdf": {SuccessRate: 0.1, TrustScore: 0.1, SampleCount: 5},
		},
	}
	profiles := &mockGatekeeperProfileReader{profiles: map[string]*domain.AgentProfile{"versatile:": profile}}

	registry := newMockAgentDeclarationSource([]domain.AgentDefinition{agent}, nil)
	cfg := defaultTestExecCfg()
	ctx := context.Background()

	// Arm OFF: global merit — high regardless of the pdf history.
	cfg.PerCapabilityMerit = false
	gkOff := NewGatekeeper(registry, cfg, WithProfiles(profiles))
	globalScore := gkOff.computeMeritBreakdown(ctx, agent, []string{"pdf"}).Score

	// Arm ON, required=pdf: tag-scoped merit — low.
	cfg.PerCapabilityMerit = true
	gkOn := NewGatekeeper(registry, cfg, WithProfiles(profiles))
	pdfScore := gkOn.computeMeritBreakdown(ctx, agent, []string{"pdf"}).Score
	if pdfScore >= globalScore {
		t.Fatalf("pdf-scoped merit (%.4f) should be BELOW global merit (%.4f)", pdfScore, globalScore)
	}

	// Arm ON, required=browser (no history): falls back to global.
	browserScore := gkOn.computeMeritBreakdown(ctx, agent, []string{"browser"}).Score
	if browserScore != globalScore {
		t.Fatalf("no-history capability should fall back to global (%.4f), got %.4f", globalScore, browserScore)
	}

	// Arm ON but no required caps: global (byte-identical path).
	noCapScore := gkOn.computeMeritBreakdown(ctx, agent, nil).Score
	if noCapScore != globalScore {
		t.Fatalf("empty required caps should use global (%.4f), got %.4f", globalScore, noCapScore)
	}
}
