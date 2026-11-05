package app

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// ADR-0139 D8 / T-20261104-leases-minted-with-no-agentid-are-usable-by-any: the plugin
// lane's agent-bound acquirer binds the lease to the agent at mint, so the agent the
// plugin dispatches can spend under it and nobody else can. The unbound acquirer still
// returns an agent-unbound lease (in-process / non-agent use).
func TestAcquireKernelLLMToken_BindsTheAgent(t *testing.T) {
	gw := subnetwork.NewLLMGateway(config.DefaultConfig().Execution)

	tok, release, err := acquireKernelLLMToken(context.Background(), gw, "chat_session_agent", 1024, time.Minute)
	if err != nil || tok == "" {
		t.Fatalf("acquire: tok=%q err=%v", tok, err)
	}
	b, ok := gw.ResolveLease(domain.LeaseID(tok))
	if !ok || b.AgentID != "chat_session_agent" {
		t.Fatalf("lease must be bound to the agent at mint, got known=%v binding=%+v", ok, b)
	}
	release()
	if _, ok := gw.ResolveLease(domain.LeaseID(tok)); ok {
		t.Fatal("release must complete the lease")
	}

	tok, release, err = acquireKernelLLMToken(context.Background(), gw, "", 1024, time.Minute)
	if err != nil || tok == "" {
		t.Fatalf("unbound acquire: tok=%q err=%v", tok, err)
	}
	if b, _ := gw.ResolveLease(domain.LeaseID(tok)); b.AgentID != "" {
		t.Fatalf("the unbound acquirer must not name an agent, got %q", b.AgentID)
	}
	release()

	// No gateway: no token, no error, as before.
	if tok, _, err := acquireKernelLLMToken(context.Background(), nil, "a", 1, time.Second); tok != "" || err != nil {
		t.Fatalf("nil gateway: tok=%q err=%v", tok, err)
	}
}
