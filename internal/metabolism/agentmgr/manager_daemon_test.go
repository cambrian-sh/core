package agentmgr

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func TestAgentManager_ReleaseInstance_JIT_Evicted(t *testing.T) {
	m := newTestManager()

	inst := domain.NewInstance("agent-jit")
	inst.Mode = domain.ModeJIT

	m.InstanceManager.mu.Lock()
	m.InstanceManager.instances[inst.ID] = inst
	m.InstanceManager.agentIndex["agent-jit"] = []string{inst.ID}
	m.InstanceManager.mu.Unlock()

	m.ReleaseInstance(inst.ID)

	if _, ok := m.InstanceManager.instances[inst.ID]; ok {
		t.Error("JIT instance should be evicted after task completion")
	}
	if ids := m.GetInstanceIDs("agent-jit"); len(ids) != 0 {
		t.Errorf("agentIndex should be empty for agent-jit, got %d IDs", len(ids))
	}
}

func TestAgentManager_ReleaseInstance_Daemon_Survives(t *testing.T) {
	m := newTestManager()

	inst := domain.NewInstance("agent-daemon")
	inst.Mode = domain.ModeDaemon

	m.InstanceManager.mu.Lock()
	m.InstanceManager.instances[inst.ID] = inst
	m.InstanceManager.agentIndex["agent-daemon"] = []string{inst.ID}
	m.InstanceManager.mu.Unlock()

	m.ReleaseInstance(inst.ID)

	if _, ok := m.InstanceManager.instances[inst.ID]; !ok {
		t.Error("Daemon instance should survive after task completion")
	}
	if ids := m.GetInstanceIDs("agent-daemon"); len(ids) != 1 {
		t.Errorf("expected 1 instance for agent-daemon, got %d", len(ids))
	}
}

func TestAgentManager_ReleaseInstance_Unknown_NoOp(t *testing.T) {
	m := newTestManager()
	m.ReleaseInstance("nonexistent-id")
}

func TestAgentManager_CallAgent_ReleasesJIT(t *testing.T) {
	m := newTestManager()
	m.InstanceManager.substrateAddr = "/tmp/substrate.sock"

	def := &domain.AgentDefinition{
		ID:       "test-jit-agent",
		Runtime:  domain.RuntimePython,
		ExecPath: "agent.py",
		Dir:      t.TempDir(),
	}

	inst := domain.NewInstance(def.ID)
	inst.Mode = domain.ModeJIT

	m.InstanceManager.mu.Lock()
	delete(m.agentIndex, def.ID)
	m.InstanceManager.mu.Unlock()

	if inst.Mode != domain.ModeJIT {
		t.Errorf("expected default mode JIT, got %s", inst.Mode)
	}
}
