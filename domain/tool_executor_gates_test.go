package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ENG-2251: Execute's gate order and outcomes, pinned before its stages were
// extracted into named steps. Each row drives one path through the chokepoint
// and records what an observer outside Execute can see: the response, whether
// the handler ran, whether an approval was asked, what the ledger recorded and
// what the conversation's action history kept once the deferred hooks ran.

type gateApproval struct {
	approve bool
	asked   int
	last    ApprovalRequest
}

func (g *gateApproval) Request(_ context.Context, r ApprovalRequest) (ApprovalDecision, error) {
	g.asked++
	g.last = r
	if !g.approve {
		return ApprovalDecision{Approved: false, State: ApprovalDenied, ApproverID: "op-1", ApprovalID: "apr-1"}, nil
	}
	return ApprovalDecision{Approved: true, ApproverID: "op-1", ApprovalID: "apr-1"}, nil
}

type gateHandler struct {
	result []byte
	err    error
	calls  int
}

func (h *gateHandler) Execute(context.Context, ToolCall) ([]byte, error) {
	h.calls++
	return h.result, h.err
}

type gateLedger struct{ kinds []LedgerKind }

func (l *gateLedger) Record(_ context.Context, rec LedgerRecord) (LedgerEntry, error) {
	l.kinds = append(l.kinds, rec.Kind)
	return LedgerEntry{ID: fmt.Sprintf("led_%d", len(l.kinds)), Kind: rec.Kind}, nil
}

type gateValidator struct{}

func (gateValidator) ValidateArgs(_ string, _ []byte, args []byte) ([]byte, error) {
	if strings.Contains(string(args), `"bad"`) {
		return nil, errors.New("args: field x must be a number")
	}
	return args, nil
}

type gateRig struct {
	e        *ToolExecutor
	h        *gateHandler
	appr     *gateApproval
	ledger   *gateLedger
	history  *ActionHistory
	refusals *RefusalRecord
}

const gateConv = "conv-gates"

func newGateRig(t *testing.T, mutate func(c *ToolExecutorControls, f *ToolExecutor)) *gateRig {
	t.Helper()
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "mcp:crm/read", Effects: []ToolEffect{EffectRead}, Schema: []byte(`{"type":"object"}`)})
	reg.Register(SystemTool{Name: "mcp:crm/write", Effects: []ToolEffect{EffectWrite}, DataWriteKinds: []string{"crm"}})
	reg.Register(SystemTool{Name: "mcp:crm/delete", Dangerous: true, Effects: []ToolEffect{EffectWrite}, DataWriteKinds: []string{"crm"}})
	r := &gateRig{
		h:        &gateHandler{result: []byte(`{"ok":true}`)},
		appr:     &gateApproval{approve: true},
		ledger:   &gateLedger{},
		history:  NewActionHistory(time.Hour, time.Now),
		refusals: NewRefusalRecord(time.Hour),
	}
	grants := grantStore("a", ToolGrant{Tool: "mcp:crm/read"}, ToolGrant{Tool: "mcp:crm/write"}, ToolGrant{Tool: "mcp:crm/delete"})
	c := ToolExecutorControls{Registry: reg, Grants: grants, Authz: AllowAllAuthorizer{}, Approval: r.appr,
		ArgValidator: gateValidator{}, Guards: &RequestGuards{Refusals: r.refusals}}
	f := ToolExecutor{MCPHandler: r.h, Ledger: r.ledger, History: r.history, InlineThreshold: 1 << 20}
	if mutate != nil {
		mutate(&c, &f)
	}
	e, err := NewToolExecutor(c, f)
	if err != nil {
		t.Fatal(err)
	}
	r.e = e
	return r
}

func gateCtx() context.Context { return WithConversationID(context.Background(), gateConv) }

func (r *gateRig) historyCount(tool, args string) int {
	return len(r.history.Matches(ActionHistoryScope(gateConv, ""), tool, []byte(args), ActionRef{}))
}

type gateOutcome struct {
	denied        bool
	reasonHas     string
	errorHas      string
	approver      string
	state         ApprovalState
	handlerCalls  int
	approvalAsked int
	ledger        string // kinds recorded, comma-joined
	history       int    // history entries the call left (after deferred hooks)
	resultCID     bool
}

func (r *gateRig) observe(out ToolCallResponse, tool, args string) gateOutcome {
	kinds := make([]string, len(r.ledger.kinds))
	for i, k := range r.ledger.kinds {
		kinds[i] = string(k)
	}
	return gateOutcome{
		denied: out.Denied, reasonHas: out.DenyReason, errorHas: out.Error, approver: out.ApproverID,
		state: out.ApprovalState, handlerCalls: r.h.calls, approvalAsked: r.appr.asked,
		ledger: strings.Join(kinds, ","), history: r.historyCount(tool, args), resultCID: out.ResultCID != "",
	}
}

func checkGate(t *testing.T, got, want gateOutcome) {
	t.Helper()
	if got.denied != want.denied || got.approver != want.approver || got.state != want.state ||
		got.handlerCalls != want.handlerCalls || got.approvalAsked != want.approvalAsked ||
		got.ledger != want.ledger || got.history != want.history || got.resultCID != want.resultCID ||
		!strings.Contains(got.reasonHas, want.reasonHas) || !strings.Contains(got.errorHas, want.errorHas) ||
		(want.reasonHas == "" && got.reasonHas != "") || (want.errorHas == "" && got.errorHas != "") {
		t.Fatalf("outcome\n got %+v\nwant %+v", got, want)
	}
}

func TestExecute_GateOrderAndOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(c *ToolExecutorControls, f *ToolExecutor)
		ctx    func() context.Context
		tool   string
		args   string
		want   gateOutcome
	}{
		{name: "resolve: unknown tool is refused", tool: "mcp:crm/nope", args: `{}`,
			want: gateOutcome{denied: true, reasonHas: "unknown tool"}},
		{name: "validate: invalid arguments are an error, nothing runs", tool: "mcp:crm/read", args: `{"x":"bad"}`,
			want: gateOutcome{errorHas: "must be a number"}},
		{name: "authorize: no grant is refused before any approval", tool: "mcp:crm/read", args: `{}`,
			mutate: func(c *ToolExecutorControls, _ *ToolExecutor) { c.Grants = NewInMemoryGrantsStore() },
			want:   gateOutcome{denied: true, reasonHas: "tool not granted"}},
		{name: "approval: dangerous tool with no approver fails closed", tool: "mcp:crm/delete", args: `{"id":1}`,
			mutate: func(c *ToolExecutorControls, _ *ToolExecutor) { c.Approval = nil },
			want:   gateOutcome{denied: true, reasonHas: "approval required but unavailable"}},
		{name: "approval: denied approval runs nothing and drops the history entry", tool: "mcp:crm/delete", args: `{"id":1}`,
			mutate: func(c *ToolExecutorControls, _ *ToolExecutor) { c.Approval = &gateApproval{approve: false} },
			want:   gateOutcome{denied: true, reasonHas: "not approved", approver: "op-1", state: ApprovalDenied, approvalAsked: 1}},
		{name: "approval: approved call runs, records its result, keeps a done history entry", tool: "mcp:crm/delete", args: `{"id":1}`,
			want: gateOutcome{approver: "op-1", handlerCalls: 1, approvalAsked: 1, ledger: "tool_result", history: 1}},
		{name: "dispatch: side-effecting call with no approval needed runs and is in history", tool: "mcp:crm/write", args: `{"id":2}`,
			want: gateOutcome{handlerCalls: 1, ledger: "tool_result", history: 1}},
		{name: "dispatch: no handler is an error after the gates", tool: "mcp:crm/read", args: `{}`,
			mutate: func(_ *ToolExecutorControls, f *ToolExecutor) { f.MCPHandler = nil },
			want:   gateOutcome{errorHas: "no tool handler configured"}},
		{name: "dispatch: a handler error is recorded and returned", tool: "mcp:crm/write", args: `{"id":3}`,
			mutate: func(_ *ToolExecutorControls, f *ToolExecutor) {
				f.MCPHandler = &gateHandler{err: errors.New("crm down")}
			},
			want: gateOutcome{errorHas: "crm down", handlerCalls: 1, ledger: "tool_result"}},
		{name: "result: a large result travels as its ledger id", tool: "mcp:crm/read", args: `{}`,
			mutate: func(_ *ToolExecutorControls, f *ToolExecutor) { f.InlineThreshold = 4 },
			want:   gateOutcome{handlerCalls: 1, ledger: "tool_result", resultCID: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newGateRig(t, tc.mutate)
			ctx := gateCtx()
			if tc.ctx != nil {
				ctx = tc.ctx()
			}
			out := r.e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: tc.tool, ArgsJSON: []byte(tc.args)})
			// handler counters live on the rig's handler unless the case replaced it
			if h, ok := r.e.MCPHandler.(*gateHandler); ok && h != r.h {
				r.h = h
			}
			if a, ok := r.e.approval.(*gateApproval); ok && a != r.appr {
				r.appr = a
			}
			checkGate(t, r.observe(out, tc.tool, tc.args), tc.want)
		})
	}
}

// Approval-recovery re-entry (tool_recovery.go): a call carrying the approval of
// exactly this call runs without asking anyone; any other call runs nothing.
func TestExecute_ApprovalRecoveryReentry(t *testing.T) {
	args := `{"id":9}`
	for _, tc := range []struct {
		name string
		pa   preApproval
		want gateOutcome
	}{
		{"the approved call runs under the recorded approver", preApproval{id: "apr-9", tool: "mcp:crm/delete", argHash: hashBytes([]byte(args)), approver: "person:ayse"},
			gateOutcome{approver: "person:ayse", handlerCalls: 1, ledger: "tool_result", history: 1}},
		{"a different call is refused and asks nobody", preApproval{id: "apr-9", tool: "mcp:crm/delete", argHash: hashBytes([]byte(`{"id":10}`)), approver: "person:ayse"},
			gateOutcome{denied: true, reasonHas: "not the one that was approved"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newGateRig(t, nil)
			ctx := context.WithValue(gateCtx(), preApprovedKey{}, tc.pa)
			out := r.e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "mcp:crm/delete", ArgsJSON: []byte(args)})
			checkGate(t, r.observe(out, "mcp:crm/delete", args), tc.want)
		})
	}
}

// The deferred hooks see Execute's FINAL response: the refusal record answers a
// repeated refused call from the record (marked), the idempotency record answers
// a repeated side-effecting step call from its stored result, and the step
// failure tracker stops a step after identical failures.
func TestExecute_DeferredHooksSeeTheFinalResponse(t *testing.T) {
	t.Run("refusal record", func(t *testing.T) {
		r := newGateRig(t, func(c *ToolExecutorControls, _ *ToolExecutor) { c.Approval = nil })
		req := ToolCallRequest{AgentID: "a", ToolName: "mcp:crm/delete", ArgsJSON: []byte(`{"id":1}`), SessionTokenID: "lease-1"}
		first := r.e.Execute(gateCtx(), req)
		second := r.e.Execute(gateCtx(), req)
		if !first.Denied || !second.Denied || !strings.HasSuffix(second.DenyReason, RepeatedRefusalNote) ||
			strings.HasSuffix(first.DenyReason, RepeatedRefusalNote) {
			t.Fatalf("first %+v\nsecond %+v", first, second)
		}
	})
	t.Run("idempotency record", func(t *testing.T) {
		r := newGateRig(t, func(c *ToolExecutorControls, _ *ToolExecutor) { c.Idempotency = NewIdempotencyCache(time.Hour) })
		ctx := WithStepCall(gateCtx(), StepCall{RunID: "run-1", StepIndex: 0})
		req := ToolCallRequest{AgentID: "a", ToolName: "mcp:crm/write", ArgsJSON: []byte(`{"id":4}`), SessionTokenID: "lease-1"}
		first := r.e.Execute(ctx, req)
		second := r.e.Execute(ctx, req)
		if r.h.calls != 1 || string(first.ResultJSON) != string(second.ResultJSON) || first.Error != "" {
			t.Fatalf("handler calls %d; first %+v; second %+v", r.h.calls, first, second)
		}
	})
	t.Run("step failure tracker", func(t *testing.T) {
		r := newGateRig(t, func(_ *ToolExecutorControls, f *ToolExecutor) {
			f.MCPHandler = &gateHandler{err: errors.New("crm down")}
			f.StepFailures = NewStepFailures(2)
		})
		h := r.e.MCPHandler.(*gateHandler)
		ctx := WithStepCall(gateCtx(), StepCall{RunID: "run-1", StepIndex: 0})
		req := ToolCallRequest{AgentID: "a", ToolName: "mcp:crm/read", ArgsJSON: []byte(`{}`), SessionTokenID: "lease-2"}
		r.e.Execute(ctx, req)
		second := r.e.Execute(ctx, req)
		third := r.e.Execute(ctx, req)
		if !strings.Contains(second.Error, "stopped") || !third.Denied || h.calls != 2 {
			t.Fatalf("handler calls %d; second %+v; third %+v", h.calls, second, third)
		}
	})
}
