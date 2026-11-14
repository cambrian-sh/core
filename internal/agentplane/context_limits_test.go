package agentplane

import (
	"context"
	"sync"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

// metadataAgent is a real AgentService over bufconn that records the handoff
// metadata it received.
type metadataAgent struct {
	pb.UnimplementedAgentServiceServer
	mu  sync.Mutex
	got map[string]string
}

func (m *metadataAgent) Execute(_ context.Context, h *pb.Handoff) (*pb.Handoff, error) {
	m.mu.Lock()
	m.got = h.GetMetadata()
	m.mu.Unlock()
	return &pb.Handoff{Payload: &pb.Object{Type: "text", Data: []byte("ok")}}, nil
}

// ENG-595 (owner decision 2026-11-14): execution.lane.context reaches a
// kernel-spawned agent on its handoff, over the real gRPC boundary; only the set
// keys are stamped, a caller's copy is overwritten, and the caller's handoff is
// not mutated.
func TestCallAgent_StampsContextLimitsOnTheHandoff(t *testing.T) {
	lis := bufconn.Listen(1 << 16)
	srv := grpc.NewServer()
	agent := &metadataAgent{}
	pb.RegisterAgentServiceServer(srv, agent)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	d := &dedicatedDialer{lis: lis}
	a := New(d)
	a.ContextLimits = domain.AgentContextLimits{BudgetTokens: 48000, ProtectTail: 5}
	in := &domain.Handoff{
		Payload: &domain.Payload{Data: []byte("hi")},
		Context: map[string]string{"task_id": "task-0", domain.HandoffKeyContextBudgetTokens: "1"},
	}
	ctx := domain.WithDedicatedInstance(context.Background())
	if _, err := a.CallAgent(ctx, "worker", in, ""); err != nil {
		t.Fatalf("CallAgent: %v", err)
	}
	agent.mu.Lock()
	got := agent.got
	agent.mu.Unlock()
	if got[domain.HandoffKeyContextBudgetTokens] != "48000" || got[domain.HandoffKeyContextProtectTail] != "5" {
		t.Fatalf("agent received %v, want the lane's budget_tokens and protect_tail", got)
	}
	if _, has := got[domain.HandoffKeyContextKeepRecentToolResults]; has {
		t.Errorf("an unset key was stamped: %v", got)
	}
	if got["task_id"] != "task-0" {
		t.Errorf("the caller's own keys were lost: %v", got)
	}
	if in.Context[domain.HandoffKeyContextBudgetTokens] != "1" || len(in.Context) != 2 {
		t.Errorf("the caller's handoff was mutated: %v", in.Context)
	}
}

// No lane context set: nothing is stamped and the handoff passes through as is.
func TestWithContextLimits_ZeroIsPassThrough(t *testing.T) {
	a := New(&dedicatedDialer{})
	in := &domain.Handoff{Context: map[string]string{"k": "v"}}
	if out := a.withContextLimits(in); out != in {
		t.Fatal("zero ContextLimits must not copy or change the handoff")
	}
}
