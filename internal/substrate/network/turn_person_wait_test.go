package network

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/scheduledtask"
)

// A turn waiting on a person (T-20261118-a-chat-turn-waiting-on-a-human-approval-is,
// ENG-2119): the approval waits of the turn's tool calls are the turn's, counted
// once when they overlap, read as live work by the stall probe, and forgotten
// with the turn.

type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *stepClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestTurnDelegations_PersonWaitIsTheTurnsAndCountsOverlapOnce(t *testing.T) {
	t.Parallel()
	d := NewTurnDelegations()
	clock := &stepClock{t: time.Unix(1_000_000, 0)}
	d.now = clock.now
	b := domain.LeaseBinding{ConversationID: "c1", OriginMessageID: "m1"}

	g1, rel1, ok := d.GuardToolCall(t.Context(), "turn-lease", b, true)
	if !ok {
		t.Fatal("a live turn's tool call is admitted")
	}
	g2, rel2, _ := d.GuardToolCall(t.Context(), "turn-lease", b, true)
	if w, waited := d.PersonWait("m1"); w || waited != 0 {
		t.Fatalf("before any approval: waiting=%v waited=%s", w, waited)
	}

	end1 := domain.BeginApprovalWait(g1)
	clock.add(10 * time.Second)
	end2 := domain.BeginApprovalWait(g2) // overlaps the first
	clock.add(20 * time.Second)
	if w, waited := d.PersonWait("m1"); !w || waited != 30*time.Second {
		t.Fatalf("during: waiting=%v waited=%s, want true 30s", w, waited)
	}
	if got := d.ConversationActivity("c1"); !got.Equal(clock.now()) {
		t.Fatalf("a turn waiting on a person is live work now, got %s", got)
	}
	end1()
	end1() // idempotent
	clock.add(5 * time.Second)
	end2()
	clock.add(time.Hour) // time after the wait is not waiting
	if w, waited := d.PersonWait("m1"); w || waited != 35*time.Second {
		t.Fatalf("after: waiting=%v waited=%s, want false 35s", w, waited)
	}
	rel1()
	rel2()
	// The waited time outlives the calls: it counts for the rest of the turn.
	if _, waited := d.PersonWait("m1"); waited != 35*time.Second {
		t.Fatalf("waited forgotten when the calls returned: %s", waited)
	}

	d.EndTurn("c1", "m1", "turn-lease", nil)
	if w, waited := d.PersonWait("m1"); w || waited != 0 {
		t.Fatalf("an ended turn waits on nobody: %v %s", w, waited)
	}
	if w, _ := d.PersonWait(""); w {
		t.Fatal("no origin, no wait")
	}
}

// A wait that ends after its turn did counts nothing and does not resurrect it.
func TestTurnDelegations_WaitEndingAfterTheTurnIsIgnored(t *testing.T) {
	t.Parallel()
	d := NewTurnDelegations()
	b := domain.LeaseBinding{ConversationID: "c1", OriginMessageID: "m1"}
	g, rel, _ := d.GuardToolCall(t.Context(), "turn-lease", b, true)
	defer rel()
	end := domain.BeginApprovalWait(g)
	d.EndTurn("c1", "m1", "turn-lease", nil)
	end()
	if w, waited := d.PersonWait("m1"); w || waited != 0 {
		t.Fatalf("got %v %s", w, waited)
	}
	if d.beginPersonWait("m1")(); func() bool { w, _ := d.PersonWait("m1"); return w }() {
		t.Fatal("a wait reported for an ended turn must count nothing")
	}
}

// ProposeScheduledTask is admitted like a tool call: a lease of an ended turn is
// refused before any approval is raised, and a proposal waiting on its approval
// when the turn ends is withdrawn — nothing is created, by a late approve either.
func TestProposeScheduledTask_IsTurnWorkAndEndsWithItsTurn(t *testing.T) {
	t.Parallel()
	s, gw := newLeaseServer(t)
	d := NewTurnDelegations()
	s.Delegations = d
	hub := domain.NewAddressedHub(domain.AddressedHubConfig{})
	evs, stop := hub.Watch(domain.ApprovalWatch{For: domain.OperatorApprover("op")})
	defer stop()
	store := &memSchedules{tasks: map[string]domain.ScheduledTask{}}
	svc, err := scheduledtask.New(store, hub, domain.ScheduleLimits{}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s.ScheduledTasks = svc
	s.RunPrincipal = func(context.Context, domain.LeaseBinding) string { return "lena" }
	lease := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "assistant", ConversationID: "c1", OriginMessageID: "m1"})
	req := &pb.ProposeScheduledTaskRequest{LeaseId: string(lease), Goal: "weekly summary", Cron: "0 8 * * 1"}

	type out struct {
		resp *pb.ProposeScheduledTaskResponse
		err  error
	}
	done := make(chan out, 1)
	go func() {
		// The worker's own RPC context: the turn's cancel does not reach it.
		r, err := s.ProposeScheduledTask(agentPlaneCtx("assistant"), req)
		done <- out{r, err}
	}()
	var id string
	select {
	case ev := <-evs:
		id = ev.Item.ID
	case <-time.After(3 * time.Second):
		t.Fatal("no approval raised")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if w, _ := d.PersonWait("m1"); w {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the proposal's approval wait is not counted as the turn waiting on a person")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if d.ConversationActivity("c1").IsZero() {
		t.Fatal("the waiting proposal is live work of the conversation")
	}

	d.EndTurn("c1", "m1", lease, nil)
	select {
	case o := <-done:
		if o.err != nil || o.resp.GetCreated() || !strings.Contains(o.resp.GetDetail(), "has ended") {
			t.Fatalf("withdrawn proposal: %+v %v", o.resp, o.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the proposal kept waiting after its turn ended")
	}
	if _, err := hub.Answer(context.Background(), id, domain.ApprovalAnswer{Approved: true, By: domain.OperatorApprover("op")}); err == nil ||
		!strings.Contains(err.Error(), "nothing was done") {
		t.Fatalf("a late approve must be refused with the reason, got %v", err)
	}
	if len(store.tasks) != 0 {
		t.Fatal("nothing may be created for an ended turn")
	}

	// A new proposal on the ended turn's lease is refused before any approval.
	r, err := s.ProposeScheduledTask(agentPlaneCtx("assistant"), req)
	if err != nil || r.GetCreated() || r.GetRejection() != pb.ScheduledTaskRejection_SCHEDULED_TASK_REJECTION_NO_DECISION {
		t.Fatalf("ended turn's lease: %+v %v", r, err)
	}
	select {
	case ev := <-evs:
		if ev.Type == domain.ApprovalEventRaised {
			t.Fatalf("an approval was raised for an ended turn: %+v", ev.Item)
		}
	case <-time.After(100 * time.Millisecond):
	}
}
