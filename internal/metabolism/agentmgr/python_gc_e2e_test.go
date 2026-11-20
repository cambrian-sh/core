package agentmgr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism"
	"github.com/cambrian-sh/core/internal/pyenv"
)

func writeGCMarker(t *testing.T, p string, v any) {
	t.Helper()
	raw, _ := json.Marshal(v)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// ADR-0157 D7.10/D7.12 (P5) end to end through the real launch path: a real
// Python agent process (the managed interpreter is this test binary serving
// Execute) runs on its layer L1; the agent's layer switches to L2; GC keeps L1
// while the instance lives; the stale instance is recycled when idle, the
// instance manager drops its generation record and tells the collector, and
// the real collector deletes L1 from the real env root. L2 and the current
// base stay.
func TestE2E_PythonLayerCollectedAfterTheInstanceRecycles(t *testing.T) {
	if testing.Short() {
		t.Skip("launches real subprocesses; skipped in -short")
	}
	root := e2eRoot(t)
	env := filepath.Join(root, "runtime", "python")
	const gen = "0123456789abcdef"
	baseHome := filepath.Join(env, "base", gen)
	writeGCMarker(t, filepath.Join(baseHome, "generation.json"), pyenv.Marker{Kind: "base", ID: gen + strings.Repeat("0", 48), Health: "ok", BuiltAt: time.Now().Add(-time.Hour)})
	writeGCMarker(t, filepath.Join(env, "current.json"), pyenv.Current{Base: gen})
	layer := func(id string) string {
		d := filepath.Join(env, "layers", "analyst", id)
		writeGCMarker(t, filepath.Join(d, "generation.json"), pyenv.LayerMarker{Kind: "layer", ID: id + strings.Repeat("0", 48),
			Agent: "analyst", Base: gen, Health: "ok", BuiltAt: time.Now().Add(-time.Minute)})
		return d
	}
	l1 := layer("1111111111111111")

	exe := probeExe(t, root)
	reg := metabolism.NewInMemoryRegistry()
	reg.SetAgent(domain.AgentDefinition{ID: "analyst", Name: "analyst", Runtime: domain.RuntimePython, ExecPath: "worker_agent.py",
		Dir: filepath.Dir(exe), TrustTier: "first-party"})
	reg.SetManifest("analyst", &domain.AgentManifest{PythonPackages: polarsDecl})
	m := NewAgentManager(reg, "127.0.0.1:1", nil)
	m.SetPythonEnv(fixedPythonEnv{pi: pyenv.Interpreter{Path: exe, Home: baseHome}})
	fl := newFakeLayers()
	fl.layers["analyst"] = pyenv.Layer{Dir: l1, Boot: filepath.Join(l1, "boot")}
	m.SetPythonLayers(fl)
	m.SetEnvExtras([]string{"AGENTMGR_CHAOS_HELPER=execprobe"})
	m.SetAgentBootTimeout(60 * time.Second)
	t.Cleanup(func() { m.EvictAgent("analyst") })

	b := &pyenv.Builder{Root: env, Platform: "windows-x86_64"}
	current := func() string { l, _ := fl.Current("analyst", pyenv.Interpreter{Home: baseHome}); return l.Dir }
	gc, err := pyenv.NewCollector(b, pyenv.GCPolicy{KeepPrevious: 1}, func(context.Context) pyenv.KeepSet {
		// Like pypackages.Service.Layers: an agent not checked since boot is absent.
		layers := map[string]string{}
		if d := current(); d != "" {
			layers["analyst"] = d
		}
		return pyenv.KeepSet{InUse: m.PythonGenerationsInUse(), Layers: layers, KnownAgents: map[string]bool{"analyst": true}}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.SetPythonGenerationRelease(gc.Trigger)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	gc.Start(ctx)
	t.Cleanup(gc.Stop)

	def, _ := reg.GetAgentByName(ctx, "analyst")
	first, err := m.GetOrBootInstance(ctx, def, "")
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if _, err := m.CallAgent(ctx, "analyst", &domain.Handoff{}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	// The agent's layer switches to L2 (a removal or a profile edit built it).
	l2 := layer("2222222222222222")
	fl.mu.Lock()
	fl.layers["analyst"] = pyenv.Layer{Dir: l2, Boot: filepath.Join(l2, "boot")}
	fl.current["analyst"] = pyenv.Layer{Dir: l2, Boot: filepath.Join(l2, "boot"), Base: baseHome}
	fl.mu.Unlock()
	// GC while the instance still runs on L1: L1 stays.
	gc.Run(ctx)
	if _, err := os.Stat(l1); err != nil {
		t.Fatal("a layer a live instance runs on was collected")
	}
	// The switch marks the instance stale; idle, it is recycled at once and its
	// release triggers the collector.
	m.MarkAgentStale("analyst")
	if _, ok := m.GetInstanceByID(first.ID); ok {
		t.Fatal("the idle stale instance must be recycled")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(l1); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("L1 not collected after the recycle: last GC %+v", gc.Last())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(l2); err != nil {
		t.Fatal("the current layer was collected")
	}
	if _, err := os.Stat(baseHome); err != nil {
		t.Fatal("the current base was collected")
	}
	// The next turn boots on L2 and runs.
	next, err := m.GetOrBootInstance(ctx, def, "")
	if err != nil || next.ID == first.ID {
		t.Fatalf("next instance: %v", err)
	}
	inUse := strings.Join(m.PythonGenerationsInUse(), "|")
	if !strings.Contains(inUse, filepath.Clean(l2)) || strings.Contains(inUse, filepath.Clean(l1)) {
		t.Fatalf("in use after the recycle: %s", inUse)
	}
}
