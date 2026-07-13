package centralexec

import (
	"context"

	"github.com/cambrian-sh/core/domain"
)

// ModelSelector binds a model (TraitModel) to a generation call via the same
// capability-belief + EFE mechanism as agents, with cost as a first-class term
// (ADR-0037 D16). It subsumes ADR-0011 cost-aware routing and ADR-0018 gateway
// model selection into active inference. Models draw region-resolved quality
// from the SAME belief store as agents — they are just another population — so
// "qwen is weak at complex code" is learned, not declared. Agents stay blind to
// the model population: this runs CE/gateway-side, no agent-facing API.
type ModelSelector struct {
	// Quality supplies region-resolved expected quality + confidence per model
	// (the belief store, models as resources).
	Quality domain.PrecisionProvider
	// Costs is the normalized cost penalty per model (ADR-0011 neuromodulator:
	// tokens / $ / latency). Missing entries default to 0.
	Costs map[string]float64
	// ExplorationBonus scales the epistemic term (deferred estimator).
	ExplorationBonus float64
}

// Select binds the EFE-minimizing model for an intent, folding the per-model
// cost penalty into the value (D16).
func (m *ModelSelector) Select(ctx context.Context, intent domain.Intent, models []domain.AgentDefinition) (domain.Selection, error) {
	quality, err := m.Quality.PrecisionFor(ctx, intent, models)
	if err != nil {
		return domain.Selection{}, err
	}
	weights := make([]domain.ModelWeight, len(quality))
	for i, q := range quality {
		weights[i] = domain.ModelWeight{
			ResourceID:      q.ResourceID,
			ExpectedQuality: q.ExpectedSuccess,
			Confidence:      q.Confidence,
			CostPenalty:     m.Costs[q.ResourceID],
		}
	}
	return domain.MinimizeModelEFE(weights, m.ExplorationBonus)
}
