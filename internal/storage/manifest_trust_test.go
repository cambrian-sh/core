package storage

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// ENG-2085 (owner decision 2026-11-18, option a): a manifest trust_tier may
// lower privilege, never raise it; a value the kernel will not apply as written
// refuses the agent.
func TestManifestTrustTier(t *testing.T) {
	cases := []struct {
		declared string
		system   bool
		want     string
		ok       bool
	}{
		{"", false, "", true},
		{"first-party", false, "first-party", true},
		{"third-party", false, "third-party", true},
		{"system", false, "", false}, // a manifest cannot grant system status
		{"vetted", false, "", false}, // not a tier an agent manifest may name
		{"Third-Party", false, "", false},
		{"", true, "", true},
		{"system", true, "system", true},
		{"third-party", true, "", false}, // a system organ runs as system regardless
		{"first-party", true, "", false},
		{"bogus", true, "", false},
	}
	for _, c := range cases {
		got, ok := manifestTrustTier("a", "a.py", c.declared, c.system)
		if got != c.want || ok != c.ok {
			t.Errorf("manifestTrustTier(%q, system=%v) = (%q, %v), want (%q, %v)", c.declared, c.system, got, ok, c.want, c.ok)
		}
	}
}

func writeAgentFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func pyAgentWithTier(tier string) string {
	return "AGENT_DESCRIPTION = \"probe\"\n\nAGENT_MANIFEST = '''\n{\"version\": \"1.0.0\", \"trust_tier\": \"" + tier + "\"}\n'''\n"
}

// Discovery carries an honoured manifest tier onto the AgentRecord for every
// local agent shape, and reports (OnRefused) every agent it refuses.
func TestDiscover_ManifestTrustTier(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, dir, "third_agent.py", pyAgentWithTier("third-party"))
	writeAgentFile(t, dir, "first_agent.py", pyAgentWithTier("first-party"))
	writeAgentFile(t, dir, "plain_agent.py", "AGENT_DESCRIPTION = \"plain\"\n")
	writeAgentFile(t, dir, "raise_agent.py", pyAgentWithTier("system"))
	writeAgentFile(t, dir, "odd_agent.py", pyAgentWithTier("vetted"))
	writeAgentFile(t, dir, "organ_agent.py", pyAgentWithTier("system"))
	writeAgentFile(t, dir, "lowered_organ_agent.py", pyAgentWithTier("third-party"))
	// Sibling manifest wins over the embedded one.
	writeAgentFile(t, dir, "sib_agent.py", pyAgentWithTier("first-party"))
	writeAgentFile(t, dir, "sib_agent.manifest.json", `{"version":"1","trust_tier":"third-party"}`)
	// JS agent with an embedded manifest.
	writeAgentFile(t, dir, "js_agent.ts", "export const AGENT_MANIFEST = `{\"version\":\"1\",\"trust_tier\":\"third-party\"}`;\n")
	// Standalone sidecar.
	writeAgentFile(t, dir, "bin/tool.bin", "binary")
	writeAgentFile(t, dir, "side.manifest.json", `{"trait":"cognitive","exec_path":"bin/tool.bin","trust_tier":"third-party"}`)
	writeAgentFile(t, dir, "sideraise.manifest.json", `{"trait":"cognitive","exec_path":"bin/tool.bin","trust_tier":"system"}`)

	system := func(id string) bool { return id == "organ_agent" || id == "lowered_organ_agent" }
	var refused []string
	got, err := DiscoverFilesystemAgentsWith(dir, system, DiscoverOptions{OnRefused: func(id string) { refused = append(refused, id) }})
	if err != nil {
		t.Fatal(err)
	}
	tiers := map[string]string{}
	for _, da := range got {
		tiers[da.Agent.ID] = da.Agent.TrustTier
	}
	want := map[string]string{
		"third_agent": "third-party", "first_agent": "first-party", "plain_agent": "",
		"organ_agent": "system", "sib_agent": "third-party", "js_agent": "third-party", "side": "third-party",
	}
	if len(tiers) != len(want) {
		t.Errorf("discovered %v, want exactly %v", tiers, want)
	}
	for id, w := range want {
		if g, ok := tiers[id]; !ok || g != w {
			t.Errorf("%s: tier %q (discovered=%v), want %q", id, g, ok, w)
		}
	}
	sort.Strings(refused)
	wantRefused := []string{"lowered_organ_agent", "odd_agent", "raise_agent", "sideraise"}
	if len(refused) != len(wantRefused) {
		t.Fatalf("refused %v, want %v", refused, wantRefused)
	}
	for i := range wantRefused {
		if refused[i] != wantRefused[i] {
			t.Fatalf("refused %v, want %v", refused, wantRefused)
		}
	}
}

// A record seeded before discovery read trust_tier keeps its SourceHash, so the
// tier must be reconciled before the unchanged-source short-circuit.
func TestUpsertDiscovered_ReconcilesTrustTierOnUnchangedHash(t *testing.T) {
	store, err := NewBBoltAdapterNoScan(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertDiscoveredAgent(DiscoveredAgent{Agent: AgentRecord{ID: "a1", SourceHash: "h1", Provisional: true}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetProvisional("a1", false); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertDiscoveredAgent(DiscoveredAgent{Agent: AgentRecord{ID: "a1", SourceHash: "h1", TrustTier: "third-party"}}); err != nil {
		t.Fatal(err)
	}
	rec, err := store.GetAgentRecord("a1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.TrustTier != "third-party" {
		t.Fatalf("trust tier = %q, want third-party (stale tier survived an unchanged-hash upsert)", rec.TrustTier)
	}
	if rec.Provisional {
		t.Fatal("a tier change must not re-provision the agent")
	}
}

// Seed drops the earlier registration of an agent it now refuses.
func TestSeed_DropsRefusedAgentsEarlierRecord(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "t.db")
	writeAgentFile(t, dir, "probe_agent.py", pyAgentWithTier("third-party"))
	a, err := NewBBoltAdapter(dbPath, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec, err := a.GetAgentRecord("probe_agent"); err != nil || rec.TrustTier != "third-party" {
		t.Fatalf("first seed: rec=%+v err=%v", rec, err)
	}
	a.Close()
	writeAgentFile(t, dir, "probe_agent.py", pyAgentWithTier("system"))
	b, err := NewBBoltAdapter(dbPath, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if rec, err := b.GetAgentRecord("probe_agent"); err == nil {
		t.Fatalf("refused agent still registered: %+v", rec)
	}
}
