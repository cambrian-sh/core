package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// ── Sidecar Manifest Discovery Tests (Issue #0008-03) ────────────────────────

// helper: write a sidecar manifest JSON and fake binary to a temp dir.
// Returns (manifestPath, binaryPath).
func writeSidecarFiles(t *testing.T, dir, name, manifestJSON, binaryContent string) (string, string) {
	t.Helper()
	manifestPath := filepath.Join(dir, name+".manifest.json")
	binaryPath := filepath.Join(dir, name)
	if err := os.WriteFile(manifestPath, []byte(manifestJSON), 0600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(binaryPath, []byte(binaryContent), 0700); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	return manifestPath, binaryPath
}

// Cycle 1 — Test 1:
// A valid sidecar manifest with "trait":"tool" registers an AgentDefinition
// with Trait=TraitTool and ExecPath resolved relative to the manifest dir.
func TestSidecar_ValidToolManifest_RegistersWithTraitTool(t *testing.T) {
	agentsDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	manifestJSON := `{
		"trait": "tool",
		"version": "1.0",
		"exec_path": "./file_writer",
		"description": "Writes content to a file.",
		"supported_formats": ["text/plain"],
		"runtime": "binary"
	}`
	writeSidecarFiles(t, agentsDir, "file_writer", manifestJSON, "fake binary content")

	adapter, err := NewBBoltAdapter(dbPath, agentsDir, nil)
	if err != nil {
		t.Fatalf("NewBBoltAdapter: %v", err)
	}
	defer adapter.Close()

	agents, err := adapter.GetAllAgentRecords()
	if err != nil {
		t.Fatalf("GetAllAgentRecords: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}

	got := agents[0]
	if got.Trait != "tool" {
		t.Errorf("expected Trait=%q, got %q", "tool", got.Trait)
	}
	// ExecPath must be resolved (absolute) and point to the binary alongside the manifest
	wantExecPath := filepath.ToSlash(filepath.Join(agentsDir, "file_writer"))
	if got.ExecPath != wantExecPath {
		t.Errorf("expected ExecPath=%q, got %q", wantExecPath, got.ExecPath)
	}
}

// Cycle 2 — Test 2:
// When "runtime" is absent from the sidecar JSON, it defaults to "binary".
func TestSidecar_AbsentRuntime_DefaultsBinary(t *testing.T) {
	agentsDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	manifestJSON := `{
		"trait": "tool",
		"version": "1.0",
		"exec_path": "./calc",
		"description": "A deterministic calculator."
	}`
	writeSidecarFiles(t, agentsDir, "calc", manifestJSON, "binary data")

	adapter, err := NewBBoltAdapter(dbPath, agentsDir, nil)
	if err != nil {
		t.Fatalf("NewBBoltAdapter: %v", err)
	}
	defer adapter.Close()

	agents, err := adapter.GetAllAgentRecords()
	if err != nil {
		t.Fatalf("GetAllAgentRecords: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	got := agents[0]
	if got.Runtime != "binary" {
		t.Errorf("expected Runtime=%q, got %q", "binary", got.Runtime)
	}
}

// Cycle 3 — Test 3:
// ComputeSidecarSourceHash changes when binary bytes change.
func TestSidecar_SourceHashChangesWhenBinaryChanges(t *testing.T) {
	manifestJSON := []byte(`{"trait":"tool","version":"1.0","exec_path":"./fw","description":"d"}`)
	binary1 := []byte("binary version 1")
	binary2 := []byte("binary version 2")

	h1 := ComputeSidecarSourceHash("1.0", manifestJSON, binary1)
	h2 := ComputeSidecarSourceHash("1.0", manifestJSON, binary2)
	if h1 == h2 {
		t.Errorf("expected different hashes when binary changes, both got %q", h1)
	}
}

// Cycle 3 — Test 4:
// ComputeSidecarSourceHash changes when manifest JSON bytes change.
func TestSidecar_SourceHashChangesWhenManifestChanges(t *testing.T) {
	manifest1 := []byte(`{"trait":"tool","version":"1.0","exec_path":"./fw","description":"d"}`)
	manifest2 := []byte(`{"trait":"tool","version":"2.0","exec_path":"./fw","description":"d"}`)
	binary := []byte("stable binary")

	h1 := ComputeSidecarSourceHash("1.0", manifest1, binary)
	h2 := ComputeSidecarSourceHash("2.0", manifest2, binary)
	if h1 == h2 {
		t.Errorf("expected different hashes when manifest changes, both got %q", h1)
	}
}

// Cycle 4 — Test 5:
// A sidecar without a "trait" field is skipped (not registered), no fatal error.
func TestSidecar_MissingTrait_IsSkipped(t *testing.T) {
	agentsDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	manifestJSON := `{
		"version": "1.0",
		"exec_path": "./mystery",
		"description": "No trait declared."
	}`
	writeSidecarFiles(t, agentsDir, "mystery", manifestJSON, "data")

	adapter, err := NewBBoltAdapter(dbPath, agentsDir, nil)
	if err != nil {
		t.Fatalf("NewBBoltAdapter: %v", err)
	}
	defer adapter.Close()

	agents, err := adapter.GetAllAgentRecords()
	if err != nil {
		t.Fatalf("GetAllAgentRecords: %v", err)
	}
	if len(agents) != 0 {
		t.Errorf("expected 0 agents (sidecar with no trait skipped), got %d", len(agents))
	}
}

// Cycle 4 — Test 6:
// A sidecar with "trait":"cognitive" is skipped (non-"tool" trait).
func TestSidecar_NonToolTrait_IsSkipped(t *testing.T) {
	agentsDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	manifestJSON := `{
		"trait": "cognitive",
		"version": "1.0",
		"exec_path": "./thinker",
		"description": "Cognitive only."
	}`
	writeSidecarFiles(t, agentsDir, "thinker", manifestJSON, "data")

	adapter, err := NewBBoltAdapter(dbPath, agentsDir, nil)
	if err != nil {
		t.Fatalf("NewBBoltAdapter: %v", err)
	}
	defer adapter.Close()

	agents, err := adapter.GetAllAgentRecords()
	if err != nil {
		t.Fatalf("GetAllAgentRecords: %v", err)
	}
	if len(agents) != 0 {
		t.Errorf("expected 0 agents (non-tool trait skipped), got %d", len(agents))
	}
}

// Cycle 5 — Test 7:
// Port assignment for sidecar Tool-Agents uses the same basePort pool as Python agents.
// Mix one Python agent + one sidecar; they must receive distinct ports.
func TestSidecar_PortAssignment_NoCollisionWithPythonAgents(t *testing.T) {
	agentsDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	// Write a Python agent
	pyContent := `
AGENT_DESCRIPTION = "A python cognitive agent."
AGENT_MANIFEST = '''
{
    "version": "1.0.0",
    "supported_formats": ["text"]
}
'''
`
	if err := os.WriteFile(filepath.Join(agentsDir, "thinker_agent.py"), []byte(pyContent), 0600); err != nil {
		t.Fatalf("write python agent: %v", err)
	}

	// Write a sidecar Tool-Agent
	manifestJSON := `{
		"trait": "tool",
		"version": "1.0",
		"exec_path": "./writer",
		"description": "Binary writer tool."
	}`
	writeSidecarFiles(t, agentsDir, "writer", manifestJSON, "elf binary")

	adapter, err := NewBBoltAdapter(dbPath, agentsDir, nil)
	if err != nil {
		t.Fatalf("NewBBoltAdapter: %v", err)
	}
	defer adapter.Close()

	agents, err := adapter.GetAllAgentRecords()
	if err != nil {
		t.Fatalf("GetAllAgentRecords: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("expected 2 agents (1 Python + 1 sidecar), got %d", len(agents))
	}
}

// Cycle 6 — Test 8:
// If binary file does not exist at the resolved exec_path, the sidecar is skipped (not registered).
// NewBBoltAdapter must not crash.
func TestSidecar_MissingBinary_IsSkipped(t *testing.T) {
	agentsDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	// Write manifest only — no binary file
	manifestJSON := `{
		"trait": "tool",
		"version": "1.0",
		"exec_path": "./ghost",
		"description": "Ghost agent."
	}`
	manifestPath := filepath.Join(agentsDir, "ghost.manifest.json")
	if err := os.WriteFile(manifestPath, []byte(manifestJSON), 0600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	// Deliberately do NOT write the binary file

	adapter, err := NewBBoltAdapter(dbPath, agentsDir, nil)
	if err != nil {
		t.Fatalf("NewBBoltAdapter must not return error when binary is missing: %v", err)
	}
	defer adapter.Close()

	agents, err := adapter.GetAllAgentRecords()
	if err != nil {
		t.Fatalf("GetAllAgentRecords: %v", err)
	}
	if len(agents) != 0 {
		t.Errorf("expected 0 agents (missing binary skipped), got %d", len(agents))
	}
}

// Cycle 7 — Test 9:
// A Python agent placed in a nested subdirectory is registered with its filename-based ID.
func TestSeed_NestedPythonAgent_RegistersWithFilenameID(t *testing.T) {
	agentsDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	nestedDir := filepath.Join(agentsDir, "system")
	if err := os.MkdirAll(nestedDir, 0700); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}

	pyContent := `
AGENT_DESCRIPTION = "Pre-plan world-state scout."
AGENT_MANIFEST = '''
{
    "version": "1.0.0",
    "supported_formats": ["text"]
}
'''
`
	if err := os.WriteFile(filepath.Join(nestedDir, "scout_agent.py"), []byte(pyContent), 0600); err != nil {
		t.Fatalf("write nested python agent: %v", err)
	}

	adapter, err := NewBBoltAdapter(dbPath, agentsDir, nil)
	if err != nil {
		t.Fatalf("NewBBoltAdapter: %v", err)
	}
	defer adapter.Close()

	agents, err := adapter.GetAllAgentRecords()
	if err != nil {
		t.Fatalf("GetAllAgentRecords: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent (nested python), got %d", len(agents))
	}

	got := agents[0]
	if got.ID != "scout_agent" {
		t.Errorf("expected ID=%q, got %q", "scout_agent", got.ID)
	}
	wantExecPath := filepath.ToSlash(filepath.Join(nestedDir, "scout_agent.py"))
	if got.ExecPath != wantExecPath {
		t.Errorf("expected ExecPath=%q, got %q", wantExecPath, got.ExecPath)
	}
}

// Cycle 7 — Test 10:
// Seed registers BOTH top-level agents/*.py and nested agents/system/*.py in a single scan.
func TestSeed_TopLevelAndNested_BothRegistered(t *testing.T) {
	agentsDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	nestedDir := filepath.Join(agentsDir, "system")
	if err := os.MkdirAll(nestedDir, 0700); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}

	pyContent := func(desc string) string {
		return `
AGENT_DESCRIPTION = "` + desc + `"
AGENT_MANIFEST = '''
{
    "version": "1.0.0",
    "supported_formats": ["text"]
}
'''
`
	}

	files := map[string]string{
		filepath.Join(agentsDir, "browser_agent.py"):                pyContent("Top-level browser agent."),
		filepath.Join(nestedDir, "scout_agent.py"):                  pyContent("System scout."),
		filepath.Join(nestedDir, "reranker_agent.py"):               pyContent("System reranker."),
		filepath.Join(nestedDir, "kg_extractor_agent.py"):           pyContent("System kg extractor."),
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	adapter, err := NewBBoltAdapter(dbPath, agentsDir, nil)
	if err != nil {
		t.Fatalf("NewBBoltAdapter: %v", err)
	}
	defer adapter.Close()

	agents, err := adapter.GetAllAgentRecords()
	if err != nil {
		t.Fatalf("GetAllAgentRecords: %v", err)
	}
	if len(agents) != 4 {
		t.Fatalf("expected 4 agents (1 top-level + 3 nested), got %d", len(agents))
	}

	wantIDs := map[string]string{
		"browser_agent":     filepath.ToSlash(filepath.Join(agentsDir, "browser_agent.py")),
		"scout_agent":       filepath.ToSlash(filepath.Join(nestedDir, "scout_agent.py")),
		"reranker_agent":    filepath.ToSlash(filepath.Join(nestedDir, "reranker_agent.py")),
		"kg_extractor_agent": filepath.ToSlash(filepath.Join(nestedDir, "kg_extractor_agent.py")),
	}

	seen := make(map[string]string, len(agents))
	for _, a := range agents {
		seen[a.ID] = a.ExecPath
	}
	for id, wantExec := range wantIDs {
		gotExec, ok := seen[id]
		if !ok {
			t.Errorf("missing agent %q", id)
			continue
		}
		if gotExec != wantExec {
			t.Errorf("agent %q: ExecPath=%q, want %q", id, gotExec, wantExec)
		}
	}
}

// Cycle 7 — Test 11:
// A sidecar Tool-Agent in a nested subdirectory is registered; exec_path resolves relative to the manifest dir.
func TestSeed_NestedSidecarAgent_RegistersAndResolvesExecPath(t *testing.T) {
	agentsDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	nestedDir := filepath.Join(agentsDir, "system")
	if err := os.MkdirAll(nestedDir, 0700); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}

	manifestJSON := `{
		"trait": "tool",
		"version": "1.0",
		"exec_path": "./sys_tool",
		"description": "Nested system tool.",
		"supported_formats": ["text/plain"]
	}`
	manifestPath := filepath.Join(nestedDir, "sys_tool.manifest.json")
	binaryPath := filepath.Join(nestedDir, "sys_tool")
	if err := os.WriteFile(manifestPath, []byte(manifestJSON), 0600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(binaryPath, []byte("binary"), 0700); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	adapter, err := NewBBoltAdapter(dbPath, agentsDir, nil)
	if err != nil {
		t.Fatalf("NewBBoltAdapter: %v", err)
	}
	defer adapter.Close()

	agents, err := adapter.GetAllAgentRecords()
	if err != nil {
		t.Fatalf("GetAllAgentRecords: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent (nested sidecar), got %d", len(agents))
	}

	got := agents[0]
	if got.ID != "sys_tool" {
		t.Errorf("expected ID=%q, got %q", "sys_tool", got.ID)
	}
	wantExecPath := filepath.ToSlash(binaryPath)
	if got.ExecPath != wantExecPath {
		t.Errorf("expected ExecPath=%q, got %q", wantExecPath, got.ExecPath)
	}
}

// Cycle 8 — Test 12:
// A Python package (__init__.py + agent.py) registers as one agent; bundled subpackages are not double-registered.
func TestSeed_PackageAgent_RegistersFromDirectory(t *testing.T) {
	agentsDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	pkgDir := filepath.Join(agentsDir, "scanner_agent")
	if err := os.MkdirAll(pkgDir, 0700); err != nil {
		t.Fatalf("mkdir pkg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "__init__.py"), nil, 0600); err != nil {
		t.Fatalf("write __init__: %v", err)
	}
	pyContent := `
AGENT_DESCRIPTION = "Package-form agent."
AGENT_MANIFEST = '''
{
    "version": "1.0.0",
    "supported_formats": ["text"]
}
'''
`
	if err := os.WriteFile(filepath.Join(pkgDir, "agent.py"), []byte(pyContent), 0600); err != nil {
		t.Fatalf("write agent.py: %v", err)
	}

	// Bundled subpackage (the kg_extractor_agent/kg_extractors/ equivalent) must NOT register as a separate agent.
	innerDir := filepath.Join(pkgDir, "kg_extractors")
	if err := os.MkdirAll(innerDir, 0700); err != nil {
		t.Fatalf("mkdir inner: %v", err)
	}
	if err := os.WriteFile(filepath.Join(innerDir, "__init__.py"), nil, 0600); err != nil {
		t.Fatalf("write inner __init__: %v", err)
	}
	if err := os.WriteFile(filepath.Join(innerDir, "common.py"), []byte("# helper"), 0600); err != nil {
		t.Fatalf("write inner common: %v", err)
	}

	adapter, err := NewBBoltAdapter(dbPath, agentsDir, nil)
	if err != nil {
		t.Fatalf("NewBBoltAdapter: %v", err)
	}
	defer adapter.Close()

	agents, err := adapter.GetAllAgentRecords()
	if err != nil {
		t.Fatalf("GetAllAgentRecords: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent (package), got %d", len(agents))
	}

	got := agents[0]
	if got.ID != "scanner_agent" {
		t.Errorf("expected ID=%q, got %q", "scanner_agent", got.ID)
	}
	wantExecPath := filepath.ToSlash(filepath.Join(pkgDir, "agent.py"))
	if got.ExecPath != wantExecPath {
		t.Errorf("expected ExecPath=%q, got %q", wantExecPath, got.ExecPath)
	}
}

// Cycle 8 — Test 13:
// AgentRecord.System is stamped from the isSystemAgent predicate the seeder is given.
func TestSeed_SystemField_PropagatedFromPredicate(t *testing.T) {
	agentsDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	mkPkg := func(name, desc string) {
		pkgDir := filepath.Join(agentsDir, name)
		if err := os.MkdirAll(pkgDir, 0700); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(pkgDir, "__init__.py"), nil, 0600); err != nil {
			t.Fatalf("write %s/__init__: %v", name, err)
		}
		pyContent := fmt.Sprintf(`
AGENT_DESCRIPTION = %q
AGENT_MANIFEST = '''
{
    "version": "1.0.0",
    "supported_formats": ["text"]
}
'''
`, desc)
		if err := os.WriteFile(filepath.Join(pkgDir, "agent.py"), []byte(pyContent), 0600); err != nil {
			t.Fatalf("write %s/agent.py: %v", name, err)
		}
	}
	mkPkg("scout_agent", "System organ.")
	mkPkg("user_helper", "User agent.")

	isSystem := map[string]bool{"scout_agent": true}
	adapter, err := NewBBoltAdapter(dbPath, agentsDir, func(id string) bool { return isSystem[id] })
	if err != nil {
		t.Fatalf("NewBBoltAdapter: %v", err)
	}
	defer adapter.Close()

	agents, err := adapter.GetAllAgentRecords()
	if err != nil {
		t.Fatalf("GetAllAgentRecords: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(agents))
	}

	byID := map[string]AgentRecord{}
	for _, a := range agents {
		byID[a.ID] = a
	}
	if !byID["scout_agent"].System {
		t.Errorf("scout_agent.System: want true, got false")
	}
	if byID["user_helper"].System {
		t.Errorf("user_helper.System: want false, got true")
	}
}

// Cycle 8 — Test 14:
// A nil isSystemAgent predicate marks no agent as a system organ.
func TestSeed_NilSystemPredicate_NoAgentMarkedSystem(t *testing.T) {
	agentsDir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	pyContent := `
AGENT_DESCRIPTION = "Even if my id matches a known system organ, nil predicate means no."
AGENT_MANIFEST = '''
{
    "version": "1.0.0",
    "supported_formats": ["text"]
}
'''
`
	if err := os.WriteFile(filepath.Join(agentsDir, "scout_agent.py"), []byte(pyContent), 0600); err != nil {
		t.Fatalf("write scout_agent.py: %v", err)
	}

	adapter, err := NewBBoltAdapter(dbPath, agentsDir, nil)
	if err != nil {
		t.Fatalf("NewBBoltAdapter: %v", err)
	}
	defer adapter.Close()

	agents, err := adapter.GetAllAgentRecords()
	if err != nil {
		t.Fatalf("GetAllAgentRecords: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0].System {
		t.Errorf("scout_agent.System with nil predicate: want false, got true")
	}
}
