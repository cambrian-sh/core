package agentmgr

import (
	"context"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/pyenv"
)

// ADR-0157 D7.12 (P5): the instance manager knows which base generation and
// layer each live Python instance runs on — recorded when its command is
// built, dropped (with the GC hook told) when the instance is torn down — so
// GC never deletes what a running process still has on sys.path.
func TestPythonGenerations_RecordedPerInstanceAndReleasedAtTeardown(t *testing.T) {
	root := t.TempDir()
	gen := filepath.Join(root, "base", "gen1")
	layerDir := filepath.Join(root, "layers", "analyst", "abcdef0123456789")
	im := NewInstanceManager("127.0.0.1:1")
	im.SetPythonEnv(fixedPythonEnv{pi: pyenv.Interpreter{Path: filepath.Join(gen, "bin", "python3"), Home: gen}})
	fl := newFakeLayers()
	fl.layers["analyst"] = pyenv.Layer{Dir: layerDir, Boot: filepath.Join(layerDir, "boot")}
	im.SetPythonLayers(fl)
	im.SetManifestResolver(manifests(map[string]*domain.AgentManifest{"analyst": {PythonPackages: polarsDecl}}))
	var released atomic.Int32
	im.SetPythonGenerationRelease(func() { released.Add(1) })

	def := &domain.AgentDefinition{ID: "analyst", Runtime: domain.RuntimePython, ExecPath: "worker_agent.py"}
	if err := im.ensurePythonLayer(context.Background(), def); err != nil {
		t.Fatal(err)
	}
	inst := domain.NewInstance("analyst")
	if _, err := im.buildAgentCmd(def, inst, ""); err != nil {
		t.Fatal(err)
	}
	got := im.PythonGenerationsInUse()
	sort.Strings(got)
	want := []string{filepath.Clean(gen), filepath.Clean(layerDir)}
	sort.Strings(want)
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("in use = %v, want %v", got, want)
	}
	// Registered like a booted instance, then torn down through the one path.
	im.mu.Lock()
	im.instances[inst.ID] = inst
	im.agentIndex["analyst"] = []string{inst.ID}
	im.mu.Unlock()
	im.EvictInstance(inst.ID)
	if n := len(im.PythonGenerationsInUse()); n != 0 || released.Load() != 1 {
		t.Fatalf("after teardown: in use %d, release hook calls %d", n, released.Load())
	}
}

// A boot that fails after the command was built drops the record again.
func TestPythonGenerations_FailedBootDropsTheRecord(t *testing.T) {
	root := t.TempDir()
	gen := filepath.Join(root, "base", "gen1")
	im := NewInstanceManager("127.0.0.1:1")
	// An interpreter that does not exist: cmd.Start fails after buildAgentCmd.
	im.SetPythonEnv(fixedPythonEnv{pi: pyenv.Interpreter{Path: filepath.Join(gen, "bin", "no-such-python"), Home: gen}})
	var released atomic.Int32
	im.SetPythonGenerationRelease(func() { released.Add(1) })
	_, err := im.bootAgentInstance(context.Background(), &domain.AgentDefinition{ID: "a", Runtime: domain.RuntimePython, ExecPath: "x.py"}, false)
	if err == nil {
		t.Fatal("boot with a missing interpreter must fail")
	}
	if n := len(im.PythonGenerationsInUse()); n != 0 || released.Load() != 1 {
		t.Fatalf("failed boot: in use %d, release calls %d", n, released.Load())
	}
}
