package network

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// stateApprovals resolves every request to one fixed state, counting requests.
type stateApprovals struct {
	state domain.ApprovalState
	calls atomic.Int32
}

func (a *stateApprovals) Request(context.Context, domain.ApprovalRequest) (domain.ApprovalDecision, error) {
	a.calls.Add(1)
	return domain.ApprovalDecision{State: a.state, Decided: true, Detail: "no approver"}, nil
}

func unroutableFixture(t *testing.T, state domain.ApprovalState, max int) (*Server, domain.LeaseID, *stateApprovals) {
	t.Helper()
	s, gw := newLeaseServer(t)
	s.ExecCfg.Tools.MaxUnroutableApprovalsPerRun = max
	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{Name: "mail_send", Dangerous: true, Effects: []domain.ToolEffect{domain.EffectWrite, domain.EffectEgress}})
	grants := domain.NewInMemoryGrantsStore()
	grants.Set("agent-a", []domain.ToolGrant{{Tool: "mail_send", Policy: domain.ToolResourcePolicy{AllowAll: true}}})
	appr := &stateApprovals{state: state}
	s.ToolExecutor = newTestToolExecutor(t, domain.ToolExecutorControls{Registry: reg, Grants: grants, Approval: appr},
		domain.ToolExecutor{Handler: toolsByName{"mail_send": {result: []byte(`{"message_id":"M-1"}`)}}})
	lease, _ := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Minute)
	gw.BindLease(lease, domain.LeaseBinding{SessionID: "ses-1", RunID: "run-1", StepIndex: 0, AgentID: "agent-a"})
	s.planActions.begin("run-1")
	t.Cleanup(func() { s.planActions.end("run-1") })
	return s, lease, appr
}

func sendMail(t *testing.T, s *Server, lease domain.LeaseID, i int) *pb.ExecuteToolResponse {
	t.Helper()
	resp, err := s.ExecuteTool(ctxWithAgentID("agent-a"), &pb.ExecuteToolRequest{
		LeaseId: string(lease), ToolName: "mail_send", ArgsJson: fmt.Sprintf(`{"to":"x%d@example.org","body":"try %d"}`, i, i),
	})
	if err != nil {
		t.Fatalf("ExecuteTool: %v", err)
	}
	return resp
}

// Part 2 of T-20261107-chat-agent-re-delegates-after-a-deterministic: a running
// plan whose calls of a tool keep coming back unroutable stops raising approvals for
// it after execution.tools.max_unroutable_approvals_per_run, however the arguments
// vary; the refusal reads as final and every attempt stays in the run's ledger.
func TestExecuteTool_BoundsRepeatedUnroutableApprovalsInARun(t *testing.T) {
	t.Parallel()
	s, lease, appr := unroutableFixture(t, domain.ApprovalUnroutable, 2)
	for i := 0; i < 5; i++ {
		if r := sendMail(t, s, lease, i); !r.Denied || !strings.Contains(r.DenyReason, "not approved: unroutable") {
			t.Fatalf("call %d: %+v", i, r)
		}
	}
	if n := appr.calls.Load(); n != 2 {
		t.Fatalf("approvals raised = %d, want 2 (then the bound refuses without asking)", n)
	}
	r := sendMail(t, s, lease, 9)
	for _, want := range []string{"execution.tools.max_unroutable_approvals_per_run", "Retrying this call will be refused the same way"} {
		if !strings.Contains(r.DenyReason, want) {
			t.Errorf("bounded refusal lacks %q: %s", want, r.DenyReason)
		}
	}
	if got := s.planActions.snapshot("run-1"); len(got) != 6 {
		t.Errorf("ledger holds %d refusals, want all 6", len(got))
	}
}

// A person's "no" and an expired approval are not routing failures: asking again
// may get a yes, so they are not bounded; and 0 disables the bound.
func TestExecuteTool_UnroutableBoundIgnoresDenialsAndCanBeDisabled(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		state domain.ApprovalState
		max   int
	}{
		"denied":   {domain.ApprovalDenied, 2},
		"expired":  {domain.ApprovalExpired, 2},
		"disabled": {domain.ApprovalUnroutable, 0},
	} {
		s, lease, appr := unroutableFixture(t, tc.state, tc.max)
		for i := 0; i < 4; i++ {
			sendMail(t, s, lease, i)
		}
		if n := appr.calls.Load(); n != 4 {
			t.Errorf("%s: approvals raised = %d, want 4", name, n)
		}
	}
}
