package kernel

import (
	"testing"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
)

// ENG-2262: the agent manager is built with the dial address the composition
// root resolved (app.agentTransport) — one address, no "localhost:"+port that a
// later override had to correct. A routable bind's address is what spawns get.
func TestNewMetabolismStack_BuildsTheManagerWithTheResolvedDialAddress(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Server.Port = "50051"
	tr := agentmgr.AgentTransport{Addr: "10.0.0.5:50051", TLSCAFile: "ca.pem", TLSServerName: "kernel.lan"}
	meta := NewMetabolismStack(&AgentRepoDecorator{}, nil, nil, nil, nil, cfg, nil, tr)
	if got := meta.Manager.InstanceManager.AgentDialAddr(); got != "10.0.0.5:50051" {
		t.Fatalf("manager dial address = %q, want the resolved 10.0.0.5:50051 (never localhost:<port>)", got)
	}
}
