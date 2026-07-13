package agentmgr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func TestBootAgent_SetsSocketPath(t *testing.T) {
	m := newTestManager()
	m.InstanceManager.substrateAddr = "/tmp/cambrian_substrate.sock"
	inst := domain.NewInstance("uds-agent")

	def := &domain.AgentDefinition{
		ID:       "uds-agent",
		Runtime:  domain.RuntimePython,
		ExecPath: "test_agent.py",
		Dir:      t.TempDir(),
	}

	cmd, err := m.buildAgentCmd(def, inst, "")
	if err != nil {
		t.Fatalf("buildAgentCmd failed: %v", err)
	}

	t.Run("uses --socket not --port", func(t *testing.T) {
		for _, arg := range cmd.Args {
			if arg == "--port" {
				t.Error("buildAgentCmd should not pass --port flag")
			}
		}
		hasSocket := false
		for _, arg := range cmd.Args {
			if arg == "--socket" {
				hasSocket = true
				break
			}
		}
		if !hasSocket {
			t.Error("buildAgentCmd should pass --socket flag")
		}
	})

	t.Run("uses --substrate-addr", func(t *testing.T) {
		hasSubstrateAddr := false
		for _, arg := range cmd.Args {
			if arg == "--substrate-addr" {
				hasSubstrateAddr = true
				break
			}
		}
		if !hasSubstrateAddr {
			t.Error("buildAgentCmd should pass --substrate-addr flag")
		}
	})
}

func TestSocketPath_UniquePerInstance(t *testing.T) {
	id1 := domain.NewInstance("agent-X")
	id2 := domain.NewInstance("agent-X")

	p1 := socketPath("agent-X", id1.ID)
	p2 := socketPath("agent-X", id2.ID)

	if p1 == "" {
		t.Error("socketPath should not be empty")
	}
	if p1 == p2 {
		t.Error("socket paths should be unique per instance ID")
	}
	if !strings.Contains(p1, "agent-X") {
		t.Errorf("socket path should contain agentID, got %s", p1)
	}
	if !strings.Contains(p1, id1.ID) {
		t.Errorf("socket path should contain instanceID, got %s", p1)
	}
	if !strings.HasSuffix(p1, ".sock") {
		t.Errorf("socket path should end with .sock, got %s", p1)
	}
}

func TestSocketPath_UnlinkOnEvict(t *testing.T) {
	tmpDir := t.TempDir()
	socketFile := filepath.Join(tmpDir, "test.sock")

	if err := os.WriteFile(socketFile, nil, 0600); err != nil {
		t.Fatalf("failed to create test socket: %v", err)
	}

	if _, err := os.Stat(socketFile); os.IsNotExist(err) {
		t.Fatal("test socket not created")
	}

	unlinkSocket(socketFile)

	if _, err := os.Stat(socketFile); !os.IsNotExist(err) {
		t.Error("socket file should be unlinked after eviction")
	}
}

func TestEvictInstance_UnlinksSocket(t *testing.T) {
	m := newTestManager()
	tmpDir := t.TempDir()

	socketFile := filepath.Join(tmpDir, "cambrian_agentX_test.sock")
	if err := os.WriteFile(socketFile, nil, 0600); err != nil {
		t.Fatalf("failed to create test socket: %v", err)
	}

	inst := domain.NewInstance("agent-X")
	inst.SocketPath = socketFile

	m.InstanceManager.mu.Lock()
	m.InstanceManager.instances[inst.ID] = inst
	m.InstanceManager.agentIndex["agent-X"] = []string{inst.ID}
	m.InstanceManager.mu.Unlock()

	m.EvictInstance(inst.ID)

	if _, err := os.Stat(socketFile); !os.IsNotExist(err) {
		t.Error("socket file should be unlinked when instance is evicted")
	}
	if _, ok := m.InstanceManager.instances[inst.ID]; ok {
		t.Error("instance should be removed from Instances after eviction")
	}
}
