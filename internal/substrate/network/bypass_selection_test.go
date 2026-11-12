package network

import (
	"context"
	"strings"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/substrate/session"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeAuctioneer is a minimal Auctioneer stand-in for the bypass path. It used to
// live in the Scout dispatcher's test file; it was re-homed here when the Scout was
// removed, because bypass_auction has always been its only other user.
type fakeAuctioneer struct {
	resp        *domain.Handoff
	err         error
	calledAgent string
	gotHandoff  *domain.Handoff
}

func (f *fakeAuctioneer) Execute(_ context.Context, _ *domain.DispatchTask, _ *domain.Handoff) (*domain.DispatchResult, error) {
	return nil, nil
}

func (f *fakeAuctioneer) CallAgent(_ context.Context, agentID string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
	f.calledAgent = agentID
	f.gotHandoff = h
	return f.resp, f.err
}

// ADR-0050 D1: bypass_auction dispatches the input verbatim to the configured
// single agent, strips the benchmark's "/plan " routing prefix, and maps the
// agent's handoff straight back to the caller.
func TestExecute_BypassAuction_DispatchesToSingleAgent(t *testing.T) {
	t.Parallel()
	fake := &fakeAuctioneer{
		resp: &domain.Handoff{
			FromAgent: "",
			Payload:   &domain.Payload{Data: []byte("the react answer")},
		},
	}
	s := &Server{
		Dispatcher: fake,
		ExecCfg: config.ExecutionConfig{
			Routing: config.RoutingConfig{
				BypassSelection: true,
				SingleAgentID:   "react_baseline_agent",
			},
		},
	}

	resp, err := s.Execute(context.Background(), &pb.Handoff{
		Payload: &pb.Object{Type: "task", Data: []byte("/plan inspect the fixtures")},
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if fake.calledAgent != "react_baseline_agent" {
		t.Fatalf("dispatched to %q, want react_baseline_agent", fake.calledAgent)
	}
	got := string(fake.gotHandoff.Payload.Data)
	if got != "inspect the fixtures" {
		t.Fatalf("agent input %q, want the /plan prefix stripped", got)
	}
	if fake.gotHandoff.Context["task_id"] != "task-0" {
		t.Fatalf("task_id = %q, want task-0", fake.gotHandoff.Context["task_id"])
	}
	if string(resp.Payload.Data) != "the react answer" {
		t.Fatalf("response payload %q, want the agent's answer", resp.Payload.Data)
	}
	if resp.FromAgent != "react_baseline_agent" {
		t.Fatalf("FromAgent = %q, want the dispatched agent id", resp.FromAgent)
	}
}

func TestExecute_BypassAuction_RequiresSingleAgentID(t *testing.T) {
	t.Parallel()
	s := &Server{
		Dispatcher: &fakeAuctioneer{},
		ExecCfg:    config.ExecutionConfig{Routing: config.RoutingConfig{BypassSelection: true}},
	}
	_, err := s.Execute(context.Background(), &pb.Handoff{
		Payload: &pb.Object{Data: []byte("anything")},
	})
	if err == nil {
		t.Fatal("expected an error when single_agent_id is empty")
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
	if !strings.Contains(err.Error(), "single_agent_id") {
		t.Fatalf("error %q should name the missing config field", err)
	}
}

// refusingRouter fails the test if Execute classifies a request in the
// single-agent mode (ADR-0050 D1 skips the router).
type refusingRouter struct{ t *testing.T }

func (r refusingRouter) Resolve(context.Context, domain.RouterInput) (*domain.RouterDecision, error) {
	r.t.Error("the router ran in the single-agent mode")
	return &domain.RouterDecision{Type: domain.DecisionPlan}, nil
}

// T-20261112-baseline-arm-run-the-use-case-journeys-through: a chat turn's
// delegation in the single-agent mode runs under the same identity as a plan
// step — a session linked to the ordering conversation, a lease bound to that
// session and a run (so an approval names the conversation owner as its
// beneficiary), the acting-for stamp, and the turn's registration — while the
// router and planner are still skipped.
func TestExecute_BypassSelection_CarriesTheCallersIdentity(t *testing.T) {
	t.Parallel()
	s, gw := newLeaseServer(t)
	mgr := session.New(newMemSessionStore())
	s.SessionMgr = mgr
	s.Router = refusingRouter{t: t}
	s.Delegations = NewTurnDelegations()
	owners := map[string]string{"conv-ada": "ada"}
	s.RunPrincipal = func(ctx context.Context, b domain.LeaseBinding) string {
		return owners[s.conversationOfBinding(ctx, b)]
	}
	fake := &fakeAuctioneer{resp: &domain.Handoff{Payload: &domain.Payload{Data: []byte("done")}}}
	// The binding is read while the agent runs: the lease is completed (and
	// unbound) when Execute returns.
	var b domain.LeaseBinding
	var bound bool
	s.Dispatcher = bindingProbe{fakeAuctioneer: fake, read: func(h *domain.Handoff) {
		b, bound = gw.ResolveLease(domain.LeaseID(h.Context["_session_token_id"]))
	}}
	s.ExecCfg = config.ExecutionConfig{Routing: config.RoutingConfig{BypassSelection: true, SingleAgentID: "uc-personal-assistant"}}

	// The chat lane's turn lease: conversation + origin message, no run.
	turn := bindNotesLease(t, gw, domain.LeaseBinding{StepIndex: -1, AgentID: "chat_agent", ConversationID: "conv-ada", OriginMessageID: "msg-1"})
	in := &pb.Handoff{
		Payload:  &pb.Object{Type: "task", Data: []byte("tidy my receipts")},
		Metadata: map[string]string{"_session_token_id": string(turn), domain.HandoffKeyActingFor: "mallory"},
	}
	resp, err := s.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(resp.Payload.Data) != "done" || fake.calledAgent != "uc-personal-assistant" {
		t.Fatalf("dispatched to %q answering %q", fake.calledAgent, resp.Payload.Data)
	}
	if !bound {
		t.Fatal("the single agent's lease is not bound")
	}
	if b.SessionID == "" || b.RunID == "" || b.StepIndex != 0 || b.AgentID != "uc-personal-assistant" {
		t.Fatalf("lease binding %+v, want session, run, step 0 and the agent", b)
	}
	if conv := s.conversationOfBinding(context.Background(), b); conv != "conv-ada" {
		t.Fatalf("the run's session is linked to %q, want the ordering conversation", conv)
	}
	if got := fake.gotHandoff.Context[domain.HandoffKeyActingFor]; got != "ada" {
		t.Fatalf("acting for %q, want the conversation owner (a caller's value must not survive)", got)
	}
}

// A worker's delegate_to_planner (a plan-step lease calling Execute) has no
// planner to reach in the single-agent mode: refused, never re-dispatched.
func TestExecute_BypassSelection_RefusesWorkerDelegation(t *testing.T) {
	t.Parallel()
	s, gw := newLeaseServer(t)
	fake := &fakeAuctioneer{resp: &domain.Handoff{Payload: &domain.Payload{Data: []byte("x")}}}
	s.Dispatcher = fake
	s.ExecCfg = config.ExecutionConfig{Routing: config.RoutingConfig{BypassSelection: true, SingleAgentID: "uc-personal-assistant"}}
	step := bindNotesLease(t, gw, domain.LeaseBinding{RunID: "run-1", StepIndex: 0, AgentID: "uc-personal-assistant"})
	_, err := s.Execute(context.Background(), &pb.Handoff{
		Payload:  &pb.Object{Data: []byte("look this up for me")},
		Metadata: map[string]string{"_session_token_id": string(step)},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
	if fake.calledAgent != "" {
		t.Fatalf("the delegation was dispatched to %q", fake.calledAgent)
	}
}

// bindingProbe reads the dispatched handoff before the fake answers.
type bindingProbe struct {
	*fakeAuctioneer
	read func(*domain.Handoff)
}

func (p bindingProbe) CallAgent(ctx context.Context, agentID string, h *domain.Handoff, ex string) (*domain.Handoff, error) {
	p.read(h)
	return p.fakeAuctioneer.CallAgent(ctx, agentID, h, ex)
}
