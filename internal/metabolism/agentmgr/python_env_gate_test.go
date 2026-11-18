package agentmgr

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/pyenv"
	"github.com/cambrian-sh/core/internal/sandbox"
)

// ADR-0157 D7.11.2: while the managed env is not ready, a Python agent is
// refused before anything is spawned, with the env's reason; other runtimes
// are not gated.
func TestPythonEnv_RefusesPythonAgentsBeforeSpawn(t *testing.T) {
	m := &AgentManager{InstanceManager: NewInstanceManager("")}
	m.SetPythonEnv(fixedPythonEnv{err: errors.New("sandbox Python env building (step: installing the base lock)")})
	im := m.InstanceManager
	for _, boot := range []func() error{
		func() error {
			_, err := im.bootAgentInstance(context.Background(), &domain.AgentDefinition{ID: "worker", Runtime: domain.RuntimePython}, false)
			return err
		},
		func() error {
			_, err := im.bootDaemonAgent(context.Background(), &domain.AgentDefinition{ID: "daemon", Runtime: domain.RuntimePython}, "s1", nil)
			return err
		},
	} {
		err := boot()
		if err == nil || !strings.Contains(err.Error(), "sandbox Python env building") || !strings.Contains(err.Error(), "not started") {
			t.Fatalf("err = %v, want the env refusal", err)
		}
	}
	if len(im.cmds) != 0 {
		t.Fatalf("a process was started: cmds=%d", len(im.cmds))
	}

	def := &domain.AgentDefinition{ID: "js", Runtime: domain.RuntimeBun}
	if err := im.checkPythonEnv(def); err != nil {
		t.Fatalf("a bun agent must not be gated: %v", err)
	}
}

// ADR-0157 D7.14 / D7.11.2: with no managed env installed there is no
// interpreter at all — never a host Python, never a configured path. Both the
// gate and the command builder refuse, naming D7.
func TestPythonEnv_UnsetRefusesNeverFallsBack(t *testing.T) {
	im := NewInstanceManager("127.0.0.1:1")
	im.SetRuntimeExecutable(domain.RuntimePython, "/usr/bin/python3") // not a Python source any more
	def := &domain.AgentDefinition{ID: "w", Runtime: domain.RuntimePython, ExecPath: "agent.py"}
	if err := im.checkPythonEnv(def); !errors.Is(err, ErrNoPythonEnv) {
		t.Fatalf("gate without env: %v, want ErrNoPythonEnv", err)
	}
	if _, err := im.buildAgentCmd(def, domain.NewInstance(def.ID), ""); !errors.Is(err, ErrNoPythonEnv) {
		t.Fatalf("buildAgentCmd without env: %v, want ErrNoPythonEnv", err)
	}
	if _, err := im.bootAgentInstance(context.Background(), def, false); !errors.Is(err, ErrNoPythonEnv) {
		t.Fatalf("boot without env: %v, want ErrNoPythonEnv", err)
	}
}

// switchingEnv returns the interpreter it currently holds: a background build
// that switches generations between spawns (D7.10.1).
type switchingEnv struct{ cur pyenv.Interpreter }

func (s *switchingEnv) Interpreter() (pyenv.Interpreter, error) { return s.cur, nil }

// The interpreter is read per spawn: after a generation switch the next
// command launches the new generation's interpreter.
func TestBuildAgentCmd_LaunchesTheCurrentGenerationPerSpawn(t *testing.T) {
	root := t.TempDir()
	genA, genB := filepath.Join(root, "base", "aaaa"), filepath.Join(root, "base", "bbbb")
	env := &switchingEnv{cur: pyenv.Interpreter{Path: filepath.Join(genA, "python.exe"), Home: genA}}
	im := NewInstanceManager("127.0.0.1:1")
	im.SetPythonEnv(env)
	def := &domain.AgentDefinition{ID: "w", Runtime: domain.RuntimePython, ExecPath: "agent.py"}
	a, err := im.buildAgentCmd(def, domain.NewInstance(def.ID), "")
	if err != nil {
		t.Fatal(err)
	}
	env.cur = pyenv.Interpreter{Path: filepath.Join(genB, "python.exe"), Home: genB}
	b, err := im.buildAgentCmd(def, domain.NewInstance(def.ID), "")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(a.Path) != genA || filepath.Dir(b.Path) != genB {
		t.Fatalf("spawns launched %s then %s; want the interpreter in %s then in %s", a.Path, b.Path, genA, genB)
	}
}

// D7.1.3 + P2 unit: a Python agent's sandbox spec binds exactly the current
// generation directory read-only — never the env root, downloads/, installer/,
// cache/ or current.json — and marks it Granted only when the generation was
// pre-granted at build (D7.7.5).
func TestAgentBinds_ManagedGenerationOnly(t *testing.T) {
	sb, _ := testAgentSandbox(t, nil)
	root := filepath.Join(t.TempDir(), "runtime", "python")
	gen := filepath.Join(root, "base", "0123456789abcdef")
	interp := filepath.Join(gen, "bin", "python3")
	for _, granted := range []bool{false, true} {
		im := NewInstanceManager("127.0.0.1:1")
		im.SetAgentContainment(AgentContainment{Sandbox: sb})
		im.SetPythonEnv(fixedPythonEnv{pi: pyenv.Interpreter{Path: interp, Home: gen, Granted: granted}})
		binds, err := im.agentBinds(context.Background(), pythonDef(), interp)
		if err != nil {
			t.Fatal(err)
		}
		var genBind *sandbox.Bind
		for i, b := range binds {
			for _, kernelOnly := range []string{"downloads", "installer", "cache", "current.json"} {
				if withinPath(b.Path, filepath.Join(root, kernelOnly)) {
					t.Fatalf("bind %s exposes the env's kernel-only %s", b.Path, kernelOnly)
				}
			}
			if withinPath(root, b.Path) {
				t.Fatalf("bind %s exposes the whole env root %s", b.Path, root)
			}
			if b.Path == gen {
				genBind = &binds[i]
			} else if b.Granted {
				t.Errorf("bind %s marked Granted; only the generation may be", b.Path)
			}
		}
		if genBind == nil {
			t.Fatalf("binds %v lack the generation %s", binds, gen)
		}
		if genBind.Write || genBind.Private {
			t.Errorf("generation bind %+v must be read-only and shared", *genBind)
		}
		if genBind.Granted != granted {
			t.Errorf("generation Granted = %v, want %v (marker)", genBind.Granted, granted)
		}
	}
}

// An interpreter that is not the env's current one (a generation switched
// between the command and the spec) is bound by its own tree, never marked
// pre-granted, and the new generation is not shown.
func TestAgentBinds_StaleInterpreterIsNotPreGranted(t *testing.T) {
	sb, _ := testAgentSandbox(t, nil)
	root := t.TempDir()
	oldGen, newGen := filepath.Join(root, "base", "old"), filepath.Join(root, "base", "new")
	im := NewInstanceManager("127.0.0.1:1")
	im.SetAgentContainment(AgentContainment{Sandbox: sb})
	im.SetPythonEnv(fixedPythonEnv{pi: pyenv.Interpreter{Path: filepath.Join(newGen, "bin", "python3"), Home: newGen, Granted: true}})
	binds, err := im.agentBinds(context.Background(), pythonDef(), filepath.Join(oldGen, "bin", "python3"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range binds {
		if b.Granted {
			t.Errorf("bind %s marked Granted for a non-current interpreter", b.Path)
		}
		if b.Path == newGen {
			t.Errorf("the new generation %s is bound for an agent launched on the old one", newGen)
		}
		found = found || b.Path == oldGen
	}
	if !found {
		t.Fatalf("binds %v lack the launched interpreter's own tree %s", binds, oldGen)
	}
}
