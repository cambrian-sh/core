package domain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Static hook scope (T-20261106-post-model-hooks-cannot-be-scoped-by-agent-or) and
// guarded control-verb arguments at post_model
// (T-20261106-post-model-hooks-do-not-guard-control-verb).

func TestHookSpec_Matches(t *testing.T) {
	t.Parallel()
	model := HookSpec{Name: "m", Points: []HookPoint{HookPreModel, HookPostModel, HookPreTool},
		Agents: []string{"research*", "chat_agent"}, Purposes: []string{"agent_step"}}
	cases := []struct {
		p              HookPoint
		agent, purpose string
		want           bool
	}{
		{HookPostModel, "researcher", "agent_step", true},
		{HookPostModel, "chat_agent", "agent_step", true},
		{HookPostModel, "coder", "agent_step", false},   // agent glob misses
		{HookPostModel, "researcher", "planner", false}, // purpose misses
		{HookPostModel, "", "planner", false},           // an organ call has no agent
		{HookPreModel, "researcher", "agent_step", true},
		{HookPreTool, "researcher", "", true}, // purposes ignored at tool points
		{HookPreTool, "coder", "", false},     // agents apply at every point
		{HookPostTool, "researcher", "", false},
	}
	for _, c := range cases {
		if got := model.Matches(c.p, c.agent, c.purpose); got != c.want {
			t.Errorf("Matches(%s, %q, %q) = %v, want %v", c.p, c.agent, c.purpose, got, c.want)
		}
	}
	// Empty scope = every event, including one with no agent.
	open := HookSpec{Name: "o", Points: []HookPoint{HookPostModel}}
	if !open.Matches(HookPostModel, "", "planner") || !open.Matches(HookPostModel, "x", "agent_step") {
		t.Error("an unscoped hook must match every call")
	}
	// `*` means "any agent", not "no agent".
	star := HookSpec{Name: "s", Points: []HookPoint{HookPostModel}, Agents: []string{"*"}}
	if star.Matches(HookPostModel, "", "planner") || !star.Matches(HookPostModel, "a", "planner") {
		t.Error("agents [*] must match every agent call and no organ call")
	}
	// Purpose globs.
	glob := HookSpec{Name: "g", Points: []HookPoint{HookPostModel}, Purposes: []string{"plan*", "router"}}
	if !glob.Matches(HookPostModel, "", "planner") || !glob.Matches(HookPostModel, "", "router") ||
		glob.Matches(HookPostModel, "", "agent_step") {
		t.Error("purpose globs")
	}
}

func TestHookChain_SkipsNonMatchingHooks(t *testing.T) {
	t.Parallel()
	obs := &recObserver{}
	called := map[string]int{}
	hook := func(name string, agents, purposes []string) Hook {
		spec := HookSpec{Name: name, Points: []HookPoint{HookPostModel}, MayRewrite: true, Agents: agents, Purposes: purposes,
			OnFailure: HookFailBlock}
		return funcHook{spec, func(_ context.Context, ev HookEvent) (HookResult, error) {
			called[name]++
			return HookResult{}, nil
		}}
	}
	c := mustChain(t, obs,
		hook("planner-only", nil, []string{"planner"}),
		hook("researcher-steps", []string{"researcher"}, []string{"agent_step"}),
		hook("everything", nil, nil))
	ev := HookEvent{Point: HookPostModel, AgentID: "researcher", Model: &HookModelCall{Purpose: "agent_step"}, Text: "t"}
	if out := c.Run(context.Background(), ev); out.Verdict != HookAllow {
		t.Fatalf("outcome = %+v", out)
	}
	if called["planner-only"] != 0 || called["researcher-steps"] != 1 || called["everything"] != 1 {
		t.Fatalf("called = %v", called)
	}
	// A skipped hook is neither invoked nor recorded — and so never a failure.
	if len(obs.invs) != 2 {
		t.Fatalf("recorded %d invocations, want 2: %+v", len(obs.invs), obs.invs)
	}
	ev = HookEvent{Point: HookPostModel, Model: &HookModelCall{Purpose: "planner"}, Text: "t"}
	c.Run(context.Background(), ev)
	if called["planner-only"] != 1 || called["researcher-steps"] != 1 || called["everything"] != 2 {
		t.Fatalf("called = %v", called)
	}

	if !c.HasFor(HookPostModel, "researcher", "agent_step") || !c.HasFor(HookPostModel, "", "router") {
		t.Error("HasFor: the unscoped hook covers every call")
	}
	scoped := mustChain(t, nil, hook("planner-only", nil, []string{"planner"}))
	if scoped.HasFor(HookPostModel, "researcher", "agent_step") || !scoped.HasFor(HookPostModel, "", "planner") {
		t.Error("HasFor must honour the scope")
	}
	if (*HookChain)(nil).HasFor(HookPostModel, "a", "agent_step") {
		t.Error("nil chain")
	}
	if specs := scoped.Specs(); len(specs) != 1 || specs[0].Purposes[0] != "planner" {
		t.Fatalf("Specs = %+v", specs)
	}
}

func TestNewHookChain_ValidatesScope(t *testing.T) {
	t.Parallel()
	nop := func(context.Context, HookEvent) (HookResult, error) { return HookResult{}, nil }
	for name, spec := range map[string]HookSpec{
		"empty agent glob":             {Name: "a", Points: []HookPoint{HookPreTool}, Agents: []string{""}},
		"empty purpose glob":           {Name: "b", Points: []HookPoint{HookPostModel}, Purposes: []string{""}},
		"purposes on a tool-only hook": {Name: "c", Points: []HookPoint{HookPreTool}, Purposes: []string{"planner"}},
	} {
		if _, err := NewHookChain([]Hook{funcHook{spec, nop}}, nil, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := HookSpec{Name: "d", Points: []HookPoint{HookPreTool, HookPostModel}, Agents: []string{"x"}, Purposes: []string{"planner"}}
	if _, err := NewHookChain([]Hook{funcHook{ok, nop}}, nil, nil); err != nil {
		t.Fatalf("valid scope refused: %v", err)
	}
}

var testGuards = []GuardedArg{{Tool: "yield_subgoal", Arg: "intent"}, {Tool: "spawn_subagent", Arg: "objective"},
	{Tool: "final_answer", Arg: "answer"}} // the last repeats the deliverable: listed once

func controlCalls(spawnArgs string) []ModelToolCall {
	return []ModelToolCall{
		{ID: "c0", Name: "yield_subgoal", Arguments: []byte(`{"intent":"look up sk-7","capability_hint":"web"}`)},
		{ID: "c1", Name: "write_file", Arguments: []byte(`{"body":"sk-1"}`)},
		{ID: "c2", Name: "spawn_subagent", Arguments: []byte(spawnArgs)},
		{ID: "c3", Name: "final_answer", Arguments: []byte(`{"answer":"done sk-9"}`)},
	}
}

func controlEvent(spawnArgs string) HookEvent {
	calls := controlCalls(spawnArgs)
	ev := HookEvent{Point: HookPostModel, Verdict: HookAllow}
	ev.ToolCalls, ev.FinalAnswer = HookReplyView(calls, "final_answer", "answer")
	ev.GuardedArgs = HookGuardedView(calls, "final_answer", "answer", testGuards)
	return ev
}

func TestHookGuardedView(t *testing.T) {
	t.Parallel()
	gs := HookGuardedView(controlCalls(`{"objective":"run sk-8","mode":"fork"}`), "final_answer", "answer", testGuards)
	want := []HookGuardedArg{
		{Tool: "yield_subgoal", Arg: "intent", Call: 0, Value: "look up sk-7"},
		{Tool: "spawn_subagent", Arg: "objective", Call: 2, Value: "run sk-8"},
		{Tool: "final_answer", Arg: "answer", Call: 3, Value: "done sk-9"},
	}
	if len(gs) != len(want) {
		t.Fatalf("guarded = %+v", gs)
	}
	for i := range want {
		if gs[i] != want[i] {
			t.Errorf("guarded[%d] = %+v, want %+v", i, gs[i], want[i])
		}
	}
	// A control verb that omits (or nulls) its argument has nothing to guard; an
	// empty argument blob neither.
	for _, args := range []string{`{"mode":"fork"}`, `{"objective":null}`, ``} {
		for _, g := range HookGuardedView(controlCalls(args), "final_answer", "answer", testGuards) {
			if g.Call == 2 {
				t.Errorf("%q: unexpected entry %+v", args, g)
			}
		}
	}
	// Present but unreadable: bad JSON, not an object, a non-string value.
	for args, why := range map[string]string{
		`{"objective": "x"`:      "not a JSON object",
		`["x"]`:                  "not a JSON object",
		`{"objective":{"a":1}}`:  "not a string",
		`{"objective":["sk-8"]}`: "not a string",
	} {
		var hit *HookGuardedArg
		for _, g := range HookGuardedView(controlCalls(args), "final_answer", "answer", testGuards) {
			if g.Call == 2 {
				g := g
				hit = &g
			}
		}
		if hit == nil || !strings.Contains(hit.Unreadable, why) || hit.Value != "" {
			t.Errorf("%s: %+v (want unreadable %q)", args, hit, why)
		}
	}
	// The deliverable is guarded with no configured list at all.
	if gs := HookGuardedView(controlCalls(`{}`), "final_answer", "answer", nil); len(gs) != 1 || gs[0].Call != 3 {
		t.Errorf("deliverable only: %+v", gs)
	}
}

func argOf(t *testing.T, raw json.RawMessage, arg string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("args %s: %v", raw, err)
	}
	s, _ := m[arg].(string)
	return s
}

func TestHookChain_GuardedArgRewritesAreWrittenIntoTheCalls(t *testing.T) {
	t.Parallel()
	var seen HookEvent
	c := mustChain(t, nil,
		funcHook{postModelSpec("a-redact", true), func(_ context.Context, ev HookEvent) (HookResult, error) {
			var res HookResult
			for _, g := range ev.GuardedArgs {
				res.GuardedArgs = append(res.GuardedArgs, HookArgRewrite{Call: g.Call, Arg: g.Arg,
					Value: strings.ReplaceAll(g.Value, "sk-", "[R]-")})
			}
			return res, nil
		}},
		funcHook{postModelSpec("b-watch", false), func(_ context.Context, ev HookEvent) (HookResult, error) {
			seen = ev
			return HookResult{}, nil
		}})
	ev := controlEvent(`{"objective":"run sk-8","mode":"fork"}`)
	out := c.Run(context.Background(), ev)
	if out.Verdict != HookAllow || !out.GuardedArgsRewritten {
		t.Fatalf("outcome = %+v", out)
	}
	if got := argOf(t, out.ToolCalls[0].Args, "intent"); got != "look up [R]-7" {
		t.Errorf("intent = %q", got)
	}
	if got := argOf(t, out.ToolCalls[0].Args, "capability_hint"); got != "web" {
		t.Errorf("other fields lost: %s", out.ToolCalls[0].Args)
	}
	if got := argOf(t, out.ToolCalls[2].Args, "objective"); got != "run [R]-8" {
		t.Errorf("objective = %q", got)
	}
	if string(out.ToolCalls[1].Args) != `{"body":"sk-1"}` {
		t.Errorf("an unguarded call was rewritten: %s", out.ToolCalls[1].Args)
	}
	// Rewriting the deliverable through the list keeps FinalAnswer in step.
	if !out.FinalAnswerRewritten || out.FinalAnswer != "done [R]-9" || argOf(t, out.ToolCalls[3].Args, "answer") != "done [R]-9" {
		t.Errorf("final answer = %q rewritten=%v args=%s", out.FinalAnswer, out.FinalAnswerRewritten, out.ToolCalls[3].Args)
	}
	// The next hook sees one state.
	if seen.FinalAnswer.Value != "done [R]-9" || seen.GuardedArgs[0].Value != "look up [R]-7" ||
		strings.Contains(string(seen.ToolCalls[2].Args), "sk-8") {
		t.Errorf("next hook saw %+v", seen.GuardedArgs)
	}
	if !strings.Contains(string(ev.ToolCalls[0].Args), "sk-7") || ev.GuardedArgs[0].Value != "look up sk-7" {
		t.Error("the input event was mutated")
	}
}

func TestHookChain_GuardedArgRules(t *testing.T) {
	t.Parallel()
	// Unreadable guarded argument: a rewriter denies without running, whatever its
	// on_failure; a non-rewriter still runs.
	called := false
	spec := postModelSpec("redact", true)
	spec.OnFailure = HookFailAllow
	c := mustChain(t, nil, funcHook{spec, func(context.Context, HookEvent) (HookResult, error) {
		called = true
		return HookResult{}, nil
	}})
	out := c.Run(context.Background(), controlEvent(`{"objective": "sk-8"`))
	if out.Verdict != HookDeny || called || !strings.Contains(out.Reason, "spawn_subagent.objective") {
		t.Fatalf("unreadable: %+v called=%v", out, called)
	}
	c = mustChain(t, nil, funcHook{postModelSpec("watch", false), func(context.Context, HookEvent) (HookResult, error) {
		called = true
		return HookResult{}, nil
	}})
	if out := c.Run(context.Background(), controlEvent(`{"objective": "sk-8"`)); out.Verdict != HookAllow || !called {
		t.Fatalf("non-rewriter: %+v called=%v", out, called)
	}

	// Deny on a guarded argument refuses the whole reply.
	c = mustChain(t, nil, funcHook{postModelSpec("dlp", false), func(_ context.Context, ev HookEvent) (HookResult, error) {
		for _, g := range ev.GuardedArgs {
			if g.Tool == "spawn_subagent" && strings.Contains(g.Value, "sk-") {
				return HookResult{Verdict: HookDeny, Reason: "secret in objective"}, nil
			}
		}
		return HookResult{}, nil
	}})
	if out := c.Run(context.Background(), controlEvent(`{"objective":"run sk-8"}`)); out.Verdict != HookDeny || out.Reason != "secret in objective" {
		t.Fatalf("deny: %+v", out)
	}
	if out := c.Run(context.Background(), controlEvent(`{"objective":"fine"}`)); out.Verdict != HookAllow {
		t.Fatalf("allow: %+v", out)
	}

	// A rewrite naming an argument the event does not list is malformed (failed
	// open under on_failure allow, nothing changed).
	c = mustChain(t, nil, funcHook{postModelSpec("rw", true), answer(HookResult{GuardedArgs: []HookArgRewrite{{Call: 1, Arg: "body", Value: "x"}}})})
	out = c.Run(context.Background(), controlEvent(`{"objective":"o"}`))
	if out.GuardedArgsRewritten || !out.Invocations[0].FailedOpen || string(out.ToolCalls[1].Args) != `{"body":"sk-1"}` {
		t.Fatalf("unlisted rewrite: %+v", out)
	}
	// Without may_rewrite: ignored and recorded.
	c = mustChain(t, nil, funcHook{postModelSpec("nr", false), answer(HookResult{GuardedArgs: []HookArgRewrite{{Call: 0, Arg: "intent", Value: "x"}}})})
	out = c.Run(context.Background(), controlEvent(`{"objective":"o"}`))
	if out.GuardedArgsRewritten || !hasEffect(out.Invocations[0].Effects, "rewrite_ignored") {
		t.Fatalf("no may_rewrite: %+v", out)
	}
}
