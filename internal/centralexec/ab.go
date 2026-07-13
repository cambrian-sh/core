package centralexec

import (
	"context"
	"hash/fnv"

	"github.com/cambrian-sh/core/domain"
)

// AuctionSelector is the A/B control arm (PRD-0037 coexistence): it adapts the
// existing Gatekeeper merit ranking to the unified ResourceSelector interface,
// selecting the highest-scored candidate and tagging the Selection "auction".
// It selects only — execution stays on the existing CallAgent path.
type AuctionSelector struct {
	Gatekeeper domain.Gatekeeper
}

// Select runs the Gatekeeper and returns the top-scored candidate.
func (s *AuctionSelector) Select(ctx context.Context, intent domain.Intent, _ []domain.AgentDefinition) (domain.Selection, error) {
	task := &domain.AuctionTask{ID: intent.ID, Description: intent.Description}
	scored, err := s.Gatekeeper.FindCandidates(ctx, task)
	if err != nil {
		return domain.Selection{}, err
	}
	if len(scored) == 0 {
		return domain.Selection{}, domain.ErrNoCandidates
	}
	best := scored[0]
	for _, c := range scored[1:] {
		if c.Score > best.Score {
			best = c
		}
	}
	return domain.Selection{
		ResourceID: best.Agent.ID,
		Mechanism:  domain.MechanismAuction,
		Confidence: best.Score,
	}, nil
}

// AssignVariant resolves the per-session selection mechanism (PRD-0037 A/B).
// "auction"/"efe" are absolute; "auto" is a session-scoped split at
// trafficPercent (0..100) via a stable FNV-1a hash of the session id, so every
// step in a plan uses the same mechanism (clean causal attribution). Anything
// else (or empty) resolves to the auction (safe default).
func AssignVariant(selectorMode string, trafficPercent int, sessionID string) string {
	switch selectorMode {
	case domain.MechanismEFE:
		return domain.MechanismEFE
	case "auto":
		if trafficPercent <= 0 {
			return domain.MechanismAuction
		}
		if trafficPercent >= 100 {
			return domain.MechanismEFE
		}
		h := fnv.New32a()
		_, _ = h.Write([]byte(sessionID))
		if int(h.Sum32()%100) < trafficPercent {
			return domain.MechanismEFE
		}
		return domain.MechanismAuction
	default:
		return domain.MechanismAuction
	}
}
