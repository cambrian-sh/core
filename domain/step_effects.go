package domain

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// Step effects: effect-checked dispatch (T-20261106-kernel-does-not-check-that-a-declared,
// owner option B, ADR-0139 observation 2026-11-06).
//
// The planner declares, per ACTION step, the effects the step needs —
// `required_effects: ["move"]` for "move each receipt to Documents/Receipts" — and
// dispatch admits only agents whose tools can produce every one of them. A
// capability TAG says what an agent claims to be good at; an EFFECT is derived from
// what its granted tools' operator policies say they do, so a profile that declares
// `file_organisation` but holds no move tool is no longer routed a move.
//
// The vocabulary is a small, fixed, config-driven set (execution.capability.
// effect_vocabulary). Every term maps onto the ADR-0086 tool effect classes
// (read, write, egress, spend, admin) — those are what tool policies declare — in
// one of two ways:
//
//   - a CLASS term holds for a tool that declares any of its classes
//     (`write` ← write);
//   - a REFINED term names one class it refines and tool-name globs, and holds for
//     a tool that declares that class AND whose name matches a glob (`move` ←
//     write + `*move*`/`*rename*`). The ADR-0086 classes cannot tell a move from a
//     write — move_file and write_file both declare `write` — and that is exactly
//     the distinction the p05 receipts journey lost.
//
// Fail-closed rules: a tool with no operator policy (inferred effects, or the
// fail-closed default an unlisted MCP tool is stamped with) contributes nothing;
// an effect outside the vocabulary matches no agent; a step whose effects no agent
// has is refused, never handed to an arbitrary agent.
// ─────────────────────────────────────────────────────────────────────────────

// StepEffectTerm is one vocabulary entry, as configured.
type StepEffectTerm struct {
	// Name is the term the planner emits (lowercase).
	Name string
	// Description is shown to the planner next to the name.
	Description string
	// Classes: a tool that declares any of these classes has this effect.
	Classes []ToolEffect
	// Refines + Tools: a tool that declares Refines AND whose base name (the part
	// after the last '/') matches one of Tools has this effect.
	Refines ToolEffect
	Tools   []string
}

// StepEffectVocabulary is the validated, immutable vocabulary.
type StepEffectVocabulary struct {
	terms []StepEffectTerm
	index map[string]int
}

// NewStepEffectVocabulary validates terms and builds the vocabulary. A term needs a
// unique lowercase name and either Classes, or Refines plus at least one Tools glob;
// every class must be an ADR-0086 class. An empty term list is valid: every
// required effect is then unmatched, which refuses every effect-declaring step
// (fail closed).
func NewStepEffectVocabulary(terms []StepEffectTerm) (*StepEffectVocabulary, error) {
	v := &StepEffectVocabulary{index: map[string]int{}}
	for i, t := range terms {
		name := strings.TrimSpace(t.Name)
		if name == "" || name != strings.ToLower(name) || strings.ContainsAny(name, " \t,") {
			return nil, fmt.Errorf("effect vocabulary entry %d: name %q must be a non-empty lowercase word", i, t.Name)
		}
		if _, dup := v.index[name]; dup {
			return nil, fmt.Errorf("effect vocabulary: duplicate term %q", name)
		}
		for _, c := range t.Classes {
			if !ValidToolEffect(c) {
				return nil, fmt.Errorf("effect vocabulary term %q: unknown effect class %q (the classes are [%s])", name, c, effectNames())
			}
		}
		refined := t.Refines != "" || len(t.Tools) > 0
		switch {
		case refined && len(t.Classes) > 0:
			return nil, fmt.Errorf("effect vocabulary term %q: set either classes or refines+tools, not both", name)
		case refined && !ValidToolEffect(t.Refines):
			return nil, fmt.Errorf("effect vocabulary term %q: refines %q is not an effect class (the classes are [%s])", name, t.Refines, effectNames())
		case refined && len(t.Tools) == 0:
			return nil, fmt.Errorf("effect vocabulary term %q: refines %q needs at least one tool glob", name, t.Refines)
		case !refined && len(t.Classes) == 0:
			return nil, fmt.Errorf("effect vocabulary term %q: needs classes, or refines plus tools", name)
		}
		t.Name = name
		t.Classes = append([]ToolEffect(nil), t.Classes...)
		t.Tools = append([]string(nil), t.Tools...)
		v.index[name] = len(v.terms)
		v.terms = append(v.terms, t)
	}
	return v, nil
}

// Terms returns the vocabulary in configured order (a copy).
func (v *StepEffectVocabulary) Terms() []StepEffectTerm {
	if v == nil {
		return nil
	}
	return append([]StepEffectTerm(nil), v.terms...)
}

// Names returns the term names in configured order.
func (v *StepEffectVocabulary) Names() []string {
	if v == nil {
		return nil
	}
	out := make([]string, len(v.terms))
	for i, t := range v.terms {
		out[i] = t.Name
	}
	return out
}

// Known reports whether name is a vocabulary term. Matching is exact after
// lowercasing and trimming — no aliasing, no fuzzy match.
func (v *StepEffectVocabulary) Known(name string) bool {
	if v == nil {
		return false
	}
	_, ok := v.index[NormalizeStepEffect(name)]
	return ok
}

// NormalizeStepEffect is the only normalisation an emitted effect gets.
func NormalizeStepEffect(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// toolHasPolicy reports whether a tool's effects come from an operator policy:
// declared on the tool (or its server default), not inferred and not the
// fail-closed default of an unlisted MCP tool. Only such tools are evidence of
// what an agent can do.
func toolHasPolicy(t SystemTool) bool {
	return len(t.Effects) > 0 && !t.EffectsInferred && !t.EffectsDefaulted
}

// toolBaseName is the part of a tool name after its last '/': `mcp:filesystem/move_file`
// → `move_file`. Refinement globs match it, so a server id never matches by accident.
func toolBaseName(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// EffectsOfTool returns the vocabulary terms one tool contributes, in vocabulary order.
// A tool without an operator policy contributes nothing.
func (v *StepEffectVocabulary) EffectsOfTool(t SystemTool) []string {
	if v == nil || !toolHasPolicy(t) {
		return nil
	}
	var out []string
	base := toolBaseName(t.Name)
	for _, term := range v.terms {
		if term.Refines != "" {
			if !t.HasEffect(term.Refines) {
				continue
			}
			for _, g := range term.Tools {
				if MatchToolGlob(g, base) {
					out = append(out, term.Name)
					break
				}
			}
			continue
		}
		for _, c := range term.Classes {
			if t.HasEffect(c) {
				out = append(out, term.Name)
				break
			}
		}
	}
	return out
}

// DeriveEffects returns the effect set the given tools produce, sorted.
func (v *StepEffectVocabulary) DeriveEffects(tools []SystemTool) []string {
	set := map[string]bool{}
	for _, t := range tools {
		for _, e := range v.EffectsOfTool(t) {
			set[e] = true
		}
	}
	out := make([]string, 0, len(set))
	for e := range set {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// MissingEffects returns the required effects an agent with the given effect set
// lacks, in required order. An effect outside the vocabulary is ALWAYS missing:
// unknown is unmatched, never "any agent".
func (v *StepEffectVocabulary) MissingEffects(required, have []string) []string {
	h := make(map[string]bool, len(have))
	for _, e := range have {
		h[e] = true
	}
	var out []string
	for _, r := range required {
		n := NormalizeStepEffect(r)
		if !v.Known(n) || !h[n] {
			out = append(out, n)
		}
	}
	return out
}

// UnknownEffects returns the required effects that are not vocabulary terms.
func (v *StepEffectVocabulary) UnknownEffects(required []string) []string {
	var out []string
	for _, r := range required {
		if !v.Known(r) {
			out = append(out, NormalizeStepEffect(r))
		}
	}
	return out
}

// AgentEffectSource answers what an agent can do, derived from its granted tools.
// Implemented by internal/supervision/gatekeeper.ToolEffectSource.
type AgentEffectSource interface {
	// Vocabulary is the configured step-effect vocabulary.
	Vocabulary() *StepEffectVocabulary
	// AgentEffects is the agent's derived effect set, sorted.
	AgentEffects(ctx context.Context, agentID string) []string
}

// Effect-check outcomes, recorded on DispatchTask.EffectCheck and logged per step.
const (
	EffectCheckEligible   = "eligible"   // at least one candidate holds every required effect
	EffectCheckRefused    = "refused"    // none does: the step is refused (NoEffectMatchError)
	EffectCheckUnverified = "unverified" // the step declares no required_effects at all (legacy plan)
	EffectCheckNone       = "none"       // the step declares an empty list: it needs no effect
)

// EffectCheckResult is the per-step record of the effect check: what the step
// required, what each considered agent can do, who was eligible and the outcome.
type EffectCheckResult struct {
	Required []string
	// AgentEffects is each considered agent's derived effect set.
	AgentEffects map[string][]string
	Eligible     []string
	Outcome      string
	// Unknown are required effects outside the vocabulary.
	Unknown []string
	// PinRefused names a preferred/pinned agent the check refused.
	PinRefused string
}

// Summary renders the result in one line, for logs, events and error messages.
func (r *EffectCheckResult) Summary() string {
	if r == nil {
		return ""
	}
	ids := make([]string, 0, len(r.AgentEffects))
	for id := range r.AgentEffects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("%s=[%s]", id, strings.Join(r.AgentEffects[id], ",")))
	}
	s := fmt.Sprintf("effects %s: required [%s]; eligible [%s]; agents %s",
		r.Outcome, strings.Join(r.Required, ","), strings.Join(r.Eligible, ","), strings.Join(parts, " "))
	if len(r.Unknown) > 0 {
		s += fmt.Sprintf("; unknown effects [%s] (not in the vocabulary)", strings.Join(r.Unknown, ","))
	}
	if r.PinRefused != "" {
		s += fmt.Sprintf("; pinned agent %s refused", r.PinRefused)
	}
	return s
}

// NoEffectMatchError refuses a step whose required effects no candidate agent has.
// It is a deterministic routing refusal: retrying the same step cannot change it
// (SelfHealer stops at once), a replan can (the DAG executor replans ONCE), and a
// second refusal in the same run becomes a question to the user — never a
// dispatch to an arbitrary agent.
type NoEffectMatchError struct {
	TaskID string
	// Missing are the required effects no eligible agent covers (for an unknown
	// effect, the effect itself).
	Missing []string
	Check   *EffectCheckResult
}

func (e *NoEffectMatchError) Error() string {
	return fmt.Sprintf("dispatch refused for step %s: no agent can %s (%s)",
		e.TaskID, strings.Join(e.Missing, " + "), e.Check.Summary())
}

// IsDispatchRefusal reports whether err is a deterministic dispatch refusal that a
// retry of the same step cannot fix.
func IsDispatchRefusal(err error) bool {
	var nem *NoEffectMatchError
	return errors.As(err, &nem)
}
