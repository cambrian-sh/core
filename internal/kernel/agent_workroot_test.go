package kernel

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// ENG-2088: building the agent sandbox at boot (agentSandboxFromConfig, the
// composition NewMetabolismStack runs before the instance manager can spawn)
// owns the work root and sweeps the instance directories no live kernel owns;
// the MCP sandbox's work root is never touched.
func TestAgentSandboxFromConfig_OwnsAndSweepsTheWorkRoot(t *testing.T) {
	work, mcp := t.TempDir(), t.TempDir()
	leftover := filepath.Join(work, "first-party", "chat_agent-1234abcd", "out", "result.txt")
	mcpDir := filepath.Join(mcp, "first-party", "github")
	for _, d := range []string{filepath.Dir(leftover), mcpDir, filepath.Join(work, "sock", "1234abcd")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(leftover, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Execution.Agents.Sandbox.Enabled = true
	cfg.Execution.Agents.Sandbox.WorkRoot = work
	cfg.MCP.Sandbox.WorkRoot = mcp
	sb := agentSandboxFromConfig(cfg)
	if sb == nil {
		t.Fatal("sandbox enabled but not built")
	}
	t.Cleanup(sb.ReleaseWorkRoot)
	for _, d := range []string{filepath.Join(work, "first-party", "chat_agent-1234abcd"), filepath.Join(work, "sock", "1234abcd")} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("boot left the orphan %s (%v)", d, err)
		}
	}
	if _, err := os.Stat(mcpDir); err != nil {
		t.Errorf("boot swept the MCP work root: %v", err)
	}
	if again, _ := sb.OwnWorkRoot(); again.Skipped != "already owned" {
		t.Errorf("the boot did not own the work root: %+v", again)
	}
}
