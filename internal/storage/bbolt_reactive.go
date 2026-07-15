package storage

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	"go.etcd.io/bbolt"
)

// REACT-01 (ADR-0061): durable-execution storage for the reactive lane. Four
// buckets back the premium ReactiveEngine's journal + idempotency + dead-letter:
//
//   - reactive_journal      seq(uint64,BE) → ReactiveJournalRecord  (append-before-eval)
//   - reactive_cursors      watchID        → seq(uint64,BE)         (per-watch ack cursor)
//   - reactive_idempotency  key            → executedAt(RFC3339Nano)(exactly-once marker)
//   - reactive_deadletter   id             → ReactiveDeadLetterRecord
//
// Storage stays domain-agnostic: the signal is carried as opaque SignalJSON bytes
// marshalled by the caller (the kernel decorator), so this layer never imports
// domain.Signal.
var (
	reactiveJournalBucket     = []byte("reactive_journal")
	reactiveCursorBucket      = []byte("reactive_cursors")
	reactiveIdempotencyBucket = []byte("reactive_idempotency")
	reactiveDeadLetterBucket  = []byte("reactive_deadletter")
)

// ReactiveJournalRecord is the raw JSON shape stored in the reactive_journal bucket.
// SignalJSON is the caller-marshalled domain.Signal (opaque to storage).
type ReactiveJournalRecord struct {
	Seq        uint64    `json:"seq"`
	StreamID   string    `json:"stream_id"`
	SignalJSON []byte    `json:"signal_json"`
	ReceivedAt time.Time `json:"received_at"`
	TTLExpires time.Time `json:"ttl_expires"`
}

// ReactiveDeadLetterRecord is the raw JSON shape stored in the reactive_deadletter
// bucket — an action that failed, or a journal signal that expired before it ran.
type ReactiveDeadLetterRecord struct {
	ID         string    `json:"id"`
	WatchID    string    `json:"watch_id"`
	ActionType string    `json:"action_type"`
	Key        string    `json:"key"`
	Reason     string    `json:"reason"`
	SignalJSON []byte    `json:"signal_json"`
	FailedAt   time.Time `json:"failed_at"`
}

// itob encodes a uint64 as 8 big-endian bytes so bbolt keys sort in seq order.
func itob(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

// AppendReactiveSignal appends a signal to the durable journal, assigning a
// monotonic sequence via the bucket's NextSequence, and returns that seq. This is
// the durability point: the signal survives a crash between receipt and action.
func (b *BBoltAdapter) AppendReactiveSignal(rec ReactiveJournalRecord) (uint64, error) {
	var seq uint64
	err := b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(reactiveJournalBucket)
		if bucket == nil {
			return fmt.Errorf("reactive_journal bucket not found")
		}
		n, err := bucket.NextSequence()
		if err != nil {
			return err
		}
		seq = n
		rec.Seq = seq
		data, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("reactive_journal marshal: %w", err)
		}
		return bucket.Put(itob(seq), data)
	})
	return seq, err
}

// ReplayReactiveFrom returns journal records with seq > afterSeq, in seq order.
// Used at engine start to resume from a per-watch cursor.
func (b *BBoltAdapter) ReplayReactiveFrom(afterSeq uint64) ([]ReactiveJournalRecord, error) {
	var recs []ReactiveJournalRecord
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(reactiveJournalBucket)
		if bucket == nil {
			return nil
		}
		c := bucket.Cursor()
		// Seek to the first key strictly after afterSeq.
		start := itob(afterSeq + 1)
		for k, v := c.Seek(start); k != nil; k, v = c.Next() {
			var rec ReactiveJournalRecord
			if json.Unmarshal(v, &rec) != nil {
				continue
			}
			recs = append(recs, rec)
		}
		return nil
	})
	return recs, err
}

// GetReactiveCursor returns the last-processed journal seq for a watch (0 if none).
func (b *BBoltAdapter) GetReactiveCursor(watchID string) (uint64, error) {
	var seq uint64
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(reactiveCursorBucket)
		if bucket == nil {
			return nil
		}
		v := bucket.Get([]byte(watchID))
		if len(v) == 8 {
			seq = binary.BigEndian.Uint64(v)
		}
		return nil
	})
	return seq, err
}

// SetReactiveCursor advances a watch's ack cursor to seq.
func (b *BBoltAdapter) SetReactiveCursor(watchID string, seq uint64) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(reactiveCursorBucket)
		if bucket == nil {
			return fmt.Errorf("reactive_cursors bucket not found")
		}
		return bucket.Put([]byte(watchID), itob(seq))
	})
}

// MarkReactiveExecutedOnce is the exactly-once primitive: an atomic check-and-set
// inside a single bbolt transaction. It returns true only the FIRST time a key is
// seen (and records executedAt); every subsequent call for the same key returns
// false. The engine executes an action only when this returns true, so replay,
// retry, or double-delivery of the same logical signal fires the action once — and
// the marker is persisted, so exactly-once survives a restart.
func (b *BBoltAdapter) MarkReactiveExecutedOnce(key string, at time.Time) (bool, error) {
	firstTime := false
	err := b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(reactiveIdempotencyBucket)
		if bucket == nil {
			return fmt.Errorf("reactive_idempotency bucket not found")
		}
		if bucket.Get([]byte(key)) != nil {
			return nil // already executed — firstTime stays false
		}
		firstTime = true
		return bucket.Put([]byte(key), []byte(at.UTC().Format(time.RFC3339Nano)))
	})
	if err != nil {
		return false, err
	}
	return firstTime, nil
}

// RecordReactiveDeadLetter persists a dead-letter entry.
func (b *BBoltAdapter) RecordReactiveDeadLetter(rec ReactiveDeadLetterRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("reactive_deadletter marshal: %w", err)
	}
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(reactiveDeadLetterBucket)
		if bucket == nil {
			return fmt.Errorf("reactive_deadletter bucket not found")
		}
		return bucket.Put([]byte(rec.ID), data)
	})
}

// ListReactiveDeadLetters returns dead-letter entries newest-first, capped at limit
// (limit <= 0 ⇒ all). Backs the operator ListWatchDeadLetters read RPC.
func (b *BBoltAdapter) ListReactiveDeadLetters(limit int) ([]ReactiveDeadLetterRecord, error) {
	var recs []ReactiveDeadLetterRecord
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(reactiveDeadLetterBucket)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, v []byte) error {
			var rec ReactiveDeadLetterRecord
			if json.Unmarshal(v, &rec) == nil {
				recs = append(recs, rec)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	// Newest-first by FailedAt.
	for i := 0; i < len(recs); i++ {
		for j := i + 1; j < len(recs); j++ {
			if recs[j].FailedAt.After(recs[i].FailedAt) {
				recs[i], recs[j] = recs[j], recs[i]
			}
		}
	}
	if limit > 0 && len(recs) > limit {
		recs = recs[:limit]
	}
	return recs, nil
}

// PruneReactiveJournal deletes journal records whose seq is at or below minAcked
// AND whose TTL has expired before now. Both conditions must hold: an un-acked or
// still-live record is never removed. Returns the count pruned. Bounded periodic
// compaction so the journal does not grow unbounded (ADR-0061).
func (b *BBoltAdapter) PruneReactiveJournal(minAcked uint64, now time.Time) (int, error) {
	pruned := 0
	err := b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(reactiveJournalBucket)
		if bucket == nil {
			return nil
		}
		c := bucket.Cursor()
		var toDelete [][]byte
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if len(k) != 8 {
				continue
			}
			seq := binary.BigEndian.Uint64(k)
			if seq > minAcked {
				break // keys are seq-ordered; nothing beyond is prunable by seq
			}
			var rec ReactiveJournalRecord
			if json.Unmarshal(v, &rec) != nil {
				continue
			}
			if !rec.TTLExpires.IsZero() && rec.TTLExpires.After(now) {
				continue // still live
			}
			kc := make([]byte, len(k))
			copy(kc, k)
			toDelete = append(toDelete, kc)
		}
		for _, k := range toDelete {
			if err := bucket.Delete(k); err != nil {
				return err
			}
			pruned++
		}
		return nil
	})
	return pruned, err
}
