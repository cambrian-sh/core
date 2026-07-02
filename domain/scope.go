package domain

import (
	"fmt"
	"sort"
	"strings"
)

// ScopeConfig describes an access boundary using a three-set opaque-tag model.
// It is purely access-control; non-access policy knobs live in PolicyConfig.
// ADR-0034 (D3).
//
//	RequiredTags  — AND : a document must carry EVERY one  (boundary)
//	AnyOfTags     — OR  : a document must carry AT LEAST ONE (visibility/source)
//	ForbiddenTags — NONE: exclude a document if it carries ANY (deny)
//
// Tag strings are opaque to Cambrian; their meaning is the integrating
// application's convention.
type ScopeConfig struct {
	RequiredTags  []string `json:"required_tags,omitempty"`
	AnyOfTags     []string `json:"any_of_tags,omitempty"`
	ForbiddenTags []string `json:"forbidden_tags,omitempty"`
}

// IsZero reports whether the scope imposes no constraints at all (unrestricted).
// A registered-but-unprofiled agent carries a zero ScopeConfig (ADR-0034 D8).
func (s ScopeConfig) IsZero() bool {
	return len(s.RequiredTags) == 0 && len(s.AnyOfTags) == 0 && len(s.ForbiddenTags) == 0
}

// Forbids reports whether tag appears in ForbiddenTags.
func (s ScopeConfig) Forbids(tag string) bool {
	for _, f := range s.ForbiddenTags {
		if f == tag {
			return true
		}
	}
	return false
}

// Validate rejects the set-logic conflicts that make a scope statically
// unsatisfiable — a "zombie" boundary that silently matches nothing. It does
// NOT check controlled-vocabulary membership; that is the caller's job at
// registration/write time (ADR-0034 D8/R5). Returns nil for a valid scope.
func (s ScopeConfig) Validate() error {
	// RequiredTags ∩ ForbiddenTags ≠ ∅ — a required tag is also forbidden.
	forbidden := make(map[string]struct{}, len(s.ForbiddenTags))
	for _, f := range s.ForbiddenTags {
		forbidden[f] = struct{}{}
	}
	var conflicts []string
	for _, r := range s.RequiredTags {
		if _, bad := forbidden[r]; bad {
			conflicts = append(conflicts, r)
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return fmt.Errorf("unsatisfiable scope: RequiredTags ∩ ForbiddenTags = {%s}", strings.Join(conflicts, ","))
	}

	// Every AnyOfTags element also forbidden — the whitelist is fully denied,
	// so no document can ever satisfy the OR-gate.
	if len(s.AnyOfTags) > 0 {
		allDenied := true
		for _, a := range s.AnyOfTags {
			if _, bad := forbidden[a]; !bad {
				allDenied = false
				break
			}
		}
		if allDenied {
			return fmt.Errorf("unsatisfiable scope: every AnyOfTags element {%s} is also forbidden", strings.Join(s.AnyOfTags, ","))
		}
	}
	return nil
}

// ScopeConsolidator is the kernel-defined scope profile for the ConsolidatorAgent's
// promotion pipeline (ADR-0034 D11). It is the ONLY authorized bridge across silos
// and does NOT use ScopeSystem (which bypasses all filtering). It can read raw
// Tier-0 customer data but is structurally blocked from secrets/internal_only/PII.
// This is a deterministic-safety exception (kernel-defined, not operator-registered),
// the same class as Reflexive Path / Omurilik routing.
var ScopeConsolidator = ScopeConfig{
	RequiredTags:  nil,
	AnyOfTags:     []string{"chat_raw", "invoice_raw", "feedback_raw"},
	ForbiddenTags: []string{"secrets", "internal_only", "PII"},
}

// ScopeConsolidatorWriteTags is the kernel-defined write classification (ADR-0035
// C2) stamped on the ConsolidatorAgent's promoted insights. It is the write-side
// counterpart to ScopeConsolidator (the read profile): the Consolidator reads raw
// Tier-0 data but writes broad, derived, k-anonymized knowledge. Deterministic-
// safety exception (kernel-defined, not operator-registered).
var ScopeConsolidatorWriteTags = []string{"company_wide", "analytics", "derived"}

// PolicyConfig carries the non-access-control policy knobs that previously lived
// on the legacy 5-field ScopeConfig (ADR-0033). Split out per ADR-0034 (D3) so
// ScopeConfig stays purely access-control. Carried in the caller payload, never
// in the access boundary.
type PolicyConfig struct {
	ToolAllowList []string
	ToolDenyList  []string
	MaxPlanSteps  int
	LLMHints      string
}

// EffectiveScope is the least-privilege intersection of a caller_scope and an
// agent_scope. AnyOf sets combine in conjunctive normal form: each side's
// AnyOfTags becomes one OR-clause and ALL clauses must be satisfied. RequiredTags
// and ForbiddenTags combine by union. ADR-0034 (D12).
//
// Precedence on a document: ForbiddenTags > AnyOfClauses > RequiredTags. A single
// forbidden tag disqualifies a document regardless of any other match.
type EffectiveScope struct {
	RequiredTags  []string   // union of caller + agent RequiredTags
	AnyOfClauses  [][]string // CNF: each inner slice is one side's AnyOfTags (OR-set); all AND-composed
	ForbiddenTags []string   // union of caller + agent ForbiddenTags

	// System, when true, marks the explicit ScopeSystem bypass for kernel-internal
	// reads that run on behalf of no agent. It must never be set by combining
	// real scopes — only via the ScopeSystem sentinel. ADR-0034 (D6).
	System bool
}

// ScopeSystem is the explicit, greppable sentinel that bypasses tag filtering for
// kernel-internal/maintenance reads (temporal decay & GC, spreading-activation
// expansion, episodic indexing). A security review can enumerate every use by
// grepping for this identifier. It is never produced by intersecting agent/caller
// scopes — only referenced deliberately. ADR-0034 (D6).
//
// Treat as read-only; do not mutate its slices.
var ScopeSystem = &EffectiveScope{System: true}

// NewEffectiveScope computes the least-privilege intersection of caller_scope and
// agent_scope (ADR-0034 D4/D12). It is named NewEffectiveScope rather than
// EffectiveScope because Go forbids a function and a type sharing one name; the
// ADR's illustrative `domain.EffectiveScope(...)` maps to this constructor.
func NewEffectiveScope(caller, agent ScopeConfig) EffectiveScope {
	eff := EffectiveScope{
		RequiredTags:  dedupUnion(caller.RequiredTags, agent.RequiredTags),
		ForbiddenTags: dedupUnion(caller.ForbiddenTags, agent.ForbiddenTags),
	}
	// CNF: each non-empty side contributes one OR-clause. A flat union would
	// wrongly admit a document the other side never authorized (D12).
	if c := dedup(caller.AnyOfTags); len(c) > 0 {
		eff.AnyOfClauses = append(eff.AnyOfClauses, c)
	}
	if a := dedup(agent.AnyOfTags); len(a) > 0 {
		eff.AnyOfClauses = append(eff.AnyOfClauses, a)
	}
	return eff
}

// Forbids reports whether tag is in the effective ForbiddenTags. A nil receiver
// (no scope) forbids nothing — callers distinguish "no scope" via fail-closed
// handling at the chokepoint, not here.
func (e *EffectiveScope) Forbids(tag string) bool {
	if e == nil {
		return false
	}
	for _, f := range e.ForbiddenTags {
		if f == tag {
			return true
		}
	}
	return false
}

// Allows reports whether a document carrying the given tags satisfies this
// effective scope. It is the AUTHORITATIVE row-level predicate: the pgvector SQL
// filter is a performance-optimized mirror of this logic, and in-memory/fake
// stores apply it directly. Precedence: ForbiddenTags > AnyOfClauses >
// RequiredTags. A System scope allows everything. A nil receiver allows nothing
// (fail-closed — callers must not reach here with a nil scope). ADR-0034 (D3/D12).
func (e *EffectiveScope) Allows(tags []string) bool {
	if e == nil {
		return false
	}
	if e.System {
		return true
	}
	has := func(t string) bool {
		for _, x := range tags {
			if x == t {
				return true
			}
		}
		return false
	}
	for _, f := range e.ForbiddenTags { // deny wins
		if has(f) {
			return false
		}
	}
	for _, r := range e.RequiredTags { // every required tag must be present
		if !has(r) {
			return false
		}
	}
	for _, clause := range e.AnyOfClauses { // each clause is an OR; all clauses ANDed
		ok := false
		for _, a := range clause {
			if has(a) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// IsZero reports whether the effective scope imposes no constraints (and is not
// the System sentinel). An empty, non-system effective scope still requires an
// explicit, non-nil presence at the chokepoint to pass the fail-closed gate.
func (e *EffectiveScope) IsZero() bool {
	if e == nil {
		return true
	}
	return !e.System && len(e.RequiredTags) == 0 && len(e.AnyOfClauses) == 0 && len(e.ForbiddenTags) == 0
}

// Unsatisfiable reports whether the effective scope can never match any document,
// returning a human-readable reason for the audit warning log. This is a SAFE
// state (zero rows / reject-all), but the operator gets no signal otherwise, so
// the chokepoint logs it. ADR-0034 (D12/R5).
func (e *EffectiveScope) Unsatisfiable() (bool, string) {
	if e == nil || e.System {
		return false, ""
	}
	forbidden := make(map[string]struct{}, len(e.ForbiddenTags))
	for _, f := range e.ForbiddenTags {
		forbidden[f] = struct{}{}
	}
	// A required tag is also forbidden.
	var conflicts []string
	for _, r := range e.RequiredTags {
		if _, bad := forbidden[r]; bad {
			conflicts = append(conflicts, r)
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return true, fmt.Sprintf("Required∩Forbidden={%s}", strings.Join(conflicts, ","))
	}
	// Some AnyOf clause is fully forbidden — it can never be satisfied.
	for _, clause := range e.AnyOfClauses {
		allDenied := true
		for _, a := range clause {
			if _, bad := forbidden[a]; !bad {
				allDenied = false
				break
			}
		}
		if allDenied && len(clause) > 0 {
			return true, fmt.Sprintf("AnyOf clause {%s} fully forbidden", strings.Join(clause, ","))
		}
	}
	return false, ""
}

// dedupUnion returns the sorted, de-duplicated union of two tag slices.
func dedupUnion(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	return dedup(append(append([]string{}, a...), b...))
}

// dedup returns a sorted, de-duplicated copy of tags (nil-in → nil-out).
func dedup(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
