package config

import "testing"

// execution.agents.sandbox: "" refuses (fail closed), unknown values are refused
// at load, and the memory default applies.
func TestAgentSandbox_ValidateAndDefaults(t *testing.T) {
	var c AgentSandboxConfig
	if errs := c.validate(); len(errs) != 0 || !c.ThirdParty.Refuses() || c.ThirdParty.Memory() != 2048 {
		t.Fatalf("zero block: errs=%v refuses=%v mem=%d", errs, c.ThirdParty.Refuses(), c.ThirdParty.Memory())
	}
	c.ThirdParty = AgentTierSandbox{OnUnavailable: "degrade", MemoryMB: 512}
	if errs := c.validate(); len(errs) != 0 || c.ThirdParty.Refuses() || c.ThirdParty.Memory() != 512 {
		t.Fatalf("degrade: errs=%v", errs)
	}
	c.ThirdParty = AgentTierSandbox{OnUnavailable: "allow", MemoryMB: -1}
	if errs := c.validate(); len(errs) != 2 {
		t.Fatalf("bad values: %v", errs)
	}
	if errs := DefaultConfig().Execution.Validate(); errs != nil {
		t.Fatalf("defaults invalid: %v", errs)
	}
}
