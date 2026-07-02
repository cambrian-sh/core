package agentmgr

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// DaemonInstance is a snapshot of a running daemon for ListRunningDaemons. ADR-0033.
type DaemonInstance struct {
	AgentID    string
	InstanceID string
	StreamID   string
	Status     string // "running" | "unavailable"
}

// DaemonSpawner is the consumer-side interface used by the premium ReactiveEngine
// to drive daemon lifecycle via WatchConfig CRUD. ADR-0033.
type DaemonSpawner interface {
	SpawnDaemon(agentID, streamID string, params map[string]any) (instanceID string, err error)
	StopDaemon(streamID string) error
	ListRunningDaemons() []DaemonInstance
}

// daemonState holds the ref-count and instance mapping for a single daemon stream.
type daemonState struct {
	refCount   int
	agentID    string // for EvictAgent lookup
	instanceID string // for markExpectedStop and telemetry
	status     string // "running" | "unavailable"
}

// daemonRegistry is the ref-count tracker embedded in AgentManager. ADR-0033.
type daemonRegistry struct {
	mu       sync.Mutex
	byStream map[string]*daemonState // key: streamID
}

func newDaemonRegistry() *daemonRegistry {
	return &daemonRegistry{byStream: make(map[string]*daemonState)}
}

// SpawnDaemon boots a TraitDaemon agent and starts the crash-watcher goroutine.
// Subsequent calls for the same streamID increment the ref-count without spawning
// a second process. ADR-0033.
func (m *AgentManager) SpawnDaemon(agentID, streamID string, params map[string]any) (string, error) {
	m.daemons.mu.Lock()
	state, exists := m.daemons.byStream[streamID]
	if exists && state.refCount > 0 {
		// Already running — increment ref and return existing instance ID.
		state.refCount++
		instanceID := state.instanceID
		m.daemons.mu.Unlock()
		return instanceID, nil
	}
	m.daemons.mu.Unlock()

	// Look up the agent definition.
	def, err := m.Registry.GetAgentByName(context.Background(), agentID)
	if err != nil || def == nil {
		return "", fmt.Errorf("SpawnDaemon: agent %q not found: %w", agentID, err)
	}

	// Boot the daemon process.
	inst, bootErr := m.InstanceManager.bootDaemonAgent(context.Background(), def, streamID, params)
	if bootErr != nil {
		return "", fmt.Errorf("SpawnDaemon: boot failed for %q: %w", agentID, bootErr)
	}

	// Register in daemon registry.
	m.daemons.mu.Lock()
	m.daemons.byStream[streamID] = &daemonState{
		refCount:   1,
		agentID:    agentID,
		instanceID: inst.ID,
		status:     "running",
	}
	m.daemons.mu.Unlock()

	// Launch crash-watcher goroutine. ADR-0033.
	cmd := m.InstanceManager.GetCmd(inst.ID)
	if cmd != nil {
		go func() {
			_ = cmd.Wait()
			// Check whether this exit was expected (StopDaemon called).
			unexpected := !m.isExpectedStop(inst.ID)
			if unexpected {
				slog.Warn("daemon crashed unexpectedly",
					"agent_id", agentID, "stream_id", streamID, "instance_id", inst.ID)
			}
			m.handleDaemonExit(inst, streamID, unexpected)
		}()
	}

	return inst.ID, nil
}

// StopDaemon marks the exit as expected and evicts the daemon instance. ADR-0033.
func (m *AgentManager) StopDaemon(streamID string) error {
	m.daemons.mu.Lock()
	state, ok := m.daemons.byStream[streamID]
	if !ok {
		m.daemons.mu.Unlock()
		return fmt.Errorf("StopDaemon: stream %q not registered", streamID)
	}
	instanceID := state.instanceID
	agentID := state.agentID
	delete(m.daemons.byStream, streamID)
	m.daemons.mu.Unlock()

	// Mark the exit as expected before killing so the crash watcher ignores it.
	if instanceID != "" {
		m.markExpectedStop(instanceID)
	}
	if agentID != "" {
		m.EvictAgent(agentID)
	}
	return nil
}

// IncrementDaemonRef records that a new WatchConfig references streamID.
// instanceID is stored so StopDaemon knows which instance to mark before eviction.
func (m *AgentManager) IncrementDaemonRef(streamID, instanceID string) {
	m.daemons.mu.Lock()
	defer m.daemons.mu.Unlock()
	s, ok := m.daemons.byStream[streamID]
	if !ok {
		s = &daemonState{status: "running"}
		m.daemons.byStream[streamID] = s
	}
	s.refCount++
	if instanceID != "" {
		s.instanceID = instanceID
	}
}

// DecrementDaemonRef decrements the ref-count. Returns (true, nil) when the
// count reaches zero and StopDaemon was called. ADR-0033.
func (m *AgentManager) DecrementDaemonRef(streamID string) (stopped bool, err error) {
	m.daemons.mu.Lock()
	s, ok := m.daemons.byStream[streamID]
	if !ok {
		m.daemons.mu.Unlock()
		return false, fmt.Errorf("daemon stream %q not registered", streamID)
	}
	s.refCount--
	if s.refCount > 0 {
		m.daemons.mu.Unlock()
		return false, nil
	}
	instanceID := s.instanceID
	agentID := s.agentID
	delete(m.daemons.byStream, streamID)
	m.daemons.mu.Unlock()

	// Mark expected before killing so crash watcher doesn't fire. ADR-0033.
	if instanceID != "" {
		m.markExpectedStop(instanceID)
	}
	if agentID != "" {
		m.EvictAgent(agentID)
	} else if instanceID != "" {
		// Fallback when agentID not stored (e.g. via IncrementDaemonRef).
		m.EvictInstance(instanceID)
	}
	return true, nil
}

// DaemonRefCount returns the current reference count for streamID. Zero means no daemon.
func (m *AgentManager) DaemonRefCount(streamID string) int {
	m.daemons.mu.Lock()
	defer m.daemons.mu.Unlock()
	if s, ok := m.daemons.byStream[streamID]; ok {
		return s.refCount
	}
	return 0
}

// SetDaemonStatus updates the operational status of a daemon stream. ADR-0033.
func (m *AgentManager) SetDaemonStatus(streamID, status string) {
	m.daemons.mu.Lock()
	defer m.daemons.mu.Unlock()
	s, ok := m.daemons.byStream[streamID]
	if !ok {
		s = &daemonState{}
		m.daemons.byStream[streamID] = s
	}
	s.status = status
}

// GetDaemonStatus returns the current status of a daemon stream. ADR-0033.
func (m *AgentManager) GetDaemonStatus(streamID string) string {
	m.daemons.mu.Lock()
	defer m.daemons.mu.Unlock()
	if s, ok := m.daemons.byStream[streamID]; ok {
		return s.status
	}
	return ""
}

// ListRunningDaemons returns all instances with mode == Daemon. ADR-0033.
func (m *AgentManager) ListRunningDaemons() []DaemonInstance {
	m.InstanceManager.mu.Lock()
	defer m.InstanceManager.mu.Unlock()

	var out []DaemonInstance
	for _, inst := range m.InstanceManager.instances {
		if inst.Mode == domain.ModeDaemon {
			out = append(out, DaemonInstance{
				AgentID:    inst.AgentID,
				InstanceID: inst.ID,
			})
		}
	}
	return out
}
