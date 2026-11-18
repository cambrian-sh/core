package kernel

import (
	"context"
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
// monotonic sequence number. A zero/negative ttl stores no expiry: the record is then
// governed by its stream's cursor floor alone (PruneReactiveJournal).
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

// ReplayStreams returns the journaled signals with seq > afterSeq on the streams
// match selects, in seq order (ADR-0061 A2.1). The signal is decoded once — the
// record header and the stream are not JSON (A2.3).
func (d *AgentRepoDecorator) ReplayStreams(match func(stream string) bool, afterSeq uint64) ([]domain.JournaledSignal, error) {
	recs, err := d.store.ReplayReactiveStreams(match, afterSeq)
	if err != nil {
		return nil, err
	}
	out := make([]domain.JournaledSignal, 0, len(recs))
	for _, r := range recs {
		var sig domain.Signal
		if json.Unmarshal(r.SignalJSON, &sig) != nil {
			continue // skip malformed
		}
		out = append(out, domain.JournaledSignal{Seq: r.Seq, Signal: sig, ReceivedAt: r.ReceivedAt})
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

// DeleteCursor removes a watch's cursor when the watch is removed (A2.9).
func (d *AgentRepoDecorator) DeleteCursor(watchID string) error {
	return d.store.DeleteReactiveCursor(watchID)
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

// PruneDeadLetters drops up to limit dead letters that failed before `before`
// (ENG-472 retention; the premium JournalGC calls it on every pass).
func (d *AgentRepoDecorator) PruneDeadLetters(before time.Time, limit int) (int, bool, error) {
	return d.store.PruneReactiveDeadLetters(before, limit)
}

// DeleteDeadLetter removes one dead letter by id (operator DeleteWatchDeadLetter).
func (d *AgentRepoDecorator) DeleteDeadLetter(_ context.Context, id string) (bool, error) {
	return d.store.DeleteReactiveDeadLetter(id)
}

// ListDeadLetters returns dead-letter entries newest-first (limit <= 0 ⇒ all).
func (d *AgentRepoDecorator) ListDeadLetters(_ context.Context, limit int) ([]domain.ReactiveDeadLetter, error) {
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

// Prune drops up to limit TTL-expired journal records at or below their stream's
// floor (ADR-0061 A2.2), reporting whether more remained at the cap (GOV-02).
func (d *AgentRepoDecorator) Prune(floor func(stream string) (uint64, bool), now time.Time, limit int) (int, bool, error) {
	return d.store.PruneReactiveJournal(floor, now, limit)
}

// PruneIdempotency drops up to limit exactly-once markers executed before
// olderThan (A2.6), reporting whether more remained at the cap.
func (d *AgentRepoDecorator) PruneIdempotency(olderThan time.Time, limit int) (int, bool, error) {
	return d.store.PruneReactiveIdempotency(olderThan, limit)
}

// RetainedWindow reports the journal's remaining seq range and record count on the
// streams match selects (nil ⇒ all), so a backtest can state the history it
// actually had rather than implying it saw all of it.
func (d *AgentRepoDecorator) RetainedWindow(match func(stream string) bool) (uint64, uint64, int, error) {
	return d.store.ReactiveJournalWindow(match)
}
