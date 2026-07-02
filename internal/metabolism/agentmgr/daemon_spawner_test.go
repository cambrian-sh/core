package agentmgr

import (
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

func makeTestDaemonManager() *AgentManager {
	m := newTestManager()
	return m
}

// Cycle 1 — Two SpawnDaemon calls for the same streamID produce ref-count=2
// and only one daemon process (the second returns the existing instanceID).
func TestDaemonSpawner_RefCount_TwoCalls_SingleProcess(t *testing.T) {
	m := makeTestDaemonManager()
	// Pre-register a daemon instance as if bootAgent ran.
	inst := domain.NewInstance("gold-tracker")
	inst.Mode = domain.ModeDaemon
	m.InstanceManager.mu.Lock()
	m.InstanceManager.instances[inst.ID] = inst
	m.InstanceManager.agentIndex["gold-tracker"] = []string{inst.ID}
	m.InstanceManager.mu.Unlock()

	// Simulate first ref (count 0→1): SpawnDaemon records it.
	m.IncrementDaemonRef("gold-tracker", inst.ID)
	if m.DaemonRefCount("gold-tracker") != 1 {
		t.Fatalf("after first spawn: want ref=1, got %d", m.DaemonRefCount("gold-tracker"))
	}

	// Second ref (count 1→2): existing instance, no new boot.
	m.IncrementDaemonRef("gold-tracker", inst.ID)
	if m.DaemonRefCount("gold-tracker") != 2 {
		t.Fatalf("after second spawn: want ref=2, got %d", m.DaemonRefCount("gold-tracker"))
	}

	// Only one instance should exist in the registry.
	ids := m.GetInstanceIDs("gold-tracker")
	if len(ids) != 1 {
		t.Errorf("want 1 running daemon, got %d", len(ids))
	}
}

// Cycle 2 — StopDaemon with ref=2 decrements to 1; daemon stays running.
func TestDaemonSpawner_StopDaemon_RefTwo_DaemonSurvives(t *testing.T) {
	m := makeTestDaemonManager()
	inst := domain.NewInstance("gold-tracker")
	inst.Mode = domain.ModeDaemon
	m.InstanceManager.mu.Lock()
	m.InstanceManager.instances[inst.ID] = inst
	m.InstanceManager.agentIndex["gold-tracker"] = []string{inst.ID}
	m.InstanceManager.mu.Unlock()
	m.IncrementDaemonRef("gold-tracker", inst.ID)
	m.IncrementDaemonRef("gold-tracker", inst.ID)

	stopped, err := m.DecrementDaemonRef("gold-tracker")
	if err != nil {
		t.Fatalf("DecrementDaemonRef: %v", err)
	}
	if stopped {
		t.Error("daemon must NOT stop when ref goes from 2→1")
	}
	if m.DaemonRefCount("gold-tracker") != 1 {
		t.Errorf("want ref=1, got %d", m.DaemonRefCount("gold-tracker"))
	}
	// Instance still alive.
	if _, ok := m.InstanceManager.instances[inst.ID]; !ok {
		t.Error("daemon instance should still be running at ref=1")
	}
}

// Cycle 3 — StopDaemon with ref=1 decrements to 0; EvictAgent called.
func TestDaemonSpawner_StopDaemon_RefOne_DaemonStopped(t *testing.T) {
	m := makeTestDaemonManager()
	inst := domain.NewInstance("gold-tracker")
	inst.Mode = domain.ModeDaemon
	m.InstanceManager.mu.Lock()
	m.InstanceManager.instances[inst.ID] = inst
	m.InstanceManager.agentIndex["gold-tracker"] = []string{inst.ID}
	m.InstanceManager.mu.Unlock()
	m.IncrementDaemonRef("gold-tracker", inst.ID)

	stopped, err := m.DecrementDaemonRef("gold-tracker")
	if err != nil {
		t.Fatalf("DecrementDaemonRef: %v", err)
	}
	if !stopped {
		t.Error("daemon must stop when ref reaches 0")
	}
	if m.DaemonRefCount("gold-tracker") != 0 {
		t.Errorf("want ref=0, got %d", m.DaemonRefCount("gold-tracker"))
	}
}

// Cycle 4 — ListRunningDaemons returns only Daemon-mode instances.
func TestDaemonSpawner_ListRunningDaemons_FiltersCorrectly(t *testing.T) {
	m := makeTestDaemonManager()

	daemon := domain.NewInstance("gold-tracker")
	daemon.Mode = domain.ModeDaemon

	jit := domain.NewInstance("analyst")
	jit.Mode = domain.ModeJIT

	m.InstanceManager.mu.Lock()
	m.InstanceManager.instances[daemon.ID] = daemon
	m.InstanceManager.instances[jit.ID] = jit
	m.InstanceManager.mu.Unlock()

	running := m.ListRunningDaemons()
	if len(running) != 1 {
		t.Fatalf("want 1 daemon, got %d: %v", len(running), running)
	}
	if running[0].AgentID != "gold-tracker" {
		t.Errorf("want gold-tracker, got %q", running[0].AgentID)
	}
}

// Cycle 5 — SetDaemonStatus and GetDaemonStatus reflect crash/run states.
func TestDaemonSpawner_DaemonStatus(t *testing.T) {
	m := makeTestDaemonManager()

	m.SetDaemonStatus("gold-tracker", "running")
	if s := m.GetDaemonStatus("gold-tracker"); s != "running" {
		t.Errorf("want 'running', got %q", s)
	}
	m.SetDaemonStatus("gold-tracker", "unavailable")
	if s := m.GetDaemonStatus("gold-tracker"); s != "unavailable" {
		t.Errorf("want 'unavailable', got %q", s)
	}
}
