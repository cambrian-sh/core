// Package operator implements the Operator Transport Plane — the sequenced gRPC
// surface for the Cambrian UI (ADR-0047). This file holds the Spool: the bounded,
// globally-sequenced in-memory buffer that decouples the synchronous EventBus
// from slow network clients (the OperatorFeed port).
package operator

import (
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Spool is a bounded, globally-sequenced in-memory event buffer implementing
// domain.OperatorFeed. Emit assigns a monotonic seq and retains the event in a
// ring bounded by age and count; ReadFrom serves a client cursor. ADR-0047 D2/D9.
type Spool struct {
	mu  sync.Mutex
	cfg SpoolConfig
	now func() time.Time

	seq    uint64                  // highest assigned sequence number
	events []domain.SequencedEvent // retained window, ascending Seq (tail at index 0)

	updateCh chan struct{} // closed and replaced on each Emit to broadcast "new event"

	// Ephemeral live-only fan-out for token chunks (ADR-0047 D12). These are
	// never retained in the ring and never consume a seq — a reconnecting client
	// resyncs accumulated text from the snapshot, not by replaying chunks.
	ephSubs map[int]chan domain.SequencedEvent
	ephNext int
}

// SpoolConfig bounds the retained window. Zero values fall back to defaults.
type SpoolConfig struct {
	// MaxAge is the retention window; events older than this are evicted.
	MaxAge time.Duration
	// MaxEvents is the hard ceiling on retained events (memory safety cap).
	MaxEvents int
}

const (
	defaultMaxAge    = 120 * time.Second
	defaultMaxEvents = 4096
)

// NewSpool constructs a Spool, applying defaults for any zero config field.
func NewSpool(cfg SpoolConfig) *Spool {
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = defaultMaxAge
	}
	if cfg.MaxEvents <= 0 {
		cfg.MaxEvents = defaultMaxEvents
	}
	return &Spool{cfg: cfg, now: time.Now, updateCh: make(chan struct{}), ephSubs: make(map[int]chan domain.SequencedEvent)}
}

// EmitEphemeral fans a live-only event (e.g. a token chunk) out to current
// streaming subscribers without retaining it or consuming a seq. Non-blocking:
// a slow subscriber drops the chunk rather than blocking the publisher.
// ADR-0047 D12.
func (s *Spool) EmitEphemeral(event domain.DomainEvent) {
	se := domain.SequencedEvent{Seq: 0, At: s.now(), Event: event}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.ephSubs {
		select {
		case ch <- se:
		default: // drop for a slow subscriber — best-effort, never blocks
		}
	}
}

// SubscribeEphemeral registers a live-only subscriber for ephemeral events. The
// returned cancel func must be called to release it.
func (s *Spool) SubscribeEphemeral() (<-chan domain.SequencedEvent, func()) {
	ch := make(chan domain.SequencedEvent, 64)
	s.mu.Lock()
	id := s.ephNext
	s.ephNext++
	s.ephSubs[id] = ch
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if c, ok := s.ephSubs[id]; ok {
			delete(s.ephSubs, id)
			close(c)
		}
	}
}

// Head returns the highest assigned sequence number (0 if nothing emitted yet).
func (s *Spool) Head() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// Updates returns a channel that is closed on the next Emit. A streaming reader
// captures it before calling ReadFrom, drains, then waits on it for the next
// event — so an Emit racing between ReadFrom and the wait is never missed.
func (s *Spool) Updates() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateCh
}

var _ domain.OperatorFeed = (*Spool)(nil)

// Emit implements domain.OperatorFeed. It assigns the next global monotonic
// sequence number under the spool lock (the single sequencer), retains the
// event, evicts past the window, and returns the assigned seq. It never blocks
// on a reader: the lock is held only for the bounded ring mutation, never while
// a network client is being served.
func (s *Spool) Emit(event domain.DomainEvent) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.seq++
	s.events = append(s.events, domain.SequencedEvent{
		Seq:   s.seq,
		At:    s.now(),
		Event: event,
	})
	s.evictLocked()

	// Broadcast to any streaming readers waiting on the previous channel.
	close(s.updateCh)
	s.updateCh = make(chan struct{})

	return s.seq
}

// ReadFrom implements domain.OperatorFeed.
func (s *Spool) ReadFrom(cursor uint64) ([]domain.SequencedEvent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Caller already has everything assigned so far — nothing new, no resync.
	if cursor >= s.seq {
		return nil, false
	}
	// The caller is missing events. If the spool is empty, or the next event it
	// needs (cursor+1) is older than the oldest retained event, those events
	// have been evicted — the caller must re-snapshot.
	if len(s.events) == 0 || cursor+1 < s.events[0].Seq {
		return nil, true
	}
	// The missing events are still retained — return the slice with Seq > cursor.
	var out []domain.SequencedEvent
	for _, e := range s.events {
		if e.Seq > cursor {
			out = append(out, e)
		}
	}
	return out, false
}

// evictLocked drops events that fall outside the count and age bounds. The
// caller must hold s.mu. Oldest-first eviction keeps s.events ascending by Seq.
func (s *Spool) evictLocked() {
	// Count cap: drop oldest until within MaxEvents.
	if over := len(s.events) - s.cfg.MaxEvents; over > 0 {
		s.events = s.events[over:]
	}
	// Age cap: drop oldest while older than the retention window.
	cutoff := s.now().Add(-s.cfg.MaxAge)
	drop := 0
	for drop < len(s.events) && s.events[drop].At.Before(cutoff) {
		drop++
	}
	if drop > 0 {
		s.events = s.events[drop:]
	}
}
