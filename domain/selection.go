package domain

import (
	"context"
	"errors"
	"sort"
)

// Selection mechanism identifiers, carried on every Selection for telemetry /
// audit partitioning (PRD-0037 A/B coexistence). The benchmark harness uses
// these to split metrics by variant without external bookkeeping.
const (
	MechanismEFE     = "efe"
	MechanismAuction = "auction"
)

// ErrNoCandidates is returned by a ResourceSelector when there is nothing to
// bind an intent to.
var ErrNoCandidates = errors.New("selection: no candidates")

// Intent is a capability-space description of a unit of work (ADR-0037 D3).
// Unlike AuctionTask it is agent-agnostic — the skeleton is drafted in intents
// and a concrete resource is bound to each at execution time. Embedding is the
// shared description-embedding vector used for capability retrieval (D2 index).
type Intent struct {
	ID          string
	Description string
	Embedding   []float32
}

// PrecisionWeight is the precision-weighted belief about one resource's fitness
// for a given intent (ADR-0037 D2/D7). ExpectedSuccess is the pragmatic value;
// Confidence drives the epistemic (exploration) value — a low-confidence belief
// has high expected information gain. The Gatekeeper (D7) shapes these weights
// (policy non-compliance ⇒ ExpectedSuccess 0).
type PrecisionWeight struct {
	ResourceID      string
	ExpectedSuccess float64 // [0,1] — pragmatic value
	Confidence      float64 // [0,1] — 1-Confidence is the epistemic value
}

// Selection is the outcome of a ResourceSelector pick (PRD-0037). Mechanism is
// "efe" or "auction" for A/B partitioning; SolicitedBid is true when a live
// RequestProposal was pulled because the posterior was flat (D6, issue 0037-05).
type Selection struct {
	ResourceID   string
	Mechanism    string
	Confidence   float64
	SolicitedBid bool
}

// PrecisionProvider resolves a candidate set into precision-weighted beliefs
// for an intent (ADR-0037 D2/D7). It is the seam between resource selection and
// the CapabilityBelief store (0037-03) + Gatekeeper precision oracle (0037-04).
// In 0037-01 a static cold-start seed satisfies it; learning swaps in later
// without touching the selector — the Central Executive owns no belief itself.
type PrecisionProvider interface {
	PrecisionFor(ctx context.Context, intent Intent, candidates []AgentDefinition) ([]PrecisionWeight, error)
}

// ResourceSelector is the abstraction behind which both selection mechanisms
// live (PRD-0037 A/B coexistence): AuctionSelector (status quo) and
// InferenceSelector (EFE). A flagged run picks one; every Selection carries its
// Mechanism so the benchmark harness can partition metrics by variant.
type ResourceSelector interface {
	Select(ctx context.Context, intent Intent, candidates []AgentDefinition) (Selection, error)
}

// MinimizeEFE selects the resource that minimizes Expected Free Energy — i.e.
// maximizes pragmatic value (expected success) plus epistemic value (an
// exploration bonus scaled by uncertainty, 1-Confidence) (ADR-0037 D9).
//
//	value(r) = ExpectedSuccess(r) + explorationBonus × (1 - Confidence(r))
//
// It is a pure, deterministic function: routing is inference arithmetic, not a
// Go switch/merit table (Zero-Hardcode Rule). Ties break lexically by
// ResourceID for reproducibility. An empty candidate set is ErrNoCandidates.
func MinimizeEFE(weights []PrecisionWeight, explorationBonus float64) (Selection, error) {
	if len(weights) == 0 {
		return Selection{}, ErrNoCandidates
	}

	ranked := make([]PrecisionWeight, len(weights))
	copy(ranked, weights)
	sort.SliceStable(ranked, func(i, j int) bool {
		vi := EFEValue(ranked[i], explorationBonus)
		vj := EFEValue(ranked[j], explorationBonus)
		if vi != vj {
			return vi > vj
		}
		return ranked[i].ResourceID < ranked[j].ResourceID
	})

	winner := ranked[0]
	return Selection{
		ResourceID: winner.ResourceID,
		Mechanism:  MechanismEFE,
		Confidence: winner.Confidence,
	}, nil
}

// EFEValue is the negative Expected Free Energy of a single candidate — higher
// is better (lower EFE): pragmatic value plus uncertainty-scaled epistemic value.
func EFEValue(w PrecisionWeight, explorationBonus float64) float64 {
	return w.ExpectedSuccess + explorationBonus*(1.0-w.Confidence)
}

// ModelWeight is the belief about a model (TraitModel) for an intent (ADR-0037
// D16): the same region-resolved quality/confidence as an agent, plus a
// first-class cost penalty (ADR-0011's neuromodulator — normalized tokens / $ /
// latency). Models are just another resource population in the belief store.
type ModelWeight struct {
	ResourceID      string
	ExpectedQuality float64 // [0,1]
	Confidence      float64 // [0,1]
	CostPenalty     float64 // subtracted from the EFE value
}

// ModelEFEValue is the negative Expected Free Energy for a model (D16):
//
//	expected_quality × confidence − cost_penalty + epistemic_value
//
// Unlike the agent value (where cost is a tiebreaker), cost is often the
// deciding term for models. Higher is better.
func ModelEFEValue(w ModelWeight, explorationBonus float64) float64 {
	return w.ExpectedQuality*w.Confidence - w.CostPenalty + explorationBonus*(1.0-w.Confidence)
}

// MinimizeModelEFE selects the model minimizing Expected Free Energy with cost
// as a first-class term (D16). Pure and deterministic; ties break lexically.
func MinimizeModelEFE(weights []ModelWeight, explorationBonus float64) (Selection, error) {
	if len(weights) == 0 {
		return Selection{}, ErrNoCandidates
	}
	ranked := make([]ModelWeight, len(weights))
	copy(ranked, weights)
	sort.SliceStable(ranked, func(i, j int) bool {
		vi := ModelEFEValue(ranked[i], explorationBonus)
		vj := ModelEFEValue(ranked[j], explorationBonus)
		if vi != vj {
			return vi > vj
		}
		return ranked[i].ResourceID < ranked[j].ResourceID
	})
	winner := ranked[0]
	return Selection{
		ResourceID: winner.ResourceID,
		Mechanism:  MechanismEFE,
		Confidence: winner.Confidence,
	}, nil
}

// IsFlatPosterior reports whether the top two candidates' EFE values are within
// margin of each other — a near-tie / novel step (ADR-0037 D6). Exactly when
// the Central Executive should solicit a live proposal (the FEP-optimal
// epistemic action). Fewer than two candidates is trivially flat.
func IsFlatPosterior(weights []PrecisionWeight, explorationBonus, margin float64) bool {
	if len(weights) < 2 {
		return true
	}
	top, second := -1.0, -1.0
	for _, w := range weights {
		v := EFEValue(w, explorationBonus)
		switch {
		case v > top:
			second, top = top, v
		case v > second:
			second = v
		}
	}
	return top-second < margin
}

// BidSolicitor pulls an agent's input-conditioned self-assessment for an intent
// — the demoted RequestProposal as a solicited epistemic action (ADR-0037 D6).
// It preserves the one thing a manifest embedding cannot reconstruct ("do I
// hold credentials for this API?") while making the bid pull, not push.
type BidSolicitor interface {
	SolicitBid(ctx context.Context, intent Intent, candidate AgentDefinition) (confidence float64, ok bool, err error)
}
