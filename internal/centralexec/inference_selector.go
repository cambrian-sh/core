// Package centralexec implements the Central-Executive Planner's resource
// selection (ADR-0037): an active-inference alternative to the auction. The
// Central Executive (Baddeley) holds no resource state of its own — it queries
// a PrecisionProvider and binds the Expected-Free-Energy-minimizing resource.
package centralexec

import (
	"context"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// InferenceSelector is the EFE-minimizing ResourceSelector (ADR-0037 D9). It is
// a deep module: a single Select method over a deep implementation (precision
// retrieval + pure EFE arithmetic). It owns no authoritative resource state —
// trust/capability are queried through Precision (D1). Routing is inference,
// not a Go switch/merit table (Zero-Hardcode Rule).
type InferenceSelector struct {
	// Precision resolves candidates into precision-weighted beliefs for the
	// intent. Required.
	Precision domain.PrecisionProvider
	// ExplorationBonus scales the epistemic (uncertainty-driven) term in the EFE
	// pick. A deferred estimator (D9) — tuned within the spike, not a feature.
	ExplorationBonus float64

	// Solicitor, when set, is the demoted RequestProposal (ADR-0037 D6): a live
	// proposal is pulled only when the posterior is flat. Optional — no bid round
	// is mandatory.
	Solicitor domain.BidSolicitor
	// FlatMargin is the EFE gap below which the top-two posterior is "flat".
	// A deferred estimator (the flat-posterior threshold).
	FlatMargin float64
	// SoftBidWeight in [0,1] is how much a solicited self-assessment folds into a
	// candidate's expected success — a soft prior, never authoritative.
	SoftBidWeight float64
}

// Select binds the EFE-winning resource for an intent. It retrieves precision
// weights from the oracle, then runs the pure MinimizeEFE core. No Auctioneer
// is on this path. When a Solicitor is wired and the posterior is flat, a live
// proposal is pulled and folded as a soft prior before the pick (D6). Returns
// domain.ErrNoCandidates when nothing is bindable.
func (s *InferenceSelector) Select(ctx context.Context, intent domain.Intent, candidates []domain.AgentDefinition) (domain.Selection, error) {
	weights, err := s.Precision.PrecisionFor(ctx, intent, candidates)
	if err != nil {
		return domain.Selection{}, err
	}

	if s.Solicitor != nil && domain.IsFlatPosterior(weights, s.ExplorationBonus, s.FlatMargin) {
		folded := make([]domain.PrecisionWeight, len(weights))
		copy(folded, weights)
		for i := range folded {
			bid, ok, berr := s.Solicitor.SolicitBid(ctx, intent, domain.AgentDefinition{ID: folded[i].ResourceID})
			if berr != nil {
				return domain.Selection{}, berr
			}
			if ok {
				folded[i].ExpectedSuccess = (1-s.SoftBidWeight)*folded[i].ExpectedSuccess + s.SoftBidWeight*bid
			}
		}
		sel, serr := domain.MinimizeEFE(folded, s.ExplorationBonus)
		if serr != nil {
			return sel, serr
		}
		sel.SolicitedBid = true
		return sel, nil
	}

	return domain.MinimizeEFE(weights, s.ExplorationBonus)
}
