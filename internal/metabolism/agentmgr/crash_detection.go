package agentmgr

import (
	"log/slog"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
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

	// Mark stream unavailable and capture the original spawn params (needed to restart)
	// before clearing the registry entry so SpawnDaemon can boot a fresh process.
	m.daemons.mu.Lock()
	var params map[string]any
	if s, ok := m.daemons.byStream[streamID]; ok {
		params = s.params
	}
	delete(m.daemons.byStream, streamID)
	m.daemons.mu.Unlock()
	m.SetDaemonStatus(streamID, "unavailable")

	m.publishEvent(domain.DaemonCrashedEvent{AgentID: inst.AgentID, StreamID: streamID})

	// REACT-04 / ADR-0070: auto-restart with backoff, or quarantine a crash-loop.
	if m.RestartPolicy == nil {
		return // no restart policy → pre-REACT-04 behavior (stays down)
	}
	delay, quarantine := m.RestartPolicy.Register(streamID)
	if quarantine {
		m.SetDaemonStatus(streamID, "quarantined")
		slog.Warn("daemon quarantined (crash-loop) — auto-restart withdrawn",
			"agent_id", inst.AgentID, "stream_id", streamID, "attempts", m.RestartPolicy.MaxAttempts)
		m.publishEvent(domain.DaemonQuarantinedEvent{
			AgentID: inst.AgentID, StreamID: streamID,
			Reason: "crash-loop", Attempts: m.RestartPolicy.MaxAttempts,
		})
		return
	}

	agentID := inst.AgentID
	slog.Info("daemon scheduled for restart", "agent_id", agentID, "stream_id", streamID, "delay", delay)
	go func() {
		time.Sleep(delay)
		if _, err := m.SpawnDaemon(agentID, streamID, params); err != nil {
			slog.Warn("daemon auto-restart failed", "agent_id", agentID, "stream_id", streamID, "err", err)
			return
		}
		m.SetDaemonStatus(streamID, "running")
		slog.Info("daemon recovered via auto-restart", "agent_id", agentID, "stream_id", streamID)
		m.publishEvent(domain.DaemonRecoveredEvent{AgentID: agentID, StreamID: streamID})
	}()
}

// publishEvent publishes a domain event on the EventBus (nil-safe), logging on failure.
func (m *AgentManager) publishEvent(evt domain.DomainEvent) {
	if m.EventBus == nil {
		return
	}
	if err := m.EventBus.Publish(evt); err != nil {
		slog.Error("crash_detection: publish event failed", "type", evt.EventType(), "err", err)
	}
}
