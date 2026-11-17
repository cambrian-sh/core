package agentmgr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/mapper"
	"github.com/cambrian-sh/core/internal/storage"
)

// ADR-0157 D1.2 for declarative profiles (ENG-1960), end to end through the
// REAL chain a profile agent's declared paths travel: the profile file →
// storage.DiscoverFilesystemAgentsWith (agentprofile validation) → the bbolt
// manifest (UpsertDiscoveredAgent) → the registry read the kernel's manifest
// resolver makes (GetManifestRecord + mapper.ManifestToDomain, exactly
// kernel.AgentRepoDecorator.GetManifest, which this package cannot import) →
// InstanceManager.GetOrBootInstance → agentBinds → admitDeclared against
// execution.agents.sandbox.allowlist_cap. No second check exists for profiles:
// they are admitted exactly like an AGENT_MANIFEST's sandbox_paths.
//
// The launched process is the probe agent (TestMain) in place of
// worker_agent.py, which needs a live kernel; the definition, manifest and
// launch arguments are the discovered profile agent's.

const profileE2EText = `name: scout
description: Scouts model caches.
role: "You scout."
capabilities:
  - analysis
skills:
  - s1
tools:
  - mcp:filesystem/read_*
trust_tier: first-party
`

// profileAgentFromDisk writes agents/profiles/scout.yaml declaring decl, runs
// discovery, stores the result in a fresh bbolt store and returns the profile
// agent's definition and a manifest resolver reading the store.
func profileAgentFromDisk(t *testing.T, root string, decl []domain.SandboxPath) (*domain.AgentDefinition, func(context.Context, string) *domain.AgentManifest) {
	t.Helper()
	agents := filepath.Join(root, "agents")
	if err := os.MkdirAll(filepath.Join(agents, storage.ProfilesDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agents, storage.ProfileRuntimeFile), []byte("# runtime\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	text := profileE2EText
	if len(decl) > 0 {
		b, err := json.Marshal(decl) // one-line JSON, Windows backslashes escaped
		if err != nil {
			t.Fatal(err)
		}
		text += "sandbox_paths: " + string(b) + "\n"
	}
	if err := os.WriteFile(filepath.Join(agents, storage.ProfilesDirName, "scout.yaml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	found, err := storage.DiscoverFilesystemAgentsWith(agents, nil, storage.DiscoverOptions{})
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewBBoltAdapterNoScan(filepath.Join(root, "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, da := range found {
		if err := store.UpsertDiscoveredAgent(da); err != nil {
			t.Fatal(err)
		}
	}
	rec, err := store.GetAgentRecord("scout")
	if err != nil || rec == nil {
		t.Fatalf("the profile agent was not registered: %v (discovered %d)", err, len(found))
	}
	am := mapper.NewAgentMapper()
	def := am.ToDomain(*rec)
	resolve := func(_ context.Context, id string) *domain.AgentManifest {
		mr, err := store.GetManifestRecord(id)
		if err != nil || mr == nil {
			t.Errorf("manifest read for %s: %v", id, err)
			return nil
		}
		m := am.ManifestToDomain(*mr)
		return &m
	}
	return &def, resolve
}

func TestE2E_ProfileAgentDeclaredPathInsideTheCapBoots(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a real subprocess; skipped in -short")
	}
	root := e2eRoot(t)
	cache := filepath.Join(root, "caches", "model")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	ro := filepath.Join(root, "caches", "weights")
	if err := os.MkdirAll(ro, 0o700); err != nil {
		t.Fatal(err)
	}
	def, resolve := profileAgentFromDisk(t, root, []domain.SandboxPath{{Path: cache, Write: true}, {Path: ro}})
	if def.Profile != "scout" || def.TrustTier != "first-party" {
		t.Fatalf("not the discovered profile agent: %+v", def)
	}
	if m := resolve(context.Background(), "scout"); m == nil || len(m.SandboxPaths) != 2 || m.SandboxPaths[0] != (domain.SandboxPath{Path: cache, Write: true}) {
		t.Fatalf("the stored manifest lost the profile's sandbox_paths: %+v", m)
	}

	workRoot := filepath.Join(root, "w")
	sb := NewAgentSandbox(AgentSandboxOptions{
		WorkRoot: workRoot, KernelDir: filepath.Join(root, "kernel"),
		KernelStatePaths: []string{filepath.Join(root, "data")},
		AllowlistCap:     []string{filepath.Join(root, "caches")},
	})
	im := NewInstanceManager("", "127.0.0.1:1")
	im.SetAgentContainment(AgentContainment{Sandbox: sb})
	im.SetManifestResolver(resolve)
	im.SetEnvExtras([]string{"AGENTMGR_CHAOS_HELPER=probe",
		"AGENTMGR_PROBE_WRITE=" + strings.Join([]string{cache, ro}, string(os.PathListSeparator))})
	// The probe stands in for worker_agent.py; everything else is the profile agent's.
	exe, _ := os.Executable()
	def.Runtime, def.ExecPath, def.Dir = domain.RuntimeBinary, exe, filepath.Dir(exe)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	inst, err := im.GetOrBootInstance(ctx, def, "")
	if err != nil {
		t.Fatalf("a profile agent declaring a path inside allowlist_cap must boot: %v", err)
	}
	if err := waitHealthy(ctx, inst.SocketPath, 10*time.Second); err != nil {
		t.Fatalf("the kernel cannot reach the confined profile agent: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(inst.SocketPath), "probe.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rep probeReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	t.Logf("probe: %s", raw)
	if rep.Write[cache] != "ok" {
		t.Errorf("the profile's writable cache is not writable: %v", rep.Write)
	}
	// Read-only is enforced where the runner confines writes to declared paths
	// (bubblewrap binds; Windows low integrity denies writes outside the scratch).
	if (runtime.GOOS == "windows" || bwrapHere()) && rep.Write[ro] != "denied" {
		t.Errorf("the profile's read-only path was writable: %v", rep.Write)
	}
	im.mu.Lock()
	pe := im.exits[inst.ID]
	im.mu.Unlock()
	im.EvictInstance(inst.ID)
	select {
	case <-pe.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the evicted agent did not exit")
	}
}

// A profile's declared path is refused at boot, named, before anything is
// spawned: outside allowlist_cap, and inside the kernel's own state even when
// the cap would admit it.
func TestE2E_ProfileAgentDeclaredPathRefusedAtBoot(t *testing.T) {
	for _, tc := range []struct {
		name, decl, want string
	}{
		{"outside-cap", filepath.Join("home", ".ssh"), "allowlist_cap"},
		{"kernel-state", filepath.Join("data", "vault"), "kernel's own state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := e2eRoot(t)
			bad := filepath.Join(root, tc.decl)
			def, resolve := profileAgentFromDisk(t, root, []domain.SandboxPath{{Path: bad}})
			capRoot := filepath.Join(root, "caches")
			if tc.name == "kernel-state" {
				capRoot = root // the cap admits it; the kernel's state still wins
			}
			sb := NewAgentSandbox(AgentSandboxOptions{WorkRoot: filepath.Join(root, "w"),
				KernelStatePaths: []string{filepath.Join(root, "data")}, AllowlistCap: []string{capRoot}})
			im := NewInstanceManager("", "127.0.0.1:1")
			im.SetAgentContainment(AgentContainment{Sandbox: sb})
			im.SetManifestResolver(resolve)
			exe, _ := os.Executable()
			def.Runtime, def.ExecPath, def.Dir = domain.RuntimeBinary, exe, filepath.Dir(exe)
			_, err := im.GetOrBootInstance(context.Background(), def, "")
			if !errors.Is(err, ErrSandboxPathRefused) || !strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a refusal naming %s (%s), got %v", bad, tc.want, err)
			}
			assertNothingRegistered(t, im, def)
			if m, _ := filepath.Glob(filepath.Join(root, "w", "first-party", "*")); len(m) != 0 {
				t.Fatalf("a refused boot left a scratch dir: %v", m)
			}
		})
	}
}
