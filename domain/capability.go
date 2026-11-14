package domain

import (
	"context"
)

// CapabilityRegion is a named region of the shared description-embedding space
// with the aggregate belief the system holds about it (ADR-0037 D2/D4). The
// label is a CapabilityClusterer name; the centroid lives in the same space as
// intent embeddings, so retrieval is nearest-region. BeliefMass is the
// aggregate credible mass across Active resources (the CapabilityBelief store
// computes it, 0037-03) — the quantity that decides whether the system can do
// this "well right now".
type CapabilityRegion struct {
	Label       string
	Centroid    []float32
	BeliefMass  float64
	SampleCount int
	// Cluster is the CapabilityClusterer group this region belongs to (D2
	// generalization). A new resource landing in an established cluster inherits
	// a cluster-level schema prior. Empty means the region stands alone.
	Cluster string
}

// RegionSource yields the raw capability regions known to the resource memory.
// It is the seam between the CapabilityCatalog (the credible-mass projection,
// 0037-02) and the CapabilityBelief store (0037-03) that owns the regions.
type RegionSource interface {
	Regions(ctx context.Context) ([]CapabilityRegion, error)
}
