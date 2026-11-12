// Package yield is the kernel consumer of an agent's yielded sub-goal
// (ADR-0041 D5, ADR-0037 D10, re-homed onto dispatch by ADR-0100 D10).
//
// A cognitive agent that meets an independent sub-task does not call a peer —
// it RETURNS a sub-goal described in capability space ("summarise the risk
// factors section", optional capability hint) and its worker is freed. The
// kernel binds that sub-goal to an executor with the same dispatch function
// that binds plan steps, runs it, and then re-dispatches the yielding agent
// with the sub-result (delegate-and-continue). The agent keeps agency over the
// final answer.
//
// History: the first consumer (YieldDriver / YieldCoordinator in
// internal/centralexec) bound sub-goals through the EFE ResourceSelector and
// was deleted with it in core b0072cc (2026-08-07). From then until this
// package, a yield reached the DAG as an EMPTY step answer — verified, cached
// and merged as though the agent had answered "" (ticket
// T-20261031-sdk-agents-can-yield-sub-goals-but-the-kernel). ADR-0100 D7/D10
// name this package as the new home and require the binder to be dispatch.
//
// Wire contract (unchanged, set by the Python SDK's yield_subgoal(),
// sdk/cambrian_agent_sdk/types.py):
//
//	yield  : payload.type = "yield_subgoal", metadata {_yield: "true",
//	         _yield_intent, _yield_capability_hint?, _yield_continuation_state? (base64)}
//	resume : metadata {_yield_result, _yield_resumed_intent,
//	         _yield_continuation_state? (verbatim)} — read by react.py's Seed C.
package yield

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/cambrian-sh/core/domain"
)

// Context keys of the yield wire contract. They are the SDK's spelling and must
// not change without changing sdk/cambrian_agent_sdk/types.py and react.py.
const (
	KeyYield             = "_yield"
	KeyIntent            = "_yield_intent"
	KeyCapabilityHint    = "_yield_capability_hint"
	KeyContinuationState = "_yield_continuation_state"
	KeyResult            = "_yield_result"
	KeyResumedIntent     = "_yield_resumed_intent"

	// ResultType is the payload type the SDK stamps on a yield (YIELD_RESULT_TYPE).
	ResultType = "yield_subgoal"
)

// Sentinel errors. Exhausting a bound FAILS the step (runner-up fallback → replan)
// rather than resuming the parent with nothing: ADR-0100 D10 #4 — "exhaustion
// resolves to replan". Each is DETERMINISTIC (domain.StepFailureDeterministic):
// the same step, composition and inputs reach the same bound again, so the
// self-healer never retries it (T-20261112-self-healer-retries-a-step-whose-yield-chain,
// owner 2026-11-12). They stay comparable with errors.Is.
var (
	// ErrMaxDepth: a sub-goal's executor yielded again past the chain-depth bound.
	ErrMaxDepth = domain.NewDeterministicError("yield: max sub-goal chain depth exceeded")
	// ErrMaxSubgoals: one step yielded more sub-goals than its budget.
	ErrMaxSubgoals = domain.NewDeterministicError("yield: sub-goal budget for this step exhausted")
	// ErrRepeatedIntent: a sub-goal repeats an intent already open on its own
	// chain — the livelock signature (an agent delegating its own task back to
	// the pool, or re-delegating after the resume told it not to).
	ErrRepeatedIntent = domain.NewDeterministicError("yield: sub-goal repeats an intent already on its chain")
	// ErrInvalidSubgoal: the yield carried no intent (the SDK refuses to build
	// one, so this is a non-SDK agent or a corrupted handoff).
	ErrInvalidSubgoal = domain.NewDeterministicError("yield: sub-goal has no intent")
	// ErrUnknownYielder: the yielding response names no agent, so there is
	// nobody to resume.
	ErrUnknownYielder = domain.NewDeterministicError("yield: yielding response has no from_agent to resume")
)

// stepBudgetKey carries a step's StepBudget on its ctx.
type stepBudgetKey struct{}

// StepBudget is one plan step's sub-goal budget. It lives on the STEP, not on one
// Resolver.Execute call: the self-healer re-runs Execute on the same step ctx, and
// a budget per call made yield_max_subgoals a per-ATTEMPT allowance (3 attempts x
// 8 sub-goals; T-20261112-self-healer-retries-a-step-whose-yield-chain, owner
// 2026-11-12: "budgets live on the step so a retry inherits them"). The depth and
// repeated-intent bounds are per chain: an attempt that reaches either fails
// deterministically and is never retried, so no later attempt can reset them.
type StepBudget struct {
	mu       sync.Mutex
	subgoals int
}

// WithStepBudget gives the plan step about to run on ctx a fresh sub-goal budget.
// The server calls it once per step, before the self-healer, so every attempt of
// the step draws on the same budget. A nested plan's steps get their own.
func WithStepBudget(ctx context.Context) context.Context {
	return context.WithValue(ctx, stepBudgetKey{}, &StepBudget{})
}

// StepBudgetFrom is the step's budget, or a fresh one for a call that is not a
// plan step (a bypass call, a plugin): its budget is the call.
func StepBudgetFrom(ctx context.Context) *StepBudget {
	if b, ok := ctx.Value(stepBudgetKey{}).(*StepBudget); ok && b != nil {
		return b
	}
	return &StepBudget{}
}

// take spends one sub-goal: the new count, or ok=false with the count so far once
// max is reached.
func (b *StepBudget) take(max int) (n int, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subgoals >= max {
		return b.subgoals, false
	}
	b.subgoals++
	return b.subgoals, true
}

// Used is how many sub-goals the step has dispatched.
func (b *StepBudget) Used() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.subgoals
}

// Limits bounds a yield tree so no agent behaviour can make it recurse forever.
// Both come from config (execution.routing.yield_max_depth /
// yield_max_subgoals; Zero-Hardcode Rule).
type Limits struct {
	// MaxDepth is the deepest sub-goal nesting allowed: 1 means a step's agent
	// may delegate, but the delegate may not delegate again. ≤ 0 refuses every
	// yield (loudly — the step fails with ErrMaxDepth instead of yielding an
	// empty answer).
	MaxDepth int
	// MaxSubgoals caps the total sub-goals dispatched for one plan step (its
	// StepBudget, across the self-healer's attempts) — or for one top-level
	// Execute/CallAgent that is not a plan step — across the whole tree and across
	// resume rounds. It is the fan-out bound: an agent that yields, is resumed, and
	// yields again is counted every time.
	MaxSubgoals int
}

// Resolver decorates a domain.StepDispatcher with the yield consumer. It is a
// drop-in for the Dispatcher at every Selector call site (DAG step, bypass arm,
// runner-up fallback, plugins), so none of them can receive a raw yield as an
// answer. Stateless across calls; safe for concurrent use.
type Resolver struct {
	inner  domain.StepDispatcher
	limits Limits
}

// NewResolver wraps inner. inner is required — it is both the binder (Execute:
// ADR-0100 D10 #2, the dispatch function is the one binder) and the caller
// (CallAgent, used to resume).
func NewResolver(inner domain.StepDispatcher, limits Limits) *Resolver {
	if inner == nil {
		panic("yield.NewResolver: inner dispatcher is required")
	}
	return &Resolver{inner: inner, limits: limits}
}

var _ domain.StepDispatcher = (*Resolver)(nil)

// IsYield reports whether h is a yielded sub-goal rather than an answer.
func IsYield(h *domain.Handoff) bool {
	if h == nil {
		return false
	}
	if h.Context != nil && h.Context[KeyYield] == "true" {
		return true
	}
	return h.Payload != nil && h.Payload.Type == ResultType
}

// Execute dispatches the step and, if the winner yields, resolves the sub-goal
// tree before returning the winner's final (non-yield) answer. Selection
// metadata (confidence, runner-ups, model allocation) is the TOP-LEVEL
// selection's: the step was bound to the yielding agent, and runner-up fallback
// must retry the step, not the sub-goal.
func (r *Resolver) Execute(ctx context.Context, task *domain.DispatchTask, in *domain.Handoff) (*domain.DispatchResult, error) {
	res, err := r.inner.Execute(ctx, task, in)
	if err != nil || res == nil || !IsYield(res.Handoff) {
		return res, err
	}
	run := &run{r: r, budget: StepBudgetFrom(ctx)}
	final, rErr := run.resolve(ctx, task, in, res.Handoff, 0, nil)
	if rErr != nil {
		// Mirror Dispatcher.Execute's call-failure shape so the server's fallback
		// still sees the runner-ups and who failed.
		if in.Context == nil {
			in.Context = map[string]string{}
		}
		in.Context["_winning_agent_id"] = res.Handoff.FromAgent
		return &domain.DispatchResult{Confidence: res.Confidence, RunnerUps: res.RunnerUps}, rErr
	}
	out := *res
	out.Handoff = final
	return &out, nil
}

// CallAgent invokes a named agent and resolves any yield it returns. The bypass
// arm and runner-up fallback call agents by name; a yield there would otherwise
// be handed straight back to the user as the answer.
func (r *Resolver) CallAgent(ctx context.Context, agentID string, handoff *domain.Handoff, excludeInstanceID string) (*domain.Handoff, error) {
	resp, err := r.inner.CallAgent(ctx, agentID, handoff, excludeInstanceID)
	if err != nil || !IsYield(resp) {
		return resp, err
	}
	if resp.FromAgent == "" {
		resp.FromAgent = agentID // we know exactly who we called
	}
	run := &run{r: r, budget: StepBudgetFrom(ctx)}
	return run.resolve(ctx, nil, handoff, resp, 0, nil)
}

// run is the per-top-level-call state; its sub-goal budget is the step's.
type run struct {
	r      *Resolver
	budget *StepBudget
}

// resolve runs the yield loop for one agent that has yielded (`first`) on
// handoff `in`. depth is that agent's own nesting (0 = the step's agent); chain
// is the intents open above it. Each iteration dispatches one sub-goal (which
// may itself yield — recursion at depth+1) and resumes the agent with the
// sub-result, until the agent answers.
func (u *run) resolve(ctx context.Context, task *domain.DispatchTask, in, first *domain.Handoff, depth int, chain []string) (*domain.Handoff, error) {
	agentID := first.FromAgent
	if agentID == "" {
		return nil, ErrUnknownYielder
	}
	resp := first
	for IsYield(resp) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		intent := strings.TrimSpace(resp.Context[KeyIntent])
		if intent == "" {
			return nil, fmt.Errorf("agent %q: %w", agentID, ErrInvalidSubgoal)
		}
		if depth >= u.r.limits.MaxDepth {
			return nil, fmt.Errorf("agent %q at depth %d yielding %q: %w", agentID, depth, intent, ErrMaxDepth)
		}
		norm := normalizeIntent(intent)
		for _, open := range chain {
			if open == norm {
				return nil, fmt.Errorf("agent %q yielding %q: %w", agentID, intent, ErrRepeatedIntent)
			}
		}
		n, ok := u.budget.take(u.r.limits.MaxSubgoals)
		if !ok {
			return nil, fmt.Errorf("agent %q yielding %q after %d sub-goals: %w", agentID, intent, n, ErrMaxSubgoals)
		}
		hint := strings.TrimSpace(resp.Context[KeyCapabilityHint])
		cont := resp.Context[KeyContinuationState]

		subTask := subDispatchTask(task, in, intent, hint, n)
		slog.Info("yield: dispatching sub-goal (ADR-0041 D5 / ADR-0100 D10)",
			"parent_agent", agentID, "intent", intent, "capability_hint", hint,
			"depth", depth+1, "subgoal", n, "task", subTask.ID)

		// Bind + call through the dispatch function: the same eligibility gate,
		// D5 resolution ladder and merit ranking a plan step gets.
		subIn := subHandoff(in, intent, subTask.ID)
		subRes, err := u.r.inner.Execute(ctx, subTask, subIn)
		if err != nil {
			// Worded for the replanner (replan_cap v3): a yielded action the agent did
			// not perform itself — typically for lack of a tool — that no other agent
			// could take either is the under-declaration signal, and the replan adds
			// the action's effect to the step's required_effects. On a composed lease
			// the sub-goal runs under the parent's composition, so dispatch never
			// fails for a missing tool by itself: the composer refuses a sub-goal whose
			// capability hint that composition does not cover, and a worker reports
			// capability_missing — both arrive here as a domain.CapabilityMissingError
			// (class needs_replan), kept in the chain by %w
			// (T-20261112-yield-sub-goal-on-a-composed-lease-runs-under).
			return nil, fmt.Errorf("yield: agent %q did not perform %q itself (it yielded it, e.g. for lack of a tool) and dispatching it failed: %w", agentID, intent, err)
		}
		if subRes == nil || subRes.Handoff == nil {
			return nil, fmt.Errorf("yield: dispatch sub-goal %q (from %q): no response", intent, agentID)
		}
		sub := subRes.Handoff
		if IsYield(sub) {
			// The delegate delegated. Its chain includes every intent open above
			// it plus this one, so a delegate handing the same work back is a
			// cycle, not another round.
			sub, err = u.resolve(ctx, subTask, subIn, sub, depth+1, append(append([]string(nil), chain...), norm))
			if err != nil {
				return nil, err
			}
		}

		// Resume the yielding agent with the sub-result (delegate-and-continue).
		resp, err = u.r.inner.CallAgent(ctx, agentID, resumeHandoff(in, intent, payloadText(sub), cont), "")
		if err != nil {
			return nil, fmt.Errorf("yield: resume %q after sub-goal %q: %w", agentID, intent, err)
		}
		if resp == nil {
			return nil, fmt.Errorf("yield: resume %q after sub-goal %q: no response", agentID, intent)
		}
		if resp.FromAgent == "" {
			resp.FromAgent = agentID
		}
		// A resumed agent that yields AGAIN gets another round (bounded by
		// MaxSubgoals); re-yielding the intent it was just given back is caught
		// by the chain check only for its descendants, so guard it here too.
		if IsYield(resp) && normalizeIntent(resp.Context[KeyIntent]) == norm {
			return nil, fmt.Errorf("agent %q re-yielded %q after its result was returned: %w", agentID, intent, ErrRepeatedIntent)
		}
	}
	return resp, nil
}

func normalizeIntent(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// subDispatchTask is the request the dispatch function binds a sub-goal with.
// ADR-0100 D10 #1: the capability hint becomes RequiredCapabilities, so a
// sub-goal goes through exactly the typed gate + D5 ladder a step does, and an
// unsatisfiable hint fails loudly (NoCapabilityMatchError) instead of routing
// to whoever ranks first. The ID is derived from the parent's, never a clock
// (AGENTS.md §2.6), so retries and logs correlate.
func subDispatchTask(parent *domain.DispatchTask, in *domain.Handoff, intent, hint string, n int) *domain.DispatchTask {
	parentID := ""
	if parent != nil {
		parentID = parent.ID
	} else if in != nil && in.Context != nil {
		parentID = in.Context["task_id"]
	}
	if parentID == "" {
		parentID = "task"
	}
	t := &domain.DispatchTask{
		ID:          fmt.Sprintf("%s/yield-%d", parentID, n),
		Description: intent,
		Subgoal:     true,
	}
	if parent != nil {
		t.Context = parent.Context
		t.Deadline = parent.Deadline
	}
	if hint != "" {
		t.RequiredCapabilities = []string{hint}
	}
	return t
}

// subHandoffKeys are the parent context keys a delegate needs. The token is
// per-session, not per-agent, so the delegate's generate() is authorised and
// billed to the same session; the rest is correlation. Everything else is the
// PARENT's routing state (_capability, _selection_reason, the parent's own
// _yield_* keys) and would misroute the delegate if copied.
var subHandoffKeys = []string{"_session_token_id", "_step_index", "_task_id"}

func subHandoff(parent *domain.Handoff, intent, taskID string) *domain.Handoff {
	h := &domain.Handoff{
		Payload: &domain.Payload{Type: "text", Data: []byte(intent)},
		Context: map[string]string{"task_id": taskID},
	}
	if parent != nil {
		if parent.Payload != nil && parent.Payload.Type != "" {
			h.Payload.Type = parent.Payload.Type
		}
		for _, k := range subHandoffKeys {
			if v := parent.Context[k]; v != "" {
				h.Context[k] = v
			}
		}
	}
	return h
}

// resumeHandoff re-dispatches the yielding agent: its original task (payload,
// working memory, lease, context) plus the delegated result. The SDK's run_think
// seeds a <delegated> card from _yield_result and continues (react.py, Seed C).
// _yield_continuation_state is returned verbatim, opaque to the kernel.
func resumeHandoff(orig *domain.Handoff, intent, subResult, cont string) *domain.Handoff {
	h := &domain.Handoff{Context: map[string]string{}}
	if orig != nil {
		h.ID = orig.ID
		h.FromAgent = orig.FromAgent
		h.ToAgent = orig.ToAgent
		h.Payload = orig.Payload
		h.WorkingMemory = orig.WorkingMemory
		h.BudgetLease = orig.BudgetLease
		for k, v := range orig.Context {
			if strings.HasPrefix(k, "_yield") {
				continue // a previous round's result must not shadow this one
			}
			h.Context[k] = v
		}
	}
	h.Context[KeyResult] = subResult
	h.Context[KeyResumedIntent] = intent
	if cont != "" {
		h.Context[KeyContinuationState] = cont
	}
	return h
}

func payloadText(h *domain.Handoff) string {
	if h == nil || h.Payload == nil {
		return ""
	}
	return string(h.Payload.Data)
}
