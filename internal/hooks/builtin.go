// Package hooks builds the ADR-0139 D11 lifecycle hooks an operator declares in
// execution.hooks: three built-in parameterised hooks and the external command
// hook. The port, the ordering and the no-widening rule live in domain
// (domain.Hook, domain.HookChain) — this package only produces hooks, so a
// hook built here has exactly the authority a plugin's hook has: it can narrow,
// it can rewrite when declared may_rewrite, and it can never widen.
package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/cambrian-sh/core/domain"
)

// toolFilter restricts a tool-point hook to the named tools (fnmatch globs, the
// profile-allowlist syntax). Empty ⇒ every tool.
type toolFilter []string

func (f toolFilter) covers(tool string) bool {
	if len(f) == 0 {
		return true
	}
	for _, p := range f {
		if domain.MatchToolGlob(p, tool) {
			return true
		}
	}
	return false
}

// DenyArgsRegex denies a tool call whose raw argument JSON matches a regex
// (kind deny_args_regex). pre_tool only.
type DenyArgsRegex struct {
	spec    domain.HookSpec
	tools   toolFilter
	pattern *regexp.Regexp
	reason  string
}

// NewDenyArgsRegex builds a deny_args_regex hook.
func NewDenyArgsRegex(spec domain.HookSpec, tools []string, pattern, reason string) (*DenyArgsRegex, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("hook %q: pattern: %w", spec.Name, err)
	}
	if reason == "" {
		reason = "arguments match a denied pattern"
	}
	return &DenyArgsRegex{spec: spec, tools: tools, pattern: re, reason: reason}, nil
}

// Spec implements domain.Hook.
func (h *DenyArgsRegex) Spec() domain.HookSpec { return h.spec }

// Run implements domain.Hook.
func (h *DenyArgsRegex) Run(_ context.Context, ev domain.HookEvent) (domain.HookResult, error) {
	if ev.Point != domain.HookPreTool || !h.tools.covers(ev.Tool) {
		return domain.HookResult{}, nil
	}
	if h.pattern.Match(ev.Args) {
		return domain.HookResult{Verdict: domain.HookDeny, Reason: h.reason}, nil
	}
	return domain.HookResult{}, nil
}

// RequireApproval narrows the named tools' calls to ask, routing them through
// the ToolExecutor's approval controller (kind require_approval). pre_tool only.
//
// Under execution.tools.tools_auto_approve the approval controller approves
// everything, so this hook then changes nothing: it routes to the approver the
// operator chose, it does not replace it.
type RequireApproval struct {
	spec   domain.HookSpec
	tools  toolFilter
	reason string
}

// NewRequireApproval builds a require_approval hook.
func NewRequireApproval(spec domain.HookSpec, tools []string, reason string) *RequireApproval {
	if reason == "" {
		reason = "operator policy requires approval for this tool"
	}
	return &RequireApproval{spec: spec, tools: tools, reason: reason}
}

// Spec implements domain.Hook.
func (h *RequireApproval) Spec() domain.HookSpec { return h.spec }

// Run implements domain.Hook.
func (h *RequireApproval) Run(_ context.Context, ev domain.HookEvent) (domain.HookResult, error) {
	if ev.Point != domain.HookPreTool || !h.tools.covers(ev.Tool) {
		return domain.HookResult{}, nil
	}
	return domain.HookResult{Verdict: domain.HookAsk, Reason: h.reason}, nil
}

// RedactOutput replaces regex matches in a tool result (post_tool) or a model
// reply (post_model) — kind redact_output. A JSON tool result is redacted value by
// value (every string, recursively) and re-encoded, so a match can never break
// the JSON; object keys are left alone.
//
// At post_model it redacts the reply text, the native tool lane's deliverable
// (the configured final-answer call's answer argument, HookEvent.FinalAnswer) and
// every guarded control-verb argument (HookEvent.GuardedArgs: yield_subgoal's
// intent, delegate_to_planner's task, …), which the kernel writes back into the
// calls. Other tool calls' arguments are not its business here: their execution
// passes the ToolExecutor's pre_tool hooks. A guarded argument the kernel cannot
// read is refused by the chain before this runs.
type RedactOutput struct {
	spec        domain.HookSpec
	tools       toolFilter
	pattern     *regexp.Regexp
	replacement string
}

// NewRedactOutput builds a redact_output hook. The spec must be may_rewrite.
func NewRedactOutput(spec domain.HookSpec, tools []string, pattern, replacement string) (*RedactOutput, error) {
	if !spec.MayRewrite {
		return nil, fmt.Errorf("hook %q: redact_output rewrites output and needs may_rewrite", spec.Name)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("hook %q: pattern: %w", spec.Name, err)
	}
	if replacement == "" {
		replacement = "[REDACTED]"
	}
	return &RedactOutput{spec: spec, tools: tools, pattern: re, replacement: replacement}, nil
}

// Spec implements domain.Hook.
func (h *RedactOutput) Spec() domain.HookSpec { return h.spec }

// Run implements domain.Hook.
func (h *RedactOutput) Run(_ context.Context, ev domain.HookEvent) (domain.HookResult, error) {
	switch ev.Point {
	case domain.HookPostTool:
		if !h.tools.covers(ev.Tool) || len(ev.Result) == 0 || !h.pattern.Match(ev.Result) {
			return domain.HookResult{}, nil
		}
		var v any
		if err := json.Unmarshal(ev.Result, &v); err != nil {
			return domain.HookResult{}, fmt.Errorf("tool result is not JSON: %w", err)
		}
		out, err := json.Marshal(h.redact(v))
		if err != nil {
			return domain.HookResult{}, err
		}
		return domain.HookResult{Result: out}, nil
	case domain.HookPostModel:
		var res domain.HookResult
		if h.pattern.MatchString(ev.Text) {
			text := h.pattern.ReplaceAllLiteralString(ev.Text, h.replacement)
			res.Text = &text
		}
		if fa := ev.FinalAnswer; fa != nil && fa.Unreadable == "" && h.pattern.MatchString(fa.Value) {
			answer := h.pattern.ReplaceAllLiteralString(fa.Value, h.replacement)
			res.FinalAnswer = &answer
		}
		// Every guarded control-verb argument (yield_subgoal.intent,
		// delegate_to_planner.task, …). The deliverable is in this list too; its
		// rewrite above is the same value, so writing it twice is idempotent.
		for _, g := range ev.GuardedArgs {
			if g.Unreadable == "" && h.pattern.MatchString(g.Value) {
				res.GuardedArgs = append(res.GuardedArgs, domain.HookArgRewrite{
					Call: g.Call, Arg: g.Arg, Value: h.pattern.ReplaceAllLiteralString(g.Value, h.replacement),
				})
			}
		}
		return res, nil
	}
	return domain.HookResult{}, nil
}

func (h *RedactOutput) redact(v any) any {
	switch t := v.(type) {
	case string:
		return h.pattern.ReplaceAllLiteralString(t, h.replacement)
	case []any:
		for i := range t {
			t[i] = h.redact(t[i])
		}
		return t
	case map[string]any:
		for k := range t {
			t[k] = h.redact(t[k])
		}
		return t
	}
	return v
}
