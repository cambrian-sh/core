package agentmgr

import (
	"log/slog"
	"sync"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// EventPublisher is the subset of domain.EventBus needed by crash detection. ADR-0033.
type EventPublisher interface {
	Publish(event domain.DomainEvent) error
}

// stoppedSet is a consume-once set of expected-stop instance IDs.
type stoppedSet struct {
	mu  sync.Mutex
	ids map[string]struct{}
}

func newStoppedSet() *stoppedSet {
	return &stoppedSet{ids: make(map[string]struct{})}
}

func (s *stoppedSet) mark(instanceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids[instanceID] = struct{}{}
}

// consume returns true and removes the entry if instanceID was expected.
func (s *stoppedSet) consume(instanceID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ids[instanceID]; ok {
		delete(s.ids, instanceID)
		return true
	}
	return false
}

// markExpectedStop records that the daemon exit for instanceID is intentional
// (triggered by StopDaemon / EvictAgent). ADR-0033.
func (m *AgentManager) markExpectedStop(instanceID string) {
	m.stopped.mark(instanceID)
}

// isExpectedStop checks (and consumes) whether the exit is expected. ADR-0033.
func (m *AgentManager) isExpectedStop(instanceID string) bool {
	return m.stopped.consume(instanceID)
}

// handleDaemonExit is called by the crash-watcher goroutine after cmd.Wait()
// returns. unexpected=true means the process died without StopDaemon. ADR-0033.
func (m *AgentManager) handleDaemonExit(inst *domain.Instance, streamID string, unexpected bool) {
	if !unexpected {
		return
	}

	// Mark stream unavailable in the daemon registry.
	m.SetDaemonStatus(streamID, "unavailable")
	// Reset ref count to 0 so SpawnDaemon can restart.
	m.daemons.mu.Lock()
	delete(m.daemons.byStream, streamID)
	m.daemons.mu.Unlock()

	// Publish DaemonCrashedEvent.
	if m.EventBus != nil {
		evt := domain.DaemonCrashedEvent{
			AgentID:  inst.AgentID,
			StreamID: streamID,
		}
		if err := m.EventBus.Publish(evt); err != nil {
			slog.Error("crash_detection: publish DaemonCrashedEvent failed",
				"agent_id", inst.AgentID, "stream_id", streamID, "err", err)
		}
	}
}
