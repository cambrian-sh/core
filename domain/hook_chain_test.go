package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// funcHook is a test hook: a spec plus a function.
type funcHook struct {
	spec HookSpec
	run  func(ctx context.Context, ev HookEvent) (HookResult, error)
}

func (h funcHook) Spec() HookSpec { return h.spec }
func (h funcHook) Run(ctx context.Context, ev HookEvent) (HookResult, error) {
	return h.run(ctx, ev)
}

func answer(res HookResult) func(context.Context, HookEvent) (HookResult, error) {
	return func(context.Context, HookEvent) (HookResult, error) { return res, nil }
}

// recObserver collects invocation records.
type recObserver struct {
	mu   sync.Mutex
	invs []HookInvocation
}

func (r *recObserver) ObserveHook(inv HookInvocation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.invs = append(r.invs, inv)
}

func mustChain(t *testing.T, obs HookObserver, hs ...Hook) *HookChain {
	t.Helper()
	c, err := NewHookChain(hs, obs, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func preTool(name string, prio int) HookSpec {
	return HookSpec{Name: name, Points: []HookPoint{HookPreTool}, Priority: prio}
}

func TestHookChain_NarrowsAllowToDenyAndAsk(t *testing.T) {
	t.Parallel()
	for _, want := range []HookVerdict{HookAsk, HookDeny} {
		c := mustChain(t, nil, funcHook{preTool("n", 0), answer(HookResult{Verdict: want, Reason: "because"})})
		out := c.Run(context.Background(), HookEvent{Point: HookPreTool, Tool: "t", Verdict: HookAllow})
		if out.Verdict != want || out.By != "n" || out.Reason != "because" {
			t.Errorf("narrow to %s: got %+v", want, out)
		}
	}
}

func TestHookChain_WideningIsIgnoredAndRecorded(t *testing.T) {
	t.Parallel()
	obs := &recObserver{}
	c := mustChain(t, obs, funcHook{preTool("widen", 0), answer(HookResult{Verdict: HookAllow})})
	for _, cur := range []HookVerdict{HookAsk, HookDeny} {
		out := c.Run(context.Background(), HookEvent{Point: HookPreTool, Verdict: cur})
		if out.Verdict != cur {
			t.Fatalf("verdict %s widened to %s", cur, out.Verdict)
		}
	}
	// The ask run invoked the hook and recorded the attempt; the deny run is
	// final before any hook runs.
	if len(obs.invs) != 1 || !hasEffect(obs.invs[0].Effects, "widen_ignored") {
		t.Fatalf("widening not recorded: %+v", obs.invs)
	}
	if obs.invs[0].Before != HookAsk || obs.invs[0].After != HookAsk {
		t.Errorf("record = %+v", obs.invs[0])
	}
}

func TestHookChain_OrderIsPriorityThenNameAndNarrowingFeedsTheNext(t *testing.T) {
	t.Parallel()
	var order []string
	var sawVerdict HookVerdict
	mk := func(name string, prio int, res HookResult) Hook {
		return funcHook{preTool(name, prio), func(_ context.Context, ev HookEvent) (HookResult, error) {
			order = append(order, name)
			if name == "z-last" {
				sawVerdict = ev.Verdict
			}
			return res, nil
		}}
	}
	c := mustChain(t, nil,
		mk("z-last", 10, HookResult{}),
		mk("b", 0, HookResult{}),
		mk("a", 0, HookResult{Verdict: HookAsk}),
	)
	c.Run(context.Background(), HookEvent{Point: HookPreTool, Verdict: HookAllow})
	if !reflect.DeepEqual(order, []string{"a", "b", "z-last"}) {
		t.Fatalf("order = %v", order)
	}
	if sawVerdict != HookAsk {
		t.Errorf("a later hook saw %q; an earlier narrowing must feed it", sawVerdict)
	}
	if got := c.Names(); !reflect.DeepEqual(got, []string{"a", "b", "z-last"}) {
		t.Errorf("Names = %v", got)
	}
}

func TestHookChain_DenyIsFinal(t *testing.T) {
	t.Parallel()
	called := false
	c := mustChain(t, nil,
		funcHook{preTool("a", 0), answer(HookResult{Verdict: HookDeny})},
		funcHook{preTool("b", 1), func(context.Context, HookEvent) (HookResult, error) { called = true; return HookResult{}, nil }},
	)
	if out := c.Run(context.Background(), HookEvent{Point: HookPreTool}); out.Verdict != HookDeny {
		t.Fatalf("verdict = %s", out.Verdict)
	}
	if called {
		t.Error("a hook after a deny must not run")
	}
}

func TestHookChain_OnFailureDefaultsAndOverrides(t *testing.T) {
	t.Parallel()
	boom := func(context.Context, HookEvent) (HookResult, error) { return HookResult{}, errors.New("boom") }
	cases := []struct {
		point HookPoint
		mode  HookFailureMode
		want  HookVerdict
	}{
		{HookPreTool, "", HookDeny},    // pre default: block
		{HookPreModel, "", HookDeny},   // pre default: block
		{HookPostTool, "", HookAllow},  // post default: allow
		{HookPostModel, "", HookAllow}, // post default: allow
		{HookPreTool, HookFailAllow, HookAllow},
		{HookPostTool, HookFailBlock, HookDeny},
	}
	for _, tc := range cases {
		obs := &recObserver{}
		c := mustChain(t, obs, funcHook{HookSpec{Name: "f", Points: []HookPoint{tc.point}, OnFailure: tc.mode}, boom})
		out := c.Run(context.Background(), HookEvent{Point: tc.point})
		if out.Verdict != tc.want {
			t.Errorf("%s on_failure=%q: verdict %s, want %s", tc.point, tc.mode, out.Verdict, tc.want)
		}
		if len(obs.invs) != 1 || obs.invs[0].Err == "" || obs.invs[0].FailedOpen != (tc.want == HookAllow) {
			t.Errorf("%s on_failure=%q: record %+v", tc.point, tc.mode, obs.invs)
		}
	}
}

func TestHookChain_LateAnswerIsATimeoutAndPanicIsAFailure(t *testing.T) {
	t.Parallel()
	// A hook that ignores ctx and answers after its deadline: its "allow + rewrite"
	// must be discarded, and with on_failure block the call is denied.
	slow := funcHook{HookSpec{Name: "slow", Points: []HookPoint{HookPreTool}, Timeout: 20 * time.Millisecond, MayRewrite: true},
		func(context.Context, HookEvent) (HookResult, error) {
			time.Sleep(60 * time.Millisecond)
			return HookResult{Args: json.RawMessage(`{"x":1}`)}, nil
		}}
	out := mustChain(t, nil, slow).Run(context.Background(), HookEvent{Point: HookPreTool})
	if out.Verdict != HookDeny || out.ArgsRewritten {
		t.Fatalf("late answer honoured: %+v", out)
	}
	panicky := funcHook{preTool("p", 0), func(context.Context, HookEvent) (HookResult, error) { panic("oops") }}
	if out := mustChain(t, nil, panicky).Run(context.Background(), HookEvent{Point: HookPreTool}); out.Verdict != HookDeny {
		t.Fatalf("panic should fail closed at pre_tool: %+v", out)
	}
}

func TestHookChain_RewritesNeedMayRewriteAndTheRightPoint(t *testing.T) {
	t.Parallel()
	obs := &recObserver{}
	noRewrite := funcHook{preTool("nr", 0), answer(HookResult{Args: json.RawMessage(`{"p":"x"}`)})}
	out := mustChain(t, obs, noRewrite).Run(context.Background(), HookEvent{Point: HookPreTool, Args: json.RawMessage(`{"p":"orig"}`)})
	if out.ArgsRewritten || string(out.Args) != `{"p":"orig"}` || !hasEffect(obs.invs[0].Effects, "rewrite_ignored") {
		t.Fatalf("rewrite without may_rewrite honoured: %+v / %+v", out, obs.invs)
	}
	// A post_tool result rewrite at pre_tool is the wrong point.
	wrong := funcHook{HookSpec{Name: "w", Points: []HookPoint{HookPreTool}, MayRewrite: true}, answer(HookResult{Result: json.RawMessage(`{}`)})}
	if out := mustChain(t, nil, wrong).Run(context.Background(), HookEvent{Point: HookPreTool}); out.ResultRewritten {
		t.Fatal("result rewrite at pre_tool honoured")
	}
	// Malformed args are a failure (block at pre_tool), not a silent no-op.
	bad := funcHook{HookSpec{Name: "b", Points: []HookPoint{HookPreTool}, MayRewrite: true}, answer(HookResult{Args: json.RawMessage(`[1]`)})}
	if out := mustChain(t, nil, bad).Run(context.Background(), HookEvent{Point: HookPreTool}); out.Verdict != HookDeny || out.ArgsRewritten {
		t.Fatalf("non-object args rewrite: %+v", out)
	}
}

func TestHookChain_EventIsACopy(t *testing.T) {
	t.Parallel()
	mut := funcHook{preTool("m", 0), func(_ context.Context, ev HookEvent) (HookResult, error) {
		ev.Args[2] = 'X'
		ev.Principals[0] = "agent:evil"
		return HookResult{}, nil
	}}
	args := json.RawMessage(`{"a":1}`)
	pr := []string{"agent:a"}
	out := mustChain(t, nil, mut).Run(context.Background(), HookEvent{Point: HookPreTool, Args: args, Principals: pr})
	if string(args) != `{"a":1}` || pr[0] != "agent:a" || string(out.Args) != `{"a":1}` {
		t.Fatalf("hook mutated the chain's state: args=%s pr=%v out=%s", args, pr, out.Args)
	}
}

func TestNewHookChain_RejectsMisdeclaredHooks(t *testing.T) {
	t.Parallel()
	ok := answer(HookResult{})
	for name, hs := range map[string][]Hook{
		"no name":      {funcHook{HookSpec{Points: []HookPoint{HookPreTool}}, ok}},
		"no points":    {funcHook{HookSpec{Name: "a"}, ok}},
		"bad point":    {funcHook{HookSpec{Name: "a", Points: []HookPoint{"pre_lunch"}}, ok}},
		"duplicate":    {funcHook{preTool("a", 0), ok}, funcHook{preTool("a", 1), ok}},
		"bad failmode": {funcHook{HookSpec{Name: "a", Points: []HookPoint{HookPreTool}, OnFailure: "maybe"}, ok}},
	} {
		if _, err := NewHookChain(hs, nil, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	var nilChain *HookChain
	if nilChain.Has(HookPreTool) || nilChain.Run(context.Background(), HookEvent{Point: HookPreTool}).Verdict != HookAllow {
		t.Error("a nil chain must be an empty chain")
	}
}

// T-20261113-four-declared-hook-points-pre-compact-post: a point with no call site
// is refused at construction, so a safety hook on it fails the boot instead of
// loading and never running. The points that fire stay valid.
func TestNewHookChain_RefusesUnfiredPoints(t *testing.T) {
	t.Parallel()
	ok := answer(HookResult{})
	for _, p := range UnfiredHookPoints {
		if p.Valid() {
			t.Errorf("%s: Valid() = true for a point that fires nowhere", p)
		}
		_, err := NewHookChain([]Hook{funcHook{HookSpec{Name: "audit", Points: []HookPoint{p}}, ok}}, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "not fired yet") {
			t.Errorf("%s: err = %v, want a not-fired-yet refusal", p, err)
		}
	}
	for _, p := range []HookPoint{HookPreTool, HookPostTool, HookPreModel, HookPostModel} {
		if !p.Valid() {
			t.Errorf("%s: a fired point must stay valid", p)
		}
	}
}
