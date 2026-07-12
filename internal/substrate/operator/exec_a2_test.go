package operator_test

import (
	"context"
	"testing"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/substrate/operator"
)

type fakeRunner struct {
	calls int
	resp  domain.ToolCallResponse
	last  domain.ToolCallRequest
}

func (f *fakeRunner) Execute(_ context.Context, req domain.ToolCallRequest) domain.ToolCallResponse {
	f.calls++
	f.last = req
	return f.resp
}

func newExecService() (*operator.Service, *operator.InMemoryAuditStore, *fakeRunner) {
	feed := operator.NewSpool(operator.SpoolConfig{})
	audit := operator.NewInMemoryAuditStore()
	grants := domain.NewInMemoryGrantsStore()
	svc := operator.NewService(feed)
	svc.SetCommandSources(audit, grants)
	runner := &fakeRunner{resp: domain.ToolCallResponse{ResultJSON: []byte(`{"ok":1}`)}}
	svc.SetToolExec(runner)
	return svc, audit, runner
}

// ExecuteTool runs the tool at ScopeSystem, records the result in the audit row,
// and a replayed command_id returns the original result without re-running (A2.2).
func TestExecuteTool_RunsAuditsAndDedupReplays(t *testing.T) {
	svc, audit, runner := newExecService()

	resp, err := svc.ExecuteTool(opCtx(), &pb.ExecuteToolOpRequest{
		CommandId: "x1", Reason: "operator repro", ToolName: "web_search", ArgsJson: `{"q":"cambrian"}`,
	})
	if err != nil {
		t.Fatalf("ExecuteTool: %v", err)
	}
	if resp.GetDeduped() || resp.GetResultJson() != `{"ok":1}` {
		t.Fatalf("first call = %+v", resp)
	}
	if runner.calls != 1 || !runner.last.System {
		t.Fatalf("runner should run once at ScopeSystem, got calls=%d system=%v", runner.calls, runner.last.System)
	}
	// The audit row captured the result for replay.
	rows, _ := audit.Query(context.Background(), operator.AuditFilter{CommandID: "x1"})
	if len(rows) != 1 || rows[0].After != `{"ok":1}` || rows[0].ActionType != "execute_tool" {
		t.Fatalf("audit row = %+v", rows)
	}

	// Replay: same command_id returns the cached result, tool not re-run.
	replay, err := svc.ExecuteTool(opCtx(), &pb.ExecuteToolOpRequest{
		CommandId: "x1", Reason: "operator repro", ToolName: "web_search", ArgsJson: `{"q":"cambrian"}`,
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.GetDeduped() || replay.GetResultJson() != `{"ok":1}` {
		t.Fatalf("replay = %+v", replay)
	}
	if runner.calls != 1 {
		t.Fatalf("replay must NOT re-run the tool, calls=%d", runner.calls)
	}
}

// A denied execution surfaces the denial and records it; command_id/reason required.
func TestExecuteTool_DeniedAndValidation(t *testing.T) {
	svc, _, runner := newExecService()
	runner.resp = domain.ToolCallResponse{Denied: true, DenyReason: "resource policy: path"}

	resp, err := svc.ExecuteTool(opCtx(), &pb.ExecuteToolOpRequest{
		CommandId: "d1", Reason: "try", ToolName: "shell_exec", ArgsJson: `{}`,
	})
	if err != nil || !resp.GetDenied() || resp.GetDenyReason() == "" {
		t.Fatalf("expected denied response, got resp=%+v err=%v", resp, err)
	}

	if _, err := svc.ExecuteTool(opCtx(), &pb.ExecuteToolOpRequest{ToolName: "x"}); err == nil {
		t.Fatal("missing command_id/reason should be InvalidArgument")
	}
}

// SetToolPolicy binds a policy onto an existing grant; it fails when the agent has
// no grant for the tool (A2.3).
func TestSetToolPolicy_RequiresGrantThenBinds(t *testing.T) {
	feed := operator.NewSpool(operator.SpoolConfig{})
	audit := operator.NewInMemoryAuditStore()
	grants := domain.NewInMemoryGrantsStore()
	svc := operator.NewService(feed)
	svc.SetCommandSources(audit, grants)

	// No grant yet ⇒ InvalidArgument.
	if _, err := svc.SetToolPolicy(opCtx(), &pb.SetToolPolicyRequest{
		CommandId: "p0", Reason: "bound", AgentId: "agent-1", ToolName: "read_file",
		Policy: &pb.ToolPolicyOp{AllowedPaths: []string{"/data"}},
	}); err == nil {
		t.Fatal("expected InvalidArgument when the grant is absent")
	}

	// Grant it, then bind the policy.
	if _, err := svc.SetToolGrant(opCtx(), &pb.SetToolGrantRequest{
		CommandId: "g1", Reason: "grant", AgentId: "agent-1", ToolName: "read_file", Granted: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetToolPolicy(opCtx(), &pb.SetToolPolicyRequest{
		CommandId: "p1", Reason: "bound", AgentId: "agent-1", ToolName: "read_file",
		Policy: &pb.ToolPolicyOp{AllowedPaths: []string{"/data"}},
	}); err != nil {
		t.Fatalf("SetToolPolicy after grant: %v", err)
	}

	got, _ := grants.GrantsFor(context.Background(), "agent-1")
	if len(got) != 1 || len(got[0].Policy.Filesystem.AllowRoots) != 1 || got[0].Policy.Filesystem.AllowRoots[0] != "/data" {
		t.Fatalf("policy not bound: %+v", got)
	}
}
