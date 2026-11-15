package network

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/session"
)

// T-20261107-tool-approvals-raised-by-chat-delegated-plan.
//
// A chat turn delegates to the planner; each plan step runs under a step lease
// bound to the plan's TASK SESSION (no conversation on the lease), and the
// session is linked to the ordering conversation (ADR-0084 D2). The SDK's
// execute_tool sends that lease ONLY in the request body — never as a header.
// Before the fix ExecuteTool resolved session, conversation and owner from the
// headers alone, so every dangerous-tool approval a plan step raised named no
// beneficiary, no conversation and no session, and a self / relationship rule
// was unroutable — denied at once, silently.

type fakeSupervisors map[string][]string // beneficiary id -> supervisor ids

func (f fakeSupervisors) Approvers(_ context.Context, b domain.PrincipalRef, relation string) ([]string, error) {
	if relation != "supervisor" {
		return nil, nil
	}
	return f[b.ID], nil
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type delegatedApprovalFixture struct {
	srv     *Server
	gw      *SubstrateLLMGateway
	hub     *domain.AddressedHub
	session domain.SessionID
	logs    *syncBuffer
}

const (
	apOwner   = "praman"
	apManager = "vantis-finance-mgr"
	apConv    = "conv-praman-1"
)

func newDelegatedApprovalFixture(t *testing.T) *delegatedApprovalFixture {
	t.Helper()
	ctx := context.Background()
	s, gw := newLeaseServer(t)
	mgr := session.New(newMemSessionStore())
	s.SessionMgr = mgr
	ses, err := mgr.CreateSession(ctx, "draft reminders", "")
	if err != nil {
		t.Fatal(err)
	}
	// What Execute does for a chat turn's delegate_to_planner (resolveBindingFromHandoff).
	if err := mgr.SaveConversationLink(ctx, ses.ID, apConv, "msg-1"); err != nil {
		t.Fatal(err)
	}
	owners := map[string]string{apConv: apOwner}
	// The production resolvers (app.conversationOwnerOf / memoryNotesPrincipal)
	// read the conversation on the lease, else on its session; the fixture's
	// conversation store is the map.
	s.ConversationOwner = func(ctx context.Context, b domain.LeaseBinding) string {
		return owners[s.conversationOfBinding(ctx, b)]
	}
	s.RunPrincipal = s.ConversationOwner

	logs := &syncBuffer{}
	policy, err := domain.NewConfigApprovalPolicy(domain.ApprovalPolicyConfig{
		Rules: []domain.ApprovalClassRule{
			{Class: domain.ApprovalClassTool, Approvers: []string{"self"}},
			// Never self-approvable: the self rule is dropped, the supervisor decides.
			{Class: "payment", Approvers: []string{"self", "relationship:supervisor"}},
			{Class: "*", Approvers: []string{"operator"}},
		},
		NeverSelfApprovable: []string{"payment"},
		ToolClasses:         map[string]string{"mcp:fin/pay_invoice": "payment"},
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := domain.NewAddressedHub(domain.AddressedHubConfig{
		Policy:         policy,
		Relationships:  fakeSupervisors{apOwner: {apManager}},
		DefaultTimeout: 5 * time.Second,
		Logger:         slog.New(slog.NewTextHandler(logs, nil)),
	})

	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{Name: "mcp:mail/mail_draft", Dangerous: true})
	reg.Register(domain.SystemTool{Name: "mcp:fin/pay_invoice", Dangerous: true})
	grants := domain.NewInMemoryGrantsStore()
	grants.Set("assistant", []domain.ToolGrant{
		{Tool: "mcp:mail/mail_draft", Policy: domain.ToolResourcePolicy{AllowAll: true}},
		{Tool: "mcp:fin/pay_invoice", Policy: domain.ToolResourcePolicy{AllowAll: true}},
	})
	s.ToolExecutor = newTestToolExecutor(t, domain.ToolExecutorControls{
		Registry: reg,
		Grants:   grants,
		Approval: hub,
	}, domain.ToolExecutor{Handler: staticToolHandler{result: []byte(`{"ok":true}`)}})
	return &delegatedApprovalFixture{srv: s, gw: gw, hub: hub, session: ses.ID, logs: logs}
}

// stepLease mints the lease the dispatcher binds for plan step i of a run under
// the fixture's task session — the binding shape of a chat-delegated plan step.
func (f *delegatedApprovalFixture) stepLease(t *testing.T, sessionID domain.SessionID) domain.LeaseID {
	t.Helper()
	return bindNotesLease(t, f.gw, domain.LeaseBinding{SessionID: sessionID, RunID: "run-1", StepIndex: 6, AgentID: "assistant"})
}

// callAsSDK calls ExecuteTool exactly as the SDK does: the lease in the body,
// x-agent-id the only header.
func (f *delegatedApprovalFixture) callAsSDK(lease domain.LeaseID, tool, args string) (*pb.ExecuteToolResponse, error) {
	return f.srv.ExecuteTool(agentPlaneCtx("assistant", "x-agent-id", "assistant"), &pb.ExecuteToolRequest{
		ToolName: tool, ArgsJson: args, LeaseId: string(lease), SessionTokenId: string(lease),
	})
}

func watchAs(h *domain.AddressedHub, id string) (<-chan domain.ApprovalEvent, func()) {
	return h.Watch(domain.ApprovalWatch{For: domain.Approver{Principal: domain.UserPrincipal(id)}})
}

func nextRaised(t *testing.T, evs <-chan domain.ApprovalEvent) domain.ApprovalItem {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ev := <-evs:
			if ev.Type == domain.ApprovalEventRaised {
				return ev.Item
			}
		case <-timeout:
			t.Fatal("no approval was raised to this approver")
			return domain.ApprovalItem{}
		}
	}
}

type callResult struct {
	resp *pb.ExecuteToolResponse
	err  error
}

func (f *delegatedApprovalFixture) callAsync(lease domain.LeaseID, tool, args string) <-chan callResult {
	out := make(chan callResult, 1)
	go func() {
		r, err := f.callAsSDK(lease, tool, args)
		out <- callResult{r, err}
	}()
	return out
}

func awaitCall(t *testing.T, ch <-chan callResult) *pb.ExecuteToolResponse {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("ExecuteTool: %v", r.err)
		}
		return r.resp
	case <-time.After(5 * time.Second):
		t.Fatal("ExecuteTool did not return")
		return nil
	}
}

// The bug, reproduced: a plan step's dangerous-tool approval (self rule) names
// the conversation owner as beneficiary, plus the ordering conversation and the
// task session, reaches the owner, and the owner's answer runs the call.
func TestDelegatedStepApproval_NamesConversationOwnerAndRoutesToSelf(t *testing.T) {
	t.Parallel()
	f := newDelegatedApprovalFixture(t)
	evs, cancel := watchAs(f.hub, apOwner)
	defer cancel()

	done := f.callAsync(f.stepLease(t, f.session), "mcp:mail/mail_draft", `{"to":"ap@clinic.example","body":"hi"}`)
	it := nextRaised(t, evs)
	if !domain.SamePerson(it.Beneficiary, domain.UserPrincipal(apOwner)) {
		t.Fatalf("beneficiary = %+v, want the conversation owner %q", it.Beneficiary, apOwner)
	}
	if it.ConversationID != apConv {
		t.Errorf("conversation = %q, want the ordering conversation %q", it.ConversationID, apConv)
	}
	if it.SessionID != string(f.session) {
		t.Errorf("session = %q, want the task session %q", it.SessionID, f.session)
	}
	if it.RuleSummary() != "self" {
		t.Errorf("rules = %s, want self", it.RuleSummary())
	}
	if _, err := f.hub.Answer(context.Background(), it.ID, domain.ApprovalAnswer{
		Approved: true, By: domain.Approver{Principal: domain.UserPrincipal(apOwner)},
	}); err != nil {
		t.Fatalf("owner's answer refused: %v", err)
	}
	if resp := awaitCall(t, done); resp.Denied || resp.Error != "" {
		t.Fatalf("approved call must run: %+v", resp)
	}
}

// Never-self: a payment class drops the self rule. The owner cannot approve
// their own payment; the supervisor resolved through the hierarchy for the
// SAME beneficiary can.
func TestDelegatedStepApproval_NeverSelfRoutesToSupervisor(t *testing.T) {
	t.Parallel()
	f := newDelegatedApprovalFixture(t)
	mgrEvs, cancelMgr := watchAs(f.hub, apManager)
	defer cancelMgr()

	done := f.callAsync(f.stepLease(t, f.session), "mcp:fin/pay_invoice", `{"invoice":"INV-1","amount":7500}`)
	it := nextRaised(t, mgrEvs)
	if !domain.SamePerson(it.Beneficiary, domain.UserPrincipal(apOwner)) || !it.NeverSelf {
		t.Fatalf("beneficiary=%+v never_self=%v", it.Beneficiary, it.NeverSelf)
	}
	if it.RuleSummary() != "relationship:supervisor" {
		t.Fatalf("rules = %s, want relationship:supervisor", it.RuleSummary())
	}
	_, err := f.hub.Answer(context.Background(), it.ID, domain.ApprovalAnswer{
		Approved: true, By: domain.Approver{Principal: domain.UserPrincipal(apOwner)},
	})
	if !errors.Is(err, domain.ErrApprovalNotAddressee) {
		t.Fatalf("the beneficiary approved a never-self payment: %v", err)
	}
	if _, err := f.hub.Answer(context.Background(), it.ID, domain.ApprovalAnswer{
		Approved: true, By: domain.Approver{Principal: domain.UserPrincipal(apManager)},
	}); err != nil {
		t.Fatalf("supervisor's answer refused: %v", err)
	}
	if resp := awaitCall(t, done); resp.Denied {
		t.Fatalf("supervisor-approved call must run: %+v", resp)
	}
}

// Unroutable is no longer silent: with nobody able to answer connected, the
// call is denied with a reason that names the state and tells the agent a
// retry is futile, and the hub logs it.
func TestDelegatedStepApproval_UnroutableLogsAndReturnsReason(t *testing.T) {
	t.Parallel()
	f := newDelegatedApprovalFixture(t)
	resp, err := f.callAsSDK(f.stepLease(t, f.session), "mcp:mail/mail_draft", `{"to":"x@y"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Denied || !strings.Contains(resp.DenyReason, "unroutable") || !strings.Contains(resp.DenyReason, "Retrying this call will be refused") {
		t.Fatalf("deny reason must name the state and the futility of a retry: %+v", resp)
	}
	if !strings.Contains(f.logs.String(), "approval_unroutable") {
		t.Fatalf("the unroutable branch must log; logs:\n%s", f.logs.String())
	}
}

// A request that names no beneficiary (here: a step whose session has no
// conversation) fails closed under a self rule with a reason saying so, logged.
func TestDelegatedStepApproval_NoBeneficiaryFailsClosedWithReason(t *testing.T) {
	t.Parallel()
	f := newDelegatedApprovalFixture(t)
	orphan, err := f.srv.SessionMgr.CreateSession(context.Background(), "no conversation", "")
	if err != nil {
		t.Fatal(err)
	}
	evs, cancel := watchAs(f.hub, apOwner) // connected, but not addressable without a beneficiary
	defer cancel()
	resp, err := f.callAsSDK(f.stepLease(t, orphan.ID), "mcp:mail/mail_draft", `{"to":"x@y"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Denied || !strings.Contains(resp.DenyReason, "names no beneficiary") {
		t.Fatalf("deny reason must say the request names no beneficiary: %+v", resp)
	}
	if !strings.Contains(f.logs.String(), "approval_fail_closed") {
		t.Fatalf("the fail-closed branch must log; logs:\n%s", f.logs.String())
	}
	select {
	case ev := <-evs:
		if ev.Type == domain.ApprovalEventRaised {
			t.Fatalf("a beneficiary-less request reached a person: %+v", ev.Item)
		}
	default:
	}
}

// The body lease is still subject to the ADR-0139 D8 ownership rule: another
// agent presenting this step's lease resolves no identity from it (and is
// refused before any approval is raised).
func TestExecuteTool_ForeignBodyLeaseConfersNoIdentity(t *testing.T) {
	t.Parallel()
	f := newDelegatedApprovalFixture(t)
	evs, cancel := watchAs(f.hub, apOwner)
	defer cancel()
	lease := f.stepLease(t, f.session)
	resp, err := f.srv.ExecuteTool(agentPlaneCtx("intruder", "x-agent-id", "intruder"), &pb.ExecuteToolRequest{
		ToolName: "mcp:mail/mail_draft", ArgsJson: `{}`, LeaseId: string(lease),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Denied {
		t.Fatalf("a foreign lease must be refused: %+v", resp)
	}
	select {
	case ev := <-evs:
		t.Fatalf("a foreign lease raised an approval: %+v", ev.Item)
	default:
	}
	ctx := withPresentedLease(agentPlaneCtx("intruder"), lease)
	if sid := f.srv.resolveCallerSession(ctx); sid != "" {
		t.Fatalf("a foreign body lease resolved session %q", sid)
	}
}
