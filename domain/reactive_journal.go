package domain

import "time"

// JournaledSignal is a durable-journal entry: a Signal paired with the monotonic
// sequence number assigned when it was appended. Returned by ReplayFrom so the
// reactive engine can resume evaluation after a crash. REACT-01 / ADR-0061.
type JournaledSignal struct {
	Seq    uint64
	Signal Signal
}

// WatchDeadLetterReader is the read port backing the OperatorConsole
// ListWatchDeadLetters RPC. Satisfied by the OSS bbolt journal decorator; nil in
// builds that never wire the reactive journal. REACT-01 / ADR-0061.
type WatchDeadLetterReader interface {
	ListDeadLetters(limit int) ([]ReactiveDeadLetter, error)
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
