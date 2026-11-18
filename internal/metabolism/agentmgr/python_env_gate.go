package agentmgr

import (
	"fmt"

	"github.com/cambrian-sh/core/domain"
)

// PythonEnvGate is the kernel-managed Python environment's readiness, as the
// launch path needs it (ADR-0157 D7.11.2; implemented by pyenv.Env): nil when
// Python agents may start, else why not with the fix named — the env is still
// building, its build failed, or it is missing/broken with build_on_boot off.
type PythonEnvGate interface {
	Ready() error
}

// SetPythonEnvGate installs the env readiness gate. The composition root sets
// it at boot (app.startPythonEnv). It is a readiness check, not a security
// control (AGENTS.md §2.3): without it the launch path behaves as before D7,
// which is what unit tests that construct a manager directly rely on; which
// interpreter an agent launches on moves to the managed env in P2 (ENG-2008).
func (m *AgentManager) SetPythonEnvGate(g PythonEnvGate) { m.InstanceManager.pythonEnv = g }

// checkPythonEnv refuses a Python agent while the managed env is not ready, so
// a refused spawn says "sandbox Python env building (step …)" or names the fix
// instead of crashing on a missing interpreter (never a fallback to the host's
// Python).
func (im *InstanceManager) checkPythonEnv(def *domain.AgentDefinition) error {
	if def.Runtime != domain.RuntimePython || im.pythonEnv == nil {
		return nil
	}
	if err := im.pythonEnv.Ready(); err != nil {
		return fmt.Errorf("agent %q not started: %w", def.ID, err)
	}
	return nil
}
