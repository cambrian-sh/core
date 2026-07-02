package centralexec

import (
	"context"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// SeedPrecisionProvider is the cold-start PrecisionProvider used by the
// foundation slice (0037-01) before the region-resolved CapabilityBelief store
// (0037-03) exists. It assigns every candidate a uniform low-confidence prior,
// making a brand-new resource immediately routable while signalling high
// uncertainty so the EFE epistemic term drives exploration (ADR-0037 D2).
type SeedPrecisionProvider struct {
	SeedExpectedSuccess float64
	SeedConfidence      float64
}

// PrecisionFor returns one seed weight per candidate. It is pure and stateless.
func (p SeedPrecisionProvider) PrecisionFor(_ context.Context, _ domain.Intent, candidates []domain.AgentDefinition) ([]domain.PrecisionWeight, error) {
	weights := make([]domain.PrecisionWeight, len(candidates))
	for i, c := range candidates {
		weights[i] = domain.PrecisionWeight{
			ResourceID:      c.ID,
			ExpectedSuccess: p.SeedExpectedSuccess,
			Confidence:      p.SeedConfidence,
		}
	}
	return weights, nil
}
