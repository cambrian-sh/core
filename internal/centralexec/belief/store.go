// Package belief implements the region-resolved CapabilityBelief store
// (ADR-0037 D2) — the heart of the Central-Executive Planner. It replaces the
// global TrustScore with belief(resource, intent) → (expected_success,
// confidence), resolved per capability-region rather than globally.
//
// It is a Complementary Learning System (CLS): a fast store (recent episodes,
// high plasticity) and a slow store (consolidated trust, high stability). The
// slow store's small consolidation rate is what lets a few bad runs fail to
// catastrophically overwrite established belief. Priors come from verified
// declarations (agents); posteriors are learned from outcomes per region, so
// two topically-similar resources diverge as one succeeds and the other fails.
//
// The store is pure in-memory data + update math; persistence and the live
// Verifier/Circadian wiring are layered on at the composition root (0037-11).
package belief

import (
	"context"
	"sync"

	"github.com/cambrian-sh/core/domain"
)

// Config holds the CLS tuning knobs. The prior↔posterior weighting and region
// representation are deferred estimators (ADR-0037 §Out of Scope) — these are
// editorial defaults, not calibrated constants.
type Config struct {
	// PriorExpectedSuccess is the verified-declaration prior expected success
	// for a resource with no posterior in a region (routable but unproven).
	PriorExpectedSuccess float64
	// FastAlpha is the EWMA plasticity of the fast (episodic) store.
	FastAlpha float64
	// SlowAlpha is the consolidation rate into the slow store. Small ⇒ established
	// belief resists transient bad runs (the stability half of CLS).
	SlowAlpha float64
	// ConfidenceK shapes confidence = n/(n+k) from sample count n.
	ConfidenceK float64
	// MinSimilarity is the cosine floor for assigning an intent to a region.
	MinSimilarity float64
}

// Outcome is a single execution result for a (resource, region) pair, expressed
// as success in [0,1] — typically 1 - Verifier prediction-error (D8).
type Outcome struct {
	Success float64
}

// regionBelief is the per-(resource,region) belief in one CLS tier.
type regionBelief struct {
	expectedSuccess float64
	sampleCount     int
}

// Store is the region-resolved CLS belief store. Safe for concurrent use.
type Store struct {
	mu      sync.RWMutex
	regions []domain.CapabilityRegion
	cfg     Config
	// fast[resource][region] and slow[resource][region]; seeded resources have a
	// nil-valued posterior until Update writes one (prior fallback applies).
	fast  map[string]map[string]*regionBelief
	slow  map[string]map[string]*regionBelief
	seen  map[string]bool // resources with a verified-declaration prior
	// regionCluster maps a region label to its cluster (empty = standalone).
	regionCluster map[string]string
}

// New constructs a belief store over a fixed region vocabulary.
func New(regions []domain.CapabilityRegion, cfg Config) *Store {
	rc := make(map[string]string, len(regions))
	for _, r := range regions {
		rc[r.Label] = r.Cluster
	}
	return &Store{
		regions:       regions,
		cfg:           cfg,
		fast:          map[string]map[string]*regionBelief{},
		slow:          map[string]map[string]*regionBelief{},
		seen:          map[string]bool{},
		regionCluster: rc,
	}
}

// SeedPrior registers a resource's verified-declaration prior, making it
// immediately routable at low confidence across all regions (D2 cold-start).
func (s *Store) SeedPrior(resourceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen[resourceID] = true
}

// Update writes an outcome to the fast (episodic) store for a (resource,
// region) pair immediately (D8). Expected success follows an EWMA at FastAlpha;
// the sample count drives confidence. The slow store is untouched until
// Consolidate runs — the plasticity half of CLS.
func (s *Store) Update(resourceID, region string, o Outcome) {
	if region == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen[resourceID] = true

	if s.fast[resourceID] == nil {
		s.fast[resourceID] = map[string]*regionBelief{}
	}
	rb := s.fast[resourceID][region]
	if rb == nil {
		// First episode: seed the EWMA from the verified-declaration prior so a
		// single outcome blends with the prior rather than replacing it.
		rb = &regionBelief{expectedSuccess: s.cfg.PriorExpectedSuccess}
		s.fast[resourceID][region] = rb
	}
	rb.expectedSuccess = (1-s.cfg.FastAlpha)*rb.expectedSuccess + s.cfg.FastAlpha*o.Success
	rb.sampleCount++
}

// UpdateForIntent is the Verifier-facing update (D8): it resolves the intent
// embedding to a region and records the outcome there. The Verifier knows the
// intent it scored, not the internal region label. A success that cannot be
// attributed to any region (no region within MinSimilarity) is skipped.
func (s *Store) UpdateForIntent(_ context.Context, resourceID string, intentEmbedding []float32, success float64) error {
	s.mu.RLock()
	region := s.regionFor(intentEmbedding)
	s.mu.RUnlock()
	if region == "" {
		return nil
	}
	s.Update(resourceID, region, Outcome{Success: success})
	return nil
}

// Consolidate interleaves the fast (episodic) store into the slow (consolidated)
// store offline (D2/D8). The first consolidation of a (resource, region)
// establishes the slow belief from the fast episodes; subsequent consolidations
// blend gently at SlowAlpha, so established belief resists transient bad runs.
// The fast store is drained — its episodes are now consolidated.
func (s *Store) Consolidate() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for resource, regions := range s.fast {
		for region, f := range regions {
			if f == nil || f.sampleCount == 0 {
				continue
			}
			if s.slow[resource] == nil {
				s.slow[resource] = map[string]*regionBelief{}
			}
			slow := s.slow[resource][region]
			if slow == nil {
				// First consolidation establishes the slow belief directly.
				s.slow[resource][region] = &regionBelief{
					expectedSuccess: f.expectedSuccess,
					sampleCount:     f.sampleCount,
				}
			} else {
				slow.expectedSuccess = (1-s.cfg.SlowAlpha)*slow.expectedSuccess + s.cfg.SlowAlpha*f.expectedSuccess
				slow.sampleCount += f.sampleCount
			}
		}
		// Drain consolidated episodes.
		delete(s.fast, resource)
	}
}

// PrecisionFor resolves a candidate set to per-resource precision weights for
// an intent (domain.PrecisionProvider). It is the seam the InferenceSelector
// consumes; the EFE pick runs over these weights (D2/D9).
func (s *Store) PrecisionFor(_ context.Context, intent domain.Intent, candidates []domain.AgentDefinition) ([]domain.PrecisionWeight, error) {
	weights := make([]domain.PrecisionWeight, 0, len(candidates))
	for _, c := range candidates {
		weights = append(weights, s.Belief(c.ID, intent.Embedding))
	}
	return weights, nil
}

// Regions reports the capability regions with their aggregate belief mass
// across Active resources (domain.RegionSource) — the credible mass the
// CapabilityCatalog projects (D4). A region's mass is the best credible
// resource's expected_success × confidence; a region no resource has earned
// reports zero mass and is therefore an impossible step.
func (s *Store) Regions(_ context.Context) ([]domain.CapabilityRegion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]domain.CapabilityRegion, len(s.regions))
	copy(out, s.regions)
	for i := range out {
		label := out[i].Label
		var bestMass float64
		var samples int
		for resource := range s.seen {
			es, n := s.posteriorLocked(resource, label)
			if n == 0 {
				continue
			}
			mass := es * s.confidenceFor(n)
			if mass > bestMass {
				bestMass = mass
			}
			samples += n
		}
		out[i].BeliefMass = bestMass
		out[i].SampleCount = samples
	}
	return out, nil
}

// regionFor returns the label of the nearest region to an embedding, or "" if
// none is within MinSimilarity.
func (s *Store) regionFor(embedding []float32) string {
	best, bestSim := "", s.cfg.MinSimilarity
	for _, r := range s.regions {
		sim := domain.CosineSimilarity(embedding, r.Centroid)
		if sim >= bestSim {
			bestSim, best = sim, r.Label
		}
	}
	return best
}

// confidenceFor maps a sample count to a confidence in (0,1) via n/(n+k).
func (s *Store) confidenceFor(sampleCount int) float64 {
	n := float64(sampleCount)
	return n / (n + s.cfg.ConfidenceK)
}

// Belief returns the precision-weighted belief about a resource for an intent
// embedding (D2). It resolves the nearest region and combines the slow+fast
// posteriors; with no posterior it falls back to the verified-declaration prior
// at low (but non-zero) confidence.
func (s *Store) Belief(resourceID string, intentEmbedding []float32) domain.PrecisionWeight {
	s.mu.RLock()
	defer s.mu.RUnlock()

	region := s.regionFor(intentEmbedding)
	es, n := s.posteriorLocked(resourceID, region)
	if n == 0 {
		// No posterior — fall back to the cluster schema prior (warm start if the
		// region's cluster is established) or the bare verified-declaration prior.
		// Either way confidence stays low: routable but unproven for THIS resource.
		return domain.PrecisionWeight{
			ResourceID:      resourceID,
			ExpectedSuccess: s.priorForLocked(region),
			Confidence:      s.confidenceFor(0) + priorFloor,
		}
	}
	return domain.PrecisionWeight{
		ResourceID:      resourceID,
		ExpectedSuccess: es,
		Confidence:      s.confidenceFor(n),
	}
}

// BeliefForSubgoal returns the starting precision for binding a resource to a
// yielded sub-goal (ADR-0037 D14): it inherits the slow store (the resource's
// consolidated global trust) but ignores the fast store, so the parent task's
// recent in-flight episodes do not leak into an unrelated sub-task. With no
// consolidated belief it falls back to the (cluster/declaration) prior.
func (s *Store) BeliefForSubgoal(resourceID string, intentEmbedding []float32) domain.PrecisionWeight {
	s.mu.RLock()
	defer s.mu.RUnlock()

	region := s.regionFor(intentEmbedding)
	if region != "" {
		if slow := s.slow[resourceID][region]; slow != nil && slow.sampleCount > 0 {
			return domain.PrecisionWeight{
				ResourceID:      resourceID,
				ExpectedSuccess: slow.expectedSuccess,
				Confidence:      s.confidenceFor(slow.sampleCount),
			}
		}
	}
	return domain.PrecisionWeight{
		ResourceID:      resourceID,
		ExpectedSuccess: s.priorForLocked(region),
		Confidence:      s.confidenceFor(0) + priorFloor,
	}
}

// priorFloor is the small confidence a verified-but-unproven resource carries,
// so a fresh resource is routable (non-zero) yet clearly uncertain.
const priorFloor = 0.05

// priorForLocked returns the starting expected success for a resource with no
// posterior in a region: the cluster schema prior if the region's cluster has
// established consolidated belief (CLS schema fast-path, D2), else the bare
// verified-declaration prior. Caller holds at least RLock.
func (s *Store) priorForLocked(region string) float64 {
	cluster := s.regionCluster[region]
	if cluster == "" {
		return s.cfg.PriorExpectedSuccess
	}
	var sum float64
	var count int
	for _, regions := range s.slow {
		for regLabel, b := range regions {
			if b == nil || b.sampleCount == 0 {
				continue
			}
			if s.regionCluster[regLabel] == cluster {
				sum += b.expectedSuccess
				count++
			}
		}
	}
	if count == 0 {
		return s.cfg.PriorExpectedSuccess
	}
	return sum / float64(count)
}

// posteriorLocked returns the combined slow+fast expected success and the total
// sample count for a (resource, region). Caller holds at least RLock.
func (s *Store) posteriorLocked(resourceID, region string) (float64, int) {
	if region == "" {
		return 0, 0
	}
	slow := s.slow[resourceID][region]
	fast := s.fast[resourceID][region]
	switch {
	case slow == nil && fast == nil:
		return 0, 0
	case slow == nil:
		return fast.expectedSuccess, fast.sampleCount
	case fast == nil:
		return slow.expectedSuccess, slow.sampleCount
	default:
		// Combine by sample-count weighting (fast recency already baked into its EWMA).
		total := slow.sampleCount + fast.sampleCount
		es := (slow.expectedSuccess*float64(slow.sampleCount) + fast.expectedSuccess*float64(fast.sampleCount)) / float64(total)
		return es, total
	}
}
