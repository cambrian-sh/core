package domain

import (
	"context"
	"errors"
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

// WatchDeadLetterRetrier replays one dead letter (OperatorConsole
// RetryWatchDeadLetter, ENG-1654, owner decision 2026-11-20 option C): only a
// failed-delivery entry (Retryable), re-submitted to ITS watch under the
// idempotency key recorded with it (REACT-01), so a replay of something that in
// fact already ran is absorbed rather than fired twice; the entry is removed once
// the replay is handed over. Satisfied by the premium reactive engine and
// registered through KernelServices.RegisterDeadLetterRetrier; nil in OSS.
type WatchDeadLetterRetrier interface {
	RetryDeadLetter(ctx context.Context, deadLetterID string) error
}

// Errors a WatchDeadLetterRetrier answers with; the operator plane maps them.
var (
	// ErrDeadLetterNotFound: no dead letter with that id (never recorded, deleted,
	// or already retried).
	ErrDeadLetterNotFound = errors.New("dead letter not found")
	// ErrDeadLetterNotRetryable: the entry is an expired signal (Retryable false):
	// delete-only.
	ErrDeadLetterNotRetryable = errors.New("dead letter is not retryable")
	// ErrDeadLetterWatchGone: the entry's watch no longer exists or is paused, so
	// there is nothing to replay it into.
	ErrDeadLetterWatchGone = errors.New("dead letter's watch is gone or inactive")
	// ErrDeadLetterRetryUnavailable: this kernel has no retrier registered.
	ErrDeadLetterRetryUnavailable = errors.New("replaying a dead letter is not available on this kernel")
)

// DeadLetterReasonExpired is the reason of a journal signal that outlived its TTL
// before it could run (premium ReactiveEngine.replay). It is the one reason a
// dead letter is NOT retryable: what it would do was decided against a world that
// has moved on (owner decision 2026-11-20, ENG-1654 option C).
const DeadLetterReasonExpired = "expired_before_replay"

// Retryable reports whether the dead letter is a FAILED DELIVERY — a signal shed
// at a full queue or an exhausted budget, or a firing the runtime refused — that
// an operator may replay. An expired signal is delete-only. The one definition:
// the operator plane's list and the premium retrier both read it.
func (d ReactiveDeadLetter) Retryable() bool {
	return d.Reason != DeadLetterReasonExpired
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
