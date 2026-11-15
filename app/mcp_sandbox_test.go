package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/integrations"
	"github.com/cambrian-sh/core/internal/sandbox"
)

// TestSandboxPolicyFromConfig: mcp.sandbox overlays the built-in defaults and
// reaches every server mapped from config (threaded to the consumer, §3.4).
func TestSandboxPolicyFromConfig(t *testing.T) {
	cat, err := integrations.Load("")
	if err != nil {
		t.Fatal(err)
	}
	off, mem, zero := false, 512, 0
	p := sandboxPolicyFromConfig(config.MCPSandboxConfig{
		DefaultTier:  "vetted",
		CatalogTiers: map[string]string{"community": "vetted"},
		WorkRoot:     t.TempDir(),
		Tiers: map[string]config.MCPSandboxTierConfig{
			"third-party": {OnUnavailable: "degrade", Runner: "docker", DenyNetwork: &off, MemoryMB: &mem, CPUTimeSec: &zero},
		},
	}, cat)
	if p.DefaultTier != sandbox.TierVetted || p.CatalogTiers["community"] != sandbox.TierVetted ||
		p.CatalogTiers["official"] != sandbox.TierVetted {
		t.Fatalf("tier mapping not applied: %+v %+v", p.DefaultTier, p.CatalogTiers)
	}
	tp := p.Tiers[sandbox.TierThirdParty]
	if tp.OnUnavailable != "degrade" || tp.Runner != "docker" || tp.DenyNetwork || tp.MemoryMB != 512 || tp.CPUTimeSec != 0 {
		t.Fatalf("tier override not applied: %+v", tp)
	}
	if !tp.ConfineFS || tp.MaxProcesses == 0 {
		t.Fatalf("unset fields must keep their defaults: %+v", tp)
	}
	if len(p.Catalog) == 0 {
		t.Fatal("catalog tiers not attached")
	}

	defaults := sandboxPolicyFromConfig(config.MCPSandboxConfig{}, cat)
	cases := []struct {
		name string
		s    config.MCPServerConfig
		want sandbox.Tier
	}{
		{"official catalog entry", config.MCPServerConfig{ID: "f", Transport: "stdio", Endpoint: "uvx", CatalogID: "fetch"}, sandbox.TierVetted},
		{"community catalog entry", config.MCPServerConfig{ID: "s", Transport: "stdio", Endpoint: "npx", CatalogID: "slack"}, sandbox.TierThirdParty},
		{"custom server", config.MCPServerConfig{ID: "c", Transport: "stdio", Endpoint: "my-server"}, sandbox.TierThirdParty},
		{"operator decision", config.MCPServerConfig{ID: "t", Transport: "stdio", Endpoint: "python", TrustTier: "first-party"}, sandbox.TierFirstParty},
	}
	for _, c := range cases {
		sc := mcpServerFromConfig(c.s, defaults)
		if sc.Sandbox.Tier != c.want || !sc.Sandbox.Resolved() {
			t.Errorf("%s: tier %s (%s), want %s", c.name, sc.Sandbox.Tier, sc.Sandbox.TierSource, c.want)
		}
	}
	// A catalog server inherits the catalog's sandbox declaration (egress).
	sc := mcpServerFromConfig(config.MCPServerConfig{ID: "s", Transport: "stdio", Endpoint: "npx", CatalogID: "slack"}, defaults)
	if sc.Sandbox.Server.Network != sandbox.NetworkEgress || !strings.Contains(strings.Join(sc.Sandbox.Server.Hosts, ","), "slack.com") {
		t.Fatalf("catalog declaration not inherited: %+v", sc.Sandbox.Server)
	}
}

func TestAnchorSandboxPaths(t *testing.T) {
	cfg := &config.Config{}
	cfg.MCP.Sandbox.WorkRoot = "sandbox"
	cfg.Storage.DataDir = "/data"
	anchorSandboxPaths(cfg, "/srv/cambrian/configs/config.json")
	if !strings.HasSuffix(strings.ReplaceAll(cfg.MCP.Sandbox.WorkRoot, `\`, "/"), "/srv/cambrian/configs/sandbox") {
		t.Fatalf("work_root not anchored: %s", cfg.MCP.Sandbox.WorkRoot)
	}
	if len(cfg.MCP.Sandbox.HidePaths) != 2 {
		t.Fatalf("hide_paths default: %v", cfg.MCP.Sandbox.HidePaths)
	}
	// ENG-681: with the backup block set, the dumps and the encrypted key copies are hidden too.
	withBackup := &config.Config{Backup: config.DefaultBackupConfig()}
	anchorSandboxPaths(withBackup, "/srv/cambrian/configs/config.json")
	hide := strings.ReplaceAll(strings.Join(withBackup.MCP.Sandbox.HidePaths, ","), `\`, "/")
	if !strings.Contains(hide, "/srv/cambrian/backups,") || !strings.HasSuffix(hide, "/srv/cambrian/backups-keys") {
		t.Fatalf("backup dirs not hidden: %v", withBackup.MCP.Sandbox.HidePaths)
	}
}

// T-20261109-configs-mcp-json-example-reference-servers: every reference server in
// configs/mcp.json.example names its catalog entry, so the ADR-0139 D8 sandbox takes
// the catalog's tier and sandbox declaration (egress for the network readers) instead
// of the default third-party tier with no network, which Windows native cannot enforce
// and therefore refused ("sandbox refused to start \"fetch\" at tier third-party").
func TestMCPExampleServersResolveThroughTheCatalog(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "configs", "mcp.json.example"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		MCP struct {
			Servers []config.MCPServerConfig `json:"servers"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	cat, err := integrations.Load("")
	if err != nil {
		t.Fatal(err)
	}
	p := sandboxPolicyFromConfig(config.MCPSandboxConfig{}, cat)
	if len(doc.MCP.Servers) == 0 {
		t.Fatal("no servers parsed from the example")
	}
	for _, s := range doc.MCP.Servers {
		spec := p.Resolve(s.ID, s.TrustTier, s.CatalogID, false, serverSandboxSpec(s.Sandbox))
		if spec.TierSource != "catalog:"+s.ID {
			t.Errorf("%s: tier from %q (catalog_id %q), want the catalog entry", s.ID, spec.TierSource, s.CatalogID)
		}
	}
	fetch := p.Resolve("fetch", "", "fetch", false, sandbox.ServerSpec{})
	if fetch.Server.Network != "egress" || fetch.Tier == sandbox.TierThirdParty {
		t.Errorf("fetch resolves to %s with network %q", fetch.Tier, fetch.Server.Network)
	}
}
