package agentmgr

import (
	"slices"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// Cycle 1: SubstrateAddr field exists and is set by NewAgentManager
func TestNewAgentManager_SubstrateAddr(t *testing.T) {
	m := NewAgentManager(nil, "/usr/bin/python3", "localhost:50051", nil)
	if m.InstanceManager.substrateAddr != "localhost:50051" {
		t.Errorf("expected SubstrateAddr=%q, got %q", "localhost:50051", m.InstanceManager.substrateAddr)
	}
}

// Cycle 0033-02: TraitDaemon injects --daemon-mode and --stream-id flags.
func TestBuildAgentCmd_DaemonFlags(t *testing.T) {
	m := NewAgentManager(nil, "/usr/bin/python3", "unix:/tmp/sub.sock", nil)
	inst := domain.NewInstance("gold-tracker")

	t.Run("daemon injects --daemon-mode and --stream-id", func(t *testing.T) {
		def := &domain.AgentDefinition{
			ID:       "gold-tracker",
			Runtime:  domain.RuntimePython,
			ExecPath: "/agents/gold_tracker_agent.py",
			Dir:      "/agents",
			Trait:    domain.TraitDaemon,
		}
		cmd, err := m.buildAgentCmd(def, inst, "gold-tracker")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !slices.Contains(cmd.Args, "--daemon-mode") {
			t.Errorf("want --daemon-mode in args, got %v", cmd.Args)
		}
		if !slices.Contains(cmd.Args, "--stream-id") {
			t.Errorf("want --stream-id in args, got %v", cmd.Args)
		}
		if !slices.Contains(cmd.Args, "gold-tracker") {
			t.Errorf("want stream ID value 'gold-tracker' in args, got %v", cmd.Args)
		}
	})

	t.Run("non-daemon does NOT get --daemon-mode", func(t *testing.T) {
		def := &domain.AgentDefinition{
			ID:       "analyst",
			Runtime:  domain.RuntimePython,
			ExecPath: "/agents/analyst_agent.py",
			Dir:      "/agents",
			Trait:    domain.TraitCognitive,
		}
		cmd, err := m.buildAgentCmd(def, inst, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if slices.Contains(cmd.Args, "--daemon-mode") {
			t.Errorf("non-daemon must NOT have --daemon-mode, got %v", cmd.Args)
		}
	})

	t.Run("daemon with empty streamID omits --stream-id flag", func(t *testing.T) {
		def := &domain.AgentDefinition{
			ID:       "sensor",
			Runtime:  domain.RuntimePython,
			ExecPath: "/agents/sensor_agent.py",
			Dir:      "/agents",
			Trait:    domain.TraitDaemon,
		}
		cmd, err := m.buildAgentCmd(def, inst, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !slices.Contains(cmd.Args, "--daemon-mode") {
			t.Errorf("want --daemon-mode, got %v", cmd.Args)
		}
		if slices.Contains(cmd.Args, "--stream-id") {
			t.Errorf("empty streamID must omit --stream-id flag, got %v", cmd.Args)
		}
	})
}

// Cycle 2: buildAgentCmd includes --socket and --substrate-addr args
func TestBuildAgentCmd_ArgsInjection(t *testing.T) {
	m := NewAgentManager(nil, "/usr/bin/python3", "/tmp/cambrian_substrate.sock", nil)
	inst := domain.NewInstance("test-agent")

	t.Run("python runtime args", func(t *testing.T) {
		def := &domain.AgentDefinition{
			ID:       "agent-1",
			Runtime:  domain.RuntimePython,
			ExecPath: "/agents/my_agent.py",
			Dir:      "/agents",
		}
		cmd, err := m.buildAgentCmd(def, inst, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		args := cmd.Args
		if !slices.Contains(args, "--socket") {
			t.Errorf("expected --socket in args, got %v", args)
		}
		if !slices.Contains(args, "--substrate-addr") {
			t.Errorf("expected --substrate-addr in args, got %v", args)
		}
		if !slices.Contains(args, "/tmp/cambrian_substrate.sock") {
			t.Errorf("expected substrate socket path in args, got %v", args)
		}
	})

	t.Run("binary runtime args", func(t *testing.T) {
		def := &domain.AgentDefinition{
			ID:       "agent-2",
			Runtime:  domain.RuntimeBinary,
			ExecPath: "/agents/my_agent",
			Dir:      "/agents",
		}
		cmd, err := m.buildAgentCmd(def, inst, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		args := cmd.Args
		if !slices.Contains(args, "--socket") {
			t.Errorf("expected --socket in args, got %v", args)
		}
		if !slices.Contains(args, "--substrate-addr") {
			t.Errorf("expected --substrate-addr in args, got %v", args)
		}
	})

	t.Run("unsupported runtime returns error", func(t *testing.T) {
		def := &domain.AgentDefinition{
			ID:      "agent-3",
			Runtime: "node",
		}
		_, err := m.buildAgentCmd(def, inst, "")
		if err == nil {
			t.Error("expected error for unsupported runtime, got nil")
		}
	})
}
