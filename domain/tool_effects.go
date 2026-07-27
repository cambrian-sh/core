package domain

import (
	"fmt"
	"sort"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// Tool effect classes (ADR-0086).
//
// A classification tag answers "what is this ABOUT". It cannot answer "what am I
// DOING to it" — reading a CRM contact and deleting one carry the same tag and
// vastly different risk. Effects are the verb half, and BOTH must pass: a tool
// invocation is permitted only if the tag predicate admits it AND every effect it
// declares is granted.
//
// The set is CLOSED (see ToolEffect in authz.go). Wildcard action strings are how
// people accidentally grant far more than they intended, so there is no globbing
// and no open namespace here.
// ─────────────────────────────────────────────────────────────────────────────

// ErrToolUnclassified is the registration error for a tool that declares no
// effects while the deployment requires them. A tool with no effects is a
// registration error, not an unrestricted tool — fail closed (ADR-0086).
type ErrToolUnclassified struct{ Tool string }

func (e *ErrToolUnclassified) Error() string {
	return fmt.Sprintf("tool %q declares no effect classes; add \"effects\":[%s] to its manifest",
		e.Tool, effectNames())
}

// ErrToolUnknownEffect is the registration error for an effect outside the closed
// set. This is ALWAYS fatal: an unrecognised effect would otherwise be silently
// dropped, and a policy denying it would then permit the very call it named.
type ErrToolUnknownEffect struct {
	Tool   string
	Effect string
}

func (e *ErrToolUnknownEffect) Error() string {
	return fmt.Sprintf("tool %q declares unknown effect %q; the closed set is [%s]",
		e.Tool, e.Effect, effectNames())
}

func effectNames() string {
	names := make([]string, len(AllToolEffects))
	for i, e := range AllToolEffects {
		names[i] = string(e)
	}
	return strings.Join(names, ",")
}

// InferEffects derives a tool's effect classes from what its manifest ALREADY
// declares, so an un-migrated tool gets a defensible classification instead of an
// empty one. It is deterministic and reads only facts the manifest states:
//
//	data_write_kinds present   → write   (it mutates a tagged store)
//	url_args present           → egress  (it can transmit outside the deployment)
//	command_args present       → write   (a shell command is a mutation surface)
//	dangerous                  → write   (the operator already said so)
//	otherwise                  → read
//
// Inference is a MIGRATION aid, not the model. A declared set always wins, and
// InferredEffects marks the tools an operator still has to classify by hand.
// Under-classifying is the risk here — hence the deliberately pessimistic
// mapping, and hence the strict mode that refuses inference altogether.
func InferEffects(t SystemTool) []ToolEffect {
	set := map[ToolEffect]bool{}
	if len(t.DataWriteKinds) > 0 || len(t.CommandArgs) > 0 || t.Dangerous {
		set[EffectWrite] = true
	}
	if len(t.URLArgs) > 0 {
		set[EffectEgress] = true
	}
	// Every tool observes something; a pure-mutation tool that returns nothing is
	// not a shape this system has.
	set[EffectRead] = true

	out := make([]ToolEffect, 0, len(set))
	for _, e := range AllToolEffects { // stable, risk-ordered
		if set[e] {
			out = append(out, e)
		}
	}
	return out
}

// ValidateRegistration checks a tool's declared effects and returns the tool with
// effects resolved — declared if present, inferred otherwise.
//
// strict=true refuses to infer: an undeclared tool fails registration
// (ErrToolUnclassified), which is the fail-closed posture a deployment adopts
// once its manifests carry effects. strict=false accepts inference and marks the
// result, so an operator can list exactly which tools are still unclassified.
//
// An effect outside the closed set is fatal in BOTH modes.
func ValidateRegistration(t SystemTool, strict bool) (SystemTool, error) {
	seen := map[ToolEffect]bool{}
	declared := make([]ToolEffect, 0, len(t.Effects))
	for _, e := range t.Effects {
		if !ValidToolEffect(e) {
			return t, &ErrToolUnknownEffect{Tool: t.Name, Effect: string(e)}
		}
		if !seen[e] {
			seen[e] = true
			declared = append(declared, e)
		}
	}
	if len(declared) > 0 {
		sortEffects(declared)
		t.Effects = declared
		// EffectsInferred is PRESERVED, not cleared. Validation is idempotent by
		// design — discovery validates, then the registry validates again — and a
		// second pass over an already-inferred tool must not erase the marker that
		// says these effects were derived rather than declared. Losing it would
		// quietly empty the migration checklist.
		return t, nil
	}
	if strict {
		return t, &ErrToolUnclassified{Tool: t.Name}
	}
	t.Effects = InferEffects(t)
	t.EffectsInferred = true
	return t, nil
}

// sortEffects orders effects by the closed set's risk order, so two tools with
// the same effects always render identically.
func sortEffects(es []ToolEffect) {
	rank := make(map[ToolEffect]int, len(AllToolEffects))
	for i, e := range AllToolEffects {
		rank[e] = i
	}
	sort.Slice(es, func(i, j int) bool { return rank[es[i]] < rank[es[j]] })
}

// HasEffect reports whether the tool declares (or was inferred to have) e.
func (t SystemTool) HasEffect(e ToolEffect) bool {
	for _, x := range t.Effects {
		if x == e {
			return true
		}
	}
	return false
}

// AuthzRef identifies a tool to the decision point (Taggable).
func (t SystemTool) AuthzRef() ResourceRef { return ResourceRef{Kind: KindTool, ID: t.Name} }

// AuthzTags presents what domain the tool touches — `crm`, `filesystem`,
// `payments`, `email`. Falls back to the data-store kinds it already declares, so
// a tool that never grew ClassificationTags is still governed by something rather
// than by nothing.
func (t SystemTool) AuthzTags() []string {
	if len(t.ClassificationTags) > 0 {
		return t.ClassificationTags
	}
	if len(t.DataReadKinds) == 0 && len(t.DataWriteKinds) == 0 {
		return nil
	}
	out := make([]string, 0, len(t.DataReadKinds)+len(t.DataWriteKinds))
	out = append(out, t.DataReadKinds...)
	out = append(out, t.DataWriteKinds...)
	return out
}
