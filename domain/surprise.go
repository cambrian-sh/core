package domain

import "context"

// SurpriseOracle answers "how much did this outcome differ from what the kernel
// expected?" — the ADR-0049 §A2.3 write gate.
//
// The gate is SURPRISE, not failure. A plan predicted to fail that fails teaches
// nothing; a plan predicted to succeed that failed is the informative one, and so is a
// surprising success. `1 − quality` would be a failure gate and would systematically
// over-record routine failures while missing every surprising success.
//
// The prediction comes from MERIT (AgentProfile.SuccessRate, or CapabilityStats when
// ROUTE-06 is armed) rather than from the agent's bid confidence, for two reasons:
// merit is already live and unconditional, whereas calibrated bids are a default-off
// arm; and merit is the kernel's own belief rather than the agent's self-report, which
// keeps the gate unforgeable in the sense ADR-0035 means.
//
// A2.3 originally said the verifier "already produces a prediction error". It does not
// — it produces a quality SCORE, an outcome with nothing to subtract it from. This port
// is where the error is actually constructed.
type SurpriseOracle interface {
	// PredictionError returns |expected − actual| in [0,1] for one step outcome, and
	// whether a prediction was available at all. No merit history ⇒ ok=false, and the
	// caller must treat that as "unknown", never as zero surprise: an agent nobody has
	// scored yet is the opposite of predictable.
	PredictionError(ctx context.Context, agentID, capability string, succeeded bool) (float64, bool)
}

// SurpriseFrom computes |expected − actual| given a merit expectation. Pure, so the
// arithmetic is testable without a profile store.
//
// `actual` is 1.0 for a successful step and 0.0 for a failed one. Using the binary
// outcome rather than a graded score is deliberate at this layer: the verifier only
// samples ~10% of a trusted agent's work, so a graded outcome would be unavailable for
// most steps, and a gate that silently degrades on nine tenths of its input is worse
// than a coarser one that always applies.
func SurpriseFrom(expected float64, succeeded bool) float64 {
	actual := 0.0
	if succeeded {
		actual = 1.0
	}
	d := expected - actual
	if d < 0 {
		d = -d
	}
	if d > 1 {
		return 1
	}
	return d
}
