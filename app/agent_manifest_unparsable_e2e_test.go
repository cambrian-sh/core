package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/storage"
)

// ENG-2094 end to end (owner decision 2026-11-18: refuse it): an agent whose
// AGENT_MANIFEST stops parsing is refused at registration (registerAgentSources
// → FilesystemAgentSource → storage discovery → pruneRefusedAgents), its earlier
// registration is dropped, and the kernel's real dispatch path — the metabolism
// stack built from the DEFAULT config (default agent sandbox) — cannot boot it:
// AgentTransport.CallAgent resolves the agent through the registry before
// GetOrBootInstance and finds nothing, and no instance is ever started. Before
// the fix the agent re-registered with an empty manifest, its declared
// third-party tier dropped, i.e. launchable first-party.
func TestE2E_UnparsableManifestAgentNeverBoots(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := storage.NewBBoltAdapterNoScan(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reg := kernel.NewAgentRepoDecorator(store)
	meta := kernel.NewMetabolismStack(reg, nil, nil, nil, nil, config.DefaultConfig(), nil)

	const id = "typo_agent"
	write := func(manifest string) {
		t.Helper()
		src := "AGENT_DESCRIPTION = \"probe\"\n\nAGENT_MANIFEST = '''\n" + manifest + "\n'''\n"
		if err := os.WriteFile(filepath.Join(dir, id+".py"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src := newBuiltinFilesystemAgentSource(dir)

	write(`{"version": "1.0.0", "trust_tier": "third-party"}`)
	registerAgentSources(ctx, reg, []AgentSource{src})
	def, err := meta.Manager.GetAgentByName(ctx, id)
	if err != nil || def.TrustTier != "third-party" {
		t.Fatalf("well-formed manifest: def=%+v err=%v (want registered third-party)", def, err)
	}

	// One typo (a trailing comma): the manifest no longer parses.
	write(`{"version": "1.0.0", "trust_tier": "third-party",}`)
	got := registerAgentSources(ctx, reg, []AgentSource{src})
	for _, da := range got {
		if da.Definition.ID == id {
			t.Fatal("an agent whose manifest does not parse was registered")
		}
	}
	if ids := src.RefusedAgentIDs(); len(ids) != 1 || ids[0] != id {
		t.Fatalf("RefusedAgentIDs = %v, want [%s]", ids, id)
	}
	if rec, err := store.GetAgentRecord(id); err == nil {
		t.Fatalf("the earlier registration survived: %+v", rec)
	}
	if def, err := meta.Manager.GetAgentByName(ctx, id); err == nil {
		t.Fatalf("registry still resolves the refused agent: %+v", def)
	}
	if _, err := meta.AgentTransport.CallAgent(ctx, id, &domain.Handoff{}, ""); err == nil {
		t.Fatal("CallAgent dispatched to an agent whose manifest does not parse")
	}
	if ids := meta.Manager.GetInstanceIDs(id); len(ids) != 0 {
		t.Fatalf("an instance was booted for the refused agent: %v", ids)
	}
}
