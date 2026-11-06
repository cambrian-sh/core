package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

// HookChain runs the registered hooks for a point in a stable order (priority,
// then name) and folds their answers into one HookOutcome. One hook's narrowing
// and rewrites feed the next. It is immutable after NewHookChain and safe for
// concurrent use; a nil *HookChain is a valid empty chain.
type HookChain struct {
	hooks    []Hook
	specs    []HookSpec
	observer HookObserver
	log      *slog.Logger
}

// Annotation bounds: hooks add context, they do not get to flood a prompt or a log.
const (
	maxHookAnnotations     = 32
	maxHookAnnotationChars = 2000
)

// NewHookChain validates and orders hooks. A hook with no name, an undeclared
// point, a duplicate name or an unknown on_failure mode is a construction error —
// a misdeclared safety control must fail the boot, not degrade silently.
func NewHookChain(hooks []Hook, observer HookObserver, log *slog.Logger) (*HookChain, error) {
	if log == nil {
		log = slog.Default()
	}
	type entry struct {
		h    Hook
		spec HookSpec
	}
	entries := make([]entry, 0, len(hooks))
	seen := make(map[string]bool, len(hooks))
	for _, h := range hooks {
		if h == nil {
			continue
		}
		s := h.Spec()
		if s.Name == "" {
			return nil, errors.New("hook: a hook has no name")
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("hook %q: declared twice", s.Name)
		}
		seen[s.Name] = true
		if len(s.Points) == 0 {
			return nil, fmt.Errorf("hook %q: declares no points", s.Name)
		}
		for _, p := range s.Points {
			if !p.Valid() {
				return nil, fmt.Errorf("hook %q: unknown point %q", s.Name, p)
			}
		}
		switch s.OnFailure {
		case "", HookFailAllow, HookFailBlock:
		default:
			return nil, fmt.Errorf("hook %q: on_failure must be allow or block (got %q)", s.Name, s.OnFailure)
		}
		if s.Timeout < 0 {
			return nil, fmt.Errorf("hook %q: negative timeout", s.Name)
		}
		if err := validateHookScope(s); err != nil {
			return nil, err
		}
		entries = append(entries, entry{h, s})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].spec.Priority != entries[j].spec.Priority {
			return entries[i].spec.Priority < entries[j].spec.Priority
		}
		return entries[i].spec.Name < entries[j].spec.Name
	})
	c := &HookChain{observer: observer, log: log}
	for _, e := range entries {
		c.hooks = append(c.hooks, e.h)
		c.specs = append(c.specs, e.spec)
	}
	return c, nil
}

// Has reports whether any hook attaches to p. Call sites check it first so an
// unhooked deployment builds no event at all.
func (c *HookChain) Has(p HookPoint) bool {
	if c == nil {
		return false
	}
	for _, s := range c.specs {
		if s.Attaches(p) {
			return true
		}
	}
	return false
}

// validateHookScope checks a spec's static scope: no empty glob, and Purposes only
// on a hook that attaches to a model point (anywhere else it would be ignored,
// which is a misdeclaration, not a scope).
func validateHookScope(s HookSpec) error {
	for _, g := range s.Agents {
		if g == "" {
			return fmt.Errorf("hook %q: agents must not contain an empty glob", s.Name)
		}
	}
	for _, g := range s.Purposes {
		if g == "" {
			return fmt.Errorf("hook %q: purposes must not contain an empty glob", s.Name)
		}
	}
	if len(s.Purposes) > 0 && !s.Attaches(HookPreModel) && !s.Attaches(HookPostModel) {
		return fmt.Errorf("hook %q: purposes scope model calls, but the hook attaches to no model point", s.Name)
	}
	return nil
}

// HasFor reports whether any hook would run at p for an event of agentID and
// model purpose — Has narrowed by the hooks' static scope (HookSpec.Matches).
func (c *HookChain) HasFor(p HookPoint, agentID, purpose string) bool {
	if c == nil {
		return false
	}
	for _, s := range c.specs {
		if s.Matches(p, agentID, purpose) {
			return true
		}
	}
	return false
}

// Specs returns copies of the hook specs in run order (boot logs, wiring).
func (c *HookChain) Specs() []HookSpec {
	if c == nil {
		return nil
	}
	out := make([]HookSpec, len(c.specs))
	for i, s := range c.specs {
		s.Points = append([]HookPoint(nil), s.Points...)
		s.Agents = append([]string(nil), s.Agents...)
		s.Purposes = append([]string(nil), s.Purposes...)
		out[i] = s
	}
	return out
}

// Names returns the hook names in run order (for boot logs and tests).
func (c *HookChain) Names() []string {
	if c == nil {
		return nil
	}
	out := make([]string, len(c.specs))
	for i, s := range c.specs {
		out[i] = s.Name
	}
	return out
}

// HookOutcome is the folded result of one chain run.
type HookOutcome struct {
	// Verdict is the final decision: never looser than the event's input verdict.
	Verdict HookVerdict
	// Reason and By name the hook that set the strictest verdict.
	Reason string
	By     string

	Args          json.RawMessage
	ArgsRewritten bool

	Messages          []ModelMessage
	MessagesRewritten bool

	Result          json.RawMessage
	ResultRewritten bool

	Text          string
	TextRewritten bool

	// ToolCalls is the reply's tool calls with every guarded-argument rewrite (the
	// deliverable's included) written into the calls' arguments; GuardedArgs are
	// the guarded arguments' current values; FinalAnswer is the deliverable's.
	// GuardedArgsRewritten says some call's arguments changed (FinalAnswerRewritten
	// implies it).
	ToolCalls            []HookToolCall
	GuardedArgs          []HookGuardedArg
	GuardedArgsRewritten bool
	FinalAnswer          string
	FinalAnswerRewritten bool

	Annotations []string
	Invocations []HookInvocation
}

// DenyReason renders the outcome's refusal for a deny reason / error string.
func (o HookOutcome) DenyReason() string {
	if o.Reason == "" {
		return "hook " + o.By
	}
	return "hook " + o.By + ": " + o.Reason
}

// Run invokes every hook attached to ev.Point, in order. ev.Verdict is the
// decision the policy already reached; an empty verdict is read as allow.
//
// Per hook: the call is bounded by the hook's timeout through ctx and runs on
// this goroutine; an error, a timeout, a panic, an unknown verdict or a malformed
// rewrite is a FAILURE, resolved by the hook's on_failure mode (block ⇒ deny,
// allow ⇒ skip). Once the verdict is deny the remaining hooks are not called —
// deny is final, and there is nothing left for them to narrow.
//
// A hook whose static scope (HookSpec.Agents / Purposes) does not cover the
// event is skipped: not invoked, not recorded, not a failure.
//
// One failure ignores on_failure and always denies: at post_model, a may_rewrite
// hook facing a deliverable or guarded argument the kernel cannot rewrite
// (HookFinalAnswer.Unreadable, HookGuardedArg.Unreadable, or a rewrite that cannot
// be written back into the call's arguments). Failing open there would pass the
// very value a redaction was declared to hide.
func (c *HookChain) Run(ctx context.Context, ev HookEvent) HookOutcome {
	if ev.Verdict == "" {
		ev.Verdict = HookAllow
	}
	out := HookOutcome{Verdict: ev.Verdict, Args: ev.Args, Result: ev.Result, Text: ev.Text, ToolCalls: ev.ToolCalls,
		GuardedArgs: ev.GuardedArgs}
	if ev.FinalAnswer != nil {
		out.FinalAnswer = ev.FinalAnswer.Value
	}
	unreadable := guardedUnreadable(ev)
	if ev.Model != nil {
		out.Messages = ev.Model.Messages
	}
	out.Annotations = append(out.Annotations, ev.Annotations...)
	if c == nil {
		return out
	}
	subject := ev.Tool
	purpose := ""
	if ev.Model != nil {
		purpose = ev.Model.Purpose
	}
	if subject == "" {
		subject = purpose
	}
	for i, h := range c.hooks {
		spec := c.specs[i]
		if !spec.Matches(ev.Point, ev.AgentID, purpose) {
			continue
		}
		if out.Verdict == HookDeny {
			break
		}
		// The hook sees the state the earlier hooks produced.
		cur := ev
		cur.Verdict = out.Verdict
		cur.Args = out.Args
		cur.Result = out.Result
		cur.Text = out.Text
		cur.ToolCalls = out.ToolCalls
		cur.GuardedArgs = out.GuardedArgs
		if ev.FinalAnswer != nil {
			fa := *ev.FinalAnswer
			fa.Value = out.FinalAnswer
			cur.FinalAnswer = &fa
		}
		cur.Annotations = out.Annotations
		if ev.Model != nil {
			m := *ev.Model
			m.Messages = out.Messages
			cur.Model = &m
		}
		inv := HookInvocation{
			Hook: spec.Name, Source: spec.Source, Point: ev.Point, Subject: subject,
			AgentID: ev.AgentID, RunID: ev.RunID, Before: out.Verdict, At: time.Now(),
		}
		var res HookResult
		var err error
		if spec.MayRewrite && ev.Point == HookPostModel && unreadable != "" {
			// Not invoked: whatever it would rewrite, the kernel could not apply.
			err = fmt.Errorf("%w: %s", errDeliverableUnwritable, unreadable)
		} else {
			res, err = c.invoke(ctx, h, spec, cur.clone())
		}
		inv.Duration = time.Since(inv.At)
		if err == nil {
			err = c.apply(&out, &inv, spec, ev.Point, ev.FinalAnswer, res)
		}
		if err != nil {
			inv.Err = err.Error()
			if spec.FailureModeFor(ev.Point) == HookFailBlock || errors.Is(err, errDeliverableUnwritable) {
				out.Verdict = HookDeny
				out.By = spec.Name
				out.Reason = "failed (on_failure: block): " + err.Error()
				inv.Effects = append(inv.Effects, "narrowed")
				inv.Reason = out.Reason
			} else {
				inv.FailedOpen = true
			}
		}
		inv.After = out.Verdict
		c.record(inv)
		out.Invocations = append(out.Invocations, inv)
	}
	return out
}

// invoke runs one hook under its timeout, converting a panic and a late answer
// into errors. No goroutine: the deadline is enforced through ctx.
func (c *HookChain) invoke(ctx context.Context, h Hook, spec HookSpec, ev HookEvent) (res HookResult, err error) {
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = DefaultHookTimeout
	}
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			res, err = HookResult{}, fmt.Errorf("panic: %v", r)
		}
	}()
	res, err = h.Run(hctx, ev)
	if err == nil && hctx.Err() != nil {
		// The hook answered after its deadline (it ignored ctx). Its answer is
		// discarded: a late verdict must not be read as a timely one.
		err = fmt.Errorf("answered after its deadline (%s): %v", timeout, hctx.Err())
	}
	if err != nil && errors.Is(hctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("timed out after %s: %w", timeout, err)
	}
	return res, err
}

// apply folds one hook's result into the outcome, enforcing no-widening and the
// may_rewrite / per-point rewrite rules. An error means the result was malformed.
func (c *HookChain) apply(out *HookOutcome, inv *HookInvocation, spec HookSpec, p HookPoint, fa *HookFinalAnswer, res HookResult) error {
	if res.Verdict != "" && res.Verdict.strictness() < 0 {
		return fmt.Errorf("unknown verdict %q", res.Verdict)
	}
	// Validate rewrites before applying anything, so a malformed result changes nothing.
	if spec.MayRewrite {
		if len(res.Args) > 0 && p == HookPreTool {
			var obj map[string]json.RawMessage
			if json.Unmarshal(res.Args, &obj) != nil || obj == nil {
				return errors.New("rewritten args are not a JSON object")
			}
		}
		if len(res.Result) > 0 && p == HookPostTool && !json.Valid(res.Result) {
			return errors.New("rewritten result is not valid JSON")
		}
	}
	// Guarded-argument rewrites (the deliverable first, then the general list),
	// computed into a copy of the calls; applied below only if everything validates.
	var argRewrites []HookArgRewrite
	var newCalls []HookToolCall
	if spec.MayRewrite && p == HookPostModel {
		if res.FinalAnswer != nil {
			if fa == nil {
				return errors.New("rewrote final_answer, but the reply makes no final-answer call")
			}
			argRewrites = append(argRewrites, HookArgRewrite{Call: fa.Call, Arg: fa.Arg, Value: *res.FinalAnswer})
		}
		argRewrites = append(argRewrites, res.GuardedArgs...)
		if len(argRewrites) > 0 {
			newCalls = cloneHookToolCalls(out.ToolCalls)
		}
		for _, r := range argRewrites {
			isFinal := fa != nil && r.Call == fa.Call && r.Arg == fa.Arg
			if !isFinal && guardedIndex(out.GuardedArgs, r) < 0 {
				return fmt.Errorf("rewrote argument %q of call %d, which is not a guarded argument of the reply", r.Arg, r.Call)
			}
			if r.Call < 0 || r.Call >= len(newCalls) {
				return fmt.Errorf("%w: call %d is out of range", errDeliverableUnwritable, r.Call)
			}
			args, err := SetToolCallStringArg(newCalls[r.Call].Args, r.Arg, r.Value)
			if err != nil {
				return fmt.Errorf("%w: %v", errDeliverableUnwritable, err)
			}
			newCalls[r.Call].Args = args
		}
	}

	if res.Verdict != "" {
		next, widened := NarrowVerdict(out.Verdict, res.Verdict)
		if widened {
			inv.Effects = append(inv.Effects, "widen_ignored")
			c.log.Warn("ADR-0139 D11: hook tried to widen a decision; ignored",
				"hook", spec.Name, "point", p, "subject", inv.Subject,
				"verdict", out.Verdict, "proposed", res.Verdict)
		} else if next != out.Verdict {
			out.Verdict = next
			out.By = spec.Name
			out.Reason = res.Reason
			inv.Effects = append(inv.Effects, "narrowed")
			inv.Reason = res.Reason
		}
	}

	rewrite := func(kind string, ok bool, allowedAt HookPoint, do func()) {
		if !ok {
			return
		}
		if !spec.MayRewrite || p != allowedAt {
			inv.Effects = append(inv.Effects, "rewrite_ignored")
			c.log.Warn("ADR-0139 D11: hook rewrite ignored (not may_rewrite, or wrong point)",
				"hook", spec.Name, "point", p, "rewrite", kind, "may_rewrite", spec.MayRewrite)
			return
		}
		do()
		if p == HookPreTool || p == HookPreModel {
			inv.Effects = append(inv.Effects, "rewrote_input")
		} else {
			inv.Effects = append(inv.Effects, "rewrote_output")
		}
	}
	rewrite("args", len(res.Args) > 0, HookPreTool, func() {
		out.Args = append(json.RawMessage(nil), res.Args...)
		out.ArgsRewritten = true
	})
	rewrite("messages", res.Messages != nil, HookPreModel, func() {
		out.Messages = cloneMessages(res.Messages)
		out.MessagesRewritten = true
	})
	rewrite("result", len(res.Result) > 0, HookPostTool, func() {
		out.Result = append(json.RawMessage(nil), res.Result...)
		out.ResultRewritten = true
	})
	rewrite("text", res.Text != nil, HookPostModel, func() {
		out.Text = *res.Text
		out.TextRewritten = true
	})
	rewrite("guarded_args", res.FinalAnswer != nil || len(res.GuardedArgs) > 0, HookPostModel, func() {
		// Copies: the event's (and the caller's) tool calls are never written into.
		out.ToolCalls = newCalls
		guarded := append([]HookGuardedArg(nil), out.GuardedArgs...)
		for _, r := range argRewrites {
			if i := guardedIndex(guarded, r); i >= 0 {
				guarded[i].Value = r.Value
			}
			if fa != nil && r.Call == fa.Call && r.Arg == fa.Arg {
				out.FinalAnswer = r.Value
				out.FinalAnswerRewritten = true
			}
		}
		out.GuardedArgs = guarded
		out.GuardedArgsRewritten = true
	})

	added := false
	for _, a := range res.Annotations {
		if a == "" || len(out.Annotations) >= maxHookAnnotations {
			continue
		}
		if len(a) > maxHookAnnotationChars {
			a = a[:maxHookAnnotationChars]
		}
		out.Annotations = append(out.Annotations, spec.Name+": "+a)
		added = true
	}
	if added {
		inv.Effects = append(inv.Effects, "annotated")
	}
	return nil
}

// record logs one invocation with structured fields and hands it to the observer.
// Narrowings, failures and ignored attempts log at warn; plain continues at debug,
// so a busy chain does not drown the log in "nothing happened".
func (c *HookChain) record(inv HookInvocation) {
	attrs := []any{
		"hook", inv.Hook, "source", inv.Source, "point", inv.Point, "subject", inv.Subject,
		"agent", inv.AgentID, "run", inv.RunID, "before", inv.Before, "after", inv.After,
		"effects", inv.Effects, "duration_ms", inv.Duration.Milliseconds(),
	}
	if inv.Reason != "" {
		attrs = append(attrs, "reason", inv.Reason)
	}
	switch {
	case inv.Err != "":
		attrs = append(attrs, "err", inv.Err, "failed_open", inv.FailedOpen)
		c.log.Warn("ADR-0139 D11: hook failed", attrs...)
	case inv.Before != inv.After || hasEffect(inv.Effects, "widen_ignored", "rewrite_ignored"):
		c.log.Warn("ADR-0139 D11: hook narrowed or was refused", attrs...)
	case len(inv.Effects) > 0:
		c.log.Info("ADR-0139 D11: hook acted", attrs...)
	default:
		c.log.Debug("ADR-0139 D11: hook continued", attrs...)
	}
	if c.observer != nil {
		c.observer.ObserveHook(inv)
	}
}

// guardedIndex finds the guarded argument a rewrite names, or -1.
func guardedIndex(gs []HookGuardedArg, r HookArgRewrite) int {
	for i, g := range gs {
		if g.Call == r.Call && g.Arg == r.Arg {
			return i
		}
	}
	return -1
}

// guardedUnreadable is why some guarded argument of a post_model event (the
// deliverable included) cannot be read, or "" when every one can.
func guardedUnreadable(ev HookEvent) string {
	if ev.Point != HookPostModel {
		return ""
	}
	if fa := ev.FinalAnswer; fa != nil && fa.Unreadable != "" {
		return fa.Unreadable
	}
	for _, g := range ev.GuardedArgs {
		if g.Unreadable != "" {
			return g.Tool + "." + g.Arg + ": " + g.Unreadable
		}
	}
	return ""
}

func hasEffect(effects []string, want ...string) bool {
	for _, e := range effects {
		for _, w := range want {
			if e == w {
				return true
			}
		}
	}
	return false
}
