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
	m.scheduleRestart(inst.AgentID, streamID, params, "crash-loop")
}

// restartLifecycle owns the pending auto-restart goroutines (AGENTS.md §3.3: one
// owner per background goroutine). StopAllDaemons closes it at kernel shutdown:
// a pending restart is cancelled instead of respawning a daemon after shutdown
// began (a stranded daemon holding e.g. a bot token is the failure class), and a
// restart already mid-boot is waited for so the daemon it starts is stopped with
// the rest. The zero value is ready to use.
type restartLifecycle struct {
	mu      sync.Mutex
	stopped bool
	stop    chan struct{}
	wg      sync.WaitGroup
}

// begin registers one restart goroutine and returns the channel that cancels
// it, or ok=false once shutdown has begun. Add happens under mu so it can never
// race the Wait in shutdown.
func (r *restartLifecycle) begin() (stop <-chan struct{}, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil, false
	}
	if r.stop == nil {
		r.stop = make(chan struct{})
	}
	r.wg.Add(1)
	return r.stop, true
}

// shutdown cancels every pending restart, refuses new ones, and waits for the
// ones already booting to return. Idempotent.
func (r *restartLifecycle) shutdown() {
	r.mu.Lock()
	if !r.stopped {
		r.stopped = true
		if r.stop != nil {
			close(r.stop)
		}
	}
	r.mu.Unlock()
	r.wg.Wait()
}

// scheduleRestart registers one restart attempt with the RestartPolicy and, unless
// the flap limit is reached, respawns the daemon after the backoff delay.
//
// A FAILED respawn (boot timeout, missing dependency, agent unregistered) is just
// another attempt: it re-registers, so it backs off and, past the limit, lands in
// the operator-visible quarantined state with a DaemonQuarantinedEvent (ADR-0070
// D2). It used to log a WARN and return, leaving the stream "unavailable" forever
// with no retry and nothing on the operator feed
// (T-20261113-daemon-auto-restart-a-failed-respawn-leaves-the).
func (m *AgentManager) scheduleRestart(agentID, streamID string, params map[string]any, reason string) {
	delay, quarantine := m.RestartPolicy.Register(streamID)
	if quarantine {
		m.SetDaemonStatus(streamID, "quarantined")
		slog.Warn("daemon quarantined — auto-restart withdrawn",
			"agent_id", agentID, "stream_id", streamID, "reason", reason, "attempts", m.RestartPolicy.MaxAttempts)
		m.publishEvent(domain.DaemonQuarantinedEvent{
			AgentID: agentID, StreamID: streamID,
			Reason: reason, Attempts: m.RestartPolicy.MaxAttempts,
		})
		return
	}
	stop, ok := m.restarts.begin()
	if !ok {
		slog.Info("daemon restart skipped: kernel is shutting down", "agent_id", agentID, "stream_id", streamID)
		return
	}

	slog.Info("daemon scheduled for restart", "agent_id", agentID, "stream_id", streamID, "delay", delay)
	go func() {
		defer m.restarts.wg.Done()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-stop:
			return
		case <-timer.C:
		}
		if _, err := m.SpawnDaemon(agentID, streamID, params); err != nil {
			slog.Warn("daemon auto-restart failed; retrying with backoff", "agent_id", agentID, "stream_id", streamID, "err", err)
			m.scheduleRestart(agentID, streamID, params, "restart-failed")
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
