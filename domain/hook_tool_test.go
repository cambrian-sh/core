package domain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// ADR-0139 D11 on the ToolExecutor: hooks run after the policy decision, narrow
// only, and a rewritten call is re-authorized.

func hookedExec(t *testing.T, tool SystemTool, grant ToolGrant, h *fakeHandler, hs ...Hook) *ToolExecutor {
	t.Helper()
	reg := NewInMemoryToolRegistry()
	reg.Register(tool)
	e := newExec(reg, grantStore("a", grant), h)
	e.Hooks = mustChain(t, nil, hs...)
	return e
}

func TestToolHooks_PreToolDenyStopsDispatch(t *testing.T) {
	t.Parallel()
	h := &fakeHandler{result: []byte(`"ok"`)}
	e := hookedExec(t, SystemTool{Name: "web"}, ToolGrant{Tool: "web", Policy: ToolResourcePolicy{AllowAll: true}}, h,
		funcHook{preTool("guard", 0), answer(HookResult{Verdict: HookDeny, Reason: "no web today"})})
	r := e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "web", ArgsJSON: []byte(`{}`)})
	if !r.Denied || !strings.Contains(r.DenyReason, "hook guard: no web today") || h.called {
		t.Fatalf("resp = %+v called=%v", r, h.called)
	}
}

func TestToolHooks_PreToolAskRoutesToApproval(t *testing.T) {
	t.Parallel()
	h := &fakeHandler{result: []byte(`"ok"`)}
	e := hookedExec(t, SystemTool{Name: "web"}, ToolGrant{Tool: "web", Policy: ToolResourcePolicy{AllowAll: true}}, h,
		funcHook{preTool("ask", 0), answer(HookResult{Verdict: HookAsk})})
	// No approval controller: an ask is fail-closed.
	if r := e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "web", ArgsJSON: []byte(`{}`)}); !r.Denied || h.called {
		t.Fatalf("ask without approver: %+v", r)
	}
	ap := &fakeApproval{approve: true}
	e.approval = ap
	r := e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "web", ArgsJSON: []byte(`{}`)})
	if r.Denied || !ap.asked || !h.called || r.ApproverID != "op-1" {
		t.Fatalf("ask with approver: %+v asked=%v", r, ap.asked)
	}
}

func TestToolHooks_CannotWidenADangerousAsk(t *testing.T) {
	t.Parallel()
	h := &fakeHandler{result: []byte(`"ok"`)}
	e := hookedExec(t, SystemTool{Name: "shell", Dangerous: true}, ToolGrant{Tool: "shell", Policy: ToolResourcePolicy{AllowAll: true}}, h,
		funcHook{preTool("yolo", 0), answer(HookResult{Verdict: HookAllow})})
	ap := &fakeApproval{approve: false}
	e.approval = ap
	r := e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "shell", ArgsJSON: []byte(`{}`)})
	if !ap.asked || !r.Denied || h.called {
		t.Fatalf("a hook's allow skipped the approval: %+v asked=%v", r, ap.asked)
	}
}

func TestToolHooks_PolicyDenyIsNeverSeenOrUndone(t *testing.T) {
	t.Parallel()
	called := false
	h := &fakeHandler{result: []byte(`"ok"`)}
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "web"})
	e := newExec(reg, NewInMemoryGrantsStore(), h) // no grant
	e.Hooks = mustChain(t, nil, funcHook{preTool("allow-all", 0), func(context.Context, HookEvent) (HookResult, error) {
		called = true
		return HookResult{Verdict: HookAllow}, nil
	}})
	if r := e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "web"}); !r.Denied || h.called || called {
		t.Fatalf("ungranted call: %+v hookCalled=%v", r, called)
	}
}

func TestToolHooks_RewriteIsReauthorized(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tool := SystemTool{Name: "read_file", PathArgs: []string{"path"}}
	grant := ToolGrant{Tool: "read_file", Policy: ToolResourcePolicy{Filesystem: FilesystemPolicy{AllowRoots: []string{root}}}}
	inRoot, _ := json.Marshal(map[string]string{"path": root + "/a.txt"})
	escape, _ := json.Marshal(map[string]string{"path": root + "/../../etc/passwd"})

	rewriteTo := func(args []byte) Hook {
		return funcHook{HookSpec{Name: "rw", Points: []HookPoint{HookPreTool}, MayRewrite: true}, answer(HookResult{Args: args})}
	}

	// A rewrite toward an ungranted path is refused by the resource policy.
	h := &fakeHandler{result: []byte(`"ok"`)}
	e := hookedExec(t, tool, grant, h, rewriteTo(escape))
	r := e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "read_file", ArgsJSON: inRoot})
	if !r.Denied || !strings.HasPrefix(r.DenyReason, "hook rewrite refused: resource policy") || h.called {
		t.Fatalf("escaping rewrite: %+v called=%v", r, h.called)
	}

	// A rewrite that stays inside the grant runs with the rewritten args.
	other, _ := json.Marshal(map[string]string{"path": root + "/b.txt"})
	h2 := &fakeHandler{result: []byte(`"ok"`)}
	e2 := hookedExec(t, tool, grant, h2, rewriteTo(other))
	r2 := e2.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "read_file", ArgsJSON: inRoot})
	if r2.Denied || string(h2.gotArgs) != string(other) || r2.ArgHash != hashBytes(other) {
		t.Fatalf("in-grant rewrite: %+v got=%s", r2, h2.gotArgs)
	}
}

func TestToolHooks_PostToolRedactsAndWithholds(t *testing.T) {
	t.Parallel()
	grant := ToolGrant{Tool: "web", Policy: ToolResourcePolicy{AllowAll: true}}
	redact := funcHook{HookSpec{Name: "red", Points: []HookPoint{HookPostTool}, MayRewrite: true},
		func(_ context.Context, ev HookEvent) (HookResult, error) {
			return HookResult{Result: json.RawMessage(strings.ReplaceAll(string(ev.Result), "s3cr3t", "[R]"))}, nil
		}}
	h := &fakeHandler{result: []byte(`{"body":"key=s3cr3t"}`)}
	r := hookedExec(t, SystemTool{Name: "web"}, grant, h, redact).
		Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "web"})
	if r.Denied || string(r.ResultJSON) != `{"body":"key=[R]"}` || r.ResultHash != hashBytes(r.ResultJSON) {
		t.Fatalf("redaction: %+v", r)
	}

	withhold := funcHook{HookSpec{Name: "wh", Points: []HookPoint{HookPostTool}}, answer(HookResult{Verdict: HookDeny, Reason: "pii"})}
	h2 := &fakeHandler{result: []byte(`{"body":"x"}`)}
	r2 := hookedExec(t, SystemTool{Name: "web"}, grant, h2, withhold).
		Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "web"})
	if !r2.Denied || r2.ResultJSON != nil || !strings.Contains(r2.DenyReason, "output withheld") || !h2.called {
		t.Fatalf("withhold: %+v", r2)
	}
}

func TestToolHooks_EventCarriesTheCall(t *testing.T) {
	t.Parallel()
	var got HookEvent
	spy := funcHook{preTool("spy", 0), func(_ context.Context, ev HookEvent) (HookResult, error) { got = ev; return HookResult{}, nil }}
	h := &fakeHandler{result: []byte(`"ok"`)}
	e := hookedExec(t, SystemTool{Name: "web", Effects: []ToolEffect{EffectEgress}}, ToolGrant{Tool: "web", Policy: ToolResourcePolicy{AllowAll: true}}, h, spy)
	ctx := WithPrincipal(context.Background(), UserPrincipal("u1"))
	e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "web", ArgsJSON: []byte(`{"q":1}`), SessionTokenID: "lease-1", TaskID: "step-1"})
	if got.Tool != "web" || string(got.Args) != `{"q":1}` || got.LeaseID != "lease-1" || got.TaskID != "step-1" ||
		got.Verdict != HookAllow || len(got.Effects) != 1 || got.Effects[0] != "egress" {
		t.Fatalf("event = %+v", got)
	}
	if len(got.Principals) != 2 || got.Principals[1] != AgentPrincipal("a").String() {
		t.Errorf("principal chain = %v", got.Principals)
	}
}

// ENG-629 (owner decision 2026-11-15): pre_tool and post_tool annotations reach the
// agent on ToolCallResponse.Annotations, BESIDE the result — the result bytes (and so
// the ledger record and replay hash) are unchanged.
func TestToolHooks_AnnotationsReachTheAgentBesideTheResult(t *testing.T) {
	t.Parallel()
	grant := ToolGrant{Tool: "web", Policy: ToolResourcePolicy{AllowAll: true}}
	pre := funcHook{preTool("pre", 0), answer(HookResult{Annotations: []string{"pre: rate limited source"}})}
	post := funcHook{HookSpec{Name: "post", Points: []HookPoint{HookPostTool}},
		answer(HookResult{Annotations: []string{"post: figures unverified"}})}
	h := &fakeHandler{result: []byte(`{"body":"x"}`)}
	r := hookedExec(t, SystemTool{Name: "web"}, grant, h, pre, post).
		Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "web"})
	if r.Denied || string(r.ResultJSON) != `{"body":"x"}` || r.ResultHash != hashBytes([]byte(`{"body":"x"}`)) {
		t.Fatalf("result must be untouched: %+v", r)
	}
	if len(r.Annotations) != 2 || !strings.Contains(r.Annotations[0], "rate limited") || !strings.Contains(r.Annotations[1], "unverified") {
		t.Fatalf("annotations = %q", r.Annotations)
	}
}
