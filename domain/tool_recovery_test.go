package domain

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// T-20261118-a-sync-approval-orphaned-by-a-kernel-restart, the tool side: a
// dangerous call whose approval is approved after a restart runs once, through
// every check again, for the person it was for.

type recoveryHandler struct {
	mu   sync.Mutex
	args []string
}

func (c *recoveryHandler) Execute(_ context.Context, call ToolCall) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.args = append(c.args, string(call.ArgsJSON))
	return []byte(`{"key":"OPS-7"}`), nil
}
func (c *recoveryHandler) calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.args...)
}

type toolRestartFixture struct {
	store   *MemoryApprovalStore
	actions *MemActionRecordStore
	reg     *InMemoryToolRegistry
	grants  *InMemoryGrantsStore
	item    ApprovalItem
}

const recoveryArgs = `{"project_key":"OPS","summary":"Database outage"}`

// raiseThenCrash raises the dangerous call on kernel run 1 and leaves its item
// pending the way a killed process does (a crash runs no deferred code, so the
// withdraw the cancel caused is undone).
func raiseThenCrash(t *testing.T) *toolRestartFixture {
	t.Helper()
	f := &toolRestartFixture{store: NewMemoryApprovalStore(), actions: NewMemActionRecordStore(), reg: NewInMemoryToolRegistry()}
	f.reg.Register(SystemTool{Name: "create_issue", Dangerous: true, Effects: []ToolEffect{EffectWrite}})
	f.grants = grantStore("assistant", ToolGrant{Tool: "create_issue"})
	h1 := NewAddressedHub(AddressedHubConfig{Store: f.store, ActionRecords: f.actions, DefaultTimeout: time.Minute})
	run1 := &recoveryHandler{}
	e1 := newExec(f.reg, f.grants, run1)
	e1.approval = h1
	ch, cancel := h1.Watch(ApprovalWatch{For: OperatorApprover("op")})
	defer cancel()
	ctx, die := context.WithCancel(WithConversationOwner(WithConversationID(context.Background(), "conv-1"), "alice"))
	done := make(chan ToolCallResponse, 1)
	go func() {
		done <- e1.Execute(ctx, ToolCallRequest{AgentID: "assistant", ToolName: "create_issue", ArgsJSON: []byte(recoveryArgs), SessionTokenID: "lease-1"})
	}()
	f.item = nextRaised(t, ch)
	die()
	<-done
	if len(run1.calls()) != 0 {
		t.Fatal("run 1 ran the call without an approval")
	}
	if f.item.Intent == nil || f.item.Intent.Tool != "create_issue" || string(f.item.Intent.Args) != recoveryArgs ||
		f.item.Intent.ConversationOwner != "alice" || f.item.Intent.Lease != "lease-1" {
		t.Fatalf("the approval did not store the call's intent: %+v", f.item.Intent)
	}
	f.item.State = ApprovalPending
	if err := f.store.PutApproval(context.Background(), f.item); err != nil {
		t.Fatal(err)
	}
	return f
}

// restart is kernel run 2 over the same stores.
func (f *toolRestartFixture) restart(dir PrincipalDirectory) (*AddressedHub, *recoveryHandler) {
	h2 := NewAddressedHub(AddressedHubConfig{Store: f.store, ActionRecords: f.actions})
	run2 := &recoveryHandler{}
	e2 := newExec(f.reg, f.grants, run2)
	e2.approval = h2
	h2.RecoverWith(ApprovalIntentTool, e2.ApprovalRecoverer(func() PrincipalDirectory { return dir }))
	return h2, run2
}

func TestToolRecovery_ApprovedAfterRestart_RunsOnce(t *testing.T) {
	f := raiseThenCrash(t)
	h2, run2 := f.restart(fakeDirectory{"alice": true})
	res, err := h2.Answer(context.Background(), f.item.ID, ApprovalAnswer{Approved: true, By: OperatorApprover("op")})
	if err != nil || res.State != ApprovalApproved || !strings.Contains(res.Detail, `create_issue ran: {"key":"OPS-7"}`) {
		t.Fatalf("approve after restart: %+v %v", res, err)
	}
	if _, err := h2.Answer(context.Background(), f.item.ID, ApprovalAnswer{Approved: true, By: OperatorApprover("op")}); !errors.Is(err, ErrApprovalResolved) {
		t.Fatalf("second approve: %v", err)
	}
	if calls := run2.calls(); len(calls) != 1 || calls[0] != recoveryArgs {
		t.Fatalf("calls = %q, want the approved call once", calls)
	}
}

func TestToolRecovery_PersonGone_Refused(t *testing.T) {
	f := raiseThenCrash(t)
	h2, run2 := f.restart(fakeDirectory{}) // alice left
	res, err := h2.Answer(context.Background(), f.item.ID, ApprovalAnswer{Approved: true, By: OperatorApprover("op")})
	if !errors.Is(err, ErrApprovalNotCarriedOut) || res.State != ApprovalWithdrawn ||
		!strings.Contains(err.Error(), "alice is no longer a known, active person") {
		t.Fatalf("approve for a departed person: %+v %v", res, err)
	}
	if len(run2.calls()) != 0 {
		t.Fatal("ran for a departed person")
	}
}

func TestToolRecovery_GrantRevoked_Refused(t *testing.T) {
	f := raiseThenCrash(t)
	f.grants.Set("assistant", nil) // the access rules changed while the kernel was down
	h2, run2 := f.restart(fakeDirectory{"alice": true})
	res, err := h2.Answer(context.Background(), f.item.ID, ApprovalAnswer{Approved: true, By: OperatorApprover("op")})
	if !errors.Is(err, ErrApprovalNotCarriedOut) || res.State != ApprovalWithdrawn || !strings.Contains(err.Error(), "tool not granted") {
		t.Fatalf("approve after the grant was revoked: %+v %v", res, err)
	}
	if len(run2.calls()) != 0 {
		t.Fatal("ran without a grant")
	}
}

func TestToolRecovery_NoDirectory_FailsClosed(t *testing.T) {
	f := raiseThenCrash(t)
	h2, run2 := f.restart(nil)
	if _, err := h2.Answer(context.Background(), f.item.ID, ApprovalAnswer{Approved: true, By: OperatorApprover("op")}); !errors.Is(err, ErrApprovalNotCarriedOut) {
		t.Fatalf("no directory must refuse: %v", err)
	}
	if len(run2.calls()) != 0 {
		t.Fatal("ran with nothing to confirm the person")
	}
}

// The pre-approval covers exactly the approved call and cannot be forged by a
// recoverer handed an item the hub does not hold as approved.
func TestToolRecovery_UnapprovedItem_RunsNothing(t *testing.T) {
	f := raiseThenCrash(t)
	h2, run2 := f.restart(fakeDirectory{"alice": true})
	e := newExec(f.reg, f.grants, run2)
	e.approval = h2
	r := e.ApprovalRecoverer(func() PrincipalDirectory { return fakeDirectory{"alice": true} })
	forged := f.item
	forged.State = ApprovalApproved // the store still says pending
	if _, err := r.Recover(context.Background(), forged); !errors.Is(err, ErrRecoveryRefused) {
		t.Fatalf("a pending item must not run: %v", err)
	}
	if len(run2.calls()) != 0 {
		t.Fatal("a forged approval ran the call")
	}
}

// Calls whose authority is lease or fleet state store no intent.
func TestToolIntent_NoneForSubtaskOrContributedCalls(t *testing.T) {
	req := ToolCallRequest{AgentID: "a", ToolName: "t"}
	if toolIntent(context.Background(), req, nil, true, false) != nil || toolIntent(context.Background(), req, nil, false, true) != nil {
		t.Fatal("a subtask or contributed call stored an intent")
	}
	ctx := WithStepCall(WithRunPrincipal(context.Background(), "bob"), StepCall{RunID: "run-1", StepIndex: 2})
	in := toolIntent(ctx, req, []byte(`{}`), false, false)
	if in == nil || in.OnBehalfOf != "bob" || !in.HasStep || in.RunID != "run-1" || in.StepIndex != 2 || in.Person() != "bob" {
		t.Fatalf("intent = %+v", in)
	}
	back := in.Context(context.Background())
	if sc, ok := StepCallFrom(back); !ok || sc.RunID != "run-1" || RunPrincipalFromContext(back) != "bob" {
		t.Fatal("the intent's context lost the step or the person")
	}
}
