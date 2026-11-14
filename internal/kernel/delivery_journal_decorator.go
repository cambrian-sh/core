package kernel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/storage"
)

// ADR-0090 D8.1 (ENG-1172): AgentRepoDecorator implements domain.DeliveryJournal
// over the kernel's bbolt store. Transition legality is decided here, against the
// domain's state machine, before the storage compare-and-set runs.

var _ domain.DeliveryJournal = (*AgentRepoDecorator)(nil)

// PrepareDelivery records in as a prepared intent unless its key exists.
func (d *AgentRepoDecorator) PrepareDelivery(_ context.Context, in domain.DeliveryIntent) (domain.DeliveryIntent, bool, error) {
	now := in.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	rec, created, err := d.store.PrepareDeliveryIntent(storage.DeliveryIntentRecord{
		Key:            in.Key,
		ConversationID: in.ConversationID,
		TxnID:          in.TxnID,
		IngressAgentID: in.Address.IngressAgentID,
		ExternalID:     in.Address.ExternalID,
		Text:           in.Text,
		State:          string(domain.DeliveryPrepared),
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	if err != nil {
		return domain.DeliveryIntent{}, false, err
	}
	return intentFromRecord(rec), created, nil
}

// AdvanceDelivery moves key from → to by compare-and-set.
func (d *AgentRepoDecorator) AdvanceDelivery(_ context.Context, key string, from, to domain.DeliveryIntentState, reason string) error {
	if !domain.CanDeliveryTransition(from, to) {
		return fmt.Errorf("delivery journal: illegal transition %s → %s for %q", from, to, key)
	}
	err := d.store.AdvanceDeliveryIntent(key, string(from), string(to), reason, time.Now().UTC(),
		to == domain.DeliveryDispatched, to == domain.DeliveryConfirmed)
	if errors.Is(err, storage.ErrDeliveryIntentConflict) {
		return fmt.Errorf("%w: %v", domain.ErrDeliveryStateConflict, err)
	}
	return err
}

// ListDeliveries returns intents in any of states, least recently updated first.
func (d *AgentRepoDecorator) ListDeliveries(_ context.Context, states []domain.DeliveryIntentState, limit int) ([]domain.DeliveryIntent, error) {
	names := make([]string, 0, len(states))
	for _, s := range states {
		names = append(names, string(s))
	}
	recs, err := d.store.ListDeliveryIntents(names, limit)
	if err != nil {
		return nil, err
	}
	out := make([]domain.DeliveryIntent, 0, len(recs))
	for _, r := range recs {
		out = append(out, intentFromRecord(r))
	}
	return out, nil
}

// PruneDeliveries removes settled and ambiguous intents last updated before `before`.
func (d *AgentRepoDecorator) PruneDeliveries(_ context.Context, before time.Time, limit int) (int, bool, error) {
	return d.store.PruneDeliveryIntents(before, limit)
}

func intentFromRecord(r storage.DeliveryIntentRecord) domain.DeliveryIntent {
	return domain.DeliveryIntent{
		Key:            r.Key,
		ConversationID: r.ConversationID,
		TxnID:          r.TxnID,
		Address:        domain.DeliveryAddress{IngressAgentID: r.IngressAgentID, ExternalID: r.ExternalID},
		Text:           r.Text,
		State:          domain.DeliveryIntentState(r.State),
		Attempts:       r.Attempts,
		Err:            r.Err,
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
	}
}
