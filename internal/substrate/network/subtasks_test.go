package network

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
)

// ADR-0150 D2: a subtask runs on a fresh worker bound to a child lease of the
// parent's session, run and conversation (T-20261113-adr-0150-slice-2-run-subtasks-kernel-tool-brief).

type subtaskGateway struct {
	mu        sync.Mutex
	next      int
	bindings  map[domain.LeaseID]domain.LeaseBinding
	completed []domain.LeaseID
}

func (g *subtaskGateway) Acquire(context.Context, domain.StepAllocation, int, time.Duration) (domain.LeaseID, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.next++
	return domain.LeaseID("child-" + string(rune('0'+g.next))), nil
}
func (g *subtaskGateway) Complete(_ context.Context, l domain.LeaseID) (llm.TokenUsage, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.completed = append(g.completed, l)
	return llm.TokenUsage{}, nil
}
func (g *subtaskGateway) EvictExpired() {}
func (g *subtaskGateway) StreamChunks(context.Context, domain.LeaseID, domain.GenerateRequest, chan<- domain.StreamChunk) error {
	return nil
}
func (g *subtaskGateway) GenerateWithTools(context.Context, domain.LeaseID, domain.GenerateRequest) (domain.GenerateResponse, error) {
	return domain.GenerateResponse{}, nil
}
func (g *subtaskGateway) BindLease(id domain.LeaseID, b domain.LeaseBinding) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.bindings[id] = b
}
func (g *subtaskGateway) ResolveLease(id domain.LeaseID) (domain.LeaseBinding, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	b, ok := g.bindings[id]
	return b, ok
}

type subtaskDispatcher struct {
	agent     string
	handoff   *domain.Handoff
	dedicated bool
	during    func(lease domain.LeaseID)
	resp      *domain.Handoff
	err       error
}

func (d *subtaskDispatcher) Execute(context.Context, *domain.DispatchTask, *domain.Handoff) (*domain.DispatchResult, error) {
	return nil, errors.New("unused")
}
func (d *subtaskDispatcher) CallAgent(ctx context.Context, agentID string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
	d.agent, d.handoff, d.dedicated = agentID, h, domain.DedicatedInstanceRequested(ctx)
	if d.during != nil {
		d.during(domain.LeaseID(h.Context["_session_token_id"]))
	}
	return d.resp, d.err
}

func TestRunSubtask_ChildLeaseBoundLikeTheParentAndComposed(t *testing.T) {
	gw := &subtaskGateway{bindings: map[domain.LeaseID]domain.LeaseBinding{
		"parent": {SessionID: "ses-1", RunID: "run-1", StepIndex: 0, AgentID: "composed_worker",
			ConversationID: "conv-1", OriginMessageID: "msg-1", ActedBy: "operator:ops", DelegationDepth: 0},
	}}
	sub := domain.NewSubtasks(0, nil)
	sub.RegisterParent("parent", "composed_worker")
	disp := &subtaskDispatcher{resp: &domain.Handoff{Payload: &domain.Payload{Type: "text", Data: []byte("3 invoices"),
		Metadata: map[string]string{domain.AgentResultPartial: "true"}}}}
	s := &Server{LLMGateway: gw, Dispatcher: disp, Subtasks: sub, Compositions: domain.NewComposedLeases(),
		ComposedRuntimeID: "composed_worker", ToolExecutor: &domain.ToolExecutor{Overlay: domain.NewRunGrantOverlay()}}
	var seenBound, seenComposed bool
	disp.during = func(l domain.LeaseID) {
		_, seenBound = sub.Binding(l)
		c, ok := s.Compositions.For(string(l))
		seenComposed = ok && c.HasTool("crm_search") && !c.HasTool("mail_send")
		sub.ObserveCall(l, domain.SystemTool{Name: "web_search", Effects: []domain.ToolEffect{domain.EffectEgressRead}})
		sub.AddEvidence(l, "led_ev")
	}
	out, err := s.RunSubtask(context.Background(), domain.SubtaskDispatch{
		ParentAgentID: "composed_worker",
		Binding: domain.SubtaskBinding{SubtaskID: "sub-x", Label: "acme", ParentLease: "parent", RootLease: "parent",
			Depth: 1, AllowedEffects: []domain.ToolEffect{domain.EffectRead}, Tools: []string{"crm_search"}},
		Task: "Objective: find invoices", Inputs: []string{"led_in"}, MaxToolRounds: 7, MaxWallSeconds: 90,
	})
	if err != nil {
		t.Fatal(err)
	}
	child := domain.LeaseID("child-1")
	b := gw.bindings[child]
	if b.SessionID != "ses-1" || b.RunID != "run-1" || b.ConversationID != "conv-1" || b.OriginMessageID != "msg-1" ||
		b.ActedBy != "operator:ops" || b.AgentID != "composed_worker" {
		t.Fatalf("child lease must be bound like the parent's: %+v", b)
	}
	if disp.agent != "composed_worker" || !disp.dedicated {
		t.Fatalf("a subtask runs on a dedicated composed-runtime instance: agent=%s dedicated=%v", disp.agent, disp.dedicated)
	}
	if !seenBound || !seenComposed {
		t.Fatalf("during the call the child lease is a bound, composed subtask lease: bound=%v composed=%v", seenBound, seenComposed)
	}
	h := disp.handoff
	// ENG-1760: the worker runs under the lane's D4 stopping rule with its own
	// caps, so at the wall-time cap it is forced to summarise (no ask-back: the
	// "_subtask" key turns ask_person off in the SDK).
	if string(h.Payload.Data) != "Objective: find invoices" || h.Context[domain.HandoffKeyLaneMaxToolRounds] != "7" ||
		h.Context[domain.HandoffKeyLaneMaxWallSeconds] != "90" || h.Context[domain.HandoffKeyLane] != domain.LaneSingleAgent ||
		h.Context[domain.HandoffKeyLaneSameResult] == "" {
		t.Fatalf("the worker gets the brief and the stopping rule with its caps: %+v", h.Context)
	}
	// The subtask marker: the SDK's is_subtask and the runtimes' hidden
	// delegate_to_planner key off it (T-20261113-composed-runtime-still-offers-delegate-to).
	if h.Context[domain.HandoffKeySubtask] != "sub-x" ||
		h.Context["task_id"] != "subtask-"+h.Context[domain.HandoffKeySubtask] {
		t.Fatalf("the worker's request must carry its subtask id under %s: %+v", domain.HandoffKeySubtask, h.Context)
	}
	comp, ok := domain.DecodeWorkerComposition(h.Context[domain.HandoffKeyComposition])
	if !ok || len(comp.LedgerRefs) != 1 || comp.LedgerRefs[0] != "led_in" {
		t.Fatalf("the composition carries the inputs: %+v", comp)
	}
	if out.Text != "3 invoices" || out.Meta[domain.AgentResultPartial] != "true" || !out.ReadWeb ||
		len(out.Evidence) != 1 || out.Evidence[0] != "led_ev" {
		t.Fatalf("outcome = %+v", out)
	}
	if sub.IsChild(child) || len(gw.completed) != 1 || gw.completed[0] != child {
		t.Fatalf("the child lease ends with the subtask: child=%v completed=%v", sub.IsChild(child), gw.completed)
	}
}

func TestRunSubtask_UnavailableWithoutTheComposedRuntime(t *testing.T) {
	s := &Server{LLMGateway: &subtaskGateway{bindings: map[domain.LeaseID]domain.LeaseBinding{}},
		Dispatcher: &subtaskDispatcher{}, Subtasks: domain.NewSubtasks(0, nil)}
	if _, err := s.RunSubtask(context.Background(), domain.SubtaskDispatch{}); !errors.Is(err, domain.ErrSubtaskUnavailable) {
		t.Fatalf("want ErrSubtaskUnavailable, got %v", err)
	}
}
