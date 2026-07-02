package centralexec

import (
	"context"
)

// BeliefUpdater is the consumer-side view of the belief store the Verifier
// signal writes to (ADR-0037 D8). It resolves an intent to a region internally;
// the Verifier supplies only what it scored.
type BeliefUpdater interface {
	UpdateForIntent(ctx context.Context, resourceID string, intentEmbedding []float32, success float64) error
}

// SurveillanceFlagger receives fast-path inhibition signals — the retained
// one-run eviction / Surveillance mode (ADR-0013/0014) that guards the
// offline-consolidation lag window (D8). Optional.
type SurveillanceFlagger interface {
	Flag(resourceID string)
}

// VerifierConsolidator turns the Verifier Pool's post-execution quality signal
// into a precision-learning update (ADR-0037 D8). It writes to the fast store
// immediately (UpdateForIntent); offline interleaving into the slow store is
// the store's Consolidate. A single below-threshold run fires fast-path
// inhibition so a flaky resource is caught before consolidation corrects it.
type VerifierConsolidator struct {
	Updater BeliefUpdater
	// InhibitionThreshold is the one-run quality floor; a run below it fires
	// fast-path inhibition.
	InhibitionThreshold float64
	// Inhibitor, when set, is notified on fast-path inhibition.
	Inhibitor SurveillanceFlagger
}

// Consume records a Verifier quality score [0,1] for a resource on an intent and
// reports whether fast-path inhibition fired (a single run below the threshold).
func (c *VerifierConsolidator) Consume(ctx context.Context, resourceID string, intentEmbedding []float32, quality float64) (bool, error) {
	if err := c.Updater.UpdateForIntent(ctx, resourceID, intentEmbedding, quality); err != nil {
		return false, err
	}
	if quality < c.InhibitionThreshold {
		if c.Inhibitor != nil {
			c.Inhibitor.Flag(resourceID)
		}
		return true, nil
	}
	return false, nil
}
