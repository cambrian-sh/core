package domain

import "time"

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
	ListDeadLetters(limit int) ([]ReactiveDeadLetter, error)
}

// WatchDeadLetterDeleter removes one dead letter by id: an operator resolving an
// entry they have dealt with (OperatorConsole DeleteWatchDeadLetter, ENG-472).
// found=false when no such entry exists. Satisfied by the OSS bbolt journal
// decorator beside the reader.
type WatchDeadLetterDeleter interface {
	DeleteDeadLetter(id string) (found bool, err error)
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
