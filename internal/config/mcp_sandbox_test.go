package config

import (
	"strings"
	"testing"
)

func TestMCPSandboxValidation(t *testing.T) {
	bad := -1
	errs := MCPSandboxConfig{
		DefaultTier:  "system",
		CatalogTiers: map[string]string{"official": "trusted"},
		Tiers: map[string]MCPSandboxTierConfig{
			"third-party": {OnUnavailable: "maybe", Runner: "vm", MemoryMB: &bad},
			"kernel":      {},
		},
		CgroupParent: "relative/cg",
	}.validate()
	joined := strings.Join(errs, "\n")
	for _, want := range []string{"default_tier", "catalog_tiers.official", "on_unavailable", "runner", "memory_mb", "kernel: unknown tier", "cgroup_parent"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing error about %s in:\n%s", want, joined)
		}
	}
	if errs := (MCPSandboxConfig{}).validate(); len(errs) != 0 {
		t.Fatalf("the empty block (all defaults) must be valid: %v", errs)
	}
}

func TestMCPServerSandboxValidation(t *testing.T) {
	cases := map[string]struct {
		tier string
		sb   *MCPServerSandboxConfig
		want string
	}{
		"system tier":       {"system", nil, "system is the kernel itself"},
		"unknown tier":      {"trusted", nil, "trust_tier"},
		"bad network":       {"", &MCPServerSandboxConfig{Network: "some"}, "sandbox.network"},
		"hosts need egress": {"", &MCPServerSandboxConfig{Hosts: []string{"a.example"}}, "needs network: egress"},
		"relative root":     {"", &MCPServerSandboxConfig{Root: "data"}, "absolute"},
		"bad runner":        {"", &MCPServerSandboxConfig{Runner: "vm"}, "sandbox.runner"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			errs := ValidateMCPServerSandbox("mcp.servers[x]", c.tier, c.sb)
			if !strings.Contains(strings.Join(errs, ";"), c.want) {
				t.Fatalf("want %q, got %v", c.want, errs)
			}
		})
	}
	ok := ValidateMCPServerSandbox("p", "third-party", &MCPServerSandboxConfig{Network: "egress", Hosts: []string{"api.example.com"}, Runner: "docker"})
	if len(ok) != 0 {
		t.Fatalf("valid declaration refused: %v", ok)
	}
	// ValidateMCPServer (boot + console writes) carries the same rules.
	if errs := ValidateMCPServer(MCPServerConfig{ID: "x", TrustTier: "system"}); len(errs) == 0 {
		t.Fatal("ValidateMCPServer must refuse trust_tier system")
	}
}
