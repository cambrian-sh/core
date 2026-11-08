package network

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// toolsByName answers each tool with a fixed result (or error).
type toolsByName map[string]struct {
	result []byte
	err    error
}

func (h toolsByName) Execute(_ context.Context, c domain.ToolCall) ([]byte, error) {
	r := h[c.ToolName]
	return r.result, r.err
}

// planActionFixture is a server whose ToolExecutor has a write tool, a read tool and
// a write tool that fails, and a step lease of run "run-1", step 2, bound to agent-a.
func planActionFixture(t *testing.T) (*Server, domain.LeaseID) {
	t.Helper()
	s, gw := newLeaseServer(t)
	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{Name: "issues_create", Effects: []domain.ToolEffect{domain.EffectRead, domain.EffectWrite}})
	reg.Register(domain.SystemTool{Name: "mail_send", Effects: []domain.ToolEffect{domain.EffectWrite, domain.EffectEgress}})
	reg.Register(domain.SystemTool{Name: "issues_search", Effects: []domain.ToolEffect{domain.EffectRead}})
	grants := domain.NewInMemoryGrantsStore()
	grants.Set("agent-a", []domain.ToolGrant{
		{Tool: "issues_create", Policy: domain.ToolResourcePolicy{AllowAll: true}},
		{Tool: "mail_send", Policy: domain.ToolResourcePolicy{AllowAll: true}},
		{Tool: "issues_search", Policy: domain.ToolResourcePolicy{AllowAll: true}},
	})
	s.ToolExecutor = &domain.ToolExecutor{Registry: reg, Grants: grants, Handler: toolsByName{
		// The MCP content-array shape the kernel stores for an unstructured result.
		"issues_create": {result: []byte(`[{"type":"text","text":"{\"key\":\"OPS-11\",\"issue\":{\"summary\":\"INC-1040\"},\"write_id\":\"W-7\"}"}]`)},
		"mail_send":     {err: errors.New("smtp timeout")},
		"issues_search": {result: []byte(`{"issues":[]}`)},
	}}
	lease, _ := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Minute)
	gw.BindLease(lease, domain.LeaseBinding{SessionID: "ses-1", RunID: "run-1", StepIndex: 2, AgentID: "agent-a"})
	return s, lease
}

func callTool(t *testing.T, s *Server, lease domain.LeaseID, tool, args string) {
	t.Helper()
	if _, err := s.ExecuteTool(ctxWithAgentID("agent-a"), &pb.ExecuteToolRequest{
		LeaseId: string(lease), ToolName: tool, ArgsJson: args,
	}); err != nil {
		t.Fatalf("ExecuteTool %s: %v", tool, err)
	}
}

// Every side-effecting call a running plan's step makes is recorded against the run
// the kernel bound the lease to, with the ids the tool returned; reads are not
// (T-20261107-plan-result-is-only-the-last-topo-step-a-plan).
func TestExecuteTool_RecordsSideEffectingCallsOfARunningPlan(t *testing.T) {
	t.Parallel()
	s, lease := planActionFixture(t)
	s.planActions.begin("run-1")
	defer s.planActions.end("run-1")

	callTool(t, s, lease, "issues_search", `{"q":"INC-1040"}`)
	callTool(t, s, lease, "issues_create", `{"project":"OPS","summary":"INC-1040 - Temperature excursion","token":"secret"}`)
	callTool(t, s, lease, "mail_send", `{"to":"ap@wychwood.example"}`)
	callTool(t, s, lease, "nonexistent_tool", `{}`)

	got := s.planActions.snapshot("run-1")
	if len(got) != 2 {
		t.Fatalf("recorded %d actions, want 2 (create + failed send; not the read, not the unknown tool): %+v", len(got), got)
	}
	c := got[0]
	if c.Tool != "issues_create" || c.Step != 2 || c.Agent != "agent-a" || c.Status != domain.PlanActionDone {
		t.Errorf("create action = %+v", c)
	}
	if strings.Join(c.IDs, ",") != "key=OPS-11,write_id=W-7" {
		t.Errorf("ids = %v", c.IDs)
	}
	if c.Args["summary"] != "INC-1040 - Temperature excursion" || c.Args["token"] != "[redacted]" {
		t.Errorf("args = %v", c.Args)
	}
	if f := got[1]; f.Tool != "mail_send" || f.Status != domain.PlanActionFailed || !strings.Contains(f.Detail, "smtp timeout") {
		t.Errorf("failed send = %+v", f)
	}

	// The Execute payload: the executor's report, then the rendered ledger.
	payload := planResultPayload("OPS-11 created", got)
	for _, want := range []string{"OPS-11 created\n\n" + domain.PlanActionsHeader, "1. step 2 · issues_create · done · ids: key=OPS-11, write_id=W-7", "2. step 2 · mail_send · failed (smtp timeout)"} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload lacks %q:\n%s", want, payload)
		}
	}
	if j := planActionsJSON(got); !strings.Contains(j, `"ids":["key=OPS-11","write_id=W-7"]`) {
		t.Errorf("metadata JSON = %s", j)
	}
}

// A call whose run is not executing (no begin, or after end) records nothing, so the
// ledger cannot grow from calls outside a plan.
func TestExecuteTool_NoRecordOutsideARunningPlan(t *testing.T) {
	t.Parallel()
	s, lease := planActionFixture(t)
	callTool(t, s, lease, "issues_create", `{"summary":"x"}`)
	if got := s.planActions.snapshot("run-1"); len(got) != 0 {
		t.Fatalf("recorded %+v for a run that was never begun", got)
	}
	s.planActions.begin("run-1")
	s.planActions.end("run-1")
	callTool(t, s, lease, "issues_create", `{"summary":"x"}`)
	if got := s.planActions.snapshot("run-1"); len(got) != 0 {
		t.Fatalf("recorded %+v after the run ended", got)
	}
}

func TestPartialPlanPayload_ReportsCompletedWorkAndLedger(t *testing.T) {
	t.Parallel()
	actions := []domain.PlanAction{{Step: 0, Tool: "mail_send", Status: domain.PlanActionDone, IDs: []string{"message_id=M-1"}}}
	got := partialPlanPayload("step 2 failed: boom", "sent to Wychwood", actions)
	for _, want := range []string{"The plan did not finish: step 2 failed: boom", "sent to Wychwood", "1. step 0 · mail_send · done · ids: message_id=M-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("payload lacks %q:\n%s", want, got)
		}
	}
	if planResultPayload("only text", nil) != "only text" {
		t.Error("no actions ⇒ the report is returned unchanged")
	}
}
