package yield

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/metabolism/dispatch"
)

// --- fakes -------------------------------------------------------------------

// agentFn is one scripted agent: it sees the handoff the kernel sent and
// returns its response (an answer or a yield).
type agentFn func(h *domain.Handoff) *domain.Handoff

// fakeDispatcher binds a task to an agent by its Description (exact match, then
// the "*" default) and calls the scripted agent. It records every Execute task
// and every CallAgent so tests can assert on what the resolver sent.
type fakeDispatcher struct {
	mu        sync.Mutex
	route     map[string]string // description → agent id
	agents    map[string]agentFn
	execErr   map[string]error // description → binder error
	tasks     []*domain.DispatchTask
	execIn    []*domain.Handoff
	calls     []string
	callIn    []*domain.Handoff
	runnerUps []domain.ScoredCandidate
}

func (f *fakeDispatcher) Execute(_ context.Context, task *domain.DispatchTask, in *domain.Handoff) (*domain.DispatchResult, error) {
	f.mu.Lock()
	f.tasks = append(f.tasks, task)
	f.execIn = append(f.execIn, in)
	f.mu.Unlock()
	if err := f.execErr[task.Description]; err != nil {
		return nil, err
	}
	id, ok := f.route[task.Description]
	if !ok {
		id = f.route["*"]
	}
	// Mirror Dispatcher.Execute: it stamps the winner's routing state on `in`.
	if in.Context == nil {
		in.Context = map[string]string{}
	}
	in.Context["_capability"] = "cap-of-" + id
	resp := f.agents[id](in)
	resp.FromAgent = id
	return &domain.DispatchResult{Handoff: resp, Confidence: 0.9, RunnerUps: f.runnerUps}, nil
}

func (f *fakeDispatcher) CallAgent(_ context.Context, agentID string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
	f.mu.Lock()
	f.calls = append(f.calls, agentID)
	f.callIn = append(f.callIn, h)
	f.mu.Unlock()
	resp := f.agents[agentID](h)
	resp.FromAgent = agentID
	return resp, nil
}

// sdkYield builds the response the Python SDK's yield_subgoal() puts on the wire
// (sdk/cambrian_agent_sdk/types.py yield_subgoal + runtime._handoff_from_result):
// payload.type "yield_subgoal", empty data, the sub-goal in metadata (→ Context).
func sdkYield(intent, hint, cont string) *domain.Handoff {
	ctx := map[string]string{"_yield": "true", "_yield_intent": intent}
	if hint != "" {
		ctx["_yield_capability_hint"] = hint
	}
	if cont != "" {
		ctx["_yield_continuation_state"] = cont
	}
	return &domain.Handoff{Payload: &domain.Payload{Type: "yield_subgoal"}, Context: ctx}
}

func answer(text string) *domain.Handoff {
	return &domain.Handoff{Payload: &domain.Payload{Type: "text", Data: []byte(text)}, Context: map[string]string{}}
}

// delegatingAgent yields `intent` on first contact and, once resumed, answers
// with what it was given — the react.py Seed C behaviour.
func delegatingAgent(intent, hint, cont string) agentFn {
	return func(h *domain.Handoff) *domain.Handoff {
		if r, ok := h.Context["_yield_result"]; ok {
			return answer("final: " + r)
		}
		return sdkYield(intent, hint, cont)
	}
}

func stepHandoff() *domain.Handoff {
	return &domain.Handoff{
		Payload: &domain.Payload{Data: []byte("analyse the 10-K")},
		Context: map[string]string{
			"task_id":           "task-0",
			"_step_index":       "0",
			"_session_token_id": "tok-1",
		},
	}
}

func stepTask() *domain.DispatchTask {
	return &domain.DispatchTask{ID: "task-0", Description: "analyse the 10-K", Context: "Subject: x"}
}

var defaultLimits = Limits{MaxDepth: 3, MaxSubgoals: 8}

// --- tests -------------------------------------------------------------------

func TestExecute_NoYieldIsPassThrough(t *testing.T) {
	f := &fakeDispatcher{
		route:  map[string]string{"*": "solo"},
		agents: map[string]agentFn{"solo": func(*domain.Handoff) *domain.Handoff { return answer("42") }},
	}
	res, err := NewResolver(f, defaultLimits).Execute(context.Background(), stepTask(), stepHandoff())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Handoff.Payload.Data); got != "42" {
		t.Fatalf("answer = %q", got)
	}
	if len(f.tasks) != 1 || len(f.calls) != 0 {
		t.Fatalf("pass-through made extra calls: tasks=%d calls=%v", len(f.tasks), f.calls)
	}
}

// The ticket's defect: without the consumer a yield came back as the step's
// (empty) answer. With it, the sub-goal is bound by dispatch, run, and the
// yielding agent is resumed with the sub-result.
func TestExecute_YieldIsDispatchedAndParentResumed(t *testing.T) {
	f := &fakeDispatcher{
		route: map[string]string{
			"analyse the 10-K":                   "analyst",
			"summarise the risk factors section": "summariser",
		},
		agents: map[string]agentFn{
			"analyst":    delegatingAgent("summarise the risk factors section", "summarisation", "Y29udA=="),
			"summariser": func(*domain.Handoff) *domain.Handoff { return answer("RISKS: rates, FX") },
		},
		runnerUps: []domain.ScoredCandidate{{Agent: domain.AgentDefinition{ID: "backup"}}},
	}
	res, err := NewResolver(f, defaultLimits).Execute(context.Background(), stepTask(), stepHandoff())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Handoff.Payload.Data); got != "final: RISKS: rates, FX" {
		t.Fatalf("final answer = %q", got)
	}
	if res.Handoff.FromAgent != "analyst" {
		t.Fatalf("final answer must be the step agent's, got %q", res.Handoff.FromAgent)
	}
	if len(res.RunnerUps) != 1 || res.Confidence != 0.9 {
		t.Fatalf("top-level selection metadata lost: %+v", res)
	}

	// Sub-goal bound through dispatch with the hint as a typed requirement.
	if len(f.tasks) != 2 {
		t.Fatalf("want 2 dispatches (step + sub-goal), got %d", len(f.tasks))
	}
	sub := f.tasks[1]
	if sub.ID != "task-0/yield-1" || sub.Description != "summarise the risk factors section" {
		t.Fatalf("sub-task = %+v", sub)
	}
	if len(sub.RequiredCapabilities) != 1 || sub.RequiredCapabilities[0] != "summarisation" {
		t.Fatalf("capability hint not carried as RequiredCapabilities: %v", sub.RequiredCapabilities)
	}
	subIn := f.execIn[1]
	if subIn.Context["_session_token_id"] != "tok-1" {
		t.Fatalf("delegate lost the session token: %v", subIn.Context)
	}
	if string(subIn.Payload.Data) != "summarise the risk factors section" {
		t.Fatalf("delegate payload = %q", subIn.Payload.Data)
	}
	if subIn.Context["_capability"] != "cap-of-summariser" {
		t.Fatalf("delegate must get its OWN routing state, got %q", subIn.Context["_capability"])
	}

	// Resume: same agent, original task + delegated result + verbatim continuation.
	if len(f.calls) != 1 || f.calls[0] != "analyst" {
		t.Fatalf("resume calls = %v", f.calls)
	}
	rh := f.callIn[0]
	if rh.Context["_yield_result"] != "RISKS: rates, FX" ||
		rh.Context["_yield_resumed_intent"] != "summarise the risk factors section" ||
		rh.Context["_yield_continuation_state"] != "Y29udA==" {
		t.Fatalf("resume context = %v", rh.Context)
	}
	if rh.Context["_session_token_id"] != "tok-1" || string(rh.Payload.Data) != "analyse the 10-K" {
		t.Fatalf("resume lost the original task: %v / %q", rh.Context, rh.Payload.Data)
	}
	if _, leaked := rh.Context["_yield"]; leaked {
		t.Fatal("resume handoff must not carry the yield marker")
	}
}

func TestExecute_NestedYieldResolvesBottomUp(t *testing.T) {
	f := &fakeDispatcher{
		route: map[string]string{"analyse the 10-K": "a", "level one": "b", "level two": "c"},
		agents: map[string]agentFn{
			"a": delegatingAgent("level one", "", ""),
			"b": delegatingAgent("level two", "", ""),
			"c": func(*domain.Handoff) *domain.Handoff { return answer("leaf") },
		},
	}
	res, err := NewResolver(f, defaultLimits).Execute(context.Background(), stepTask(), stepHandoff())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Handoff.Payload.Data); got != "final: final: leaf" {
		t.Fatalf("answer = %q", got)
	}
	if f.tasks[2].ID != "task-0/yield-1/yield-2" {
		t.Fatalf("nested sub-task id = %q", f.tasks[2].ID)
	}
}

func TestExecute_DepthLimitStopsAnEndlessChain(t *testing.T) {
	// Every agent delegates a NEW intent, so only the depth bound can stop it.
	var n int
	var mu sync.Mutex
	endless := func(*domain.Handoff) *domain.Handoff {
		mu.Lock()
		n++
		i := n
		mu.Unlock()
		return sdkYield(fmt.Sprintf("refine %d", i), "", "")
	}
	f := &fakeDispatcher{
		route:     map[string]string{"*": "deep"},
		agents:    map[string]agentFn{"deep": endless},
		runnerUps: []domain.ScoredCandidate{{Agent: domain.AgentDefinition{ID: "backup"}}},
	}
	in := stepHandoff()
	res, err := NewResolver(f, Limits{MaxDepth: 2, MaxSubgoals: 100}).Execute(context.Background(), stepTask(), in)
	if !errors.Is(err, ErrMaxDepth) {
		t.Fatalf("want ErrMaxDepth, got %v", err)
	}
	// step + depth 1 + depth 2 dispatched; the depth-2 agent's yield is refused.
	if len(f.tasks) != 3 {
		t.Fatalf("dispatches = %d, want 3", len(f.tasks))
	}
	// Failure keeps the fallback inputs, like Dispatcher.Execute's call failure.
	if res == nil || len(res.RunnerUps) != 1 || in.Context["_winning_agent_id"] != "deep" {
		t.Fatalf("fallback inputs lost: res=%+v ctx=%v", res, in.Context)
	}
}

func TestExecute_ZeroDepthRefusesYieldLoudly(t *testing.T) {
	f := &fakeDispatcher{
		route:  map[string]string{"*": "a"},
		agents: map[string]agentFn{"a": delegatingAgent("x", "", "")},
	}
	_, err := NewResolver(f, Limits{MaxDepth: 0, MaxSubgoals: 8}).Execute(context.Background(), stepTask(), stepHandoff())
	if !errors.Is(err, ErrMaxDepth) {
		t.Fatalf("a refused yield must fail the step, not answer empty; got %v", err)
	}
}

func TestExecute_SubgoalBudgetBoundsYieldResumeLoops(t *testing.T) {
	// The step agent yields a fresh intent on EVERY resume — fan-out over time.
	var rounds int
	greedy := func(h *domain.Handoff) *domain.Handoff {
		rounds++
		return sdkYield(fmt.Sprintf("part %d", rounds), "", "")
	}
	f := &fakeDispatcher{
		route: map[string]string{"analyse the 10-K": "greedy", "*": "worker"},
		agents: map[string]agentFn{
			"greedy": greedy,
			"worker": func(*domain.Handoff) *domain.Handoff { return answer("ok") },
		},
	}
	_, err := NewResolver(f, Limits{MaxDepth: 3, MaxSubgoals: 4}).Execute(context.Background(), stepTask(), stepHandoff())
	if !errors.Is(err, ErrMaxSubgoals) {
		t.Fatalf("want ErrMaxSubgoals, got %v", err)
	}
	if got := len(f.tasks) - 1; got != 4 {
		t.Fatalf("sub-goals dispatched = %d, want 4", got)
	}
}

func TestExecute_UnknownCapabilityHintFailsLoudly(t *testing.T) {
	noMatch := &domain.NoCapabilityMatchError{TaskID: "task-0/yield-1", Unmatched: []string{"astrology"}}
	f := &fakeDispatcher{
		route:   map[string]string{"*": "a"},
		agents:  map[string]agentFn{"a": delegatingAgent("cast a horoscope", "astrology", "")},
		execErr: map[string]error{"cast a horoscope": noMatch},
	}
	_, err := NewResolver(f, defaultLimits).Execute(context.Background(), stepTask(), stepHandoff())
	var got *domain.NoCapabilityMatchError
	if !errors.As(err, &got) || got.Unmatched[0] != "astrology" {
		t.Fatalf("want the dispatcher's NoCapabilityMatchError, got %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("parent must not be resumed with nothing after a failed sub-goal: %v", f.calls)
	}
}

func TestExecute_DelegateHandingWorkBackIsACycle(t *testing.T) {
	f := &fakeDispatcher{
		route: map[string]string{"analyse the 10-K": "a", "do the thing": "b"},
		agents: map[string]agentFn{
			"a": delegatingAgent("do the thing", "", ""),
			"b": delegatingAgent("  Do the   THING ", "", ""), // same intent, different spelling
		},
	}
	_, err := NewResolver(f, defaultLimits).Execute(context.Background(), stepTask(), stepHandoff())
	if !errors.Is(err, ErrRepeatedIntent) {
		t.Fatalf("want ErrRepeatedIntent, got %v", err)
	}
}

func TestExecute_ReYieldingTheReturnedIntentIsRefused(t *testing.T) {
	stubborn := func(*domain.Handoff) *domain.Handoff { return sdkYield("same job", "", "") }
	f := &fakeDispatcher{
		route: map[string]string{"analyse the 10-K": "a", "*": "w"},
		agents: map[string]agentFn{
			"a": stubborn,
			"w": func(*domain.Handoff) *domain.Handoff { return answer("done") },
		},
	}
	_, err := NewResolver(f, defaultLimits).Execute(context.Background(), stepTask(), stepHandoff())
	if !errors.Is(err, ErrRepeatedIntent) {
		t.Fatalf("want ErrRepeatedIntent, got %v", err)
	}
	if len(f.tasks) != 2 {
		t.Fatalf("the repeated intent must not be dispatched again: %d dispatches", len(f.tasks))
	}
}

func TestExecute_EmptyIntentIsInvalid(t *testing.T) {
	f := &fakeDispatcher{
		route:  map[string]string{"*": "a"},
		agents: map[string]agentFn{"a": func(*domain.Handoff) *domain.Handoff { return sdkYield("  ", "", "") }},
	}
	_, err := NewResolver(f, defaultLimits).Execute(context.Background(), stepTask(), stepHandoff())
	if !errors.Is(err, ErrInvalidSubgoal) {
		t.Fatalf("want ErrInvalidSubgoal, got %v", err)
	}
}

// The bypass arm and runner-up fallback call agents by name; a yield there used
// to be returned to the user verbatim.
func TestCallAgent_ResolvesYield(t *testing.T) {
	f := &fakeDispatcher{
		route: map[string]string{"look it up": "searcher"},
		agents: map[string]agentFn{
			"single":   delegatingAgent("look it up", "search", ""),
			"searcher": func(*domain.Handoff) *domain.Handoff { return answer("found") },
		},
	}
	resp, err := NewResolver(f, defaultLimits).CallAgent(context.Background(), "single", stepHandoff(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resp.Payload.Data); got != "final: found" {
		t.Fatalf("answer = %q", got)
	}
	if f.tasks[0].ID != "task-0/yield-1" {
		t.Fatalf("sub-task id derived from the handoff's task_id: %q", f.tasks[0].ID)
	}
}

// The resolver over the REAL ADR-0100 Dispatcher: the sub-goal goes through the
// gatekeeper with the hint as RequiredCapabilities and only the winner is called.
func TestResolver_OverRealDispatcher(t *testing.T) {
	gk := &recordingGatekeeper{byDesc: map[string][]domain.ScoredCandidate{
		"analyse the 10-K": {{Agent: domain.AgentDefinition{ID: "analyst"}, Score: 0.8}},
		"summarise risks":  {{Agent: domain.AgentDefinition{ID: "summariser"}, Score: 0.7}},
	}}
	caller := &scriptCaller{agents: map[string]agentFn{
		"analyst":    delegatingAgent("summarise risks", "summarisation", ""),
		"summariser": func(*domain.Handoff) *domain.Handoff { return answer("rates") },
	}}
	d := &dispatch.Dispatcher{Gatekeeper: gk, Caller: caller, ExecCfg: config.ExecutionConfig{}, Rand: rand.New(rand.NewSource(1))}
	res, err := NewResolver(d, defaultLimits).Execute(context.Background(), stepTask(), stepHandoff())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Handoff.Payload.Data); got != "final: rates" {
		t.Fatalf("answer = %q", got)
	}
	if want := "analyst,summariser,analyst"; strings.Join(caller.calls, ",") != want {
		t.Fatalf("calls = %v, want %s", caller.calls, want)
	}
	if rc := gk.tasks[1].RequiredCapabilities; len(rc) != 1 || rc[0] != "summarisation" {
		t.Fatalf("gatekeeper saw sub-goal requirements %v", rc)
	}
}

type recordingGatekeeper struct {
	byDesc map[string][]domain.ScoredCandidate
	tasks  []*domain.DispatchTask
}

func (g *recordingGatekeeper) FindCandidates(_ context.Context, t *domain.DispatchTask) ([]domain.ScoredCandidate, error) {
	g.tasks = append(g.tasks, t)
	return g.byDesc[t.Description], nil
}

func (g *recordingGatekeeper) FindModelCandidates(context.Context, []string) ([]domain.ScoredCandidate, error) {
	return nil, nil
}

type scriptCaller struct {
	agents map[string]agentFn
	calls  []string
}

func (c *scriptCaller) CallAgent(_ context.Context, id string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
	c.calls = append(c.calls, id)
	r := c.agents[id](h)
	r.FromAgent = id
	return r, nil
}
