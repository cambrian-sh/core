package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// ── ADR-0125: JS agent discovery shapes ──────────────────────────────────────

func discoverIn(t *testing.T, agentsDir string) map[string]DiscoveredAgent {
	t.Helper()
	found, err := DiscoverFilesystemAgents(agentsDir, nil)
	if err != nil {
		t.Fatalf("DiscoverFilesystemAgents: %v", err)
	}
	byID := make(map[string]DiscoveredAgent, len(found))
	for _, da := range found {
		if _, dup := byID[da.Agent.ID]; dup {
			t.Fatalf("duplicate registration for id %q", da.Agent.ID)
		}
		byID[da.Agent.ID] = da
	}
	return byID
}

func TestDiscover_SingleFileTS_RegistersAsBun(t *testing.T) {
	agentsDir := t.TempDir()
	src := `export const AGENT_DESCRIPTION = "Summarizes tickets."
` + "export const AGENT_MANIFEST = `{\"version\":\"1.2.0\",\"capabilities\":[\"summarize\"]}`" + `
console.log("agent body")
`
	if err := os.WriteFile(filepath.Join(agentsDir, "ticket_agent.ts"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	byID := discoverIn(t, agentsDir)
	got, ok := byID["ticket_agent"]
	if !ok {
		t.Fatalf("ticket_agent not discovered; got %v", byID)
	}
	if got.Agent.Runtime != "bun" {
		t.Errorf("runtime: want bun for .ts, got %q", got.Agent.Runtime)
	}
	if got.Agent.ExecPath != "ticket_agent.ts" {
		t.Errorf("exec path: want relative ticket_agent.ts, got %q", got.Agent.ExecPath)
	}
	if got.Agent.Description != "Summarizes tickets." {
		t.Errorf("description regex failed: got %q", got.Agent.Description)
	}
	if got.Manifest.Version != "1.2.0" || len(got.Manifest.Capabilities) != 1 {
		t.Errorf("template-literal manifest not parsed: %+v", got.Manifest)
	}
}

func TestDiscover_SingleFileJS_RegistersAsNode(t *testing.T) {
	agentsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(agentsDir, "legacy_agent.js"), []byte(`const AGENT_DESCRIPTION = "Legacy JS agent."`), 0o600); err != nil {
		t.Fatal(err)
	}

	byID := discoverIn(t, agentsDir)
	got, ok := byID["legacy_agent"]
	if !ok {
		t.Fatalf("legacy_agent not discovered; got %v", byID)
	}
	if got.Agent.Runtime != "node" {
		t.Errorf("runtime: want node for .js, got %q", got.Agent.Runtime)
	}
}

func TestDiscover_SiblingManifest_OverridesRuntimeWithoutDoubleRegistration(t *testing.T) {
	agentsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(agentsDir, "fast_agent.js"), []byte(`const AGENT_DESCRIPTION = "Runs under bun."`), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := `{"version":"2.0","runtime":"bun","capabilities":["fast"]}`
	if err := os.WriteFile(filepath.Join(agentsDir, "fast_agent.manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	byID := discoverIn(t, agentsDir) // discoverIn fails the test on duplicates
	got, ok := byID["fast_agent"]
	if !ok {
		t.Fatalf("fast_agent not discovered; got %v", byID)
	}
	if len(byID) != 1 {
		t.Fatalf("want exactly 1 record (sidecar is the source's manifest, not a standalone agent), got %d", len(byID))
	}
	if got.Agent.Runtime != "bun" {
		t.Errorf("runtime: manifest override must win over .js default, got %q", got.Agent.Runtime)
	}
	if got.Agent.ManifestVersion != "2.0" {
		t.Errorf("manifest version: want 2.0, got %q", got.Agent.ManifestVersion)
	}
}

func TestDiscover_JSPackage_RegistersWithDirNameID(t *testing.T) {
	agentsDir := t.TempDir()
	pkg := filepath.Join(agentsDir, "triage_agent")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"name":"triage-agent"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "agent.ts"), []byte(`export const AGENT_DESCRIPTION = "Triage."`), 0o600); err != nil {
		t.Fatal(err)
	}

	byID := discoverIn(t, agentsDir)
	got, ok := byID["triage_agent"]
	if !ok {
		t.Fatalf("triage_agent not discovered; got %v", byID)
	}
	if got.Agent.ExecPath != "triage_agent/agent.ts" {
		t.Errorf("exec path: want triage_agent/agent.ts, got %q", got.Agent.ExecPath)
	}
	if got.Agent.Runtime != "bun" {
		t.Errorf("runtime: want bun, got %q", got.Agent.Runtime)
	}
}

func TestDiscover_PythonPackageWinsOverJS(t *testing.T) {
	agentsDir := t.TempDir()
	pkg := filepath.Join(agentsDir, "hybrid_agent")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"__init__.py":  "",
		"agent.py":     `AGENT_DESCRIPTION = "Python wins."`,
		"package.json": `{}`,
		"agent.ts":     `export const AGENT_DESCRIPTION = "JS loses."`,
	} {
		if err := os.WriteFile(filepath.Join(pkg, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	byID := discoverIn(t, agentsDir)
	got, ok := byID["hybrid_agent"]
	if !ok || len(byID) != 1 {
		t.Fatalf("want exactly hybrid_agent, got %v", byID)
	}
	if got.Agent.Runtime != "python" {
		t.Errorf("runtime: python form must win in a mixed package, got %q", got.Agent.Runtime)
	}
}

func TestDiscover_NodeModulesAreNeverAgents(t *testing.T) {
	agentsDir := t.TempDir()
	dep := filepath.Join(agentsDir, "node_modules", "some-lib")
	if err := os.MkdirAll(dep, 0o755); err != nil {
		t.Fatal(err)
	}
	// A dependency legitimately shipping a file named agent.js.
	if err := os.WriteFile(filepath.Join(dep, "agent.js"), []byte(`module.exports = {}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// And even a package.json+agent.ts combo inside node_modules.
	if err := os.WriteFile(filepath.Join(dep, "package.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	byID := discoverIn(t, agentsDir)
	if len(byID) != 0 {
		t.Errorf("nothing under node_modules may register, got %v", byID)
	}
}
