package agentmgr

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

type gateFunc func() error

func (g gateFunc) Ready() error { return g() }

// ADR-0157 D7.11.2: while the managed env is not ready, a Python agent is
// refused before anything is spawned, with the env's reason; other runtimes
// are not gated.
func TestPythonEnvGate_RefusesPythonAgentsBeforeSpawn(t *testing.T) {
	m := &AgentManager{InstanceManager: NewInstanceManager("definitely-not-an-interpreter", "")}
	m.SetPythonEnvGate(gateFunc(func() error {
		return errors.New("sandbox Python env building (step: installing the base lock)")
	}))
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

func TestPythonEnvGate_ReadyOrUnsetPasses(t *testing.T) {
	im := NewInstanceManager("python", "")
	def := &domain.AgentDefinition{ID: "w", Runtime: domain.RuntimePython}
	if err := im.checkPythonEnv(def); err != nil {
		t.Fatalf("no gate: %v", err)
	}
	im.pythonEnv = gateFunc(func() error { return nil })
	if err := im.checkPythonEnv(def); err != nil {
		t.Fatalf("ready gate: %v", err)
	}
}
