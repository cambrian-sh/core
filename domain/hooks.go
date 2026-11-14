package domain

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Lifecycle hooks (ADR-0139 D11; the C5 tool decision contract).
//
// A hook is code an operator (config) or a plugin (app.Registry.AddHook) attaches
// to a lifecycle point. It sees an immutable event and answers with a HookResult:
// continue, narrow the decision, rewrite input or output (only when declared
// may_rewrite), or add context. The rule that makes hooks safe to hand to a plugin
// is structural, not advisory:
//
//	A HOOK CAN NEVER WIDEN A DECISION.
//
// Hooks run AFTER the policy decision (HookChain.Run is handed the decision the
// reference monitor already reached), and the chain only ever moves a verdict
// toward deny: allow → ask → deny. A hook answering "allow" to an ask or a deny is
// ignored and logged as a widening attempt. A rewritten tool call is re-validated
// by the ToolExecutor (grant, resource policy, scope, effects run again on the
// rewritten arguments), so a rewrite cannot reach what the original call could not.
//
// Which points fire today:
//
//   - pre_tool / post_tool — ToolExecutor.Execute, after the policy check and
//     around dispatch;
//   - pre_model / post_model — the llm.Provider request path (organ handles and the
//     agent-plane GenerateOn/GenerateOnObserved/StreamOn chain). Streamed calls fire
//     pre_model only: chunks already delivered cannot be rewritten or withheld.
//     post_model sees the WHOLE reply: its text, its tool calls (read-only) and the
//     native tool lane's deliverable — the configured final-answer call's answer
//     argument (HookFinalAnswer), which a may_rewrite hook may rewrite and the
//     kernel writes back into that call's JSON arguments (hook_reply.go) — and,
//     generalised, every configured guarded outbound argument of the control
//     verbs that never reach the ToolExecutor (HookGuardedArg).
//
// A hook may be scoped statically by agent (HookSpec.Agents, every point) and by
// model-call purpose (HookSpec.Purposes, model points); a non-matching hook is
// skipped. Tool points keep their per-hook tool filter (config `tools`).
//
// Declared but NOT fired yet, and therefore refused (Valid is false, so config
// validation and NewHookChain reject a hook naming them):
//
//   - pre_compact / post_compact — ADR-0139 D3 (context engine compaction);
//   - run_start / run_end — ADR-0139 C6/D7 (durable runs and the event log).
//
// Until 2026-11-13 they were accepted and silently never ran, so an operator's
// run_start safety hook loaded without error and did nothing
// (T-20261113-four-declared-hook-points-pre-compact-post). The work that owns
// each moment adds its HookChain.Run call site and moves the point into
// AllHookPoints in the same change (AGENTS.md §4.2: ship wired).
//
// subagent_start / subagent_end were removed with spawn_subagent (ADR-0145 D14).

// ErrHookDenied is returned on a model path a hook narrowed. The model path has no
// approval channel, so a hook's ask is refused there like a deny. It is a
// request-class error: it never triggers provider fallback.
var ErrHookDenied = errors.New("denied by hook")

// HookPoint names a lifecycle moment a hook can attach to.
type HookPoint string

const (
	HookPreTool     HookPoint = "pre_tool"
	HookPostTool    HookPoint = "post_tool"
	HookPreModel    HookPoint = "pre_model"
	HookPostModel   HookPoint = "post_model"
	HookPreCompact  HookPoint = "pre_compact"
	HookPostCompact HookPoint = "post_compact"
	HookRunStart    HookPoint = "run_start"
	HookRunEnd      HookPoint = "run_end"
)

// AllHookPoints lists every point a hook may attach to — the points that fire —
// in lifecycle order.
var AllHookPoints = []HookPoint{
	HookPreTool, HookPostTool, HookPreModel, HookPostModel,
}

// UnfiredHookPoints are declared for future call sites but fire nowhere yet, so
// they are not Valid: a hook naming one would load and never run.
var UnfiredHookPoints = []HookPoint{
	HookPreCompact, HookPostCompact, HookRunStart, HookRunEnd,
}

// Valid reports whether p is a hook point a hook may attach to (one that fires).
func (p HookPoint) Valid() bool {
	for _, q := range AllHookPoints {
		if p == q {
			return true
		}
	}
	return false
}

// before reports whether the point precedes the thing it guards (pre_*, *_start).
// Those default to on_failure: block; the rest default to allow.
func (p HookPoint) before() bool {
	switch p {
	case HookPreTool, HookPreModel, HookPreCompact, HookRunStart:
		return true
	}
	return false
}

// HookVerdict is the decision a hook sees and may narrow. It is the C5 decision
// vocabulary minus defer (which is a scheduling answer, not a hook's to give).
type HookVerdict string

const (
	HookAllow HookVerdict = "allow"
	HookAsk   HookVerdict = "ask"
	HookDeny  HookVerdict = "deny"
)

// strictness orders verdicts; a hook may only move up this order. -1 = unknown.
func (v HookVerdict) strictness() int {
	switch v {
	case HookAllow:
		return 0
	case HookAsk:
		return 1
	case HookDeny:
		return 2
	}
	return -1
}

// NarrowVerdict combines the current verdict with a proposed one. The result is
// the stricter of the two; widened reports that the proposal tried to loosen the
// decision (and was ignored). This is the whole of the no-widening rule.
func NarrowVerdict(cur, proposed HookVerdict) (result HookVerdict, widened bool) {
	if proposed.strictness() < cur.strictness() {
		return cur, true
	}
	return proposed, false
}

// HookFailureMode is what a hook's error, timeout or panic means.
type HookFailureMode string

const (
	// HookFailAllow: a failed hook is logged and skipped (fail open).
	HookFailAllow HookFailureMode = "allow"
	// HookFailBlock: a failed hook denies the call (fail closed).
	HookFailBlock HookFailureMode = "block"
)

// DefaultHookTimeout bounds a hook whose spec sets none.
const DefaultHookTimeout = 5 * time.Second

// HookSpec is a hook's static declaration.
type HookSpec struct {
	// Name is unique across the chain; it is the second ordering key.
	Name string
	// Points the hook attaches to. A hook is called only at these.
	Points []HookPoint
	// Priority orders hooks: lower runs first; ties break by Name.
	Priority int
	// MayRewrite permits the input/output rewrites in HookResult. Without it a
	// rewrite is ignored and logged.
	MayRewrite bool
	// OnFailure overrides the per-point default (block for pre_*/ *_start, allow
	// for post_* / *_end).
	OnFailure HookFailureMode
	// Timeout bounds one invocation. 0 ⇒ DefaultHookTimeout.
	Timeout time.Duration
	// Source says where the hook came from ("config", "plugin:<id>"), for the log.
	Source string
	// Agents scopes the hook to events whose AgentID matches one of these globs
	// (MatchToolGlob: `*` and `?`, `*` crosses `/`). It applies at EVERY point —
	// a tool call, a model call and a subagent boundary all carry the acting
	// agent. Empty ⇒ every event, including one with no agent; a non-empty list
	// never matches an event with no agent id (an organ call such as the planner's
	// has none). T-20261106-post-model-hooks-cannot-be-scoped-by-agent-or.
	Agents []string
	// Purposes scopes the MODEL points (pre_model, post_model) to calls whose
	// purpose label matches one of these globs. The labels are the Provider's:
	// the organ purposes "router", "planner", "verifier", "memory", "facts";
	// "thought" for an in-kernel thought step (the kernel answering a plan step
	// itself); "agent_step" for every agent-plane gateway call (GenerateOn,
	// GenerateOnObserved, StreamOn); "model" for a model-bound handle. A hook on
	// deliverables (what reaches the caller) is purposes [agent_step, thought].
	// Ignored at the other points. Empty ⇒ every purpose.
	Purposes []string
}

// Matches reports whether the hook runs for one event: it attaches to p and its
// static scope (Agents, Purposes) covers the event's agent and model purpose. A
// non-matching hook is skipped — not invoked, not recorded, never a failure.
// The scope is static, so a caller can decide before the event exists (the live
// step-output suppression rule asks it before the reply's text is generated).
func (s HookSpec) Matches(p HookPoint, agentID, purpose string) bool {
	if !s.Attaches(p) {
		return false
	}
	if len(s.Agents) > 0 && !matchAnyGlob(s.Agents, agentID) {
		return false
	}
	if p.model() && len(s.Purposes) > 0 && !matchAnyGlob(s.Purposes, purpose) {
		return false
	}
	return true
}

// model reports whether p is a model point (the points Purposes scopes).
func (p HookPoint) model() bool { return p == HookPreModel || p == HookPostModel }

// matchAnyGlob reports whether v is non-empty and matches one of patterns.
func matchAnyGlob(patterns []string, v string) bool {
	if v == "" {
		return false
	}
	for _, g := range patterns {
		if MatchToolGlob(g, v) {
			return true
		}
	}
	return false
}

// FailureModeFor resolves the on_failure mode at one point.
func (s HookSpec) FailureModeFor(p HookPoint) HookFailureMode {
	switch s.OnFailure {
	case HookFailAllow, HookFailBlock:
		return s.OnFailure
	}
	if p.before() {
		return HookFailBlock
	}
	return HookFailAllow
}

// Attaches reports whether the hook is declared for point p.
func (s HookSpec) Attaches(p HookPoint) bool {
	for _, q := range s.Points {
		if q == p {
			return true
		}
	}
	return false
}

// HookModelCall summarises one model request for pre_model / post_model.
type HookModelCall struct {
	// Purpose is the system role, "thought" for an in-kernel thought step, or
	// "agent_step" for the agent-plane gateway.
	Purpose string `json:"purpose,omitempty"`
	// Model is the generator id when the caller fixed one (empty = resolved later).
	Model string `json:"model,omitempty"`
	// System is the text of the request's system blocks, read-only.
	System []string `json:"system,omitempty"`
	// Messages is the conversation; a may_rewrite pre_model hook may replace it.
	Messages []ModelMessage `json:"messages,omitempty"`
	// Tools names the tools offered to the model, read-only.
	Tools []string `json:"tools,omitempty"`
	// Streamed marks a streamed call (post_model does not fire for it).
	Streamed bool `json:"streamed,omitempty"`
}

// HookEvent is what a hook receives. The chain hands every hook its own deep copy,
// so a hook that mutates the event changes nothing; only HookResult has effect.
type HookEvent struct {
	Point HookPoint `json:"point"`
	// RunID is the task session the call belongs to (empty outside a session).
	RunID string `json:"run_id,omitempty"`
	// LeaseID is the per-step budget lease (session token) the call runs under.
	LeaseID string `json:"lease_id,omitempty"`
	TaskID  string `json:"task_id,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
	// Principals is the principal chain: the authenticated caller (sub), the
	// acting agent (act), then the task beneficiary when one is set.
	Principals []string `json:"principals,omitempty"`

	// Tool events.
	Tool      string          `json:"tool,omitempty"`
	Args      json.RawMessage `json:"args,omitempty"`
	Effects   []string        `json:"effects,omitempty"`
	Dangerous bool            `json:"dangerous,omitempty"`
	// Result is the tool's output (post_tool only).
	Result json.RawMessage `json:"result,omitempty"`

	// Model events.
	Model *HookModelCall `json:"model,omitempty"`
	// Text is the model's reply text (post_model only).
	Text string `json:"text,omitempty"`
	// ToolCalls are the reply's tool calls (post_model only), read-only. The
	// final-answer call reflects any earlier hook's FinalAnswer rewrite.
	ToolCalls []HookToolCall `json:"tool_calls,omitempty"`
	// FinalAnswer is the reply's deliverable on the native tool lane (post_model
	// only; nil when the reply makes no final-answer call). A refusal refuses the
	// whole reply, tool calls included.
	FinalAnswer *HookFinalAnswer `json:"final_answer,omitempty"`
	// GuardedArgs are the reply's guarded outbound arguments (post_model only):
	// in call order, one entry per call to a guarded tool that carries the
	// guarded argument — the final answer and the configured control-verb
	// arguments (execution.hooks.guarded_arguments: yield_subgoal.intent,
	// spawn_subagent.objective, …). A may_rewrite hook rewrites them through
	// HookResult.GuardedArgs. FinalAnswer is the same final-answer entry, kept for
	// hooks written before the list existed.
	GuardedArgs []HookGuardedArg `json:"guarded_args,omitempty"`

	// Verdict is the decision so far: the reference monitor's, narrowed by every
	// earlier hook in the chain.
	Verdict HookVerdict `json:"verdict"`
	// Annotations are the context earlier hooks added.
	Annotations []string `json:"annotations,omitempty"`
	// Detail carries point-specific facts for the declared-but-not-yet-fired
	// points (compaction, run and subagent boundaries).
	Detail map[string]string `json:"detail,omitempty"`
}

// clone deep-copies the event so a hook cannot reach the chain's state.
func (e HookEvent) clone() HookEvent {
	c := e
	c.Principals = append([]string(nil), e.Principals...)
	c.Args = append(json.RawMessage(nil), e.Args...)
	c.Effects = append([]string(nil), e.Effects...)
	c.Result = append(json.RawMessage(nil), e.Result...)
	c.Annotations = append([]string(nil), e.Annotations...)
	c.ToolCalls = cloneHookToolCalls(e.ToolCalls)
	if e.FinalAnswer != nil {
		fa := *e.FinalAnswer
		c.FinalAnswer = &fa
	}
	c.GuardedArgs = append([]HookGuardedArg(nil), e.GuardedArgs...)
	if e.Model != nil {
		m := *e.Model
		m.System = append([]string(nil), e.Model.System...)
		m.Messages = cloneMessages(e.Model.Messages)
		m.Tools = append([]string(nil), e.Model.Tools...)
		c.Model = &m
	}
	if e.Detail != nil {
		c.Detail = make(map[string]string, len(e.Detail))
		for k, v := range e.Detail {
			c.Detail[k] = v
		}
	}
	return c
}

func cloneHookToolCalls(in []HookToolCall) []HookToolCall {
	if in == nil {
		return nil
	}
	out := make([]HookToolCall, len(in))
	for i, c := range in {
		out[i] = c
		out[i].Args = append(json.RawMessage(nil), c.Args...)
	}
	return out
}

func cloneMessages(in []ModelMessage) []ModelMessage {
	if in == nil {
		return nil
	}
	out := make([]ModelMessage, len(in))
	for i, m := range in {
		out[i] = m
		out[i].ToolCalls = append([]ModelToolCall(nil), m.ToolCalls...)
	}
	return out
}

// HookResult is a hook's answer. The zero value is "continue".
type HookResult struct {
	// Verdict narrows the decision: "" or allow = no objection; ask = require an
	// approval; deny = refuse. Loosening the current verdict is ignored.
	Verdict HookVerdict `json:"verdict,omitempty"`
	// Reason explains a narrowing; it reaches the deny reason the agent sees.
	Reason string `json:"reason,omitempty"`
	// Args replaces the tool arguments (pre_tool, may_rewrite). Must be a JSON
	// object. The rewritten call is re-authorized from the grant onward.
	Args json.RawMessage `json:"args,omitempty"`
	// Messages replaces the model conversation (pre_model, may_rewrite).
	Messages []ModelMessage `json:"messages,omitempty"`
	// Result replaces the tool output (post_tool, may_rewrite). Must be valid JSON.
	Result json.RawMessage `json:"result,omitempty"`
	// Text replaces the model reply text (post_model, may_rewrite).
	Text *string `json:"text,omitempty"`
	// FinalAnswer replaces the deliverable — the final-answer call's answer
	// argument (post_model, may_rewrite). The kernel writes it back into that
	// call's JSON arguments, keeping the other fields. Rewriting a reply that has
	// no readable deliverable is a malformed result.
	FinalAnswer *string `json:"final_answer,omitempty"`
	// GuardedArgs replaces guarded outbound arguments (post_model, may_rewrite),
	// each named by its call index and argument as HookEvent.GuardedArgs lists
	// them. The kernel writes each value back into that call's JSON arguments.
	// Naming an argument the event does not list is a malformed result. Applied
	// after FinalAnswer; for the same argument the later value wins.
	GuardedArgs []HookArgRewrite `json:"guarded_args,omitempty"`
	// Annotations add context for later hooks and the invocation record.
	Annotations []string `json:"annotations,omitempty"`
}

// Hook is the D11 port. Implementations must honour ctx: the chain bounds each
// call with the hook's timeout through ctx and runs it on the caller's goroutine
// (no goroutine per event, AGENTS.md §3.3), so a hook that ignores ctx holds the
// call — and its late answer is still discarded as a timeout.
type Hook interface {
	Spec() HookSpec
	Run(ctx context.Context, ev HookEvent) (HookResult, error)
}

// HookInvocation is the record of one hook call and what it did.
type HookInvocation struct {
	Hook   string
	Source string
	Point  HookPoint
	// Subject is the tool name or the model purpose.
	Subject string
	AgentID string
	RunID   string
	Before  HookVerdict
	After   HookVerdict
	// Effects lists what the invocation changed: "narrowed", "rewrote_input",
	// "rewrote_output", "annotated", and the ignored attempts "widen_ignored",
	// "rewrite_ignored". Empty = continue.
	Effects []string
	Reason  string
	// Err is set when the hook failed; FailedOpen says the failure was skipped.
	Err        string
	FailedOpen bool
	Duration   time.Duration
	At         time.Time
}

// HookObserver receives every hook invocation record. It is the seam the event
// log (ADR-0139 D7) and the premium decision journal subscribe to. It MUST return
// promptly and MUST NOT panic: it is called synchronously on the tool and model
// paths.
type HookObserver interface {
	ObserveHook(HookInvocation)
}

// MultiHookObserver fans one record out to several observers.
type MultiHookObserver []HookObserver

// ObserveHook implements HookObserver.
func (m MultiHookObserver) ObserveHook(inv HookInvocation) {
	for _, o := range m {
		if o != nil {
			o.ObserveHook(inv)
		}
	}
}
