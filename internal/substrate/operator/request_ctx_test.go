package operator

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
)

// T-20261118-remaining-context-background-leaks-on-request (ENG-2055): the
// operator handlers hand the RPC's ctx to their seams, so a deadline, a
// cancellation and the caller's scope reach the composition root instead of a
// detached context.Background() (AGENTS.md §2.7).

type reqCtxKey struct{}

func reqCtx() context.Context {
	return context.WithValue(context.Background(), reqCtxKey{}, "rpc")
}

func carriesReqCtx(ctx context.Context) bool {
	return ctx != nil && ctx.Value(reqCtxKey{}) == "rpc"
}

type ctxConfigWriter struct {
	stubConfigWriter
	got context.Context
}

func (c *ctxConfigWriter) SetConfig(ctx context.Context, v map[string]float64) ([]ConfigWriteOutcome, error) {
	c.got = ctx
	return c.stubConfigWriter.SetConfig(ctx, v)
}

func TestSetConfig_PassesTheRPCContextToTheWriter(t *testing.T) {
	w := &ctxConfigWriter{}
	s := newWriteService(w, nil)
	if _, err := s.SetConfig(reqCtx(), &pb.SetConfigOpRequest{
		CommandId: "c-ctx", Reason: "r", Values: map[string]float64{"execution.ewma_alpha": 0.5},
	}); err != nil {
		t.Fatal(err)
	}
	if !carriesReqCtx(w.got) {
		t.Fatal("ConfigWriter.SetConfig must receive the RPC ctx")
	}
}

type ctxIntegrations struct {
	stubIntegrations
	got context.Context
}

func (c *ctxIntegrations) Integrations(ctx context.Context) []*pb.IntegrationOp {
	c.got = ctx
	return c.stubIntegrations.Integrations(ctx)
}

func TestListIntegrations_PassesTheRPCContext(t *testing.T) {
	s := newWriteService(nil, nil)
	m := &ctxIntegrations{}
	s.SetIntegrationManager(m)
	if _, err := s.ListIntegrations(reqCtx(), &pb.ListIntegrationsOpRequest{}); err != nil {
		t.Fatal(err)
	}
	if !carriesReqCtx(m.got) {
		t.Fatal("IntegrationManager.Integrations must receive the RPC ctx")
	}
}

type ctxMCP struct {
	got *context.Context
}

func (c ctxMCP) MCPServers(ctx context.Context) []MCPServerInfo { *c.got = ctx; return nil }
func (c ctxMCP) MCPConfigured() bool                            { return true }

func TestListMCPServers_PassesTheRPCContext(t *testing.T) {
	var got context.Context
	s := &Service{}
	s.SetWave1Reads(nil, ctxMCP{got: &got}, nil, nil)
	if _, err := s.ListMCPServers(reqCtx(), &pb.ListMCPServersOpRequest{}); err != nil {
		t.Fatal(err)
	}
	if !carriesReqCtx(got) {
		t.Fatal("MCPServerLister.MCPServers must receive the RPC ctx")
	}
}

type ctxSubmitter struct {
	stubSubmitter
	got []context.Context
}

func (c *ctxSubmitter) KnownAgent(ctx context.Context, id string) bool {
	c.got = append(c.got, ctx)
	return true
}

func TestSubmitPlan_PinLookupRunsUnderTheRPCContext(t *testing.T) {
	sub := &ctxSubmitter{}
	s := newSubmitService(sub)
	if _, err := s.SubmitPlan(reqCtx(), &pb.SubmitPlanOpRequest{
		CommandId: "c-pin", Reason: "r", DryRun: true,
		Steps: []*pb.AuthoredStepOp{{Query: "q", PreferredAgent: "chat_agent", FanOutOver: -1}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(sub.got) == 0 {
		t.Fatal("a pinned step must be checked with KnownAgent")
	}
	for _, c := range sub.got {
		if !carriesReqCtx(c) {
			t.Fatal("PlanSubmitter.KnownAgent must receive the RPC ctx")
		}
	}
}
