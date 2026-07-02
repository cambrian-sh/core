package centralexec

import (
	"context"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// blockPolicy blocks one named resource and rate-limits another.
type blockPolicy struct {
	blocked   string
	limited   string
	limitMult float64
}

func (p blockPolicy) Evaluate(_ context.Context, _ domain.Intent, c domain.AgentDefinition) PolicyVerdict {
	switch c.ID {
	case p.blocked:
		return PolicyVerdict{Blocked: true}
	case p.limited:
		return PolicyVerdict{PrecisionMultiplier: p.limitMult}
	default:
		return PolicyVerdict{PrecisionMultiplier: 1.0}
	}
}

// The re-pointed Gatekeeper shapes the selector's posterior as a knowledge
// source: a policy-blocked candidate gets precision 0 (ADR-0037 D7, 0037-04 #1).
func TestPolicyPrecisionShaper_BlocksAndModulates(t *testing.T) {
	base := []domain.PrecisionWeight{
		{ResourceID: "ok", ExpectedSuccess: 0.8, Confidence: 0.5},
		{ResourceID: "blocked", ExpectedSuccess: 0.9, Confidence: 0.5},
		{ResourceID: "limited", ExpectedSuccess: 0.8, Confidence: 0.5},
	}
	shaper := PolicyPrecisionShaper{Policy: blockPolicy{blocked: "blocked", limited: "limited", limitMult: 0.5}}

	out, err := shaper.Shape(context.Background(), domain.Intent{}, base)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	byID := map[string]domain.PrecisionWeight{}
	for _, w := range out {
		byID[w.ResourceID] = w
	}
	if byID["blocked"].ExpectedSuccess != 0 {
		t.Errorf("blocked ExpectedSuccess = %v, want 0 (hard block)", byID["blocked"].ExpectedSuccess)
	}
	if byID["limited"].ExpectedSuccess != 0.4 {
		t.Errorf("limited ExpectedSuccess = %v, want 0.4 (0.8×0.5 rate-limit modulation)", byID["limited"].ExpectedSuccess)
	}
	if byID["ok"].ExpectedSuccess != 0.8 {
		t.Errorf("ok ExpectedSuccess = %v, want 0.8 (unchanged)", byID["ok"].ExpectedSuccess)
	}
}

// Adding a second knowledge source requires no change to selection — the
// provider applies the whole chain (ADR-0037 D7 blackboard, 0037-04 #2).
func TestShapedPrecisionProvider_ComposesKnowledgeSources(t *testing.T) {
	base := SeedPrecisionProvider{SeedExpectedSuccess: 1.0, SeedConfidence: 0.5}
	// Two independent shapers stacked: a block, then a global 0.5 modulation.
	s1 := PolicyPrecisionShaper{Policy: blockPolicy{blocked: "x", limitMult: 1.0}}
	s2 := PolicyPrecisionShaper{Policy: blockPolicy{limited: "y", limitMult: 0.5, blocked: "\x00none"}}
	prov := &ShapedPrecisionProvider{Base: base, Shapers: []PrecisionShaper{s1, s2}}

	candidates := []domain.AgentDefinition{{ID: "x"}, {ID: "y"}, {ID: "z"}}
	out, err := prov.PrecisionFor(context.Background(), domain.Intent{}, candidates)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	byID := map[string]domain.PrecisionWeight{}
	for _, w := range out {
		byID[w.ResourceID] = w
	}
	if byID["x"].ExpectedSuccess != 0 {
		t.Errorf("x blocked by first source, ExpectedSuccess = %v, want 0", byID["x"].ExpectedSuccess)
	}
	if byID["y"].ExpectedSuccess != 0.5 {
		t.Errorf("y modulated by second source, ExpectedSuccess = %v, want 0.5", byID["y"].ExpectedSuccess)
	}
	if byID["z"].ExpectedSuccess != 1.0 {
		t.Errorf("z untouched, ExpectedSuccess = %v, want 1.0", byID["z"].ExpectedSuccess)
	}
}

// ShapedPrecisionProvider is itself a PrecisionProvider (drop-in for the selector).
var _ domain.PrecisionProvider = (*ShapedPrecisionProvider)(nil)
