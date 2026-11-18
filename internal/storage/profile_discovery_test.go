package storage

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

const testProfile = `name: %s
description: Does %s things.
role: "You do things."
capabilities:
  - analysis
skills:
  - s1
tools:
  - mcp:filesystem/read_*
trust_tier: first-party
model: role:planner
`

func writeProfileTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, ProfileRuntimeFile), []byte("# runtime\nAGENT_DESCRIPTION = \"x\"\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "chat_agent.py"), []byte("AGENT_DESCRIPTION = \"chat\"\n"), 0o644))
	must(os.MkdirAll(filepath.Join(dir, ProfilesDirName), 0o755))
	for _, n := range []string{"alpha", "beta"} {
		must(os.WriteFile(filepath.Join(dir, ProfilesDirName, n+".yaml"),
			[]byte(strings.ReplaceAll(strings.ReplaceAll(testProfile, "%s", n), "Does "+n, "Does "+n)), 0o644))
	}
	// Invalid: unknown field. Must be skipped with an error, never fatal.
	must(os.WriteFile(filepath.Join(dir, ProfilesDirName, "broken.yaml"),
		[]byte(strings.ReplaceAll(testProfile, "%s", "broken")+"bogus: 1\n"), 0o644))
	// Collides with a file agent's id.
	must(os.WriteFile(filepath.Join(dir, ProfilesDirName, "chat_agent.yaml"),
		[]byte(strings.ReplaceAll(testProfile, "%s", "chat_agent")), 0o644))
	return dir
}

func TestDiscover_OneAgentPerProfile(t *testing.T) {
	dir := writeProfileTree(t)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	got, err := DiscoverFilesystemAgentsWith(dir, nil, DiscoverOptions{SkillTools: map[string][]string{"s1": {"mcp:filesystem/read_file"}}})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]DiscoveredAgent{}
	for _, da := range got {
		byID[da.Agent.ID] = da
	}
	if _, ok := byID["worker_agent"]; ok {
		t.Fatal("the profile runtime itself must not be registered as an agent")
	}
	if _, ok := byID["broken"]; ok {
		t.Fatal("an invalid profile was registered")
	}
	if !strings.Contains(logs.String(), "agent profile INVALID") || !strings.Contains(logs.String(), "broken.yaml") {
		t.Fatalf("an invalid profile must be a loud error log; got:\n%s", logs.String())
	}
	if da := byID["chat_agent"]; da.Agent.Profile != "" {
		t.Fatal("a profile colliding with a file agent's id must not replace it")
	}
	a, ok := byID["alpha"]
	if !ok {
		t.Fatalf("profile alpha not discovered; got %v", got)
	}
	rec := a.Agent
	if rec.ExecPath != ProfileRuntimeFile || rec.Runtime != "python" || rec.Profile != "alpha" {
		t.Errorf("record: %+v", rec)
	}
	if !slices.Equal(rec.LaunchArgs, []string{"--profile", "alpha", "--agent-id", "alpha"}) {
		t.Errorf("launch args: %v", rec.LaunchArgs)
	}
	if rec.TrustTier != "first-party" || rec.ModelPreference != "role:planner" || rec.Description != "Does alpha things." {
		t.Errorf("profile fields: %+v", rec)
	}
	m := a.Manifest
	if !slices.Equal(m.Capabilities, []string{"analysis"}) || len(m.Tools) != 0 ||
		!slices.Equal(m.ToolAllowlist, []string{"mcp:filesystem/read_*"}) || !slices.Equal(m.Skills, []string{"s1"}) {
		t.Errorf("manifest: %+v", m)
	}
	if !IsProfileRuntime(rec) {
		t.Error("IsProfileRuntime(profile record) = false")
	}

	// The source hash covers the profile file: editing it changes the hash.
	before := byID["beta"].Agent.SourceHash
	if before == rec.SourceHash {
		t.Error("two profiles share a source hash")
	}
	p := filepath.Join(dir, ProfilesDirName, "beta.yaml")
	b, _ := os.ReadFile(p)
	if err := os.WriteFile(p, append(b, []byte("max_tokens: 2048\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	again, _ := DiscoverFilesystemAgentsWith(dir, nil, DiscoverOptions{})
	for _, da := range again {
		if da.Agent.ID == "beta" && da.Agent.SourceHash == before {
			t.Error("editing a profile did not change its source hash")
		}
	}
}

func TestDiscover_ProfileSkillChecks(t *testing.T) {
	dir := writeProfileTree(t)
	// s1 grants a tool outside the profiles' allowlist ⇒ every profile is invalid.
	got, _ := DiscoverFilesystemAgentsWith(dir, nil, DiscoverOptions{SkillTools: map[string][]string{"s1": {"mcp:shell/run"}}})
	for _, da := range got {
		if da.Agent.Profile != "" {
			t.Fatalf("profile %s registered despite a skill granting a tool outside its allowlist", da.Agent.ID)
		}
	}
	// Without a skill index the skill checks are skipped (the SDK's skills_dir=None).
	got, _ = DiscoverFilesystemAgents(dir, nil)
	n := 0
	for _, da := range got {
		if da.Agent.Profile != "" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("want alpha and beta without the skill checks, got %d profile agents", n)
	}
}

func TestDiscover_ShippedProfiles(t *testing.T) {
	agentsDir, _ := filepath.Abs(filepath.Join("..", "..", "agents"))
	got, err := DiscoverFilesystemAgents(agentsDir, domain.IsSystemAgent)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"assistant": false, "researcher": false, "data-analyst": false}
	for _, da := range got {
		if da.Agent.ID == "worker_agent" {
			t.Error("worker_agent.py registered as an agent")
		}
		if _, ok := want[da.Agent.ID]; ok {
			want[da.Agent.ID] = true
			if da.Agent.System {
				t.Errorf("%s: a starter profile must not be a system agent", da.Agent.ID)
			}
			if len(da.Manifest.Capabilities) == 0 || len(da.Manifest.ToolAllowlist) == 0 {
				t.Errorf("%s: manifest missing capabilities or allowlist: %+v", da.Agent.ID, da.Manifest)
			}
		}
	}
	for id, seen := range want {
		if !seen {
			t.Errorf("shipped profile %s not discovered", id)
		}
	}
}

func TestUpsert_RefreshesProfileFieldsOnSourceChange(t *testing.T) {
	a, err := NewBBoltAdapterNoScan(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	rec := AgentRecord{ID: "p", ExecPath: ProfileRuntimeFile, SourceHash: "h1", Profile: "p",
		LaunchArgs: []string{"--profile", "p"}, TrustTier: "first-party"}
	if err := a.UpsertDiscoveredAgent(DiscoveredAgent{Agent: rec}); err != nil {
		t.Fatal(err)
	}
	rec.SourceHash, rec.TrustTier, rec.ModelPreference = "h2", "third-party", "mimo"
	if err := a.UpsertDiscoveredAgent(DiscoveredAgent{Agent: rec}); err != nil {
		t.Fatal(err)
	}
	got, err := a.GetAgentRecord("p")
	if err != nil {
		t.Fatal(err)
	}
	if got.TrustTier != "third-party" || got.ModelPreference != "mimo" || !got.Provisional {
		t.Fatalf("profile fields not refreshed on source change: %+v", got)
	}
}

// ADR-0157 D1.2 (ENG-1960): a profile's sandbox_paths reach its manifest and
// survive the bbolt round trip the boot reads them from; a profile that declares
// none carries none, and an invalid declaration skips the profile.
func TestDiscover_ProfileSandboxPathsReachTheManifest(t *testing.T) {
	dir := writeProfileTree(t)
	p := filepath.Join(dir, ProfilesDirName, "alpha.yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	decl := `sandbox_paths: [{"path": "/srv/models"}, {"path": "~/cache/hf", "write": true}]` + "\n"
	if err := os.WriteFile(p, append(b, []byte(decl)...), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := strings.ReplaceAll(testProfile, "%s", "gamma") + `sandbox_paths: [{"path": "models"}]` + "\n"
	if err := os.WriteFile(filepath.Join(dir, ProfilesDirName, "gamma.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverFilesystemAgentsWith(dir, nil, DiscoverOptions{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]DiscoveredAgent{}
	for _, da := range got {
		byID[da.Agent.ID] = da
	}
	want := []SandboxPathRecord{{Path: "/srv/models"}, {Path: "~/cache/hf", Write: true}}
	if !slices.Equal(byID["alpha"].Manifest.SandboxPaths, want) {
		t.Fatalf("alpha manifest sandbox_paths = %+v, want %+v", byID["alpha"].Manifest.SandboxPaths, want)
	}
	if byID["beta"].Manifest.SandboxPaths != nil {
		t.Errorf("a profile without sandbox_paths must carry none: %+v", byID["beta"].Manifest.SandboxPaths)
	}
	if _, ok := byID["gamma"]; ok {
		t.Error("a profile with a relative sandbox path was registered")
	}

	a, err := NewBBoltAdapterNoScan(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.UpsertDiscoveredAgent(byID["alpha"]); err != nil {
		t.Fatal(err)
	}
	rec, err := a.GetManifestRecord("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.SandboxPaths, want) {
		t.Fatalf("stored manifest sandbox_paths = %+v, want %+v", rec.SandboxPaths, want)
	}
}

// ADR-0157 D7.6 (ENG-2009): a profile's python_packages reach its manifest and
// survive the JSON round trip of the registry record; an invalid declaration
// (a version range) skips the profile.
func TestDiscover_ProfilePythonPackagesReachTheManifest(t *testing.T) {
	dir := writeProfileTree(t)
	p := filepath.Join(dir, ProfilesDirName, "alpha.yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	decl := `python_packages: [{"name": "polars", "version": "1.9.0"}, {"name": "httpx", "version": "0.27.2", "extras": ["http2"]}]` + "\n"
	if err := os.WriteFile(p, append(b, []byte(decl)...), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := strings.ReplaceAll(testProfile, "%s", "gamma") + `python_packages: [{"name": "polars", "version": ">=1"}]` + "\n"
	if err := os.WriteFile(filepath.Join(dir, ProfilesDirName, "gamma.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverFilesystemAgentsWith(dir, nil, DiscoverOptions{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]DiscoveredAgent{}
	for _, da := range got {
		byID[da.Agent.ID] = da
	}
	raw, err := json.Marshal(byID["alpha"].Manifest)
	if err != nil {
		t.Fatal(err)
	}
	var back ManifestRecord
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.PythonPackages) != 2 || back.PythonPackages[0].Name != "polars" || back.PythonPackages[0].Version != "1.9.0" ||
		len(back.PythonPackages[1].Extras) != 1 || back.PythonPackages[1].Extras[0] != "http2" {
		t.Fatalf("alpha manifest python_packages = %+v", back.PythonPackages)
	}
	if byID["beta"].Manifest.PythonPackages != nil {
		t.Errorf("a profile without python_packages must carry none: %+v", byID["beta"].Manifest.PythonPackages)
	}
	if _, ok := byID["gamma"]; ok {
		t.Error("a profile with a version range was registered")
	}
}
