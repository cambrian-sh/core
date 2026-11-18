package agentmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/pyenv"
)

// testRegistry is a simple in-memory implementation of domain.AgentRegistry
// used only for unit tests in this package.
type testRegistry struct {
	agents    map[string]domain.AgentDefinition
	manifests map[string]*domain.AgentManifest
}

func newTestRegistry() *testRegistry {
	return &testRegistry{
		agents:    make(map[string]domain.AgentDefinition),
		manifests: make(map[string]*domain.AgentManifest),
	}
}

func (r *testRegistry) GetAgentByName(_ context.Context, name string) (*domain.AgentDefinition, error) {
	agent, ok := r.agents[name]
	if !ok {
		return nil, fmt.Errorf("agent not found: %s", name)
	}
	return &agent, nil
}

func (r *testRegistry) GetAllAgents(_ context.Context) ([]domain.AgentDefinition, error) {
	var list []domain.AgentDefinition
	for _, agent := range r.agents {
		list = append(list, agent)
	}
	return list, nil
}

func (r *testRegistry) GetManifest(_ context.Context, agentID string) (*domain.AgentManifest, error) {
	m, ok := r.manifests[agentID]
	if !ok {
		return &domain.AgentManifest{}, nil
	}
	return m, nil
}

// newTestManager returns a minimal AgentManager for tests.
func newTestManager() *AgentManager {
	return &AgentManager{
		Registry:        newTestRegistry(),
		InstanceManager: withPython(NewInstanceManager(""), "python"),
		AgentConnector:  NewAgentConnector(),
		daemons:         newDaemonRegistry(),
		stopped:         newStoppedSet(),
	}
}

// fixedPythonEnv is a PythonEnv double: one interpreter (or one refusal).
// Home is empty unless a test sets it, so the sandbox derives the runtime
// paths from the interpreter itself (runtimePaths), as for any interpreter.
type fixedPythonEnv struct {
	pi  pyenv.Interpreter
	err error
}

func (f fixedPythonEnv) Interpreter() (pyenv.Interpreter, error) { return f.pi, f.err }

// withPython gives a test manager a fixed Python interpreter.
func withPython(im *InstanceManager, path string) *InstanceManager {
	im.SetPythonEnv(fixedPythonEnv{pi: pyenv.Interpreter{Path: path}})
	return im
}

func withPythonM(m *AgentManager, path string) *AgentManager {
	m.SetPythonEnv(fixedPythonEnv{pi: pyenv.Interpreter{Path: path}})
	return m
}

// managedInterpreterForTest takes a kernel-managed base generation's
// interpreter (ADR-0157 D7 P2) and reads its generation.json for the directory
// the sandbox shows and the pre-grant; any other Python fails the test.
func managedInterpreterForTest(t *testing.T, py string) pyenv.Interpreter {
	t.Helper()
	abs, err := filepath.Abs(py)
	if err != nil {
		t.Fatal(err)
	}
	for _, home := range []string{filepath.Dir(abs), filepath.Dir(filepath.Dir(abs))} {
		raw, err := os.ReadFile(filepath.Join(home, "generation.json"))
		if err != nil {
			continue
		}
		var m pyenv.Marker
		if err := json.Unmarshal(raw, &m); err != nil || m.Kind != "base" {
			t.Fatalf("%s: generation.json is not a base generation marker (%v)", home, err)
		}
		return pyenv.Interpreter{Path: abs, Home: home, Granted: m.Granted}
	}
	t.Fatalf("%s is not a kernel-managed interpreter (no generation.json beside it)", py)
	return pyenv.Interpreter{}
}
