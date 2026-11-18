package agentmgr

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/pyenv"
)

// PythonEnv is the kernel-managed Python environment as the launch path needs
// it (ADR-0157 D7; implemented by pyenv.Env): the current base generation's
// real interpreter for the next spawn, or why Python agents may not start with
// the fix named — the env is still building, its build failed, or it is
// missing/broken with build_on_boot off.
//
// It is the ONE interpreter for every Python agent the kernel spawns, with the
// sandbox on or off (D7.14): metabolism.python_executable is retired and there
// is no host-Python fallback.
type PythonEnv interface {
	Interpreter() (pyenv.Interpreter, error)
}

// ErrNoPythonEnv refuses a Python agent when no managed env was installed: the
// kernel never falls back to a Python of the host's (ADR-0157 D7.11.2).
var ErrNoPythonEnv = errors.New("no kernel-managed sandbox Python env is configured (ADR-0157 D7); the kernel launches Python agents only on the interpreter it downloaded and verified")

// SetPythonEnv installs the managed env. The composition root sets it at boot
// (app.startPythonEnv) before any agent boots. Without it every Python spawn
// is refused (ErrNoPythonEnv) — fail closed, never a host interpreter.
// (AgentManager inherits it from its embedded InstanceManager.)
func (im *InstanceManager) SetPythonEnv(e PythonEnv) { im.pythonEnv = e }

// pythonInterpreter is the interpreter a Python agent launches on now.
func (im *InstanceManager) pythonInterpreter(def *domain.AgentDefinition) (pyenv.Interpreter, error) {
	if im.pythonEnv == nil {
		return pyenv.Interpreter{}, fmt.Errorf("agent %q not started: %w", def.ID, ErrNoPythonEnv)
	}
	pi, err := im.pythonEnv.Interpreter()
	if err != nil {
		return pyenv.Interpreter{}, fmt.Errorf("agent %q not started: %w", def.ID, err)
	}
	return pi, nil
}

// checkPythonEnv refuses a Python agent while the managed env is not ready, so
// a refused spawn says "sandbox Python env building (step …)" or names the fix
// instead of crashing on a missing interpreter.
func (im *InstanceManager) checkPythonEnv(def *domain.AgentDefinition) error {
	if def.Runtime != domain.RuntimePython {
		return nil
	}
	_, err := im.pythonInterpreter(def)
	return err
}

// pythonRuntimeBind is the read-only tree a Python agent launched on interp is
// shown: exactly the managed env's current generation directory (D7.1.3 — never
// downloads/, installer/, cache/ or current.json), marked granted when the
// generation was pre-granted at build (D7.7.5). ok is false when interp is not
// the env's current interpreter (a generation switched between the command
// and the sandbox spec); the caller then derives the runtime paths from interp
// itself, ungranted.
func (im *InstanceManager) pythonRuntimeBind(interp string) (home string, granted, ok bool) {
	if im.pythonEnv == nil {
		return "", false, false
	}
	pi, err := im.pythonEnv.Interpreter()
	if err != nil || pi.Home == "" || filepath.Clean(pi.Path) != filepath.Clean(interp) {
		return "", false, false
	}
	return filepath.Clean(pi.Home), pi.Granted, true
}
