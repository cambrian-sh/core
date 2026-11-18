package agentmgr

import (
	"context"
	"log/slog"
	"os/exec"

	"github.com/cambrian-sh/core/domain"
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
	// RestartPolicy drives auto-restart-with-backoff + flap quarantine on an unexpected
	// daemon exit (REACT-04 / ADR-0070). nil ⇒ no auto-restart (pre-REACT-04 behavior).
	RestartPolicy *DaemonRestartPolicy
	// restarts owns the pending auto-restart goroutines; StopAllDaemons cancels
	// and drains them (crash_detection.go, AGENTS.md §3.3).
	restarts restartLifecycle
	// DaemonBootHook, when non-nil, replaces the real bootDaemonAgent path in SpawnDaemon
	// (returning an instance id + the launched process). It exists so the chaos/kill test
	// can drive the real crash-watcher → restart loop with controllable OS processes.
	DaemonBootHook func(agentID, streamID string, params map[string]any) (instanceID string, cmd *exec.Cmd, err error)
}

// GetManifest delegates to the agent registry's ManifestReader.
func (am *AgentManager) GetManifest(ctx context.Context, agentID string) (*domain.AgentManifest, error) {
	return am.Registry.GetManifest(ctx, agentID)
}

// NewAgentManager creates a wired AgentManager.
func NewAgentManager(reg domain.AgentRegistry, substrateAddr string, memoryAgent domain.MemoryAgent) *AgentManager {
	im := NewInstanceManager(substrateAddr)
	// SEC-01: let the InstanceManager honor each agent's declared memory_limit_mb
	// over the global default at spawn time.
	// The boot path's ctx (the caller's, ENG-1467 item 7) reaches the registry read.
	im.SetManifestResolver(func(ctx context.Context, agentID string) *domain.AgentManifest {
		m, err := reg.GetManifest(ctx, agentID)
		if err != nil {
			// AGENTS.md §3.2: the fallback (the global memory cap) is the documented
			// default, but it must not be silent — a registry read error here means
			// the agent's own memory_limit_mb is not being honoured.
			slog.Warn("SEC-01: agent manifest unreadable; spawning with the global memory limit",
				"agent_id", agentID, "err", err)
			return nil
		}
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

// GetAgentByName looks up an agent definition by name via the embedded Registry.
func (m *AgentManager) GetAgentByName(ctx context.Context, name string) (*domain.AgentDefinition, error) {
	return m.Registry.GetAgentByName(ctx, name)
}
