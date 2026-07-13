package centralexec

import (
	"context"

	"github.com/cambrian-sh/core/domain"
)

// PrecisionShaper modulates precision weights for an intent (ADR-0037 D7). It
// is a blackboard knowledge source: each shaper independently reshapes the
// posterior. New gatekeeping (compliance, anomaly, rate-limit) is added as
// another shaper — the selector never needs to know it exists.
type PrecisionShaper interface {
	Shape(ctx context.Context, intent domain.Intent, weights []domain.PrecisionWeight) ([]domain.PrecisionWeight, error)
}

// PolicyVerdict is one policy's ruling on a candidate. Blocked is a hard block
// (precision 0). PrecisionMultiplier in [0,1] is temporal modulation (e.g. a
// rate-limited resource), applied only when not Blocked.
type PolicyVerdict struct {
	Blocked             bool
	PrecisionMultiplier float64
}

// PolicyOracle evaluates a single candidate against a policy. This is the
// re-pointed Gatekeeper contract — it shapes, it does not compete (D7).
type PolicyOracle interface {
	Evaluate(ctx context.Context, intent domain.Intent, candidate domain.AgentDefinition) PolicyVerdict
}

// PolicyPrecisionShaper adapts a PolicyOracle into a PrecisionShaper. A blocked
// candidate's precision goes to 0 (ExpectedSuccess 0, Confidence 1 so its EFE
// epistemic term also vanishes — a true hard block). A modulated candidate's
// ExpectedSuccess is scaled by the multiplier.
type PolicyPrecisionShaper struct {
	Policy PolicyOracle
}

// Shape applies the policy to each weight. Weights are matched to candidates by
// ResourceID via a synthetic AgentDefinition (the oracle keys on ID).
func (s PolicyPrecisionShaper) Shape(ctx context.Context, intent domain.Intent, weights []domain.PrecisionWeight) ([]domain.PrecisionWeight, error) {
	out := make([]domain.PrecisionWeight, len(weights))
	copy(out, weights)
	for i := range out {
		v := s.Policy.Evaluate(ctx, intent, domain.AgentDefinition{ID: out[i].ResourceID})
		switch {
		case v.Blocked:
			out[i].ExpectedSuccess = 0
			out[i].Confidence = 1
		case v.PrecisionMultiplier != 1.0 && v.PrecisionMultiplier >= 0:
			out[i].ExpectedSuccess *= v.PrecisionMultiplier
		}
	}
	return out, nil
}

// ShapedPrecisionProvider composes a base PrecisionProvider (the belief store)
// with an ordered chain of knowledge-source shapers (D7 blackboard). It is
// itself a PrecisionProvider, so the InferenceSelector consumes the shaped
// posterior with no awareness of the individual sources.
type ShapedPrecisionProvider struct {
	Base    domain.PrecisionProvider
	Shapers []PrecisionShaper
}

// PrecisionFor retrieves the base weights then runs every shaper in order.
func (p *ShapedPrecisionProvider) PrecisionFor(ctx context.Context, intent domain.Intent, candidates []domain.AgentDefinition) ([]domain.PrecisionWeight, error) {
	weights, err := p.Base.PrecisionFor(ctx, intent, candidates)
	if err != nil {
		return nil, err
	}
	for _, sh := range p.Shapers {
		weights, err = sh.Shape(ctx, intent, weights)
		if err != nil {
			return nil, err
		}
	}
	return weights, nil
}
