package agentmgr

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/pyenv"
	"github.com/cambrian-sh/core/internal/sandbox"
)

// PythonLayers is the per-agent package layer service as the launch path needs
// it (ADR-0157 D7.5-D7.8, P3; implemented by pypackages.Service):
//
//   - Ensure runs before every spawn of a Python agent (verifyRuntimeDeps, with
//     the boot's ctx): with no declared packages it clears the agent's layer;
//     otherwise it raises any missing python_package card, refuses while one is
//     unanswered or denied (the error names them), and installs the approved
//     set into the agent's own layer;
//   - Current is read by the command builder and the sandbox spec of that same
//     spawn (no I/O): the layer Ensure produced on this base generation.
type PythonLayers interface {
	Ensure(ctx context.Context, agentID string, decl []domain.PythonPackage, base pyenv.Interpreter) (pyenv.Layer, error)
	Current(agentID string, base pyenv.Interpreter) (pyenv.Layer, error)
}

// ErrNoPythonLayers refuses an agent that declares python_packages when the
// kernel has no package layer service: its packages could never be approved
// or installed, and starting it without them would hide the gap.
var ErrNoPythonLayers = errors.New("no per-agent Python package service is configured (ADR-0157 D7.6); an agent that declares python_packages cannot start")

// SetPythonLayers installs the layer service. The composition root sets it at
// boot (app.wirePythonPackages) before any agent boots; without it an agent
// that declares python_packages is refused (fail closed, like SetPythonEnv).
func (im *InstanceManager) SetPythonLayers(l PythonLayers) { im.pythonLayers = l }

// declaredPythonPackages is the agent's manifest python_packages.
func (im *InstanceManager) declaredPythonPackages(ctx context.Context, def *domain.AgentDefinition) []domain.PythonPackage {
	if im.manifestFor == nil {
		return nil
	}
	if m := im.manifestFor(ctx, def.ID); m != nil {
		return m.PythonPackages
	}
	return nil
}

// ensurePythonLayer is the pre-spawn package check of a Python agent.
func (im *InstanceManager) ensurePythonLayer(ctx context.Context, def *domain.AgentDefinition) error {
	if def.Runtime != domain.RuntimePython {
		return nil
	}
	decl := im.declaredPythonPackages(ctx, def)
	if im.pythonLayers == nil {
		if len(decl) > 0 {
			return fmt.Errorf("agent %q not started: %w", def.ID, ErrNoPythonLayers)
		}
		return nil
	}
	pi, err := im.pythonInterpreter(def)
	if err != nil {
		return err
	}
	_, err = im.pythonLayers.Ensure(ctx, def.ID, decl, pi)
	return err
}

// currentPythonLayer is the layer the agent launches with on this base
// generation (zero Layer when it has none or no layer service is set).
func (im *InstanceManager) currentPythonLayer(def *domain.AgentDefinition, pi pyenv.Interpreter) (pyenv.Layer, error) {
	if im.pythonLayers == nil {
		return pyenv.Layer{}, nil
	}
	return im.pythonLayers.Current(def.ID, pi)
}

// layerEnvOf is the environment that loads a layer: nothing when there is
// none, else PYTHONPATH=<layer>/boot, whose sitecustomize adds the layer's
// site-packages after the base's own directories (pyenv.Layer).
func layerEnvOf(l pyenv.Layer) []string {
	if l.Dir == "" {
		return nil
	}
	return []string{"PYTHONPATH=" + l.Boot}
}

// pythonLayerEnv is layerEnvOf(the agent's current layer).
func (im *InstanceManager) pythonLayerEnv(def *domain.AgentDefinition, pi pyenv.Interpreter) ([]string, error) {
	l, err := im.currentPythonLayer(def, pi)
	if err != nil {
		return nil, err
	}
	return layerEnvOf(l), nil
}

// pythonLayerBind is the agent's layer as a sandbox bind: read-only, private to
// this agent, pre-granted on Windows when the build set its ACE (D7.7.5). A
// zero Bind when it has none.
func (im *InstanceManager) pythonLayerBind(def *domain.AgentDefinition, baseHome string) (sandbox.Bind, error) {
	if im.pythonLayers == nil {
		return sandbox.Bind{}, nil
	}
	l, err := im.pythonLayers.Current(def.ID, pyenv.Interpreter{Home: baseHome})
	if err != nil || l.Dir == "" {
		return sandbox.Bind{}, err
	}
	return sandbox.Bind{Path: filepath.Clean(l.Dir), Private: true, Granted: l.Granted}, nil
}

// withPythonPath sets PYTHONPATH from layer (or leaves env as it is when layer
// is empty), dropping any PYTHONPATH an operator's passthrough carried, so the
// agent loads exactly its own layer.
func withPythonPath(env, layer []string) []string {
	if len(layer) == 0 {
		return env
	}
	out := env[:0:0]
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, "PYTHONPATH") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, layer...)
}
