package domain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// --- fakes ---

type fakeHandler struct {
	result []byte
	err    error
	called bool
	gotArgs []byte
}

func (f *fakeHandler) Execute(_ context.Context, call ToolCall) ([]byte, error) {
	f.called = true
	f.gotArgs = call.ArgsJSON
	return f.result, f.err
}

type fakeApproval struct {
	approve bool
	asked   bool
}

func (f *fakeApproval) Request(_ context.Context, _ ApprovalRequest) (ApprovalDecision, error) {
	f.asked = true
	return ApprovalDecision{Approved: f.approve, ApproverID: "op-1"}, nil
}

func newExec(reg ToolRegistry, grants GrantsProvider, h ToolHandler) *ToolExecutor {
	return &ToolExecutor{Registry: reg, Grants: grants, Handler: h, InlineThreshold: 1 << 20}
}

func grantStore(agentID string, grants ...ToolGrant) *InMemoryGrantsStore {
	s := NewInMemoryGrantsStore()
	s.Set(agentID, grants)
	return s
}

// fakeArgCS serves one CID's content with a configurable owner session.
type fakeArgCS struct {
	data  []byte
	owner string
}

func (f *fakeArgCS) Put(context.Context, []byte, string, []string, string) (CID, error) {
	return "", nil
}
func (f *fakeArgCS) Get(_ context.Context, cid CID) (*ContextNode, error) {
	if cid == "cid-1" {
		return &ContextNode{CID: cid, Data: f.data, OwnerSession: f.owner}, nil
	}
	return nil, errors.New("not found")
}
func (f *fakeArgCS) Has(context.Context, CID) (bool, error) { return false, nil }
func (f *fakeArgCS) GC(context.Context, []CID) error        { return nil }

// ADR-0048 #3: a {"$cid":"…"} arg is resolved to the stored content before dispatch;
// non-reference args pass through untouched.
func TestExecute_HydratesPublicCIDArg(t *testing.T) {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "write_file"})
	h := &fakeHandler{result: []byte(`{"ok":1}`)}
	e := newExec(reg, grantStore("a", ToolGrant{Tool: "write_file"}), h)
	e.ContentStore = &fakeArgCS{data: []byte("FULL DOCUMENT BODY"), owner: ""} // ownerless/public

	args := []byte(`{"path":"a.md","content":{"$cid":"cid-1"}}`)
	e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "write_file", ArgsJSON: args})

	if !h.called {
		t.Fatal("handler should have run")
	}
	var got map[string]any
	if err := json.Unmarshal(h.gotArgs, &got); err != nil {
		t.Fatalf("handler args not JSON: %v", err)
	}
	if got["content"] != "FULL DOCUMENT BODY" {
		t.Errorf("content arg must be hydrated from cid; got %v", got["content"])
	}
	if got["path"] != "a.md" {
		t.Errorf("non-reference args must pass through; got %v", got["path"])
	}
}

// Another session's private blob (owner != caller) must NOT be hydrated — the ref is
// left untouched rather than leaking the content into the tool.
func TestExecute_DoesNotHydrateUnauthorizedCID(t *testing.T) {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "write_file"})
	h := &fakeHandler{result: []byte(`{}`)}
	e := newExec(reg, grantStore("a", ToolGrant{Tool: "write_file"}), h)
	e.ContentStore = &fakeArgCS{data: []byte("SECRET"), owner: "other-session"}

	// ctx carries no session ⇒ caller "" ⇒ CanReadContentNode(owner!="" , "") is false.
	args := []byte(`{"content":{"$cid":"cid-1"}}`)
	e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "write_file", ArgsJSON: args})

	if strings.Contains(string(h.gotArgs), "SECRET") {
		t.Error("must NOT hydrate another session's private content")
	}
}

// AvailableTools (the ReAct prompt menu) must match what Execute will allow:
// anonymous ⇒ nothing; granted ⇒ exactly the granted tools; unrestricted ⇒ all.
func TestToolExecutor_AvailableToolsMatchesAuthority(t *testing.T) {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "read_file"})
	reg.Register(SystemTool{Name: "execute_command"})
	h := &fakeHandler{result: []byte(`{}`)}

	// Anonymous principal sees no menu (fail-closed, same as grantFor).
	e := newExec(reg, grantStore("a", ToolGrant{Tool: "read_file"}), h)
	if got := e.AvailableTools(context.Background(), ""); len(got) != 0 {
		t.Errorf("anonymous principal must see no tools, got %d", len(got))
	}

	// Granted principal sees exactly its granted tool, not the whole registry.
	got := e.AvailableTools(context.Background(), "a")
	if len(got) != 1 || got[0].Name != "read_file" {
		t.Errorf("granted agent should see only [read_file], got %v", toolNames(got))
	}

	// Unrestricted bypass: a named agent sees every registered tool.
	eu := newExec(reg, grantStore("a"), h)
	eu.Unrestricted = true
	if got := eu.AvailableTools(context.Background(), "a"); len(got) != 2 {
		t.Errorf("unrestricted named agent should see all 2 tools, got %v", toolNames(got))
	}
	// Even unrestricted, an anonymous principal still sees nothing.
	if got := eu.AvailableTools(context.Background(), ""); len(got) != 0 {
		t.Errorf("unrestricted must NOT expose tools to an anonymous principal, got %d", len(got))
	}
}

func toolNames(ts []SystemTool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}

// Unknown tool, unknown principal, and un-granted tool are all denied (fail-closed).
func TestToolExecutor_DenialsFailClosed(t *testing.T) {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "read_file"})
	h := &fakeHandler{result: []byte(`{"ok":true}`)}

	// unknown tool
	e := newExec(reg, grantStore("a", ToolGrant{Tool: "read_file"}), h)
	if r := e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "nope"}); !r.Denied {
		t.Error("unknown tool should be denied")
	}
	// un-granted tool
	if r := e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "read_file"}); r.Denied {
		// granted — should NOT be denied here
		t.Errorf("granted read_file denied: %s", r.DenyReason)
	}
	e2 := newExec(reg, grantStore("a" /* no grants for read_file */), h)
	if r := e2.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "read_file"}); !r.Denied {
		t.Error("un-granted tool should be denied")
	}
	// empty principal
	if r := e.Execute(context.Background(), ToolCallRequest{AgentID: "", ToolName: "read_file"}); !r.Denied {
		t.Error("empty principal should be denied (fail-closed)")
	}
}

// The resource policy is enforced on the tool's declared path arg.
func TestToolExecutor_ResourcePolicyEnforced(t *testing.T) {
	root := t.TempDir()
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "read_file", PathArgs: []string{"path"}})
	h := &fakeHandler{result: []byte(`"ok"`)}
	grant := ToolGrant{Tool: "read_file", Policy: ToolResourcePolicy{Filesystem: FilesystemPolicy{AllowRoots: []string{root}}}}
	e := newExec(reg, grantStore("a", grant), h)

	// in-root → allowed, handler called
	args, _ := json.Marshal(map[string]string{"path": root + "/notes.txt"})
	if r := e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "read_file", ArgsJSON: args}); r.Denied {
		t.Errorf("in-root path denied: %s", r.DenyReason)
	}
	if !h.called {
		t.Error("handler should run for an authorized call")
	}

	// out-of-root → denied, handler NOT called
	h2 := &fakeHandler{result: []byte(`"ok"`)}
	e2 := newExec(reg, grantStore("a", grant), h2)
	bad, _ := json.Marshal(map[string]string{"path": root + "/../escape.txt"})
	if r := e2.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "read_file", ArgsJSON: bad}); !r.Denied {
		t.Error("out-of-root path should be denied")
	}
	if h2.called {
		t.Error("handler must NOT run when the resource policy denies")
	}
}

// A dangerous tool gates on approval; fail-closed when no controller is wired.
func TestToolExecutor_DangerousApproval(t *testing.T) {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "execute_command", Dangerous: true})
	grant := ToolGrant{Tool: "execute_command"}

	// no approval controller wired → denied (fail-closed)
	hNil := &fakeHandler{result: []byte(`"ran"`)}
	eNil := newExec(reg, grantStore("a", grant), hNil)
	if r := eNil.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "execute_command"}); !r.Denied {
		t.Error("dangerous tool with no approver must be denied (fail-closed)")
	}
	if hNil.called {
		t.Error("handler must not run without approval")
	}

	// approved
	hOK := &fakeHandler{result: []byte(`"ran"`)}
	eOK := newExec(reg, grantStore("a", grant), hOK)
	eOK.Approval = &fakeApproval{approve: true}
	if r := eOK.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "execute_command"}); r.Denied {
		t.Errorf("approved dangerous tool denied: %s", r.DenyReason)
	}
	if !hOK.called {
		t.Error("approved dangerous tool should run")
	}

	// denied
	hNo := &fakeHandler{result: []byte(`"ran"`)}
	eNo := newExec(reg, grantStore("a", grant), hNo)
	eNo.Approval = &fakeApproval{approve: false}
	if r := eNo.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "execute_command"}); !r.Denied {
		t.Error("operator-denied dangerous tool must be denied")
	}
	if hNo.called {
		t.Error("operator-denied tool must not run")
	}
}

// Happy path returns the handler result with audit hashes; a handler error is a
// structured response, never a crash.
func TestToolExecutor_ResultAndErrorContract(t *testing.T) {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "echo"})
	grant := ToolGrant{Tool: "echo"}

	h := &fakeHandler{result: []byte(`{"text":"hi"}`)}
	e := newExec(reg, grantStore("a", grant), h)
	r := e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "echo", ArgsJSON: []byte(`{"x":1}`)})
	if r.Denied || r.Error != "" {
		t.Fatalf("happy path failed: denied=%v err=%q", r.Denied, r.Error)
	}
	if string(r.ResultJSON) != `{"text":"hi"}` {
		t.Errorf("result = %s, want the handler output", r.ResultJSON)
	}
	if r.ArgHash == "" || r.ResultHash == "" {
		t.Error("audit hashes must be populated")
	}

	// handler error → structured, no panic
	hErr := &fakeHandler{err: errors.New("boom")}
	eErr := newExec(reg, grantStore("a", grant), hErr)
	re := eErr.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "echo"})
	if re.Error == "" || !strings.Contains(re.Error, "boom") {
		t.Errorf("handler error should surface structurally, got %q", re.Error)
	}
}

// Unrestricted mode: every named agent may call every registered tool with an
// allow-all policy (operator-chosen bypass). Approval for dangerous tools still
// applies; a truly anonymous principal is still denied.
func TestToolExecutor_UnrestrictedMode(t *testing.T) {
	root := t.TempDir()
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "read_file", PathArgs: []string{"path"}})
	h := &fakeHandler{result: []byte(`"ok"`)}

	// No grants store entries at all, but unrestricted is on.
	e := &ToolExecutor{Registry: reg, Grants: NewInMemoryGrantsStore(), Handler: h, Unrestricted: true, InlineThreshold: 1 << 20}

	// An un-granted agent can call the tool, and any path is allowed.
	args, _ := json.Marshal(map[string]string{"path": root + "/../anywhere.txt"})
	if r := e.Execute(context.Background(), ToolCallRequest{AgentID: "any-agent", ToolName: "read_file", ArgsJSON: args}); r.Denied {
		t.Errorf("unrestricted: call denied: %s", r.DenyReason)
	}
	if !h.called {
		t.Error("unrestricted: handler should run")
	}

	// A truly anonymous principal is still denied.
	if r := e.Execute(context.Background(), ToolCallRequest{AgentID: "", ToolName: "read_file"}); !r.Denied {
		t.Error("unrestricted must still deny an empty principal")
	}
}

// A sandboxed evaluation session auto-approves a dangerous tool (the interview
// has no operator); a non-evaluation session still hits the operator gate; and
// an empty token is never an evaluation (fail-closed to approval).
func TestToolExecutor_EvaluationSessionAutoApproves(t *testing.T) {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "execute_command", Dangerous: true})
	grant := ToolGrant{Tool: "execute_command"}
	evals := NewInMemoryEvaluationSessions()
	evals.Mark("interview-tok")

	// Evaluation session: auto-approved even though the operator approver would deny.
	hEval := &fakeHandler{result: []byte(`"ran"`)}
	eEval := newExec(reg, grantStore("a", grant), hEval)
	eEval.Approval = &fakeApproval{approve: false} // operator would deny
	eEval.EvalSessions = evals
	r := eEval.Execute(context.Background(), ToolCallRequest{
		AgentID: "a", ToolName: "execute_command", SessionTokenID: "interview-tok",
	})
	if r.Denied {
		t.Fatalf("evaluation session must auto-approve dangerous tool, denied: %s", r.DenyReason)
	}
	if !hEval.called {
		t.Error("evaluation session: handler should run")
	}
	if r.ApproverID != "evaluation-sandbox" {
		t.Errorf("evaluation auto-approval should be audited, ApproverID=%q", r.ApproverID)
	}

	// Non-evaluation session under the same executor still gates on the operator.
	hLive := &fakeHandler{result: []byte(`"ran"`)}
	eLive := newExec(reg, grantStore("a", grant), hLive)
	eLive.Approval = &fakeApproval{approve: false}
	eLive.EvalSessions = evals
	if r := eLive.Execute(context.Background(), ToolCallRequest{
		AgentID: "a", ToolName: "execute_command", SessionTokenID: "live-tok",
	}); !r.Denied {
		t.Error("non-evaluation session must still be operator-gated")
	}
	if hLive.called {
		t.Error("operator-denied live session must not run")
	}

	// Empty token is never an evaluation, even with a set wired.
	hAnon := &fakeHandler{result: []byte(`"ran"`)}
	eAnon := newExec(reg, grantStore("a", grant), hAnon)
	eAnon.Approval = &fakeApproval{approve: false}
	eAnon.EvalSessions = evals
	if r := eAnon.Execute(context.Background(), ToolCallRequest{
		AgentID: "a", ToolName: "execute_command", SessionTokenID: "",
	}); !r.Denied {
		t.Error("empty session token must fail closed to operator approval")
	}
}

// The auto-approve controller (tools_auto_approve) approves every dangerous call.
func TestAlwaysApproveController(t *testing.T) {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "execute_command", Dangerous: true})
	h := &fakeHandler{result: []byte(`"ran"`)}
	e := newExec(reg, grantStore("a", ToolGrant{Tool: "execute_command"}), h)
	e.Approval = AlwaysApproveController{}
	r := e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "execute_command"})
	if r.Denied {
		t.Fatalf("auto-approve must run dangerous tool, denied: %s", r.DenyReason)
	}
	if r.ApproverID != "auto-approve" {
		t.Errorf("auto-approval should be audited, ApproverID=%q", r.ApproverID)
	}
}

var _ ApprovalController = (*fakeApproval)(nil)
var _ ApprovalController = AlwaysApproveController{}
var _ EvaluationSessionSet = (*InMemoryEvaluationSessions)(nil)
var _ EvaluationSessionMarker = (*InMemoryEvaluationSessions)(nil)
