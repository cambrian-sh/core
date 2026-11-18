package agentmgr

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/pyenv"
	"github.com/cambrian-sh/core/internal/sandbox"
)

// fakeLayers is a PythonLayers with a per-agent outcome.
type fakeLayers struct {
	mu      sync.Mutex
	layers  map[string]pyenv.Layer
	refuse  map[string]error
	ensured map[string][]domain.PythonPackage
	current map[string]pyenv.Layer
}

func newFakeLayers() *fakeLayers {
	return &fakeLayers{layers: map[string]pyenv.Layer{}, refuse: map[string]error{}, ensured: map[string][]domain.PythonPackage{}, current: map[string]pyenv.Layer{}}
}

func (f *fakeLayers) Ensure(_ context.Context, agent string, decl []domain.PythonPackage, base pyenv.Interpreter) (pyenv.Layer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured[agent] = decl
	if err := f.refuse[agent]; err != nil {
		return pyenv.Layer{}, err
	}
	l := f.layers[agent]
	if len(decl) == 0 {
		l = pyenv.Layer{}
	}
	l.Base = base.Home
	f.current[agent] = l
	return l, nil
}

func (f *fakeLayers) Current(agent string, base pyenv.Interpreter) (pyenv.Layer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := f.current[agent]
	if l.Dir == "" {
		return pyenv.Layer{}, nil
	}
	if l.Base != base.Home {
		return pyenv.Layer{}, errors.New("base changed")
	}
	return l, nil
}

func manifests(m map[string]*domain.AgentManifest) func(context.Context, string) *domain.AgentManifest {
	return func(_ context.Context, id string) *domain.AgentManifest { return m[id] }
}

var polarsDecl = []domain.PythonPackage{{Name: "polars", Version: "1.9.0"}}

// ADR-0157 D7.6.3: an agent whose declared packages are not approved is not
// started — before any process exists — with the reason the layer service
// gave; an agent without a declaration on the same manager is unaffected.
func TestPythonLayers_HeldAgentIsRefusedBeforeSpawn(t *testing.T) {
	root := t.TempDir()
	gen := filepath.Join(root, "base", "gen1")
	im := NewInstanceManager("127.0.0.1:1")
	im.SetPythonEnv(fixedPythonEnv{pi: pyenv.Interpreter{Path: filepath.Join(gen, "bin", "python3"), Home: gen}})
	fl := newFakeLayers()
	fl.refuse["held"] = errors.New("agent held not started: declared python packages not approved: waiting for an Operator to approve polars==1.9.0 (approval apr-1)")
	im.SetPythonLayers(fl)
	im.SetManifestResolver(manifests(map[string]*domain.AgentManifest{"held": {PythonPackages: polarsDecl}}))
	_, err := im.bootAgentInstance(context.Background(), &domain.AgentDefinition{ID: "held", Runtime: domain.RuntimePython}, false)
	if err == nil || !strings.Contains(err.Error(), "apr-1") || !strings.Contains(err.Error(), "polars==1.9.0") {
		t.Fatalf("held agent: %v", err)
	}
	if len(im.cmds) != 0 {
		t.Fatalf("a held agent must not spawn a process: %d", len(im.cmds))
	}
	if err := im.verifyRuntimeDeps(context.Background(), &domain.AgentDefinition{ID: "free", Runtime: domain.RuntimePython}); err != nil {
		t.Fatalf("an agent that declares nothing is unaffected: %v", err)
	}
	if got := fl.ensured["free"]; got != nil {
		t.Errorf("free agent's Ensure got a declaration: %v", got)
	}
}

// Fail closed: declarations with no layer service, or on a non-Python agent,
// refuse the spawn with the reason named.
func TestPythonLayers_FailClosed(t *testing.T) {
	im := NewInstanceManager("127.0.0.1:1")
	im.SetPythonEnv(fixedPythonEnv{pi: pyenv.Interpreter{Path: "/env/base/g/bin/python3", Home: "/env/base/g"}})
	im.SetManifestResolver(manifests(map[string]*domain.AgentManifest{"py": {PythonPackages: polarsDecl}, "js": {PythonPackages: polarsDecl}}))
	err := im.verifyRuntimeDeps(context.Background(), &domain.AgentDefinition{ID: "py", Runtime: domain.RuntimePython})
	if !errors.Is(err, ErrNoPythonLayers) {
		t.Fatalf("no layer service ⇒ refused: %v", err)
	}
	im.SetPythonLayers(newFakeLayers())
	err = im.verifyRuntimeDeps(context.Background(), &domain.AgentDefinition{ID: "js", Runtime: domain.RuntimeBun})
	if err == nil || !strings.Contains(err.Error(), "python_packages") || !strings.Contains(err.Error(), "bun") {
		t.Fatalf("a bun agent that declares python_packages must be refused: %v", err)
	}
}

// The command of an agent with a layer loads it through PYTHONPATH=<layer>/boot
// on the base generation's real interpreter (no venv); an operator's
// passthrough PYTHONPATH never reaches it; an agent without a layer gets none.
func TestPythonLayers_CommandLoadsTheLayer(t *testing.T) {
	t.Setenv("PYTHONPATH", "/somewhere/else")
	root := t.TempDir()
	gen := filepath.Join(root, "base", "gen1")
	interp := filepath.Join(gen, "bin", "python3")
	layerDir := filepath.Join(root, "layers", "analyst", "abcdef0123456789")
	im := NewInstanceManager("127.0.0.1:1")
	im.SetEnvPassthrough([]string{"PYTHONPATH"})
	im.SetPythonEnv(fixedPythonEnv{pi: pyenv.Interpreter{Path: interp, Home: gen}})
	fl := newFakeLayers()
	fl.layers["analyst"] = pyenv.Layer{Dir: layerDir, Boot: filepath.Join(layerDir, "boot")}
	im.SetPythonLayers(fl)
	im.SetManifestResolver(manifests(map[string]*domain.AgentManifest{"analyst": {PythonPackages: polarsDecl}}))
	for _, id := range []string{"analyst", "other"} {
		def := &domain.AgentDefinition{ID: id, Runtime: domain.RuntimePython, ExecPath: "worker_agent.py"}
		if err := im.ensurePythonLayer(context.Background(), def); err != nil {
			t.Fatal(err)
		}
		cmd, err := im.buildAgentCmd(def, domain.NewInstance(id), "")
		if err != nil {
			t.Fatal(err)
		}
		if cmd.Path != interp && cmd.Args[0] != interp {
			t.Fatalf("%s must run the base generation's real interpreter, got %s", id, cmd.Path)
		}
		var pp []string
		for _, kv := range cmd.Env {
			if strings.HasPrefix(strings.ToUpper(kv), "PYTHONPATH=") {
				pp = append(pp, kv)
			}
		}
		switch id {
		case "analyst":
			if len(pp) != 1 || pp[0] != "PYTHONPATH="+filepath.Join(layerDir, "boot") {
				t.Fatalf("analyst env PYTHONPATH = %v, want exactly its layer's boot dir", pp)
			}
		case "other":
			for _, kv := range pp {
				if strings.Contains(kv, layerDir) {
					t.Fatalf("another agent must not load analyst's layer: %v", pp)
				}
			}
		}
	}
}

// Sandbox spec: the agent's layer is one more read-only bind, private to it
// (AppContainer: its own capability) and pre-granted when the build set the
// ACE; another agent's spec does not contain it.
func TestPythonLayers_SandboxBindsOnlyTheOwnLayer(t *testing.T) {
	sb, _ := testAgentSandbox(t, nil)
	root := filepath.Join(t.TempDir(), "runtime", "python")
	gen := filepath.Join(root, "base", "0123456789abcdef")
	interp := filepath.Join(gen, "bin", "python3")
	layerDir := filepath.Join(root, "layers", "analyst", "fedcba9876543210")
	im := NewInstanceManager("127.0.0.1:1")
	im.SetAgentContainment(AgentContainment{Sandbox: sb})
	im.SetPythonEnv(fixedPythonEnv{pi: pyenv.Interpreter{Path: interp, Home: gen, Granted: true}})
	fl := newFakeLayers()
	fl.layers["analyst"] = pyenv.Layer{Dir: layerDir, Boot: filepath.Join(layerDir, "boot"), Granted: true}
	im.SetPythonLayers(fl)
	im.SetManifestResolver(manifests(map[string]*domain.AgentManifest{"analyst": {PythonPackages: polarsDecl}}))
	bindsFor := func(id string) []sandbox.Bind {
		def := &domain.AgentDefinition{ID: id, Runtime: domain.RuntimePython, ExecPath: "worker_agent.py"}
		if err := im.ensurePythonLayer(context.Background(), def); err != nil {
			t.Fatal(err)
		}
		binds, err := im.agentBinds(context.Background(), def, interp)
		if err != nil {
			t.Fatal(err)
		}
		return binds
	}
	var layerBind *sandbox.Bind
	for _, b := range bindsFor("analyst") {
		if withinPath(root, b.Path) {
			t.Fatalf("bind %s exposes the env root", b.Path)
		}
		if b.Path == filepath.Clean(layerDir) {
			bb := b
			layerBind = &bb
		}
	}
	if layerBind == nil || layerBind.Write || !layerBind.Private || !layerBind.Granted {
		t.Fatalf("analyst's layer must be a private, read-only, pre-granted bind: %+v", layerBind)
	}
	for _, b := range bindsFor("other") {
		if withinPath(b.Path, filepath.Join(root, "layers")) {
			t.Fatalf("another agent's spec must not contain a layer: %s", b.Path)
		}
	}
}
