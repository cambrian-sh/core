package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/agentpool"
)

// ENG-1634 (owner decision 2026-11-20): a turn whose worker is killed is re-driven
// ONCE, on the same lease, only when the killed attempt began no side-effecting
// call; otherwise it fails loudly, and a second kill fails it too.

// redrivePool answers each dispatch with the next scripted outcome; before
// answering it runs the step's calls (the side-effecting calls the killed attempt
// "began") through the turn record, as the tool executor does.
type redrivePool struct {
	mu     sync.Mutex
	steps  []redriveStep
	leases []domain.LeaseID
	calls  *domain.TurnCalls
}

type redriveStep struct {
	err   error
	reply string
	// begin names side-effecting tools the attempt begins before its outcome.
	begin []string
}

func (p *redrivePool) Dispatch(_ context.Context, h *domain.Handoff) (*domain.Handoff, error) {
	p.mu.Lock()
	n := len(p.leases)
	lease := domain.LeaseID(h.Context["_session_token_id"])
	p.leases = append(p.leases, lease)
	step := redriveStep{reply: "fallback reply"}
	if n < len(p.steps) {
		step = p.steps[n]
	}
	p.mu.Unlock()
	for _, tool := range step.begin {
		p.calls.Begin(lease, tool, []byte(`{"to":"x"}`))
	}
	if step.err != nil {
		return nil, step.err
	}
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte(step.reply)}}, nil
}

func (p *redrivePool) dispatches() []domain.LeaseID {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]domain.LeaseID(nil), p.leases...)
}

func setupRedrive(t *testing.T, track bool, steps ...redriveStep) (*TurnService, *redrivePool, *domain.TurnCalls) {
	t.Helper()
	store := newMemStore()
	if err := store.CreateConversation(context.Background(), domain.Conversation{
		ID: "c1", OwnerID: "u1", Status: domain.ConversationOpen, Profile: domain.ProfileEmployee,
	}); err != nil {
		t.Fatal(err)
	}
	calls := domain.NewTurnCalls(nil)
	pool := &redrivePool{steps: steps, calls: calls}
	n := 0
	svc := NewTurnService(store, pool, func(context.Context, int, time.Duration) (string, func(), error) {
		n++
		return fmt.Sprintf("lease-%d", n), func() {}, nil
	})
	if track {
		svc.SetTurnCalls(calls)
	}
	return svc, pool, calls
}

func turnOnce(t *testing.T, svc *TurnService) (domain.Message, error) {
	t.Helper()
	return svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "send the summary to the team"})
}

// Killed before any side-effecting call: re-driven once on the SAME lease, and
// the person gets the answer.
func TestRedrive_AKilledTurnThatBeganNothingIsRedrivenOnceOnTheSameLease(t *testing.T) {
	for name, kill := range map[string]error{
		"mid-turn (Unavailable)": rawWorkerKill,
		"lost at dispatch":       fmt.Errorf("%w: %v", agentpool.ErrWorkerLost, rawWorkerKill),
	} {
		t.Run(name, func(t *testing.T) {
			svc, pool, calls := setupRedrive(t, true, redriveStep{err: kill}, redriveStep{reply: "done: summary sent"})
			msg, err := turnOnce(t, svc)
			if err != nil || msg.Content != "done: summary sent" {
				t.Fatalf("turn = %+v, %v; want the re-driven answer", msg, err)
			}
			d := pool.dispatches()
			if len(d) != 2 || d[0] != d[1] || d[0] == "" {
				t.Fatalf("dispatches = %v, want two on the same lease", d)
			}
			if n, _ := calls.Begun(d[0]); n != 0 {
				t.Fatalf("the turn's lease is still tracked after the turn (begun=%d)", n)
			}
		})
	}
}

// Killed after it began a side-effecting call: not re-run (it could do it twice);
// the person is told plainly, naming nothing internal, and the cause names the tool.
func TestRedrive_AKilledTurnThatBeganAnActionFailsLoudly(t *testing.T) {
	svc, pool, _ := setupRedrive(t, true, redriveStep{err: rawWorkerKill, begin: []string{"send_email"}}, redriveStep{reply: "must not run"})
	_, err := turnOnce(t, svc)
	var ti *domain.TurnInterruption
	if !errors.As(err, &ti) || ti.Reason != domain.TurnInterruptAssistantRestartedAfterAction {
		t.Fatalf("err = %v, want assistant_restarted_after_action", err)
	}
	assertPlain(t, "after-action message", ti.Message)
	if !strings.Contains(ti.Cause.Error(), "send_email") {
		t.Errorf("the cause (for logs) must name what already began: %v", ti.Cause)
	}
	if n := len(pool.dispatches()); n != 1 {
		t.Fatalf("%d dispatches, want 1 — a turn that began an action is never re-run", n)
	}
}

// The re-drive's worker is killed too: the turn ends, loudly, and no third attempt.
func TestRedrive_ASecondKillEndsTheTurn(t *testing.T) {
	svc, pool, _ := setupRedrive(t, true, redriveStep{err: rawWorkerKill}, redriveStep{err: rawWorkerKill}, redriveStep{reply: "must not run"})
	_, err := turnOnce(t, svc)
	var ti *domain.TurnInterruption
	if !errors.As(err, &ti) || ti.Reason != domain.TurnInterruptAssistantRestartedTwice {
		t.Fatalf("err = %v, want assistant_restarted_twice", err)
	}
	assertPlain(t, "twice message", ti.Message)
	if n := len(pool.dispatches()); n != 2 {
		t.Fatalf("%d dispatches, want exactly 2", n)
	}
}

// Without the turn record the lane cannot tell what the attempt began: no re-drive,
// the old plain "assistant restarted".
func TestRedrive_WithoutTheTurnRecordAKilledTurnIsNotRerun(t *testing.T) {
	svc, pool, _ := setupRedrive(t, false, redriveStep{err: rawWorkerKill}, redriveStep{reply: "must not run"})
	_, err := turnOnce(t, svc)
	var ti *domain.TurnInterruption
	if !errors.As(err, &ti) || ti.Reason != domain.TurnInterruptAssistantRestarted {
		t.Fatalf("err = %v, want assistant_restarted", err)
	}
	if n := len(pool.dispatches()); n != 1 {
		t.Fatalf("%d dispatches, want 1", n)
	}
}

// A failure that is not a worker loss (here a provider outage the gateway
// recorded on the lease) is never re-driven.
func TestRedrive_OnlyAWorkerLossIsRedriven(t *testing.T) {
	svc, pool, _ := setupRedrive(t, true, redriveStep{err: rawWorkerKill}, redriveStep{reply: "must not run"})
	svc.SetLeaseFaultProbe(func(domain.LeaseID) domain.ProviderFault { return domain.ProviderFaultUnavailable })
	_, err := turnOnce(t, svc)
	var ti *domain.TurnInterruption
	if !errors.As(err, &ti) || ti.Reason != domain.TurnInterruptProviderUnavailable {
		t.Fatalf("err = %v, want provider_unavailable", err)
	}
	if n := len(pool.dispatches()); n != 1 {
		t.Fatalf("%d dispatches, want 1", n)
	}
	svc2, pool2, _ := setupRedrive(t, true, redriveStep{err: agentpool.ErrPoolBusy}, redriveStep{reply: "must not run"})
	if _, err := turnOnce(t, svc2); err == nil || len(pool2.dispatches()) != 1 {
		t.Fatalf("a shed turn must not be re-driven: %v (%d dispatches)", err, len(pool2.dispatches()))
	}
}

// ENG-2266: a re-sent turn whose dispatch could not even dial the worker never
// reached it — it is "not delivered", not a second kill: the lane re-sends again
// (within the deadline) and the person gets the answer. Only the dial failure is
// trusted for that; a loss that reached the worker still counts.
func TestRedrive_AReSendThatCouldNotDialIsNotASecondKill(t *testing.T) {
	dial := status.Error(codes.Unavailable, `connection error: desc = "transport: Error while dialing: dial unix x.sock: connect: No connection could be made"`)
	svc, pool, _ := setupRedrive(t, true,
		redriveStep{err: rawWorkerKill}, redriveStep{err: dial}, redriveStep{err: dial}, redriveStep{reply: "done after the replacement"})
	msg, err := turnOnce(t, svc)
	if err != nil || msg.Content != "done after the replacement" {
		t.Fatalf("turn = %+v, %v; undelivered re-sends must not end the turn", msg, err)
	}
	if n := len(pool.dispatches()); n != 4 {
		t.Fatalf("%d dispatches, want 4 (loss, two undelivered re-sends, the answer)", n)
	}
	if agentpool.NotDelivered(rawWorkerKill) || !agentpool.NotDelivered(dial) {
		t.Fatal("only a dial failure means not delivered")
	}
	// The pool wraps its cause (%w): ErrWorkerLost decides nothing by itself.
	if !agentpool.NotDelivered(fmt.Errorf("%w: %w", agentpool.ErrWorkerLost, dial)) ||
		agentpool.NotDelivered(fmt.Errorf("%w: %w", agentpool.ErrWorkerLost, rawWorkerKill)) {
		t.Fatal("ErrWorkerLost means not delivered only when its cause was")
	}
	// The daemon layer refusing before any call (the stream has no live instance).
	refused := fmt.Errorf("CallDaemon: %w: no daemon registered for stream %q", domain.ErrDaemonUnavailable, "chat-1")
	if !agentpool.NotDelivered(fmt.Errorf("%w: %w", agentpool.ErrWorkerLost, refused)) {
		t.Fatal("a call the daemon layer refused never reached a worker")
	}
}
