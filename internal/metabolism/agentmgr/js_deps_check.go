package agentmgr

import (
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/cambrian-sh/core/domain"
)

// verifyJSDeps is the PLAT-01 fail-fast check for bun/node agents (ADR-0125 D6):
// when a package.json governs the agent (in its entry file's directory or at
// the agents root) but no node_modules exists in either place, the spawn would
// crash on the first import with a module-resolution error that names neither
// the agent nor the fix — so boot fails here instead, naming both. JS deps are
// declared where the ecosystem declares them (package.json), never duplicated
// into the manifest, which is why this checks the filesystem rather than a
// manifest field the way verifyPythonDeps does.
func (im *InstanceManager) verifyJSDeps(def *domain.AgentDefinition) error {
	if !domain.IsJSRuntime(def.Runtime) {
		return nil
	}
	// ExecPath is relative to Dir (slash-separated, see discovery).
	execDir := filepath.Join(def.Dir, filepath.FromSlash(path.Dir(def.ExecPath)))
	rootDir := def.Dir

	pkgDir := ""
	for _, d := range []string{execDir, rootDir} {
		if fileIsRegular(filepath.Join(d, "package.json")) {
			pkgDir = d
			break
		}
	}
	if pkgDir == "" {
		return nil // dependency-free agent — nothing to verify
	}
	for _, d := range []string{execDir, rootDir} {
		if st, err := os.Stat(filepath.Join(d, "node_modules")); err == nil && st.IsDir() {
			return nil
		}
	}
	return fmt.Errorf("agent %q: %s declares dependencies but node_modules is missing — run `bun install` in %s", def.ID, filepath.Join(pkgDir, "package.json"), pkgDir)
}

func fileIsRegular(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}
