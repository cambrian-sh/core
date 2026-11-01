package network

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
)

// meteredRouter plays layer-3 classification through the meter exactly as the
// SpendMeter's decorator does for every organ generator (Admit with the prompt
// estimate, then Record the provider usage against the call's ctx); the
// decorator itself is proven in llm/spend_attribution_test.go. What this file
// proves is Execute's half: the ctx the router runs under.
type meteredRouter struct {
	meter    *llm.SpendMeter
	decision domain.DecisionType
	reached  int
}

func (r *meteredRouter) Resolve(ctx context.Context, _ domain.RouterInput) (*domain.RouterDecision, error) {
	if err := r.meter.Admit(ctx, string(domain.PurposeRouter), 10); err != nil {
		return nil, err
	}
	r.reached++
	r.meter.Record(ctx, string(domain.PurposeRouter), "", llm.TokenUsage{PromptTokens: 40, CompletionTokens: 10, TotalTokens: 50})
	return &domain.RouterDecision{Type: r.decision}, nil
}

func withSessionHeader(sid domain.SessionID) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(sessionHeader, string(sid)))
}

// (a) A caller continuing an existing session: router classification is booked
// to the session and is part of this Execute's _llm_* figures.
func TestExecute_RouterSpendLandsInExistingSession(t *testing.T) {
	t.Parallel()
	meter := llm.NewSpendMeter(0, nil, nil)
	gw := meteredGateway(meter, &usageStreamer{text: "x", prompt: 120, compl: 30})
	s, _, _ := ceilingServer(t, meter, gw, 1)
	r := &meteredRouter{meter: meter, decision: domain.DecisionPlan}
	s.Router = r
	ses, err := s.SessionMgr.CreateSession(context.Background(), "goal", "")
	if err != nil {
		t.Fatal(err)
	}

	resp, err := s.Execute(withSessionHeader(ses.ID), &pb.Handoff{Id: "t", Payload: &pb.Object{Data: []byte("do it")}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	md := resp.GetMetadata()
	if md[MetaLLMCalls] != "2" || md[MetaLLMInputTokens] != "160" || md[MetaLLMOutputTokens] != "40" {
		t.Fatalf("per-Execute spend = calls %q in %q out %q, want router 40/10 + step 120/30",
			md[MetaLLMCalls], md[MetaLLMInputTokens], md[MetaLLMOutputTokens])
	}
	if got := meter.Session(ses.ID); got.Calls != 2 || got.TotalTokens() != 200 {
		t.Fatalf("session = %+v, want router + step", got)
	}
	if nb := meter.NoSession(); nb.Calls != 0 {
		t.Fatalf("router spend leaked into the no-session bucket: %+v", nb)
	}
}

// (a) A new session (no caller session yet): the router runs on a tab that is
// settled into the session Execute opens, so the figures still include it.
func TestExecute_RouterSpendSettlesIntoNewSession(t *testing.T) {
	t.Parallel()
	meter := llm.NewSpendMeter(0, nil, nil)
	gw := meteredGateway(meter, &usageStreamer{text: "x", prompt: 120, compl: 30})
	s, _, _ := ceilingServer(t, meter, gw, 1)
	s.Router = &meteredRouter{meter: meter, decision: domain.DecisionPlan}

	resp, err := s.Execute(context.Background(), &pb.Handoff{Id: "t", Payload: &pb.Object{Data: []byte("do it")}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	md := resp.GetMetadata()
	if md[MetaLLMCalls] != "2" || md[MetaLLMInputTokens] != "160" || md[MetaLLMOutputTokens] != "40" {
		t.Fatalf("per-Execute spend = calls %q in %q out %q, want router + step",
			md[MetaLLMCalls], md[MetaLLMInputTokens], md[MetaLLMOutputTokens])
	}
	if nb := meter.NoSession(); nb.Calls != 0 {
		t.Fatalf("router spend leaked into the no-session bucket: %+v", nb)
	}
}

// (a) A session already over its ceiling is refused AT the router: the plan
// ends visibly with ErrSpendCeiling (ResourceExhausted), nothing is classified,
// planned or dispatched, and the spend-ceiling record is written.
func TestExecute_RouterRefusedOverSessionCeiling(t *testing.T) {
	t.Parallel()
	meter := llm.NewSpendMeter(100, nil, nil)
	gw := meteredGateway(meter, &usageStreamer{text: "x", prompt: 120, compl: 30})
	s, d, ev := ceilingServer(t, meter, gw, 1)
	r := &meteredRouter{meter: meter, decision: domain.DecisionPlan}
	s.Router = r
	ses, err := s.SessionMgr.CreateSession(context.Background(), "goal", "")
	if err != nil {
		t.Fatal(err)
	}
	meter.RecordSession(ses.ID, "planner", "", llm.TokenUsage{PromptTokens: 90, CompletionTokens: 20, TotalTokens: 110})

	_, err = s.Execute(withSessionHeader(ses.ID), &pb.Handoff{Id: "t", Payload: &pb.Object{Data: []byte("do it")}})
	if !errors.Is(err, llm.ErrSpendCeiling) {
		t.Fatalf("want ErrSpendCeiling from the router, got %v", err)
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("want ResourceExhausted, got %v", status.Code(err))
	}
	if r.reached != 0 || d.calls != 0 {
		t.Fatalf("refused router must not classify or dispatch: reached %d, dispatches %d", r.reached, d.calls)
	}
	ev.mu.Lock()
	defer ev.mu.Unlock()
	found := false
	for _, e := range ev.events {
		found = found || (e.AgentID == spendCeilingAgentID && e.BudgetOverrun)
	}
	if !found {
		t.Fatalf("no spend-ceiling record: %+v", ev.events)
	}
}

// (c) A turn that never gets a session (CHAT, here answered not_implemented in
// OSS) books its router spend to the kernel no-session bucket.
func TestExecute_ChatTurnRouterSpendGoesToKernelBucket(t *testing.T) {
	t.Parallel()
	meter := llm.NewSpendMeter(0, nil, nil)
	gw := meteredGateway(meter, &usageStreamer{text: "x", prompt: 120, compl: 30})
	s, d, _ := ceilingServer(t, meter, gw, 1)
	s.Router = &meteredRouter{meter: meter, decision: domain.DecisionChat}

	resp, err := s.Execute(context.Background(), &pb.Handoff{Id: "t", Payload: &pb.Object{Data: []byte("hi")}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.GetPayload().GetType() != "not_implemented" || d.calls != 0 {
		t.Fatalf("chat turn: payload %q dispatches %d", resp.GetPayload().GetType(), d.calls)
	}
	if nb := meter.NoSession(); nb.Calls != 1 || nb.TotalTokens() != 50 {
		t.Fatalf("%s = %+v, want the router call", llm.NoSessionBucket, nb)
	}
}

// (d) The step's TaskEvent bills the provider usage its lease reconciled, not
// len/4, and is not flagged estimated.
func TestExecute_StepTaskEventCarriesProviderUsage(t *testing.T) {
	t.Parallel()
	meter := llm.NewSpendMeter(0, nil, nil)
	gw := meteredGateway(meter, &usageStreamer{text: "x", prompt: 1200, compl: 300})
	s, _, ev := ceilingServer(t, meter, gw, 1)
	s.Manager.DefaultInputCostPer1M, s.Manager.DefaultOutputCostPer1M = 2.0, 8.0

	resp, err := s.Execute(context.Background(), &pb.Handoff{Id: "t", Payload: &pb.Object{Data: []byte("do it")}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for k := range resp.GetMetadata() {
		if k == domain.HandoffKeyStepPromptTokens || k == domain.HandoffKeyStepCompletionTokens {
			t.Fatalf("step-usage key %q leaked into the plan result", k)
		}
	}
	ev.mu.Lock()
	defer ev.mu.Unlock()
	var step *domain.TaskEvent
	for i := range ev.events {
		if ev.events[i].AgentID == "a" {
			step = &ev.events[i]
		}
	}
	if step == nil {
		t.Fatalf("no step TaskEvent: %+v", ev.events)
	}
	// 1200 * $2/1M + 300 * $8/1M
	if step.PromptTokens != 1200 || step.CompletionTokens != 300 || step.TotalTokens != 1500 || step.UsageEstimated ||
		step.EstimatedCost < 0.0048-1e-9 || step.EstimatedCost > 0.0048+1e-9 {
		t.Fatalf("step TaskEvent = %+v, want provider usage 1200/300 priced at $0.0048, not estimated", step)
	}
}
