package chat

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
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
//
// The turn's own clocks — the turn deadline (watchDeadline) and the stall
// watchdog — read an injected e2eClock, not the wall clock
// (T-20261119-teste2e, ENG-2253). The fake time moves only when the test moves
// it, and only while the turn is waiting on the person, so however long a loaded
// machine takes to schedule the in-process work around the wait (seconds, under
// a busy `make per-pr` with -race), it never spends the turn budget or reads as
// silence. The approval itself still runs on the real hub, scheduled-task gate
// and agent-plane RPC; only the turn's reading of time is controlled.

const (
	e2eStall    = 300 * time.Millisecond
	e2eTurn     = 800 * time.Millisecond
	e2eWindow   = 3 * time.Second
	e2eApproval = 1500 * time.Millisecond
	// e2eLongWindow is the approval window of a test whose person answers at once
	// in real time and late in fake time: long enough that the wall-clock
	// backstop (TurnTimeout + PersonWaitAllowance) never decides the test.
	e2eLongWindow = 30 * time.Second
)

// e2eClock is the turn's injected clock. Its time moves only by advanceWaiting,
// which the test calls while the turn waits on the person; the moved time is
// also credited as person-wait time (personWait reports it), as
// network.TurnDelegations accounts a real wait.
type e2eClock struct {
	mu       sync.Mutex
	base     time.Time
	offset   time.Duration
	credit   time.Duration
	progress time.Time
}

func newE2EClock() *e2eClock {
	now := time.Now()
	return &e2eClock{base: now, progress: now}
}

func (c *e2eClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.base.Add(c.offset)
}

// advanceWaiting moves the turn's clock by d of waiting on the person.
func (c *e2eClock) advanceWaiting(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
	c.credit += d
}

// markProgress records that the turn reported progress at the current fake time
// (the tool call returned and the worker carries on).
func (c *e2eClock) markProgress() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.progress = c.base.Add(c.offset)
}

func (c *e2eClock) lastProgress(string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.progress
}

// activeNow maps a real liveness source onto the fake clock: active "now" while
// the source reports any activity, nothing otherwise.
func (c *e2eClock) activeNow(src func(string) time.Time) func(string) time.Time {
	return func(conv string) time.Time {
		if src(conv).IsZero() {
			return time.Time{}
		}
		return c.now()
	}
}

// personWait is the real TurnDelegations.PersonWait's waiting flag, with the
// wait measured on the fake clock.
func (c *e2eClock) personWait(real func(string) (bool, time.Duration)) func(string) (bool, time.Duration) {
	return func(origin string) (bool, time.Duration) {
		waiting, _ := real(origin)
		c.mu.Lock()
		defer c.mu.Unlock()
		return waiting, c.credit
	}
}

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
	// compose is how long, in REAL time, the worker takes to write its reply
	// after the tool returned. The turn's (fake) clock does not move meanwhile,
	// so a correct turn deadline has its whole budget left; one that had spent
	// the person's wait as turn time sees several polls in which to cut the turn.
	compose time.Duration
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
		if a.compose > 0 {
			select {
			case <-time.After(a.compose):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
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
	clock     *e2eClock
	waiting   func() bool // the conversation's turn is waiting on a person right now
}

func newApprovalE2E(t *testing.T, window time.Duration) *approvalE2E {
	t.Helper()
	gw, d := newGateway(), network.NewTurnDelegations()
	hub := domain.NewAddressedHub(domain.AddressedHubConfig{})
	approvals, stop := opRequests(hub)
	t.Cleanup(stop)
	store := &memSchedules{tasks: map[string]domain.ScheduledTask{}}
	sched, err := scheduledtask.New(store, hub, domain.ScheduleLimits{}, window)
	if err != nil {
		t.Fatal(err)
	}
	srv := &network.Server{LLMGateway: gw, Delegations: d, ScheduledTasks: sched,
		RunPrincipal: func(context.Context, domain.LeaseBinding) string { return "u1" }}
	agent := &scheduleAgent{srv: srv, called: make(chan struct{}), resp: make(chan *pb.ProposeScheduledTaskResponse, 1),
		compose: 100 * time.Millisecond}

	// Wired as app/boot_server.go wires it.
	svc := inflightService(t, gw, agent)
	svc.SetDelegationEnder(d)
	// The production probes (app/boot_server.go), read on the injected clock: the
	// worker's last progress stays put unless the test marks it, and the gateway's
	// in-flight calls and a turn waiting on a person count as activity "now".
	clock := newE2EClock()
	svc.now = clock.now
	svc.SetLivenessProbe(LatestActivity(clock.lastProgress,
		clock.activeNow(gw.ConversationActivity), clock.activeNow(d.ConversationActivity)))
	svc.SetPersonWaitProbe(clock.personWait(d.PersonWait))
	svc.PersonWaitAllowance = window
	svc.StallTimeout = e2eStall
	svc.TurnTimeout = e2eTurn
	svc.stallPoll = 20 * time.Millisecond
	waiting := func() bool { return !d.ConversationActivity("c1").IsZero() }
	return &approvalE2E{svc: svc, hub: hub, store: store, agent: agent, approvals: approvals,
		clock: clock, waiting: waiting}
}

// The person approves at ~150 s (scaled): on the turn's clock, five stall
// windows and twice the turn timeout after the approval was raised. The turn
// waits for them and completes with the created task.
//
// Deterministic under load (ENG-2253): the turn's clock moves only while the real
// approval hub holds the turn waiting on the person, so no scheduling delay before
// or after the wait can spend the turn budget. Were the deadline NOT paused for
// the person, or the wait read as silence, the 1.6 s of waiting would exceed the
// 800 ms budget / 300 ms stall window and the turn would fail — the property
// under test.
func TestE2E_ApprovalAnsweredLateInItsWindowCompletesTheTurn(t *testing.T) {
	e := newApprovalE2E(t, e2eLongWindow)
	var answered atomic.Bool
	go func() {
		r := <-e.approvals
		// The turn must be waiting on the person (the real TurnDelegations says
		// so) before its clock moves; the hub has raised the item, so this is brief.
		for until := time.Now().Add(10 * time.Second); !e.waiting(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(until) {
				t.Errorf("the turn never reported waiting on the person")
				return
			}
		}
		e.clock.advanceWaiting(2 * e2eTurn)
		// Both watchdogs observe the long wait for several polls before the answer.
		time.Sleep(5 * e.svc.stallPoll)
		answered.Store(true)
		e.clock.markProgress() // the tool call returns; the worker carries on
		if !opSubmit(e.hub, r.ID, true, "op") {
			t.Errorf("the approval must still be answerable inside its window")
		}
	}()
	got, err := e.svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "set up a recurring task every Monday 09:00"})
	if err != nil {
		t.Fatalf("a turn waiting on a person inside the approval window must not fail: %v", err)
	}
	if !answered.Load() {
		t.Fatal("the turn ended before the person answered")
	}
	if waited := e.clock.now().Sub(e.clock.base); waited < e2eApproval {
		t.Fatalf("the turn's clock moved %s while it waited, want at least %s", waited, e2eApproval)
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
	e := newApprovalE2E(t, e2eWindow)
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
	e := newApprovalE2E(t, e2eWindow)
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
