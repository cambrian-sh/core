package chat

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// The turn deadline pauses while the turn waits on a person, within the
// person-wait allowance (SetPersonWaitProbe; T-20261118-a-chat-turn-waiting-on-a-
// human-approval-is). The probe is scripted here; approval_wait_e2e_test.go drives
// the real one.

// deadlineOnlyPool never answers: the turn ends only by its deadline.
type deadlineOnlyPool struct {
	mu  sync.Mutex
	got *domain.Handoff
}

func (p *deadlineOnlyPool) Dispatch(ctx context.Context, h *domain.Handoff) (*domain.Handoff, error) {
	p.mu.Lock()
	p.got = h
	p.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func personWaitService(t *testing.T, pool Dispatcher, probe func(string) (bool, time.Duration)) *TurnService {
	t.Helper()
	svc := NewTurnService(newMemStore(), pool, nil)
	if err := svc.store.CreateConversation(context.Background(), domain.Conversation{
		ID: "c1", OwnerID: "u1", Status: domain.ConversationOpen, Profile: domain.ProfileEmployee,
	}); err != nil {
		t.Fatal(err)
	}
	svc.StallTimeout = 0
	svc.TurnTimeout = 300 * time.Millisecond
	svc.PersonWaitAllowance = 10 * time.Second
	svc.stallPoll = 10 * time.Millisecond
	svc.SetPersonWaitProbe(probe)
	return svc
}

// A turn that never waits on a person still ends at TurnTimeout, not at
// TurnTimeout + the allowance its RPC deadline carries.
func TestTurn_WithoutAWaitTheDeadlineIsTurnTimeout(t *testing.T) {
	svc := personWaitService(t, &deadlineOnlyPool{}, func(string) (bool, time.Duration) { return false, 0 })
	start := time.Now()
	_, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if el := time.Since(start); el < 300*time.Millisecond || el > 3*time.Second {
		t.Fatalf("ended after %s, want about the 300 ms turn timeout", el)
	}
}

// Time waiting on a person does not spend the turn's budget.
func TestTurn_TimeWaitingOnAPersonDoesNotSpendTheBudget(t *testing.T) {
	var mu sync.Mutex
	var since time.Time
	// The turn waits on a person for its first 600 ms (twice the turn timeout).
	probe := func(string) (bool, time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		if since.IsZero() {
			since = time.Now()
		}
		el := time.Since(since)
		if el < 600*time.Millisecond {
			return true, el
		}
		return false, 600 * time.Millisecond
	}
	pool := &deadlineOnlyPool{}
	svc := personWaitService(t, pool, probe)
	start := time.Now()
	_, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if el := time.Since(start); el < 900*time.Millisecond {
		t.Fatalf("ended after %s: the 600 ms wait must not count against the 300 ms budget", el)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if got := pool.got.Context[HandoffKeyTurnBudgetMs]; got != "300" {
		t.Fatalf("the worker must be told its work budget, got %q", got)
	}
}

// The pause is bounded by the allowance: a wait longer than it is turn time.
func TestTurn_PersonWaitBeyondTheAllowanceSpendsTheBudget(t *testing.T) {
	svc := personWaitService(t, &deadlineOnlyPool{}, func(string) (bool, time.Duration) { return true, time.Hour })
	svc.PersonWaitAllowance = 200 * time.Millisecond
	start := time.Now()
	_, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("a wait past the allowance must not hold the turn open (%s)", el)
	}
}

// Without an allowance (or a probe) nothing changes: no budget key, wall-clock deadline.
func TestTurn_NoAllowanceKeepsTheWallClockDeadline(t *testing.T) {
	pool := &deadlineOnlyPool{}
	svc := personWaitService(t, pool, func(string) (bool, time.Duration) { return true, 0 })
	svc.PersonWaitAllowance = 0
	start := time.Now()
	_, err := svc.Turn(context.Background(), TurnRequest{ConversationID: "c1", Text: "hi"})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatalf("want the plain 300 ms deadline, got %v after %s", err, time.Since(start))
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if _, ok := pool.got.Context[HandoffKeyTurnBudgetMs]; ok {
		t.Fatal("no budget key without a person-wait allowance")
	}
}
