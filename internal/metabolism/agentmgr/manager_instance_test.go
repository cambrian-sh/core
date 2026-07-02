package agentmgr

import (
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

func TestAgentManager_Instances(t *testing.T) {
	m := newTestManager()

	inst := domain.NewInstance("agent-1")

	m.InstanceManager.mu.Lock()
	m.InstanceManager.instances[inst.ID] = inst
	m.InstanceManager.agentIndex["agent-1"] = append(m.InstanceManager.agentIndex["agent-1"], inst.ID)
	m.InstanceManager.mu.Unlock()

	t.Run("get by instance ID", func(t *testing.T) {
		retrieved, ok := m.InstanceManager.instances[inst.ID]
		if !ok {
			t.Fatal("Instance not found in Instances map")
		}
		if retrieved.AgentID != "agent-1" {
			t.Errorf("expected agent-1, got %s", retrieved.AgentID)
		}
	})

	t.Run("get by agent ID", func(t *testing.T) {
		ids := m.GetInstanceIDs("agent-1")
		if len(ids) != 1 {
			t.Fatalf("expected 1 instance for agent-1, got %d", len(ids))
		}
		if ids[0] != inst.ID {
			t.Errorf("expected instance ID %s, got %s", inst.ID, ids[0])
		}
	})

	t.Run("unknown agent returns empty", func(t *testing.T) {
		ids := m.GetInstanceIDs("nonexistent")
		if len(ids) != 0 {
			t.Errorf("expected empty for unknown agent, got %d IDs", len(ids))
		}
	})
}

func TestAgentManager_MultipleInstancesPerAgent(t *testing.T) {
	m := newTestManager()

	i1 := domain.NewInstance("agent-X")
	i2 := domain.NewInstance("agent-X")
	i3 := domain.NewInstance("agent-Y")

	m.InstanceManager.mu.Lock()
	m.InstanceManager.instances[i1.ID] = i1
	m.InstanceManager.instances[i2.ID] = i2
	m.InstanceManager.instances[i3.ID] = i3
	m.InstanceManager.agentIndex["agent-X"] = []string{i1.ID, i2.ID}
	m.InstanceManager.agentIndex["agent-Y"] = []string{i3.ID}
	m.InstanceManager.mu.Unlock()

	idsX := m.GetInstanceIDs("agent-X")
	if len(idsX) != 2 {
		t.Fatalf("expected 2 instances for agent-X, got %d", len(idsX))
	}

	idsY := m.GetInstanceIDs("agent-Y")
	if len(idsY) != 1 {
		t.Fatalf("expected 1 instance for agent-Y, got %d", len(idsY))
	}
}

func TestAgentManager_EvictRemovesFromBothMaps(t *testing.T) {
	m := newTestManager()

	i1 := domain.NewInstance("agent-A")
	i2 := domain.NewInstance("agent-A")

	m.InstanceManager.mu.Lock()
	m.InstanceManager.instances[i1.ID] = i1
	m.InstanceManager.instances[i2.ID] = i2
	m.InstanceManager.agentIndex["agent-A"] = []string{i1.ID, i2.ID}
	m.InstanceManager.mu.Unlock()

	m.EvictInstance(i1.ID)

	t.Run("evicted instance removed from Instances", func(t *testing.T) {
		if _, ok := m.InstanceManager.instances[i1.ID]; ok {
			t.Error("evicted instance still in Instances map")
		}
	})

	t.Run("evicted instance removed from agentIndex", func(t *testing.T) {
		ids := m.GetInstanceIDs("agent-A")
		if len(ids) != 1 {
			t.Fatalf("expected 1 remaining instance, got %d", len(ids))
		}
		if ids[0] != i2.ID {
			t.Errorf("expected remaining instance %s, got %s", i2.ID, ids[0])
		}
	})

	t.Run("unevicted instance still accessible", func(t *testing.T) {
		if _, ok := m.InstanceManager.instances[i2.ID]; !ok {
			t.Error("surviving instance missing from Instances map")
		}
	})
}

func TestAgentManager_KillAllAgents(t *testing.T) {
	m := newTestManager()

	i1 := domain.NewInstance("agent-B")
	i2 := domain.NewInstance("agent-C")

	m.InstanceManager.mu.Lock()
	m.InstanceManager.instances[i1.ID] = i1
	m.InstanceManager.instances[i2.ID] = i2
	m.InstanceManager.agentIndex["agent-B"] = []string{i1.ID}
	m.InstanceManager.agentIndex["agent-C"] = []string{i2.ID}
	m.InstanceManager.mu.Unlock()

	m.killAllInstances()

	if len(m.InstanceManager.instances) != 0 {
		t.Errorf("Instances not empty after killAll, got %d remaining", len(m.InstanceManager.instances))
	}
	if len(m.InstanceManager.agentIndex) != 0 {
		t.Errorf("agentIndex not empty after killAll, got %d agents", len(m.InstanceManager.agentIndex))
	}
}
