package config

import (
	"encoding/json"
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

// ADR-0157 S1 keys: on by default since S7; first-party degrades unless set to
// refuse (Q2); runner, docker image, cap paths and on_unavailable are validated.
func TestAgentSandbox_S1Keys(t *testing.T) {
	d := DefaultConfig().Execution.Agents.Sandbox
	if !d.Enabled || d.FirstParty.Refuses() || d.RunnerOrDefault() != "native" {
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

// ADR-0157 S2: execution.agents.sandbox.network — "" is per_tier (third-party
// kernel-only, first-party normal network), kernel_only_all applies kernel-only
// to every tier, anything else is refused at load.
func TestAgentSandbox_NetworkKey(t *testing.T) {
	d := DefaultConfig().Execution.Agents.Sandbox
	if d.KernelOnlyAll() || d.NetworkOrDefault() != AgentNetworkPerTier {
		t.Fatalf("default: kernel_only_all=%v network=%q", d.KernelOnlyAll(), d.NetworkOrDefault())
	}
	for _, v := range []string{"", AgentNetworkPerTier, AgentNetworkKernelOnlyAll} {
		c := AgentSandboxConfig{Network: v}
		if errs := c.validate(); len(errs) != 0 {
			t.Fatalf("network %q refused: %v", v, errs)
		}
		if c.KernelOnlyAll() != (v == AgentNetworkKernelOnlyAll) {
			t.Fatalf("network %q: KernelOnlyAll=%v", v, c.KernelOnlyAll())
		}
	}
	bad := AgentSandboxConfig{Network: "none"}
	if errs := bad.validate(); len(errs) != 1 || !strings.Contains(errs[0], "execution.agents.sandbox.network") {
		t.Fatalf("bad network: %v", errs)
	}
}

// ADR-0157 S7: the sandbox is on for an install that never set the key, and an
// operator's explicit false — in a config file or a CAMBRIAN_* variable — still
// turns it off. The key has no omitempty, so the defaults layer (marshalled
// from DefaultConfig) carries true and a layer's false overrides it.
func TestAgentSandbox_S7DefaultOnExplicitOffSurvives(t *testing.T) {
	dir := baseBundle(t)
	cfg, prov := loadFrom(t, dir, nil)
	if !cfg.Execution.Agents.Sandbox.Enabled {
		t.Fatal("a config that never sets execution.agents.sandbox.enabled must run with the sandbox on")
	}
	if got := prov.Source("execution.agents.sandbox.enabled"); got != SourceDefault {
		t.Fatalf("enabled source = %q, want the default", got)
	}

	writeCfg(t, dir, "config.json", `{
	  "database": {"host": "localhost", "user": "u", "password": "p", "dbname": "d"},
	  "execution": {"agents": {"sandbox": {"enabled": false}}}
	}`)
	if cfg, _ := loadFrom(t, dir, nil); cfg.Execution.Agents.Sandbox.Enabled {
		t.Fatal("an explicit enabled: false in config.json was lost")
	}

	off := baseBundle(t)
	t.Setenv("CAMBRIAN_EXECUTION__AGENTS__SANDBOX__ENABLED", "false")
	if cfg, _ := loadFrom(t, off, nil); cfg.Execution.Agents.Sandbox.Enabled {
		t.Fatal("CAMBRIAN_EXECUTION__AGENTS__SANDBOX__ENABLED=false did not turn the sandbox off")
	}

	b, err := json.Marshal(AgentSandboxConfig{})
	if err != nil || !strings.Contains(string(b), `"enabled":false`) {
		t.Fatalf("an explicit false must survive a marshal (no omitempty): %s %v", b, err)
	}
}
