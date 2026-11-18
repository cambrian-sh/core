package domain

import (
	"context"
	"time"
)

// JournaledSignal is a durable-journal entry: a Signal paired with the monotonic
// sequence number assigned when it was appended. Returned by ReplayFrom so the
// reactive engine can resume evaluation after a crash. REACT-01 / ADR-0061.
type JournaledSignal struct {
	Seq    uint64
	Signal Signal
	// ReceivedAt is when the journal accepted the signal (the record header's
	// received_at, ADR-0061 A2.3). A replaying engine stamps it — or the signal's
	// own Timestamp — as the ingest origin of the replayed work (ADR-0137 D5.2), so
	// a replay of a signal that predates an erase cannot resurrect what it erased.
	// Zero for records written before the header carried it.
	ReceivedAt time.Time
}

// WatchDeadLetterReader is the read port backing the OperatorConsole
// ListWatchDeadLetters RPC. Satisfied by the OSS bbolt journal decorator; nil in
// builds that never wire the reactive journal. REACT-01 / ADR-0061.
type WatchDeadLetterReader interface {
	ListDeadLetters(ctx context.Context, limit int) ([]ReactiveDeadLetter, error)
}

// WatchDeadLetterDeleter removes one dead letter by id: an operator resolving an
// entry they have dealt with (OperatorConsole DeleteWatchDeadLetter, ENG-472).
// found=false when no such entry exists. Satisfied by the OSS bbolt journal
// decorator beside the reader.
type WatchDeadLetterDeleter interface {
	DeleteDeadLetter(ctx context.Context, id string) (found bool, err error)
}

// ReactiveDeadLetter records a reactive action that could not be delivered — an
// action that failed, or a journal signal that expired past its TTL before it ran.
// Surfaced to operators via the OperatorConsole ListWatchDeadLetters read RPC so a
// watch's failures are visible rather than silently dropped. REACT-01 / ADR-0061.
type ReactiveDeadLetter struct {
	ID         string
	WatchID    string
	ActionType string
	Key        string
	Reason     string
	Signal     Signal
	FailedAt   time.Time
}

// ReferencesAny reports whether the dead letter's signal NAMES one of ids: a string
// value anywhere in its payload (nested maps and lists included) equal to an id. It is
// the match of the ADR-0137 D7 dead-letter erasure step (ENG-1966): a dead letter
// keeps the whole signal, so one that names an erased document, chunk, evidence row
// or content address (an ingress delivery's staged cid) is deleted with it. Exact
// equality only — never a substring or similarity match.
func (d ReactiveDeadLetter) ReferencesAny(ids map[string]bool) bool {
	if len(ids) == 0 {
		return false
	}
	return valueReferences(d.Signal.Payload, ids, 0)
}

// valueReferences walks a decoded payload value; depth bounds a pathological nesting.
func valueReferences(v any, ids map[string]bool, depth int) bool {
	if depth > 32 {
		return false
	}
	switch x := v.(type) {
	case string:
		return ids[x]
	case []string:
		for _, s := range x {
			if ids[s] {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if valueReferences(e, ids, depth+1) {
				return true
			}
		}
	case map[string]any:
		for _, e := range x {
			if valueReferences(e, ids, depth+1) {
				return true
			}
		}
	}
	return false
}
