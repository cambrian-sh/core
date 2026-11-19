package storage

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"go.etcd.io/bbolt"
)

// REACT-01 (ADR-0061): durable-execution storage for the reactive lane. Four
// buckets back the premium ReactiveEngine's journal + idempotency + dead-letter:
//
//   - reactive_journal_v2   <stream>/seq(uint64,BE) → header|signal JSON (A2.1, A2.3)
//   - reactive_cursors      watchID        → seq(uint64,BE)         (per-watch ack cursor)
//   - reactive_idempotency  key            → executedAt(RFC3339Nano)(exactly-once marker)
//   - reactive_deadletter   id             → ReactiveDeadLetterRecord
//
// The legacy flat `reactive_journal` bucket (seq → JSON record, every stream in one
// key space) is migrated into reactive_journal_v2 on open and then deleted
// (ADR-0061 A2.10, migrateReactiveJournal).
//
// Storage stays domain-agnostic: the signal is carried as opaque SignalJSON bytes
// marshalled by the caller (the kernel decorator), so this layer never imports
// domain.Signal.
var (
	// reactiveJournalLegacyBucket is the pre-A2 flat journal. Never created any
	// more; read only by the migration.
	reactiveJournalLegacyBucket = []byte("reactive_journal")
	// reactiveJournalBucket is the A2 parent bucket. It holds ONLY nested
	// per-stream buckets, and its NextSequence is the journal-wide monotonic seq
	// (A2.1): one counter for every stream, so a watch keeps one cursor and replay
	// order across streams is append order.
	reactiveJournalBucket     = []byte("reactive_journal_v2")
	reactiveCursorBucket      = []byte("reactive_cursors")
	reactiveIdempotencyBucket = []byte("reactive_idempotency")
	reactiveDeadLetterBucket  = []byte("reactive_deadletter")
)

// emptyStreamBucket names the stream bucket for a signal with an empty StreamID.
// bbolt refuses a zero-length bucket name, and a NUL byte is not a stream id any
// producer emits.
var emptyStreamBucket = []byte{0}

// journalHeaderLen is the fixed record header (ADR-0061 A2.3):
// ttl_expires (8 B, unix nanos, 0 = no expiry) | received_at (8 B, unix nanos).
// Prune and the retained-window read only these 16 bytes and never decode the
// signal.
const journalHeaderLen = 16

// migrateBatch bounds each migration transaction (A2.10), for the same reason
// A1.1 bounds a prune pass: one transaction over a large legacy bucket would hold
// bbolt's single write lock for the whole copy.
const migrateBatch = 10000

// ReactiveJournalRecord is one journal record as the storage API exchanges it.
// On disk (A2.3) Seq is the key, StreamID is the bucket name, and the value is the
// 16-byte header followed by SignalJSON — neither is repeated in the value. The
// JSON tags are the legacy flat-bucket encoding, read only by the migration.
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

// streamBucketName maps a stream id to its nested bucket name.
func streamBucketName(stream string) []byte {
	if stream == "" {
		return emptyStreamBucket
	}
	return []byte(stream)
}

// streamFromBucketName is the inverse of streamBucketName.
func streamFromBucketName(name []byte) string {
	if bytes.Equal(name, emptyStreamBucket) {
		return ""
	}
	return string(name)
}

// unixNanos encodes t for the header; the zero time is 0 ("unset"), because
// time.Time{}.UnixNano() is outside int64's range and undefined.
func unixNanos(t time.Time) uint64 {
	if t.IsZero() {
		return 0
	}
	return uint64(t.UnixNano())
}

func fromUnixNanos(n uint64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(n)).UTC()
}

// encodeJournalValue builds the A2.3 value: header then the signal JSON, once.
func encodeJournalValue(rec ReactiveJournalRecord) []byte {
	v := make([]byte, journalHeaderLen+len(rec.SignalJSON))
	binary.BigEndian.PutUint64(v[0:8], unixNanos(rec.TTLExpires))
	binary.BigEndian.PutUint64(v[8:16], unixNanos(rec.ReceivedAt))
	copy(v[journalHeaderLen:], rec.SignalJSON)
	return v
}

// journalTTL reads a value's ttl_expires header (0 = no expiry) without touching
// the signal. ok is false for a value too short to carry a header.
func journalTTL(v []byte) (ttlNanos uint64, ok bool) {
	if len(v) < journalHeaderLen {
		return 0, false
	}
	return binary.BigEndian.Uint64(v[0:8]), true
}

// decodeJournalValue copies a stored value out of bbolt's mmap (valid only inside
// the transaction) into a record.
func decodeJournalValue(stream string, k, v []byte) (ReactiveJournalRecord, bool) {
	if len(k) != 8 || len(v) < journalHeaderLen {
		return ReactiveJournalRecord{}, false
	}
	sig := make([]byte, len(v)-journalHeaderLen)
	copy(sig, v[journalHeaderLen:])
	return ReactiveJournalRecord{
		Seq:        binary.BigEndian.Uint64(k),
		StreamID:   stream,
		SignalJSON: sig,
		TTLExpires: fromUnixNanos(binary.BigEndian.Uint64(v[0:8])),
		ReceivedAt: fromUnixNanos(binary.BigEndian.Uint64(v[8:16])),
	}, true
}

// streamBuckets lists the stream names under the journal parent. Every key of the
// parent is a nested bucket (v == nil); anything else is skipped defensively.
func streamBuckets(parent *bbolt.Bucket) [][]byte {
	var names [][]byte
	_ = parent.ForEach(func(k, v []byte) error {
		if v == nil {
			names = append(names, append([]byte(nil), k...))
		}
		return nil
	})
	return names
}

// AppendReactiveSignal appends a signal to the durable journal under its stream,
// assigning the journal-wide monotonic seq from the parent bucket's NextSequence,
// and returns that seq. This is the durability point: the signal survives a crash
// between receipt and action.
//
// BATCHED WHEN CONCURRENT (ADR-0061 A2.7). db.Batch coalesces concurrent
// appends into one transaction and one fsync; it returns only after the
// transaction holding this record has committed, so durability is unchanged.
// Batch may re-run the function on its own after a sibling in the batch failed,
// which is safe here: the function has no effect outside the transaction except
// assigning seq, which the re-run overwrites.
//
// A LONE appender does not use Batch. bbolt starts a batch only when it fills
// (MaxBatchSize) or its window (MaxBatchDelay, 10 ms) elapses, so a single
// producer would wait out the window on every append: measured on the A2 gate,
// 553/s with a plain Update and 81/s through Batch. With nobody to share a
// transaction with there is nothing to gain by waiting, so the first appender in
// flight commits directly and only those that arrive while one is in flight
// coalesce.
func (b *BBoltAdapter) AppendReactiveSignal(rec ReactiveJournalRecord) (uint64, error) {
	var seq uint64
	value := encodeJournalValue(rec)
	commit := b.db.Batch
	if b.journalAppenders.Add(1) == 1 {
		commit = b.db.Update
	}
	defer b.journalAppenders.Add(-1)
	err := commit(func(tx *bbolt.Tx) error {
		parent := tx.Bucket(reactiveJournalBucket)
		if parent == nil {
			return fmt.Errorf("reactive_journal_v2 bucket not found")
		}
		n, err := parent.NextSequence()
		if err != nil {
			return err
		}
		sb, err := parent.CreateBucketIfNotExists(streamBucketName(rec.StreamID))
		if err != nil {
			return fmt.Errorf("reactive_journal_v2 stream bucket %q: %w", rec.StreamID, err)
		}
		// Keys within a stream bucket only ever ascend (one global counter), so
		// pages can be packed full: a split then only ever happens at the right
		// edge, and the file grows at the rate of the data rather than twice it.
		sb.FillPercent = 1.0
		seq = n
		return sb.Put(itob(n), value)
	})
	return seq, err
}

// AppendReactiveSignals appends a page of signals in ONE transaction and one
// fsync, returning their seqs in input order (ENG-1292). It is the batched form
// of AppendReactiveSignal for a producer that holds a whole page at once — a
// history fill journaling hundreds of records — where one durable append per
// record would make the fill fsync-bound. Each record gets the next journal-wide
// seq, exactly as if appended one by one, so replay and cursors cannot tell the
// difference. All or nothing: a failure commits none of the page, and the caller
// re-sends it (its cursor has not moved). An empty page is a no-op.
func (b *BBoltAdapter) AppendReactiveSignals(recs []ReactiveJournalRecord) ([]uint64, error) {
	if len(recs) == 0 {
		return nil, nil
	}
	seqs := make([]uint64, len(recs))
	err := b.db.Update(func(tx *bbolt.Tx) error {
		parent := tx.Bucket(reactiveJournalBucket)
		if parent == nil {
			return fmt.Errorf("reactive_journal_v2 bucket not found")
		}
		for i, rec := range recs {
			n, err := parent.NextSequence()
			if err != nil {
				return err
			}
			sb, err := parent.CreateBucketIfNotExists(streamBucketName(rec.StreamID))
			if err != nil {
				return fmt.Errorf("reactive_journal_v2 stream bucket %q: %w", rec.StreamID, err)
			}
			sb.FillPercent = 1.0 // ascending keys only, as in AppendReactiveSignal
			if err := sb.Put(itob(n), encodeJournalValue(rec)); err != nil {
				return err
			}
			seqs[i] = n
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return seqs, nil
}

// ReplayReactiveStreams returns the journal records with seq > afterSeq on every
// stream for which match returns true, merged into seq order (ADR-0061 A2.1).
// Streams that do not match are never read, so an idle stream — or a large one no
// replaying watch subscribes to — costs nothing. A nil match reads every stream.
//
// match runs inside a read transaction and must not call back into the store.
func (b *BBoltAdapter) ReplayReactiveStreams(match func(stream string) bool, afterSeq uint64) ([]ReactiveJournalRecord, error) {
	var recs []ReactiveJournalRecord
	buckets := 0
	err := b.db.View(func(tx *bbolt.Tx) error {
		parent := tx.Bucket(reactiveJournalBucket)
		if parent == nil {
			return nil
		}
		start := itob(afterSeq + 1)
		for _, name := range streamBuckets(parent) {
			stream := streamFromBucketName(name)
			if match != nil && !match(stream) {
				continue
			}
			sb := parent.Bucket(name)
			if sb == nil {
				continue
			}
			buckets++
			c := sb.Cursor()
			for k, v := c.Seek(start); k != nil; k, v = c.Next() {
				if rec, ok := decodeJournalValue(stream, k, v); ok {
					recs = append(recs, rec)
				}
			}
		}
		return nil
	})
	if buckets > 1 {
		// Each bucket is already in seq order; the merge across buckets is what
		// keeps replay in append order for a prefix or match-all reader.
		sort.Slice(recs, func(i, j int) bool { return recs[i].Seq < recs[j].Seq })
	}
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

// DeleteReactiveCursor removes a watch's cursor (ADR-0061 A2.9): a cursor
// belongs to its watch and goes when the watch goes. Deleting an absent cursor is
// not an error.
func (b *BBoltAdapter) DeleteReactiveCursor(watchID string) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(reactiveCursorBucket)
		if bucket == nil {
			return nil
		}
		return bucket.Delete([]byte(watchID))
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

// PruneReactiveIdempotency deletes up to limit exactly-once markers whose
// executedAt is before olderThan, and reports whether more remained at the cap
// (ADR-0061 A2.6; limit <= 0 ⇒ unbounded).
//
// Safe because a duplicate can only arrive while the original signal is still
// replay-eligible (journal TTL) or inside its dedup window; the caller passes a
// cutoff older than both, so a marker past it guards nothing that can recur.
//
// The keys are hashes, not times, so finding the old ones is a full scan. It runs
// in a READ transaction, and only the deletions take the write lock, so a pass
// that finds nothing to delete never stalls an append.
//
// A marker whose value does not parse as a time is kept: its age is unknown, and
// dropping an exactly-once marker of unknown age is the one mistake here that
// could re-execute an action.
func (b *BBoltAdapter) PruneReactiveIdempotency(olderThan time.Time, limit int) (pruned int, more bool, err error) {
	var victims [][]byte
	err = b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(reactiveIdempotencyBucket)
		if bucket == nil {
			return nil
		}
		c := bucket.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			at, perr := time.Parse(time.RFC3339Nano, string(v))
			if perr != nil || !at.Before(olderThan) {
				continue
			}
			if limit > 0 && len(victims) >= limit {
				more = true
				break
			}
			victims = append(victims, append([]byte(nil), k...))
		}
		return nil
	})
	if err != nil || len(victims) == 0 {
		return 0, more, err
	}
	err = b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(reactiveIdempotencyBucket)
		if bucket == nil {
			return nil
		}
		for _, k := range victims {
			// A marker is written once and never rewritten (MarkReactiveExecutedOnce
			// refuses an existing key), so what the scan saw is still what is there.
			if bucket.Get(k) == nil {
				continue
			}
			if derr := bucket.Delete(k); derr != nil {
				return derr
			}
			pruned++
		}
		return nil
	})
	return pruned, more, err
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

// PruneReactiveDeadLetters drops up to limit dead letters that failed before
// `before` (limit <= 0 ⇒ unbounded) and reports whether more remained at the cap.
// A dead letter embeds the full signal (user chat text included), so it gets a
// retention like every other copy of personal content (ENG-472, owner decision
// 2026-11-15: a separate knob, default 30 days — the premium JournalGC calls this).
func (b *BBoltAdapter) PruneReactiveDeadLetters(before time.Time, limit int) (removed int, more bool, err error) {
	err = b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(reactiveDeadLetterBucket)
		if bucket == nil {
			return nil
		}
		var victims [][]byte
		c := bucket.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var rec ReactiveDeadLetterRecord
			if json.Unmarshal(v, &rec) != nil || !rec.FailedAt.Before(before) {
				continue
			}
			if limit > 0 && len(victims) >= limit {
				more = true
				break
			}
			victims = append(victims, append([]byte(nil), k...))
		}
		for _, k := range victims {
			if derr := bucket.Delete(k); derr != nil {
				return derr
			}
			removed++
		}
		return nil
	})
	return removed, more, err
}

// DeleteReactiveDeadLetter removes one dead letter by id (an operator resolving
// it, ENG-472). found=false when no such entry exists.
func (b *BBoltAdapter) DeleteReactiveDeadLetter(id string) (found bool, err error) {
	err = b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(reactiveDeadLetterBucket)
		if bucket == nil || bucket.Get([]byte(id)) == nil {
			return nil
		}
		found = true
		return bucket.Delete([]byte(id))
	})
	return found, err
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

// PruneReactiveJournal drops up to limit journal records that are TTL-expired and
// at or below their stream's floor, and reports whether more remained at the cap
// (limit <= 0 ⇒ unbounded).
//
// PER-STREAM FLOOR (ADR-0061 A2.2, replacing A1.2's single global floor). floor
// reports, for one stream, the lowest cursor among the watches that consume it
// and whether any watch does. A record on a stream with a consumer is prunable
// only at or below that floor; a stream nothing consumes is governed by TTL alone.
// The global floor this replaces let one idle watch on a quiet stream keep every
// OTHER stream's records forever (965,266 retained on 2026-11-02, floor at seq 3).
// floor is called inside the write transaction and must not call back into the
// store; the GC computes it from a snapshot taken before the pass.
//
// PREFIX WALK (A2.4). Keys in a stream bucket ascend with arrival, so each bucket
// is walked from its first key and the walk stops at the first record above the
// floor or not yet expired. Only the 16-byte header is read; no signal is
// decoded. Stopping at the first live record is conservative when the TTL setting
// changed between runs (a later record that expires sooner waits for the earlier
// one), never the other way round.
//
// A record with no expiry (header 0) is governed by its floor alone, as in A1. A
// value too short to carry a header cannot be replayed (the reader skips it), so
// it is dropped once it is within the floor.
//
// BOUNDED (A1.1, GOV-02): bbolt serializes writers, so an unbounded pass over a
// large backlog would stall every signal append behind it. A stream bucket left
// empty is deleted, so the parent's key space tracks the live streams.
func (b *BBoltAdapter) PruneReactiveJournal(floor func(stream string) (seq uint64, hasConsumer bool), now time.Time, limit int) (pruned int, more bool, err error) {
	nowNanos := unixNanos(now)
	err = b.db.Update(func(tx *bbolt.Tx) error {
		parent := tx.Bucket(reactiveJournalBucket)
		if parent == nil {
			return nil
		}
		for _, name := range streamBuckets(parent) {
			sb := parent.Bucket(name)
			if sb == nil {
				continue
			}
			var fl uint64
			hasConsumer := false
			if floor != nil {
				fl, hasConsumer = floor(streamFromBucketName(name))
			}
			var toDelete [][]byte
			c := sb.Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				if len(k) != 8 {
					continue
				}
				if hasConsumer && binary.BigEndian.Uint64(k) > fl {
					break // nothing beyond is acked by every consumer of this stream
				}
				if ttl, ok := journalTTL(v); ok && ttl != 0 && ttl > nowNanos {
					break // still replay-eligible, and later arrivals expire later
				}
				if limit > 0 && pruned+len(toDelete) >= limit {
					// Another record was prunable and is left for the next pass.
					// Reported so a caller can tell "done" from "capped"; the GC
					// runs the next pass promptly instead of after a full interval.
					more = true
					break
				}
				toDelete = append(toDelete, append([]byte(nil), k...))
			}
			for _, k := range toDelete {
				if derr := sb.Delete(k); derr != nil {
					return derr
				}
			}
			pruned += len(toDelete)
			if len(toDelete) > 0 {
				if k, _ := sb.Cursor().First(); k == nil {
					if derr := parent.DeleteBucket(name); derr != nil {
						return derr
					}
				}
			}
			if more {
				return nil
			}
		}
		return nil
	})
	return pruned, more, err
}

// ReactiveJournalWindow reports what the journal still holds on the streams match
// selects (nil ⇒ every stream): the oldest and newest seq present, and how many
// records there are.
//
// Exists for GOV-02's backtest requirement. `Backtest` replays journalled history
// to answer "would this watch have fired?", and pruning silently shortens the
// history it can reach — so the window has to be stateable alongside the verdicts
// rather than left for the reader to assume. The stream filter (A2) makes the
// window describe the streams the backtested watch actually reads.
//
// An empty selection returns (0, 0, 0, nil): no window, stated as such.
func (b *BBoltAdapter) ReactiveJournalWindow(match func(stream string) bool) (oldestSeq, newestSeq uint64, count int, err error) {
	err = b.db.View(func(tx *bbolt.Tx) error {
		parent := tx.Bucket(reactiveJournalBucket)
		if parent == nil {
			return nil
		}
		for _, name := range streamBuckets(parent) {
			if match != nil && !match(streamFromBucketName(name)) {
				continue
			}
			sb := parent.Bucket(name)
			if sb == nil {
				continue
			}
			c := sb.Cursor()
			first, _ := c.First()
			last, _ := c.Last()
			if len(first) != 8 || len(last) != 8 {
				continue
			}
			lo, hi := binary.BigEndian.Uint64(first), binary.BigEndian.Uint64(last)
			if count == 0 || lo < oldestSeq {
				oldestSeq = lo
			}
			if hi > newestSeq {
				newestSeq = hi
			}
			count += sb.Stats().KeyN
		}
		return nil
	})
	return oldestSeq, newestSeq, count, err
}

// migrateReactiveJournal moves the legacy flat `reactive_journal` bucket into
// reactive_journal_v2 under each record's stream, keeping its seq, then deletes
// the legacy bucket (ADR-0061 A2.10). Called on open, after the buckets exist.
//
// IDEMPOTENT. Each batch moves records and deletes them from the legacy bucket in
// the same transaction, so a crash between batches leaves every record in exactly
// one place, and the next open resumes where this one stopped. With no legacy
// bucket it is a no-op.
//
// SEQ CONTINUITY. Cursors hold legacy seqs, so the v2 counter is first raised to
// at least the legacy bucket's sequence (and its last key): a new append must
// never reuse a seq a cursor has already passed.
//
// EVERY RECORD IS MOVED. A2.10 says records already prunable under A2.2 are
// dropped rather than moved, but the per-stream floor needs the registered
// watches and their streams, which only the premium engine knows — storage is
// opened before any plugin is built. Dropping on TTL alone would be wrong for a
// stream with an un-acked consumer, so the migration moves everything and the
// first GC pass (which drains continuously while a backlog remains) prunes what
// A2.2 allows. A record that does not decode is dropped: the reader skipped it
// before, so it was never replayable.
func migrateReactiveJournal(db *bbolt.DB) (moved, dropped int, err error) {
	// Seq continuity first, in its own transaction, so it holds even if a batch
	// below fails.
	legacyPresent := false
	err = db.Update(func(tx *bbolt.Tx) error {
		legacy := tx.Bucket(reactiveJournalLegacyBucket)
		if legacy == nil {
			return nil
		}
		legacyPresent = true
		parent, cerr := tx.CreateBucketIfNotExists(reactiveJournalBucket)
		if cerr != nil {
			return cerr
		}
		high := legacy.Sequence()
		if k, _ := legacy.Cursor().Last(); len(k) == 8 {
			if s := binary.BigEndian.Uint64(k); s > high {
				high = s
			}
		}
		if parent.Sequence() < high {
			return parent.SetSequence(high)
		}
		return nil
	})
	if err != nil || !legacyPresent {
		return 0, 0, err
	}

	for {
		done := false
		err = db.Update(func(tx *bbolt.Tx) error {
			legacy := tx.Bucket(reactiveJournalLegacyBucket)
			if legacy == nil {
				done = true
				return nil
			}
			parent := tx.Bucket(reactiveJournalBucket)
			var keys [][]byte
			c := legacy.Cursor()
			for k, v := c.First(); k != nil && len(keys) < migrateBatch; k, v = c.Next() {
				keys = append(keys, append([]byte(nil), k...))
				var rec ReactiveJournalRecord
				if len(k) != 8 || json.Unmarshal(v, &rec) != nil {
					dropped++
					continue
				}
				sb, cerr := parent.CreateBucketIfNotExists(streamBucketName(rec.StreamID))
				if cerr != nil {
					return cerr
				}
				sb.FillPercent = 1.0
				if perr := sb.Put(k, encodeJournalValue(rec)); perr != nil {
					return perr
				}
				moved++
			}
			if len(keys) == 0 {
				done = true
				return tx.DeleteBucket(reactiveJournalLegacyBucket)
			}
			for _, k := range keys {
				if derr := legacy.Delete(k); derr != nil {
					return derr
				}
			}
			return nil
		})
		if err != nil {
			return moved, dropped, fmt.Errorf("reactive journal migration: %w", err)
		}
		if done {
			break
		}
	}
	slog.Info("DB (BBOLT): migrated the reactive journal to per-stream buckets (ADR-0061 A2.10)",
		"moved", moved, "dropped_undecodable", dropped)
	return moved, dropped, nil
}
