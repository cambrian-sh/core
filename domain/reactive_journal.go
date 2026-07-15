package domain

import "time"

// JournaledSignal is a durable-journal entry: a Signal paired with the monotonic
// sequence number assigned when it was appended. Returned by ReplayFrom so the
// reactive engine can resume evaluation after a crash. REACT-01 / ADR-0061.
type JournaledSignal struct {
	Seq    uint64
	Signal Signal
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
