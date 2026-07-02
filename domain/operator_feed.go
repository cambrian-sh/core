package domain

import "time"

// SequencedEvent wraps a DomainEvent with the global monotonic sequence number
// the OperatorFeed assigns at spool-entry, plus the time it was sequenced. It is
// the proto-free unit the operator plane's spool retains; the network layer maps
// it to a pb.OperatorEvent at the boundary, keeping domain free of any
// proto import. ADR-0047 D2/D4.
type SequencedEvent struct {
	// Seq is the global monotonic sequence number; it is the client's resume
	// cursor. Strictly increasing and unique across all events on the feed.
	Seq uint64
	// At is the time the event entered the spool (sequencing time).
	At time.Time
	// Event is the underlying domain event.
	Event DomainEvent
}

// OperatorFeed is the outbound port that decouples the synchronous EventBus from
// slow network clients (ADR-0047 D2). The EventBus→feed bridge Emits domain
// events; the StreamEvents RPC drains them via ReadFrom against a client cursor.
//
// Implementations must be safe for concurrent use from multiple goroutines and
// must never block a caller of Emit on a slow or absent reader — a lagging
// client is forced to resync, it never back-pressures the publisher.
type OperatorFeed interface {
	// Emit assigns the next global monotonic sequence number to event, retains
	// it in the bounded spool (evicting as needed), and returns the assigned
	// sequence number. Emit never blocks on a reader.
	Emit(event DomainEvent) uint64

	// ReadFrom returns the retained events with Seq > cursor, in ascending Seq
	// order. resync is true when the event the caller needs next (cursor+1) has
	// already been evicted from the spool window — the caller must re-snapshot;
	// in that case events is nil. When the caller is already current
	// (cursor >= the latest assigned seq), events is nil and resync is false.
	ReadFrom(cursor uint64) (events []SequencedEvent, resync bool)
}
