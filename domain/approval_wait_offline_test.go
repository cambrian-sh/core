package domain

import (
	"context"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// ENG-653 (owner decision 2026-11-15): a rule with WaitOffline parks a BLOCKING
// request whose approvers have no connected surface, instead of failing it at
// once as unroutable; the person answers it later and the caller gets that
// answer. Without the flag the historical fail-closed stays.
func TestHub_WaitOfflineParksTheRequestUntilThePersonAnswers(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	pol, err := NewConfigApprovalPolicy(ApprovalPolicyConfig{Rules: []ApprovalClassRule{
		{Class: ApprovalClassTool, Approvers: []string{"operator"}, WaitOffline: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	h := NewAddressedHub(AddressedHubConfig{Policy: pol, DefaultTimeout: 3 * time.Second})

	done := raiseAsync(context.Background(), h, ApprovalRequest{ToolName: "execute_command"})
	// Nobody is connected: the item must be pending, not unroutable.
	var pending []ApprovalItem
	deadline := time.Now().Add(2 * time.Second)
	for len(pending) == 0 && time.Now().Before(deadline) {
		pending, _ = h.List(context.Background(), ApprovalQuery{States: []ApprovalState{ApprovalPending}})
		time.Sleep(10 * time.Millisecond)
	}
	if len(pending) != 1 {
		t.Fatalf("the request must be parked pending while nobody is connected, got %+v", pending)
	}
	// The operator connects later and answers from the inbox.
	if _, err := h.Answer(context.Background(), pending[0].ID, ApprovalAnswer{Approved: true, By: OperatorApprover("op-1")}); err != nil {
		t.Fatal(err)
	}
	if d := <-done; !d.Approved {
		t.Fatalf("the parked request must carry the late answer: %+v", d)
	}
}

func TestHub_WithoutWaitOfflineStillFailsClosedAtOnce(t *testing.T) {
	pol, err := NewConfigApprovalPolicy(ApprovalPolicyConfig{Rules: []ApprovalClassRule{
		{Class: ApprovalClassTool, Approvers: []string{"operator"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	h := NewAddressedHub(AddressedHubConfig{Policy: pol, DefaultTimeout: 3 * time.Second})
	start := time.Now()
	d, err := h.Request(context.Background(), ApprovalRequest{ToolName: "execute_command"})
	if err != nil || d.Approved || d.State != ApprovalUnroutable || time.Since(start) > time.Second {
		t.Fatalf("default: unroutable at once, got %+v %v after %s", d, err, time.Since(start))
	}
}
