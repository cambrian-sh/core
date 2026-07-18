package operator

import (
	"context"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// defaultRosterHeartbeat is how often the latched roster is re-broadcast.
const defaultRosterHeartbeat = 10 * time.Second

// RosterLatch fixes the empty-capabilities routing-measurement bug. The agent roster
// (agent_id → capabilities) reaches the operator feed only via one-time boot
// AgentReadyEvents, which live in the age/count-bounded spool. A consumer that connects
// AFTER boot — notably the benchmark harness in --no-supervise mode — races the spool's
// eviction: once the low-seq boot events are gone, a resync jumps the cursor past them and
// the roster is never delivered, so every auction winner scores "declares no capabilities".
//
// RosterLatch caches the latest AgentReadyEvent per agent and periodically RE-EMITS the
// full roster to the feed with fresh sequence numbers. Those land after any subscriber's
// resync point, so a late/reconnecting consumer reliably receives the current roster. The
// harness treats AgentReadyOp idempotently (last write wins) and filters it out of task
// data, so the re-emissions are harmless duplicates, not noise.
type RosterLatch struct {
	feed *Spool

	mu     sync.Mutex
	roster map[string]domain.AgentReadyEvent
}

// NewRosterLatch builds a latch that re-broadcasts through feed.
func NewRosterLatch(feed *Spool) *RosterLatch {
	return &RosterLatch{feed: feed, roster: make(map[string]domain.AgentReadyEvent)}
}

// AgentCapabilitySource enumerates registered agents and their declared capabilities from
// the authoritative manifests. The roster is seeded from it at boot so the feed carries the
// agent→capabilities roster EVEN WHEN no interview runs — with disable_interviews the
// interview worker never emits AgentReadyEvent, so the manifest is the only capability source.
type AgentCapabilitySource interface {
	GetAllAgents(ctx context.Context) ([]domain.AgentDefinition, error)
	GetManifest(ctx context.Context, agentID string) (*domain.AgentManifest, error)
}

// Seed populates the roster from the manifest source (call once at boot, before Start).
// Mirrors the interview worker's preferCaps: use declared capabilities, else fall back to
// tools. A later AgentReadyEvent from the interview path (fresher: trust score, updated
// caps) overwrites the seed via Observe.
func (r *RosterLatch) Seed(ctx context.Context, src AgentCapabilitySource) {
	if src == nil {
		return
	}
	agents, err := src.GetAllAgents(ctx)
	if err != nil {
		return
	}
	for _, a := range agents {
		m, err := src.GetManifest(ctx, a.ID)
		if err != nil || m == nil {
			continue
		}
		caps := m.Capabilities
		if len(caps) == 0 {
			caps = m.Tools
		}
		if len(caps) == 0 {
			continue // nothing to route on
		}
		r.mu.Lock()
		if _, exists := r.roster[a.ID]; !exists {
			r.roster[a.ID] = domain.AgentReadyEvent{AgentID: a.ID, Capabilities: caps}
		}
		r.mu.Unlock()
	}
}

// Observe caches an AgentReadyEvent. Wire it to the EventBus alongside SubscribeBridge so
// the latch sees the same ready events the bridge forwards. Non-AgentReady events are ignored.
func (r *RosterLatch) Observe(e domain.DomainEvent) {
	ev, ok := e.(domain.AgentReadyEvent)
	if !ok || ev.AgentID == "" {
		return
	}
	r.mu.Lock()
	r.roster[ev.AgentID] = ev
	r.mu.Unlock()
}

// Start launches the heartbeat: every interval (<=0 ⇒ default) it re-emits the cached
// roster to the feed until ctx is cancelled. Re-emits go straight to the spool (not the
// bus), so they never re-trigger Observe.
func (r *RosterLatch) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultRosterHeartbeat
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.reemit()
			}
		}
	}()
}

func (r *RosterLatch) reemit() {
	r.mu.Lock()
	evs := make([]domain.AgentReadyEvent, 0, len(r.roster))
	for _, e := range r.roster {
		evs = append(evs, e)
	}
	r.mu.Unlock()
	for _, e := range evs {
		r.feed.Emit(e)
	}
}
