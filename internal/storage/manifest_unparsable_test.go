package storage

import (
	"path/filepath"
	"sort"
	"testing"
)

// manifestShape writes one agent of a given manifest shape under dir; good
// selects a well-formed manifest (trust_tier third-party), else a malformed one
// (a trailing comma: the one-typo case ENG-2094 is about).
type manifestShape struct {
	id    string
	write func(t *testing.T, dir string, good bool)
}

func manifestJSON(good bool) string {
	if good {
		return `{"version": "1.0.0", "trust_tier": "third-party"}`
	}
	return `{"version": "1.0.0", "trust_tier": "third-party",}`
}

var manifestShapes = []manifestShape{
	{"emb_agent", func(t *testing.T, dir string, good bool) {
		writeAgentFile(t, dir, "emb_agent.py", "AGENT_DESCRIPTION = \"probe\"\n\nAGENT_MANIFEST = '''\n"+manifestJSON(good)+"\n'''\n")
	}},
	{"pkg_agent", func(t *testing.T, dir string, good bool) {
		writeAgentFile(t, dir, "pkg_agent/__init__.py", "")
		writeAgentFile(t, dir, "pkg_agent/agent.py", "AGENT_MANIFEST = '''\n"+manifestJSON(good)+"\n'''\n")
	}},
	{"sib_agent", func(t *testing.T, dir string, good bool) {
		writeAgentFile(t, dir, "sib_agent.py", "AGENT_DESCRIPTION = \"probe\"\n")
		writeAgentFile(t, dir, "sib_agent.manifest.json", manifestJSON(good))
	}},
	{"js_agent", func(t *testing.T, dir string, good bool) {
		writeAgentFile(t, dir, "js_agent.ts", "export const AGENT_MANIFEST = `"+manifestJSON(good)+"`;\n")
	}},
	{"jssib_agent", func(t *testing.T, dir string, good bool) {
		writeAgentFile(t, dir, "jssib_agent.js", "export const x = 1;\n")
		writeAgentFile(t, dir, "jssib_agent.manifest.json", manifestJSON(good))
	}},
	{"side", func(t *testing.T, dir string, good bool) {
		writeAgentFile(t, dir, "bin/side.bin", "binary")
		m := `{"trait":"cognitive","exec_path":"bin/side.bin","trust_tier":"third-party"}`
		if !good {
			m = `{"trait":"cognitive","exec_path":"bin/side.bin","trust_tier":"third-party"`
		}
		writeAgentFile(t, dir, "side.manifest.json", m)
	}},
}

// ENG-2094 (owner decision 2026-11-18: refuse it): every manifest shape
// discovery reads — embedded Python (file and package), sibling JSON for a
// Python or JS agent, embedded JS, standalone sidecar — refuses its agent when
// the manifest is present but does not parse, and reports it through
// OnRefused. An agent with no manifest still registers with defaults.
func TestDiscover_UnparsableManifestRefusesTheAgent(t *testing.T) {
	for _, s := range manifestShapes {
		t.Run(s.id, func(t *testing.T) {
			dir := t.TempDir()
			s.write(t, dir, false)
			writeAgentFile(t, dir, "plain_agent.py", "AGENT_DESCRIPTION = \"no manifest\"\n")
			writeAgentFile(t, dir, "plainjs_agent.ts", "export const x = 1;\n")
			var refused []string
			got, err := DiscoverFilesystemAgentsWith(dir, nil, DiscoverOptions{OnRefused: func(id string) { refused = append(refused, id) }})
			if err != nil {
				t.Fatal(err)
			}
			ids := map[string]AgentRecord{}
			for _, da := range got {
				ids[da.Agent.ID] = da.Agent
			}
			if _, ok := ids[s.id]; ok {
				t.Fatalf("%s registered despite an unparsable manifest: %+v", s.id, ids[s.id])
			}
			if len(refused) != 1 || refused[0] != s.id {
				t.Fatalf("refused = %v, want [%s]", refused, s.id)
			}
			for _, plain := range []string{"plain_agent", "plainjs_agent"} {
				rec, ok := ids[plain]
				if !ok {
					t.Fatalf("%s (no manifest) not registered: %v", plain, ids)
				}
				if rec.TrustTier != "" {
					t.Errorf("%s: trust tier %q, want the default \"\"", plain, rec.TrustTier)
				}
			}
		})
	}
}

// The same shapes well-formed still register at their declared tier: the
// refusal is about the parse, not the shape.
func TestDiscover_WellFormedManifestShapesRegister(t *testing.T) {
	dir := t.TempDir()
	var want []string
	for _, s := range manifestShapes {
		s.write(t, dir, true)
		want = append(want, s.id)
	}
	var refused []string
	got, err := DiscoverFilesystemAgentsWith(dir, nil, DiscoverOptions{OnRefused: func(id string) { refused = append(refused, id) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(refused) != 0 {
		t.Fatalf("refused %v, want none", refused)
	}
	var ids []string
	for _, da := range got {
		ids = append(ids, da.Agent.ID)
		if da.Agent.TrustTier != "third-party" {
			t.Errorf("%s: tier %q, want third-party", da.Agent.ID, da.Agent.TrustTier)
		}
	}
	sort.Strings(ids)
	sort.Strings(want)
	if len(ids) != len(want) {
		t.Fatalf("discovered %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("discovered %v, want %v", ids, want)
		}
	}
}

// Seed drops the earlier registration of an agent whose manifest stopped
// parsing, for every shape, so it cannot stay launchable at the tier it had.
func TestSeed_UnparsableManifestDropsEarlierRecord(t *testing.T) {
	for _, s := range manifestShapes {
		t.Run(s.id, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(t.TempDir(), "t.db")
			s.write(t, dir, true)
			a, err := NewBBoltAdapter(dbPath, dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			if rec, err := a.GetAgentRecord(s.id); err != nil || rec.TrustTier != "third-party" {
				t.Fatalf("first seed: rec=%+v err=%v", rec, err)
			}
			a.Close()
			s.write(t, dir, false)
			b, err := NewBBoltAdapter(dbPath, dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			if rec, err := b.GetAgentRecord(s.id); err == nil {
				t.Fatalf("agent with an unparsable manifest still registered: %+v", rec)
			}
		})
	}
}
