package domain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// post_model sees the whole reply: its tool calls and the final-answer deliverable
// (T-20261106-post-model-hooks-never-see-native-tool-call).

func replyCalls(finalArgs string) []ModelToolCall {
	return []ModelToolCall{
		{ID: "c0", Name: "write_file", Arguments: []byte(`{"path":"a","body":"sk-1"}`)},
		{ID: "c1", Name: "final_answer", Arguments: []byte(finalArgs)},
	}
}

func postModelEvent(finalArgs string) HookEvent {
	ev := HookEvent{Point: HookPostModel, Text: "narration", Verdict: HookAllow}
	ev.ToolCalls, ev.FinalAnswer = HookReplyView(replyCalls(finalArgs), "final_answer", "answer")
	return ev
}

func postModelSpec(name string, rewrite bool) HookSpec {
	return HookSpec{Name: name, Points: []HookPoint{HookPostModel}, MayRewrite: rewrite}
}

func TestHookReplyView_ExposesCallsAndTheDeliverable(t *testing.T) {
	t.Parallel()
	view, fa := HookReplyView(replyCalls(`{"answer":"hi","type":"text"}`), "final_answer", "answer")
	if len(view) != 2 || view[0].Name != "write_file" || string(view[0].Args) != `{"path":"a","body":"sk-1"}` || view[1].ID != "c1" {
		t.Fatalf("view = %+v", view)
	}
	if fa == nil || fa.Call != 1 || fa.Value != "hi" || fa.Unreadable != "" || fa.Tool != "final_answer" || fa.Arg != "answer" {
		t.Fatalf("final answer = %+v", fa)
	}
	if _, fa := HookReplyView(replyCalls(`{"answer":"hi"}`), "respond", "body"); fa != nil {
		t.Fatalf("the configured tool is the deliverable, not the literal name: %+v", fa)
	}
	if v, fa := HookReplyView(nil, "final_answer", "answer"); v != nil || fa != nil {
		t.Fatal("a reply with no calls has no view")
	}
	for args, why := range map[string]string{
		`not json`:                 "not a JSON object",
		`["x"]`:                    "not a JSON object",
		`{"type":"text"}`:          `no "answer"`,
		`{"answer":{"nested":1}}`:  "not a string",
		`{"answer":null,"type":1}`: "not a string",
	} {
		if _, fa := HookReplyView(replyCalls(args), "final_answer", "answer"); fa == nil || !strings.Contains(fa.Unreadable, why) {
			t.Errorf("%s: %+v (want unreadable %q)", args, fa, why)
		}
	}
	two := append(replyCalls(`{"answer":"a"}`), ModelToolCall{ID: "c2", Name: "final_answer", Arguments: []byte(`{"answer":"b"}`)})
	if _, fa := HookReplyView(two, "final_answer", "answer"); fa == nil || fa.Unreadable == "" || fa.Value != "" {
		t.Errorf("two deliverables must be unreadable: %+v", fa)
	}
}

func TestSetToolCallStringArg_KeepsOtherFieldsAndValidJSON(t *testing.T) {
	t.Parallel()
	out, err := SetToolCallStringArg([]byte(`{"answer":"x","type":"text","n":[1,2]}`), "answer", `<b>"q" & é`)
	if err != nil || !json.Valid(out) {
		t.Fatalf("out=%s err=%v", out, err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if got["answer"] != `<b>"q" & é` || got["type"] != "text" || len(got["n"].([]any)) != 2 {
		t.Fatalf("got %v", got)
	}
	if !strings.Contains(string(out), `<b>`) || !strings.Contains(string(out), `& `) {
		t.Errorf("HTML-escaped: %s", out)
	}
	if _, err := SetToolCallStringArg([]byte(`nope`), "answer", "x"); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

func TestHookChain_FinalAnswerRewriteIsWrittenIntoTheCall(t *testing.T) {
	t.Parallel()
	var seen HookEvent
	c := mustChain(t, nil,
		funcHook{postModelSpec("a-redact", true), func(_ context.Context, ev HookEvent) (HookResult, error) {
			s := strings.ReplaceAll(ev.FinalAnswer.Value, "sk-9", "[R]")
			return HookResult{FinalAnswer: &s}, nil
		}},
		funcHook{postModelSpec("b-watch", false), func(_ context.Context, ev HookEvent) (HookResult, error) {
			seen = ev
			return HookResult{}, nil
		}})
	ev := postModelEvent(`{"answer":"key sk-9","type":"text"}`)
	out := c.Run(context.Background(), ev)
	if out.Verdict != HookAllow || !out.FinalAnswerRewritten || out.FinalAnswer != "key [R]" {
		t.Fatalf("outcome = %+v", out)
	}
	var args map[string]string
	if err := json.Unmarshal(out.ToolCalls[1].Args, &args); err != nil || args["answer"] != "key [R]" || args["type"] != "text" {
		t.Fatalf("rewritten call args = %s (%v)", out.ToolCalls[1].Args, err)
	}
	if string(out.ToolCalls[0].Args) != `{"path":"a","body":"sk-1"}` {
		t.Errorf("a non-final call was rewritten: %s", out.ToolCalls[0].Args)
	}
	// The later hook sees one consistent state: field and call both rewritten.
	if seen.FinalAnswer.Value != "key [R]" || strings.Contains(string(seen.ToolCalls[1].Args), "sk-9") {
		t.Errorf("next hook saw %+v / %s", seen.FinalAnswer, seen.ToolCalls[1].Args)
	}
	// The caller's event is never written into.
	if !strings.Contains(string(ev.ToolCalls[1].Args), "sk-9") {
		t.Error("the input event's tool calls were mutated")
	}
}

func TestHookChain_UnreadableDeliverableDeniesARewriter(t *testing.T) {
	t.Parallel()
	called := false
	obs := &recObserver{}
	// on_failure: allow is explicit — and still does not fail open here.
	spec := postModelSpec("redact", true)
	spec.OnFailure = HookFailAllow
	c := mustChain(t, obs, funcHook{spec, func(context.Context, HookEvent) (HookResult, error) {
		called = true
		return HookResult{}, nil
	}})
	out := c.Run(context.Background(), postModelEvent(`{"answer": "sk-9"`)) // truncated JSON
	if out.Verdict != HookDeny || out.By != "redact" || called {
		t.Fatalf("outcome = %+v called=%v", out, called)
	}
	if len(obs.invs) != 1 || obs.invs[0].Err == "" || obs.invs[0].FailedOpen {
		t.Fatalf("invocation = %+v", obs.invs)
	}

	// A hook that may not rewrite still runs and may refuse on the raw arguments.
	c = mustChain(t, nil, funcHook{postModelSpec("deny", false), func(_ context.Context, ev HookEvent) (HookResult, error) {
		if strings.Contains(string(ev.ToolCalls[ev.FinalAnswer.Call].Args), "sk-9") {
			return HookResult{Verdict: HookDeny, Reason: "secret"}, nil
		}
		return HookResult{}, nil
	}})
	if out := c.Run(context.Background(), postModelEvent(`{"answer": "sk-9"`)); out.Verdict != HookDeny || out.Reason != "secret" {
		t.Fatalf("non-rewriting deny: %+v", out)
	}
}

func TestHookChain_DenyOnTheDeliverable(t *testing.T) {
	t.Parallel()
	c := mustChain(t, nil, funcHook{postModelSpec("deny", false), func(_ context.Context, ev HookEvent) (HookResult, error) {
		if ev.FinalAnswer != nil && strings.Contains(ev.FinalAnswer.Value, "sk-") {
			return HookResult{Verdict: HookDeny, Reason: "secret in answer"}, nil
		}
		return HookResult{}, nil
	}})
	if out := c.Run(context.Background(), postModelEvent(`{"answer":"sk-9"}`)); out.Verdict != HookDeny {
		t.Fatalf("outcome = %+v", out)
	}
	if out := c.Run(context.Background(), postModelEvent(`{"answer":"fine"}`)); out.Verdict != HookAllow {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestHookChain_FinalAnswerRewriteRules(t *testing.T) {
	t.Parallel()
	s := "x"
	// Without may_rewrite: ignored, recorded.
	c := mustChain(t, nil, funcHook{postModelSpec("nr", false), answer(HookResult{FinalAnswer: &s})})
	out := c.Run(context.Background(), postModelEvent(`{"answer":"a"}`))
	if out.FinalAnswerRewritten || out.FinalAnswer != "a" || !hasEffect(out.Invocations[0].Effects, "rewrite_ignored") {
		t.Fatalf("outcome = %+v", out)
	}
	// No deliverable to rewrite: a malformed result, resolved by on_failure (allow).
	c = mustChain(t, nil, funcHook{postModelSpec("rw", true), answer(HookResult{FinalAnswer: &s})})
	out = c.Run(context.Background(), HookEvent{Point: HookPostModel, Text: "t"})
	if out.Verdict != HookAllow || out.FinalAnswerRewritten || !out.Invocations[0].FailedOpen {
		t.Fatalf("outcome = %+v", out)
	}
	// Widening stays ignored with a deliverable present.
	c = mustChain(t, nil,
		funcHook{postModelSpec("a", false), answer(HookResult{Verdict: HookAsk})},
		funcHook{postModelSpec("b", false), answer(HookResult{Verdict: HookAllow})})
	out = c.Run(context.Background(), postModelEvent(`{"answer":"a"}`))
	if out.Verdict != HookAsk || !hasEffect(out.Invocations[1].Effects, "widen_ignored") {
		t.Fatalf("outcome = %+v", out)
	}
}
