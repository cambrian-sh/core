package kernel

import (
	"testing"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
	"github.com/cambrian-sh/core/internal/metabolism/yield"
)

// ADR-0041 D5 / ADR-0100 D10: the Selector every caller dispatches through must
// be the yield consumer around the Dispatcher. Core b0072cc dropped the old
// consumer silently; this pins the production wiring so it cannot happen again.
func TestMetabolismStack_SelectorResolvesYields(t *testing.T) {
	cfg := config.DefaultConfig()
	// The agent sandbox is on by default and its boot owns and sweeps the work
	// root (ENG-2088): never the developer's real one.
	cfg.Execution.Agents.Sandbox.WorkRoot = t.TempDir()
	meta := NewMetabolismStack(&AgentRepoDecorator{}, nil, nil, nil, nil, cfg, nil, agentmgr.AgentTransport{Addr: "127.0.0.1:" + cfg.Server.Port})
	t.Cleanup(meta.agentSandbox.ReleaseWorkRoot)
	if _, ok := meta.Selector.(*yield.Resolver); !ok {
		t.Fatalf("MetabolismStack.Selector = %T, want *yield.Resolver wrapping the Dispatcher", meta.Selector)
	}
	if meta.Dispatcher == nil {
		t.Fatal("the concrete Dispatcher must still be exposed for EventBus wiring")
	}
}
