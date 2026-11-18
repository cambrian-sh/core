package domain

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Waiting on a person (approval_wait.go; T-20261118-a-chat-turn-waiting-on-a-human-
// approval-is): the hub reports the exact span a requester blocks on a person, and a
// late answer to a withdrawn request is refused with the reason.

func TestApprovalWaitHook_ChainsAndIsSafeWithoutOne(t *testing.T) {
	BeginApprovalWait(context.Background())() // no hook: a no-op

	var log []string
	ctx := WithApprovalWaitHook(context.Background(), func() func() {
		log = append(log, "outer+")
		return func() { log = append(log, "outer-") }
	})
	ctx = WithApprovalWaitHook(ctx, func() func() {
		log = append(log, "inner+")
		return func() { log = append(log, "inner-") }
	})
	BeginApprovalWait(ctx)()
	if got := strings.Join(log, ","); got != "outer+,inner+,inner-,outer-" {
		t.Fatalf("hook order %s", got)
	}
}

// The hub marks the requester as waiting from the moment it blocks until the
// answer, and not before raise or after it returns.
func TestAddressedHub_ReportsTheWaitOnAPersonForExactlyTheBlockingSpan(t *testing.T) {
	h := NewAddressedHub(AddressedHubConfig{DefaultTimeout: 5 * time.Second})
	evs, stop := h.Watch(opWatch("op-1"))
	defer stop()
	var waiting atomic.Int32
	var waits atomic.Int32
	ctx := WithApprovalWaitHook(context.Background(), func() func() {
		waiting.Add(1)
		waits.Add(1)
		return func() { waiting.Add(-1) }
	})
	dec := raiseAsync(ctx, h, ApprovalRequest{AgentID: "a", ToolName: "execute_code"})
	it := nextRaised(t, evs)
	deadline := time.Now().Add(3 * time.Second)
	for waiting.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the requester blocked on a person without reporting the wait")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, By: OperatorApprover("op-1")}); err != nil {
		t.Fatal(err)
	}
	if d := <-dec; !d.Approved {
		t.Fatalf("decision %+v", d)
	}
	if waiting.Load() != 0 || waits.Load() != 1 {
		t.Fatalf("after the answer: waiting=%d waits=%d, want 0 and 1", waiting.Load(), waits.Load())
	}
}

// A requester whose context is cancelled with a cause (a chat turn that ended)
// withdraws its approval with that cause; a late approve is refused, says nothing
// was done and why, and runs nothing.
func TestAddressedHub_LateAnswerToAWithdrawnRequestIsRefusedWithTheReason(t *testing.T) {
	h := NewAddressedHub(AddressedHubConfig{DefaultTimeout: 5 * time.Second})
	evs, stop := h.Watch(opWatch("op-1"))
	defer stop()
	ctx, cancel := context.WithCancelCause(context.Background())
	turnEnded := errors.New("the chat turn that ordered this work has ended")
	dec := raiseAsync(ctx, h, ApprovalRequest{AgentID: "a", ToolName: "execute_code"})
	it := nextRaised(t, evs)
	cancel(errors.Join(turnEnded, errors.New("internal: lease xyz revoked")))
	if d := <-dec; d.Approved {
		t.Fatal("a withdrawn request must not be approved")
	}
	res := nextResolved(t, evs)
	if res.State != ApprovalWithdrawn || !strings.Contains(res.Detail, turnEnded.Error()) || strings.Contains(res.Detail, "internal") {
		t.Fatalf("resolution %s %q: want withdrawn with the first line of the cause only", res.State, res.Detail)
	}
	_, err := h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, By: OperatorApprover("op-1")})
	if !errors.Is(err, ErrApprovalResolved) {
		t.Fatalf("want ErrApprovalResolved, got %v", err)
	}
	for _, want := range []string{"withdrawn", "nothing was done", turnEnded.Error()} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q must say %q", err, want)
		}
	}
}
