package app

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// progressHolder is the late-binding indirection between a plugin and the chat lane.
//
// The ordering problem it solves: plugins are Built before the chat lane is constructed,
// so a plugin cannot be handed the TurnService directly — it does not exist yet. Reordering
// construction to suit one capability would be the wrong trade; a holder that is installed
// early and filled later is smaller and cannot break the rest of the wiring.
//
// It is itself a domain.ProgressSink, so the chat lane can be given the holder unconditionally
// at construction and never needs to know whether anyone eventually turned up.
type progressHolder struct {
	sink atomic.Pointer[domain.ProgressSink]

	// lastActivity is when each conversation last reported doing something. It is
	// recorded whether or not a plugin is listening, because it is not telemetry: it
	// is the only signal the kernel has that a turn is still alive.
	//
	// A turn that stops emitting has stopped working — the emission points sit on
	// memory retrieval and tool execution, which is where a turn spends its time. So
	// silence here is the difference between "thinking hard" and "wedged", which
	// nothing else in the system can currently tell apart.
	mu           sync.Mutex
	lastActivity map[string]time.Time
}

// set installs (or clears) the delegate. Safe to call at any point during boot.
func (h *progressHolder) set(s domain.ProgressSink) {
	if s == nil {
		h.sink.Store(nil)
		return
	}
	h.sink.Store(&s)
}

// Progress forwards to the installed sink, or discards.
//
// Deliberately swallows a nil delegate rather than guarding at each call site: the emission
// points are on the turn path, and progress must never be able to fail the work it
// describes (ADR-0098 D5).
func (h *progressHolder) Progress(ctx context.Context, u domain.ProgressUpdate) {
	h.touch(u.ConversationID, u.Final)
	if p := h.sink.Load(); p != nil {
		(*p).Progress(ctx, u)
	}
}

// touch records activity, or forgets the conversation when its turn ends.
func (h *progressHolder) touch(conversationID string, final bool) {
	if conversationID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastActivity == nil {
		h.lastActivity = map[string]time.Time{}
	}
	if final {
		delete(h.lastActivity, conversationID)
		return
	}
	h.lastActivity[conversationID] = time.Now()
}

// LastActivity returns when a conversation last reported progress, or the zero time if it
// has none in flight.
func (h *progressHolder) LastActivity(conversationID string) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastActivity[conversationID]
}

// progressDelivererHolder is the mirror image: a plugin needs to SEND progress, but the
// delivery service is also built after plugins. Same reasoning, opposite direction.
type progressDelivererHolder struct {
	fn atomic.Pointer[func(ctx context.Context, conversationID, text string, final bool) error]
}

func (h *progressDelivererHolder) set(f func(ctx context.Context, conversationID, text string, final bool) error) {
	if f == nil {
		h.fn.Store(nil)
		return
	}
	h.fn.Store(&f)
}

// deliver forwards to the installed delivery function. A nil delegate is not an error:
// before the delivery path exists — or in a deployment with no ingress at all — there is
// simply nobody to tell.
func (h *progressDelivererHolder) deliver(ctx context.Context, conversationID, text string, final bool) error {
	if p := h.fn.Load(); p != nil {
		return (*p)(ctx, conversationID, text, final)
	}
	return nil
}
