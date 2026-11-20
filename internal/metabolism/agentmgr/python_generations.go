package agentmgr

import "path/filepath"

// Which Python generation each live instance runs on (ADR-0157 D7.12, P5).
//
// Generations are immutable and a running instance keeps the base generation
// and layer it started with (D7.10.1), so the env's garbage collector must
// never delete either while the instance lives. buildAgentCmd records both
// when it builds a Python agent's command; detachLocked (every teardown path)
// and a failed boot drop the record, and a drop tells the GC collector
// (onPythonRelease) that something may have become collectable — the
// superseded layer of a recycled instance goes right after the recycle.

type pythonGeneration struct {
	base  string // the base generation directory (pyenv.Interpreter.Home)
	layer string // the agent's layer directory, "" when it has none
}

// SetPythonGenerationRelease sets the hook told when an instance that ran on a
// recorded Python generation is gone. Set it before agents boot.
func (im *InstanceManager) SetPythonGenerationRelease(fn func()) {
	im.mu.Lock()
	im.onPythonRelease = fn
	im.mu.Unlock()
}

func (im *InstanceManager) recordPythonGeneration(instanceID, base, layer string) {
	if base == "" && layer == "" {
		return
	}
	g := pythonGeneration{base: cleanOrEmpty(base), layer: cleanOrEmpty(layer)}
	im.mu.Lock()
	if im.pyGens == nil {
		im.pyGens = map[string]pythonGeneration{}
	}
	im.pyGens[instanceID] = g
	im.mu.Unlock()
}

// dropPythonGeneration forgets a record (a failed boot) and runs the release
// hook.
func (im *InstanceManager) dropPythonGeneration(instanceID string) {
	im.mu.Lock()
	released := im.forgetPythonGenerationLocked(instanceID)
	im.mu.Unlock()
	if released != nil {
		released()
	}
}

// forgetPythonGenerationLocked drops the record and returns the release hook
// to run after im.mu is released (nil when there was no record). im.mu held.
func (im *InstanceManager) forgetPythonGenerationLocked(instanceID string) func() {
	if _, ok := im.pyGens[instanceID]; !ok {
		return nil
	}
	delete(im.pyGens, instanceID)
	return im.onPythonRelease
}

// PythonGenerationsInUse lists the base generation and layer directories live
// instances run on (deduplicated).
func (im *InstanceManager) PythonGenerationsInUse() []string {
	im.mu.Lock()
	defer im.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, g := range im.pyGens {
		for _, d := range []string{g.base, g.layer} {
			if d != "" && !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	return out
}

func cleanOrEmpty(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Clean(p)
}
