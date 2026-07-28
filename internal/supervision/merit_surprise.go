package supervision

import (
	"context"

	"github.com/cambrian-sh/core/domain"
	memstore "github.com/cambrian-sh/core/internal/memory/store"
)

// MeritSurpriseOracle implements domain.SurpriseOracle over the merit the
// ProfileAggregator already maintains (ADR-0049 §A2.3).
//
// It lives in supervision rather than memory because merit is supervision's data: this
// is the trust layer answering "what did we expect of this agent", and the memory layer
// merely consumes the answer.
type MeritSurpriseOracle struct {
	Profiles memstore.ProfileStore
}

// PredictionError returns |expected − actual| for one step outcome.
//
// The expectation is capability-scoped when ROUTE-06 has recorded stats for the step's
// capability, and falls back to the agent's global success rate otherwise. That order
// matters: a generalist with a high global rate can still be poor at one capability,
// and the whole point of the gate is to notice exactly that mismatch.
//
// Returns ok=false when there is no history to predict from. An unscored agent is not
// "unsurprising" — it is unknown — and the caller must not read a zero as confidence.
func (o *MeritSurpriseOracle) PredictionError(ctx context.Context, agentID, capability string, succeeded bool) (float64, bool) {
	if o == nil || o.Profiles == nil || agentID == "" {
		return 0, false
	}
	profile, err := o.Profiles.GetProfile(ctx, agentID, "")
	if err != nil || profile == nil {
		return 0, false
	}
	if capability != "" {
		if stat, ok := profile.CapabilityStats[capability]; ok && stat.SampleCount > 0 {
			return domain.SurpriseFrom(stat.SuccessRate, succeeded), true
		}
	}
	// A provisional agent has a placeholder rate, not an earned one; treating it as a
	// prediction would manufacture surprise out of a default.
	if profile.Provisional {
		return 0, false
	}
	if profile.SuccessRate <= 0 {
		return 0, false
	}
	return domain.SurpriseFrom(profile.SuccessRate, succeeded), true
}
