package kernel

import (
	"encoding/json"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/storage"
)

// AgentRepoDecorator satisfies app.ReactiveJournal (REACT-01 / ADR-0061) by mapping
// domain carrier types to the storage DTOs and marshalling the domain.Signal to the
// opaque SignalJSON the storage layer stores. The assertion cannot live in the app
// package's terms here (app imports kernel), so satisfaction is structural — checked
// at the app.ReactiveServices{Journal: reg} literal.

// AppendSignal durably records a signal before condition evaluation and returns its
// monotonic sequence number. A zero/negative ttl stores no expiry (never pruned by TTL).
func (d *AgentRepoDecorator) AppendSignal(sig domain.Signal, ttl time.Duration) (uint64, error) {
	data, err := json.Marshal(sig)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	var ttlExpires time.Time
	if ttl > 0 {
		ttlExpires = now.Add(ttl)
	}
	return d.store.AppendReactiveSignal(storage.ReactiveJournalRecord{
		StreamID:   sig.StreamID,
		SignalJSON: data,
		ReceivedAt: now,
		TTLExpires: ttlExpires,
	})
}

// ReplayFrom returns journaled signals with seq strictly greater than afterSeq.
func (d *AgentRepoDecorator) ReplayFrom(afterSeq uint64) ([]domain.JournaledSignal, error) {
	recs, err := d.store.ReplayReactiveFrom(afterSeq)
	if err != nil {
		return nil, err
	}
	out := make([]domain.JournaledSignal, 0, len(recs))
	for _, r := range recs {
		var sig domain.Signal
		if json.Unmarshal(r.SignalJSON, &sig) != nil {
			continue // skip malformed
		}
		out = append(out, domain.JournaledSignal{Seq: r.Seq, Signal: sig})
	}
	return out, nil
}

// GetCursor returns the last-acked journal seq for a watch (0 if none).
func (d *AgentRepoDecorator) GetCursor(watchID string) (uint64, error) {
	return d.store.GetReactiveCursor(watchID)
}

// SetCursor advances a watch's ack cursor.
func (d *AgentRepoDecorator) SetCursor(watchID string, seq uint64) error {
	return d.store.SetReactiveCursor(watchID, seq)
}

// MarkExecutedOnce is the exactly-once primitive: true only the first time key is seen.
func (d *AgentRepoDecorator) MarkExecutedOnce(key string) (bool, error) {
	return d.store.MarkReactiveExecutedOnce(key, time.Now().UTC())
}

// RecordDeadLetter persists an undeliverable action or an expired signal.
func (d *AgentRepoDecorator) RecordDeadLetter(dl domain.ReactiveDeadLetter) error {
	data, err := json.Marshal(dl.Signal)
	if err != nil {
		return err
	}
	return d.store.RecordReactiveDeadLetter(storage.ReactiveDeadLetterRecord{
		ID:         dl.ID,
		WatchID:    dl.WatchID,
		ActionType: dl.ActionType,
		Key:        dl.Key,
		Reason:     dl.Reason,
		SignalJSON: data,
		FailedAt:   dl.FailedAt,
	})
}

// ListDeadLetters returns dead-letter entries newest-first (limit <= 0 ⇒ all).
func (d *AgentRepoDecorator) ListDeadLetters(limit int) ([]domain.ReactiveDeadLetter, error) {
	recs, err := d.store.ListReactiveDeadLetters(limit)
	if err != nil {
		return nil, err
	}
	out := make([]domain.ReactiveDeadLetter, 0, len(recs))
	for _, r := range recs {
		var sig domain.Signal
		_ = json.Unmarshal(r.SignalJSON, &sig) // best-effort; empty Signal on failure
		out = append(out, domain.ReactiveDeadLetter{
			ID:         r.ID,
			WatchID:    r.WatchID,
			ActionType: r.ActionType,
			Key:        r.Key,
			Reason:     r.Reason,
			Signal:     sig,
			FailedAt:   r.FailedAt,
		})
	}
	return out, nil
}

// Prune drops journal records at/below minAcked whose TTL has expired.
func (d *AgentRepoDecorator) Prune(minAcked uint64) (int, error) {
	return d.store.PruneReactiveJournal(minAcked, time.Now().UTC())
}
