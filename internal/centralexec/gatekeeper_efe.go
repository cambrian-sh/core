package centralexec

import (
	"context"

	"github.com/cambrian-sh/core/domain"
)

// CandidateSource is the narrow Gatekeeper view the EFE arm needs: discover the
// qualified candidate slate for a task (declaration/scope filtering reused).
type CandidateSource interface {
	FindCandidates(ctx context.Context, task *domain.AuctionTask) ([]domain.ScoredCandidate, error)
}

// GatekeeperEFESelector is the live "efe" arm (ADR-0037 / Fix 4): it discovers
// candidates through the existing Gatekeeper, then binds the EFE-minimizing
// resource — no Auctioneer, no bid round.
//
// Cold start (before the belief store is wired, ADR-0037 D2/D7): the Gatekeeper
// is itself the precision oracle. Each candidate's expected-success prior is
// seeded from the **Gatekeeper's per-candidate merit score** (`ScoredCandidate.Score`)
// rather than a uniform constant. Discarding that score made every EFE value
// identical, so MinimizeEFE fell through to its lexical (alphabetical ResourceID)
// tie-break and the alphabetically-first agent won every task. Seeding from the
// score restores task-sensitive routing immediately; the belief-store-backed
// provider can still be swapped in later for region-resolved posteriors.
type GatekeeperEFESelector struct {
	gatekeeper       CandidateSource
	explorationBonus float64
}

// NewGatekeeperEFESelector builds the EFE arm over a Gatekeeper. The Gatekeeper's
// scores supply the cold-start precision priors (see type doc).
func NewGatekeeperEFESelector(gk CandidateSource, explorationBonus float64) *GatekeeperEFESelector {
	return &GatekeeperEFESelector{gatekeeper: gk, explorationBonus: explorationBonus}
}

// Select discovers candidates via the Gatekeeper then runs the EFE pick, seeding
// each candidate's expected-success prior from its Gatekeeper merit score.
func (g *GatekeeperEFESelector) Select(ctx context.Context, intent domain.Intent, _ []domain.AgentDefinition) (domain.Selection, error) {
	task := &domain.AuctionTask{ID: intent.ID, Description: intent.Description}
	scored, err := g.gatekeeper.FindCandidates(ctx, task)
	if err != nil {
		return domain.Selection{}, err
	}
	candidates := make([]domain.AgentDefinition, len(scored))
	scores := make(map[string]float64, len(scored))
	for i, sc := range scored {
		candidates[i] = sc.Agent
		scores[sc.Agent.ID] = sc.Score
	}
	inner := &InferenceSelector{
		// Low uniform confidence keeps the cold-start epistemic (exploration) term
		// alive; the per-candidate score differentiates the pragmatic term so the
		// pick is no longer a lexical tie. Fallback 0.5 for any unscored candidate.
		Precision:        gatekeeperScorePrecision{scores: scores, confidence: 0.1, fallback: 0.5},
		ExplorationBonus: g.explorationBonus,
	}
	return inner.Select(ctx, intent, candidates)
}

// gatekeeperScorePrecision is a cold-start PrecisionProvider that seeds each
// candidate's expected success from the Gatekeeper's merit score (held in a
// per-Select map keyed by resource ID), keeping confidence low and uniform so
// the EFE epistemic term still drives exploration (ADR-0037 D2/D7).
type gatekeeperScorePrecision struct {
	scores     map[string]float64
	confidence float64
	fallback   float64
}

func (p gatekeeperScorePrecision) PrecisionFor(_ context.Context, _ domain.Intent, candidates []domain.AgentDefinition) ([]domain.PrecisionWeight, error) {
	weights := make([]domain.PrecisionWeight, len(candidates))
	for i, c := range candidates {
		es, ok := p.scores[c.ID]
		if !ok {
			es = p.fallback
		}
		weights[i] = domain.PrecisionWeight{
			ResourceID:      c.ID,
			ExpectedSuccess: es,
			Confidence:      p.confidence,
		}
	}
	return weights, nil
}
