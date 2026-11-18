package chat

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/scheduledtask"
	"github.com/cambrian-sh/core/internal/substrate/network"
)

// A chat turn waiting on a human approval
// (T-20261118-a-chat-turn-waiting-on-a-human-approval-is, ENG-2119), end to end
// through the real chat path: TurnService (stall watchdog, person-wait deadline,
// turn end) → the chat agent's schedule_task call on its turn lease through the
// real agent-plane RPC (network.Server.ProposeScheduledTask, admitTurnWork) → the
// real scheduled-task gate → the real approval hub. The chat agent is a scripted
// worker standing in for the model that asks for the approval-gated tool.
//
// Times are the readiness run's, scaled 1:100: stall window 90 s → 300 ms, turn
// timeout 240 s → 800 ms, schedule approval window 300 s → 3 s, the person
// answering at ~150 s → 1.5 s.

const (
	e2eStall    = 300 * time.Millisecond
	e2eTurn     = 800 * time.Millisecond
	e2eWindow   = 3 * time.Second
	e2eApproval = 1500 * time.Millisecond
)

// memSchedules is an in-memory principal schedule store.
type memSchedules struct {
	mu    sync.Mutex
	tasks map[string]domain.ScheduledTask
}

func (m *memSchedules) Check(context.Context, domain.ScheduledTaskSpec, domain.ScheduleLimits) error {
	return nil
}

func (m *memSchedules) Create(_ context.Context, sp domain.ScheduledTaskSpec, _ domain.ScheduleLimits) (domain.ScheduledTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := domain.ScheduledTaskID(sp)
	t := domain.ScheduledTask{ID: id, OwnerID: sp.OwnerID, Goal: sp.Goal, Cron: sp.Cron, Active: true, ApprovedBy: sp.ApprovedBy}
	m.tasks[id] = t
	return t, nil
}

func (m *memSchedules) List(_ context.Context, owner string) ([]domain.ScheduledTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.ScheduledTask
	for _, t := range m.tasks {
		if owner == "" || t.OwnerID == owner {
			out = append(out, t)
		}
	}
	return out, nil
}

func (m *memSchedules) SetActive(context.Context, string, string, bool) error { return nil }
func (m *memSchedules) Delete(context.Context, string, string) error          { return nil }

func (m *memSchedules) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tasks)
}

// scheduleAgent is the chat worker: it calls schedule_task on the turn lease the
// way the SDK does — an agent-plane RPC on the worker's OWN context, which the
// turn's cancel never reaches — and replies with what the tool returned.
type scheduleAgent struct {
	srv    *network.Server
	called chan struct{}
	resp   chan *pb.ProposeScheduledTaskResponse
}

func (a *scheduleAgent) Dispatch(ctx context.Context, h *domain.Handoff) (*domain.Handoff, error) {
	lease := h.Context["_session_token_id"]
	if b := h.Context[HandoffKeyTurnBudgetMs]; b != "800" {
		return nil, errors.New("the worker must be told its work budget (TurnTimeout), got " + b)
	}
	done := make(chan *pb.ProposeScheduledTaskResponse, 1)
	go func() {
		close(a.called)
		wctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-agent-id", "chat_agent"))
		resp, err := a.srv.ProposeScheduledTask(wctx, &pb.ProposeScheduledTaskRequest{
			LeaseId: lease, Goal: "weekly report", Cron: "0 9 * * 1",
		})
		if err != nil {
			resp = &pb.ProposeScheduledTaskResponse{Detail: "rpc error: " + err.Error()}
		}
		a.resp <- resp
		done <- resp
	}()
	select {
	case resp := <-done:
		reply := "Not created: " + resp.GetDetail()
		if resp.GetCreated() {
			reply = "Done: your weekly task " + resp.GetTask().GetId() + " is set up."
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte(reply)}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type approvalE2E struct {
	svc       *TurnService
	hub       *domain.AddressedHub
	store     *memSchedules
	agent     *scheduleAgent
	approvals <-chan domain.ApprovalRequest
}

func newApprovalE2E(t *testing.T) *approvalE2E {
	t.Helper()
	gw, d := newGateway(), network.NewTurnDelegations()
	hub := domain.NewAddressedHub(domain.AddressedHubConfig{})
	approvals, stop := opRequests(hub)
	t.Cleanup(stop)
	store := &memSchedules{tasks: map[string]domain.ScheduledTask{}}
	sched, err := scheduledtask.New(store, hub, domain.ScheduleLimits{}, e2eWindow)
	if err != nil {
		t.Fatal(err)
	}
	srv := &network.Server{LLMGateway: gw, Delegations: d, ScheduledTasks: sched,
		RunPrincipal: func(context.Context, domain.LeaseBinding) string { return "u1" }}
	agent := &scheduleAgent{srv: srv, called: make(chan struct{}), resp: make(chan *pb.ProposeScheduledTaskResponse, 1)}

	// Wired as app/boot_server.go wires it.
	svc := inflightService(t, gw, agent)
	svc.SetDelegationEnder(d)
	frozen := time.Now()
	svc.SetLivenessProbe(LatestActivity(func(string) time.Time { return frozen }, gw.ConversationActivity, d.ConversationActivity))
	svc.SetPersonWaitProbe(d.PersonWait)
	svc.PersonWaitAllowance = e2eWindow
	svc.StallTimeout = e2eStall
	svc.TurnTimeout = e2eTurn
	svc.stallPoll = 20 * time.Millisecond
	return &approvalE2E{svc: svc, hub: hub, store: store, agent: agent, approvals: approvals}
}

// The person approves at ~150 s (scaled): five stall windows and almost twice
// the turn timeout after the approval was raised. The turn waits for them and
// completes with the created task.
func TestE2E_ApprovalAnsweredLateInItsWindowCompletesTheTurn(t *testing.T) {
	e := newApprovalE2E(t)
	go func() {
		r := <-e.approvals
		time.Sleep(e2eApproval)
		if !opSubmit(e.hub, r.ID, true, "op") {
			t.Errorf("the approval must still be answerable inside its window")
		}
	}()
	start := time.Now()
	got, err := e.svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "set up a recurring task every Monday 09:00"})
	if err != nil {
		t.Fatalf("a turn waiting on a person inside the approval window must not fail: %v", err)
	}
	if el := time.Since(start); el < e2eApproval {
		t.Fatalf("the turn ended after %s, before the person answered", el)
	}
	if !strings.HasPrefix(got.Content, "Done: your weekly task") {
		t.Fatalf("reply %q", got.Content)
	}
	if e.store.count() != 1 {
		t.Fatalf("tasks = %d, want the approved one", e.store.count())
	}
}

// Nobody answers: the turn waits out the approval window (past both the stall
// window and the turn timeout) and ends with a reply saying nothing was created.
func TestE2E_UnansweredApprovalEndsTheTurnAtItsWindowWithAClearReply(t *testing.T) {
	e := newApprovalE2E(t)
	go func() { <-e.approvals }() // a person who never answers
	start := time.Now()
	got, err := e.svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "set up a recurring task every Monday 09:00"})
	if err != nil {
		t.Fatalf("an unanswered approval must end the turn with a reply, not a failure: %v", err)
	}
	if el := time.Since(start); el < e2eWindow {
		t.Fatalf("the turn ended after %s, inside the %s approval window", el, e2eWindow)
	}
	if !strings.Contains(got.Content, "nobody approved the schedule in time; nothing was created") {
		t.Fatalf("reply %q must say no one approved in time and nothing was created", got.Content)
	}
	if e.store.count() != 0 {
		t.Fatal("an unanswered schedule must create nothing")
	}
}

// The turn ends while the approval is pending (the person's client hung up). The
// turn's end withdraws the approval: a late approve is refused with the reason,
// and nothing is created — no silent execution.
func TestE2E_ApproveAfterTheTurnEndedIsRefusedAndRunsNothing(t *testing.T) {
	e := newApprovalE2E(t)
	ctx, cancel := context.WithCancel(context.Background())
	var raised domain.ApprovalRequest
	go func() {
		raised = <-e.approvals
		time.Sleep(2 * e2eStall) // well past the stall window: the turn is still waiting
		cancel()
	}()
	_, err := e.svc.Turn(ctx, TurnRequest{ConversationID: "c1", Text: "set up a recurring task every Monday 09:00"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want the caller's cancellation, got %v", err)
	}
	// The agent's call returns once the turn ended: withdrawn, nothing created.
	select {
	case resp := <-e.agent.resp:
		if resp.GetCreated() || !strings.Contains(resp.GetDetail(), "has ended") {
			t.Fatalf("the tool must report the turn ended and nothing was created, got %+v", resp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the schedule proposal kept waiting after its turn ended")
	}
	_, aerr := e.hub.Answer(context.Background(), raised.ID, domain.ApprovalAnswer{Approved: true, By: domain.OperatorApprover("op")})
	if !errors.Is(aerr, domain.ErrApprovalResolved) {
		t.Fatalf("a late approve must be refused, got %v", aerr)
	}
	for _, want := range []string{"withdrawn", "nothing was done", "chat turn"} {
		if !strings.Contains(aerr.Error(), want) {
			t.Fatalf("the approver's refusal %q must say %q", aerr, want)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if e.store.count() != 0 {
		t.Fatal("a late approve of an ended turn must not create the task")
	}
}
