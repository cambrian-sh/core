package network

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
)

// grantingCaller plays the retrieval_agent: while it holds the dispatcher's lease
// it loads a system skill (a run-scoped grant keyed by the lease) and makes one
// managed LLM call under that lease.
type grantingCaller struct {
	overlay *domain.RunGrantOverlay
	gw      *SubstrateLLMGateway
	lease   domain.LeaseID
	binding domain.LeaseBinding
}

func (c *grantingCaller) CallAgent(ctx context.Context, _ string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
	c.lease = domain.LeaseID(h.Context["_session_token_id"])
	c.binding, _ = c.gw.ResolveLease(c.lease)
	c.overlay.Activate(string(c.lease), []string{"skill_tool"})
	out := make(chan domain.StreamChunk, 8)
	if err := c.gw.StreamChunks(ctx, c.lease, "decompose this", domain.GenerateOptions{}, out); err != nil {
		return nil, err
	}
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte(`{"sub_questions":["a"],"refs":[]}`)}}, nil
}

// T-20261101-retrieval-dispatch-lease-retrieval-dispatch-go + ADR-0131 D1/D3:
// the retrieval dispatcher's lease is (1) bound to the caller's session, (2)
// charged back to the step lease of the agent that asked, and (3) completed
// through the gateway hook NewServer registers, which clears skill grants
// conferred under it.
func TestRetrievalDispatch_LeaseIsSessionBoundChargedAndCleared(t *testing.T) {
	t.Parallel()
	meter := llm.NewSpendMeter(0, nil, nil)
	gw := meteredGateway(meter, &usageStreamer{text: "x", prompt: 40, compl: 10})
	s := NewServer(nil, nil, nil, config.ExecutionConfig{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, gw, nil, nil)
	overlay := domain.NewRunGrantOverlay()
	s.ToolExecutor = &domain.ToolExecutor{Overlay: overlay}

	// The asking agent's step lease, presented on its QueryMemory RPC.
	stepLease, _ := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, 0)
	ctx := domain.WithSessionID(context.Background(), "ses-r")
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(leaseHeader, string(stepLease)))

	caller := &grantingCaller{overlay: overlay, gw: gw}
	d := &RetrievalDispatcher{Caller: caller, Gateway: gw}
	if subqs, _, err := d.Decompose(ctx, "who and when?"); err != nil || len(subqs) != 1 {
		t.Fatalf("Decompose: %v %v", subqs, err)
	}

	if caller.binding.SessionID != "ses-r" {
		t.Fatalf("retrieval lease bound to %q, want the caller's session", caller.binding.SessionID)
	}
	if overlay.Granted(string(caller.lease), "skill_tool") {
		t.Fatal("a grant conferred under the retrieval lease survived the dispatch")
	}
	if got := meter.Session("ses-r"); got.TotalTokens() != 50 {
		t.Fatalf("retrieval spend not attributed to the session: %+v", got)
	}
	u, err := gw.Complete(context.Background(), stepLease)
	if err != nil {
		t.Fatalf("Complete step lease: %v", err)
	}
	if u.TotalTokens != 50 || u.PromptTokens != 40 {
		t.Fatalf("retrieval chain not charged to the step lease: %+v", u)
	}
}
