package kernel

import (
	"testing"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/sandbox"
)

// ENG-2112: execution.agents.sandbox.<tier>.max_processes / max_tasks /
// scratch_mb reach the agent sandbox the composition root builds — the
// defaults when unset, the operator's values when set.
func TestAgentSandboxFromConfig_CarriesTheTierLimits(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Execution.Agents.Sandbox.Enabled = true
	cfg.Execution.Agents.Sandbox.WorkRoot = t.TempDir()
	cfg.MCP.Sandbox.WorkRoot = t.TempDir()
	cfg.Execution.Agents.Sandbox.ThirdParty.ScratchMB = 77
	sb := agentSandboxFromConfig(cfg)
	if sb == nil {
		t.Fatal("sandbox enabled but not built")
	}
	t.Cleanup(sb.ReleaseWorkRoot)
	fp, tp := sb.Policy().Tiers[sandbox.TierFirstParty], sb.Policy().Tiers[sandbox.TierThirdParty]
	if fp.MaxProcesses != 32 || fp.MaxTasks != 1024 || fp.ScratchMB != 4096 {
		t.Fatalf("first-party defaults did not reach the policy: %+v", fp)
	}
	if tp.MaxProcesses != 16 || tp.MaxTasks != 512 || tp.ScratchMB != 77 {
		t.Fatalf("third-party limits did not reach the policy: %+v", tp)
	}
}
