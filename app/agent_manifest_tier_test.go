package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/kernel"
	"github.com/cambrian-sh/core/internal/storage"
)

func writeTierAgent(t *testing.T, dir, id, tier string) {
	t.Helper()
	m := `{"version": "1.0.0"}`
	if tier != "" {
		m = `{"version": "1.0.0", "trust_tier": "` + tier + `"}`
	}
	src := "AGENT_DESCRIPTION = \"probe\"\n\nAGENT_MANIFEST = '''\n" + m + "\n'''\n"
	if err := os.WriteFile(filepath.Join(dir, id+".py"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ENG-2085 through the composition root's registration: a manifest
// trust_tier that lowers privilege reaches the registry; one that would raise
// it refuses the agent AND drops the earlier registration, so the agent cannot
// stay launchable at the tier it had.
func TestRegisterAgentSources_ManifestTrustTier(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := storage.NewBBoltAdapterNoScan(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reg := kernel.NewAgentRepoDecorator(store)

	writeTierAgent(t, dir, "third_agent", "third-party")
	writeTierAgent(t, dir, "climber_agent", "")
	src := newBuiltinFilesystemAgentSource(dir)
	registerAgentSources(ctx, reg, []AgentSource{src})
	if rec, err := store.GetAgentRecord("third_agent"); err != nil || rec.TrustTier != "third-party" {
		t.Fatalf("third_agent: rec=%+v err=%v (want trust_tier third-party)", rec, err)
	}
	if _, err := store.GetAgentRecord("climber_agent"); err != nil {
		t.Fatalf("climber_agent first registration: %v", err)
	}

	// The agent now asks for system: refused, and its earlier record dropped.
	writeTierAgent(t, dir, "climber_agent", "system")
	got := registerAgentSources(ctx, reg, []AgentSource{src})
	for _, da := range got {
		if da.Definition.ID == "climber_agent" {
			t.Fatal("a manifest asking for system was registered")
		}
	}
	if rec, err := store.GetAgentRecord("climber_agent"); err == nil {
		t.Fatalf("refused agent's earlier registration survived: %+v", rec)
	}
	if ids := src.RefusedAgentIDs(); len(ids) != 1 || ids[0] != "climber_agent" {
		t.Fatalf("RefusedAgentIDs = %v", ids)
	}
}

// pruneRefusedAgents spares an id another source registered this boot.
func TestPruneRefusedAgents_SparesAnIDRegisteredElsewhere(t *testing.T) {
	var deleted []string
	pruneRefusedAgents(context.Background(), pruneRecorder{&deleted}, []string{"a", "b"},
		[]DiscoveredAgent{{Definition: domain.AgentDefinition{ID: "b"}}})
	if len(deleted) != 1 || deleted[0] != "a" {
		t.Fatalf("deleted %v, want [a]", deleted)
	}
}

type pruneRecorder struct{ ids *[]string }

func (p pruneRecorder) DeleteAgent(_ context.Context, id string) error {
	*p.ids = append(*p.ids, id)
	return nil
}
