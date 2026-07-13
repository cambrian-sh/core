package centralexec

import (
	"context"

	"github.com/cambrian-sh/core/domain"
)

// CapabilityCatalog is the capability-grounded planning vocabulary (ADR-0037
// D4): a pure projection over the resource memory yielding "what the system can
// do well right now" — the regions with credible belief mass across Active
// resources. The Planner drafts intents against this vocabulary pre-draft, so a
// step requiring a capability no resource provides is structurally unreachable
// at generation time rather than emitted and failed at dispatch.
//
// It is a deep module: a narrow interface over the RegionSource seam, holding
// no regions of its own.
type CapabilityCatalog struct {
	// Source is the resource memory's raw region view (the CapabilityBelief
	// store, 0037-03). Required.
	Source domain.RegionSource
	// MinBeliefMass is the credibility floor — a region below it has no credible
	// belief mass and does not appear in the catalog.
	MinBeliefMass float64
	// MinSimilarity is the cosine floor for an intent to land in a region.
	MinSimilarity float64
}

// Regions returns the credible projection: only regions whose belief mass meets
// the floor. Pure with respect to the Source — it filters, never mutates.
func (c *CapabilityCatalog) Regions(ctx context.Context) ([]domain.CapabilityRegion, error) {
	raw, err := c.Source.Regions(ctx)
	if err != nil {
		return nil, err
	}
	var credible []domain.CapabilityRegion
	for _, r := range raw {
		if r.BeliefMass >= c.MinBeliefMass {
			credible = append(credible, r)
		}
	}
	return credible, nil
}

// Reachable reports whether an intent embedding lands in a credible region.
// It returns the nearest credible region (by cosine) and true iff that region
// is within MinSimilarity. An intent whose only nearby region lacks credible
// belief mass is unreachable — the structurally-impossible step (D4).
func (c *CapabilityCatalog) Reachable(ctx context.Context, intentEmbedding []float32) (domain.CapabilityRegion, bool, error) {
	regions, err := c.Regions(ctx)
	if err != nil {
		return domain.CapabilityRegion{}, false, err
	}
	var best domain.CapabilityRegion
	bestSim := -1.0
	for _, r := range regions {
		sim := domain.CosineSimilarity(intentEmbedding, r.Centroid)
		if sim > bestSim {
			bestSim, best = sim, r
		}
	}
	if bestSim < c.MinSimilarity {
		return domain.CapabilityRegion{}, false, nil
	}
	return best, true, nil
}

// Vocabulary returns the labels of the credible regions — the capability-space
// vocabulary the Planner drafts intents against, pre-draft (D4).
func (c *CapabilityCatalog) Vocabulary(ctx context.Context) ([]string, error) {
	regions, err := c.Regions(ctx)
	if err != nil {
		return nil, err
	}
	labels := make([]string, 0, len(regions))
	for _, r := range regions {
		labels = append(labels, r.Label)
	}
	return labels, nil
}
