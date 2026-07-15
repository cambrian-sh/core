package agentmgr

import (
	"github.com/cambrian-sh/core/domain"
	"context"
)

// AgentManager is the thin facade over InstanceManager (process lifecycle)
// and AgentConnector (gRPC/A2A connections). It retains the Registry and
// MemoryAgent fields that span both concerns, while delegating all
// operational methods to the embedded managers.
type AgentManager struct {
	Registry domain.AgentRegistry

	*InstanceManager
	*AgentConnector

	MemoryAgent domain.MemoryAgent

	DefaultInputCostPer1M  float64
	DefaultOutputCostPer1M float64

	// daemons tracks ref-counts and status for running daemon agents. ADR-0033.
	daemons *daemonRegistry
	// stopped records instance IDs whose exit is expected (from StopDaemon). ADR-0033.
	stopped *stoppedSet
	// EventBus receives DaemonCrashedEvent on unexpected daemon exits. ADR-0033. nil-safe.
	EventBus EventPublisher
}

// GetManifest delegates to the agent registry's ManifestReader.
func (am *AgentManager) GetManifest(ctx context.Context, agentID string) (*domain.AgentManifest, error) {
	return am.Registry.GetManifest(ctx, agentID)
}

// NewAgentManager creates a wired AgentManager.
func NewAgentManager(reg domain.AgentRegistry, pyPath string, substrateAddr string, memoryAgent domain.MemoryAgent) *AgentManager {
	im := NewInstanceManager(pyPath, substrateAddr)
	// SEC-01: let the InstanceManager honor each agent's declared memory_limit_mb
	// over the global default at spawn time.
	im.SetManifestResolver(func(agentID string) *domain.AgentManifest {
		m, _ := reg.GetManifest(context.Background(), agentID)
		return m
	})
	return &AgentManager{
		Registry:        reg,
		InstanceManager: im,
		AgentConnector:  NewAgentConnector(),
		MemoryAgent:     memoryAgent,
		daemons:         newDaemonRegistry(),
		stopped:         newStoppedSet(),
	}
}

// Restore delegates to AgentConnector with the embedded Registry.
func (m *AgentManager) Restore(agentID, taskID string) error {
	return m.AgentConnector.Restore(m.Registry, agentID, taskID)
}

// GetAgentByName looks up an agent definition by name via the embedded Registry.
func (m *AgentManager) GetAgentByName(ctx context.Context, name string) (*domain.AgentDefinition, error) {
	return m.Registry.GetAgentByName(ctx, name)
}
