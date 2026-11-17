package config

import (
	"strings"
	"testing"
)

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

// ADR-0157 S1 keys: off by default; first-party degrades unless set to refuse
// (Q2); runner, docker image, cap paths and on_unavailable are validated.
func TestAgentSandbox_S1Keys(t *testing.T) {
	d := DefaultConfig().Execution.Agents.Sandbox
	if d.Enabled || d.FirstParty.Refuses() || d.RunnerOrDefault() != "native" {
		t.Fatalf("defaults: enabled=%v first-party refuses=%v runner=%s", d.Enabled, d.FirstParty.Refuses(), d.RunnerOrDefault())
	}
	ok := AgentSandboxConfig{Enabled: true, Runner: "docker", DockerImage: "cambrian/kernel:1",
		AllowlistCap: []string{"/var/cache/models", "~/.cache/huggingface"}, FirstParty: AgentFirstPartySandbox{OnUnavailable: "refuse"}}
	if errs := ok.validate(); len(errs) != 0 || !ok.FirstParty.Refuses() {
		t.Fatalf("valid block refused: %v", errs)
	}
	bad := AgentSandboxConfig{Enabled: true, Runner: "podman", AllowlistCap: []string{"models"},
		FirstParty: AgentFirstPartySandbox{OnUnavailable: "maybe"}, HidePaths: []string{" "}}
	if errs := bad.validate(); len(errs) != 4 {
		t.Fatalf("want 4 errors (runner, cap, first_party, hide_paths), got %v", errs)
	}
	noImage := AgentSandboxConfig{Enabled: true, Runner: "docker"}
	if errs := noImage.validate(); len(errs) != 1 || !strings.Contains(errs[0], "docker_image") {
		t.Fatalf("docker without an image: %v", errs)
	}
}
