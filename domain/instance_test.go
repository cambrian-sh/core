package domain

import (
	"testing"
)

func TestInstance_NewInstance_HasUniqueID(t *testing.T) {
	i1 := NewInstance("agent-1")
	i2 := NewInstance("agent-1")

	if i1.ID == "" {
		t.Error("Instance.ID should not be empty")
	}
	if i2.ID == "" {
		t.Error("Instance.ID should not be empty")
	}
	if i1.ID == i2.ID {
		t.Errorf("two instances of same agent should have different IDs, got %s twice", i1.ID)
	}
	if i1.AgentID != "agent-1" {
		t.Errorf("expected AgentID agent-1, got %s", i1.AgentID)
	}
}

func TestInstance_DefaultModeIsJIT(t *testing.T) {
	i := NewInstance("agent-x")
	if i.Mode != ModeJIT {
		t.Errorf("expected default Mode JIT, got %s", i.Mode)
	}
}

func TestInstance_ModeConstants(t *testing.T) {
	if ModeJIT == ModePool {
		t.Error("ModeJIT and ModePool should be distinct")
	}
	if ModeJIT == ModeDaemon {
		t.Error("ModeJIT and ModeDaemon should be distinct")
	}
	if string(ModeJIT) == "" {
		t.Error("ModeJIT string representation should not be empty")
	}
}
