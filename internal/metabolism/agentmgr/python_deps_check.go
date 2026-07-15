package agentmgr

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// checkPythonDeps verifies every import name in deps resolves in the target
// Python WITHOUT importing it (importlib.util.find_spec — fast, avoids paying a
// torch/docling import just to check presence). It returns an error naming the
// missing modules, so a missing package becomes an install-time failure with the
// dep named rather than a silent post-spawn ImportError crash (PLAT-01).
func checkPythonDeps(ctx context.Context, pythonPath string, deps []string) error {
	if len(deps) == 0 {
		return nil
	}
	script := "import importlib.util as u,sys\n" +
		"deps=" + pyStrList(deps) + "\n" +
		"missing=[m for m in deps if u.find_spec(m) is None]\n" +
		"sys.stdout.write(','.join(missing))\n" +
		"sys.exit(1 if missing else 0)\n"

	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, pythonPath, "-c", script)
	cmd.Env = allowlistedAgentEnv(nil) // SEC-01: the check runs deny-by-default too

	out, err := cmd.Output()
	if err == nil {
		return nil
	}
	if missing := strings.TrimSpace(string(out)); missing != "" {
		return fmt.Errorf("missing Python dependencies: %s (pip install -r the agent's requirements.txt)", missing)
	}
	return fmt.Errorf("python dependency self-check could not run: %w", err)
}

// pyStrList renders a Go string slice as a Python list literal. Quotes are
// stripped from each item so the interpolated names cannot break out of the
// literal (the names are import identifiers, not arbitrary code).
func pyStrList(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, s := range items {
		s = strings.NewReplacer("'", "", "\"", "", "\n", "", "\\", "").Replace(s)
		quoted = append(quoted, "'"+s+"'")
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

// verifyPythonDeps runs the PLAT-01 dependency self-check for a Python agent
// using its manifest-declared python_deps. No manifest / no deps ⇒ nil (agents
// that don't declare deps keep today's behavior).
func (im *InstanceManager) verifyPythonDeps(ctx context.Context, def *domain.AgentDefinition) error {
	if def.Runtime != domain.RuntimePython || im.manifestFor == nil {
		return nil
	}
	m := im.manifestFor(def.ID)
	if m == nil || len(m.PythonDeps) == 0 {
		return nil
	}
	if err := checkPythonDeps(ctx, im.pythonPath, m.PythonDeps); err != nil {
		return fmt.Errorf("agent %q: %w", def.ID, err)
	}
	return nil
}
