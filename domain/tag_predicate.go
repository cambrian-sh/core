package domain

import (
	"fmt"
	"sort"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// The APPLIED form of an authorization decision (ADR-0085).
//
// TagSet and TagPredicate are pure data. The kernel APPLIES a predicate (that is
// enforcement, and it stays here); the kernel never COMPOSES two of them — no
// intersection, no precedence, no inheritance, no validation of what any tag
// means. Composition is the decision point's job and lives in the policy plugin.
//
// This is the Windows split made concrete: AccessCheck is in the kernel, the DACL
// is data that arrives from outside.
// ─────────────────────────────────────────────────────────────────────────────

// TagSet is an opaque, uninterpreted three-set classification term as AUTHORED —
// carried on a session, an agent record, or a policy rule. The kernel stores and
// transports it; only the decision point gives it meaning.
//
//	RequiredTags  — AND : must carry EVERY one   (boundary)
//	AnyOfTags     — OR  : must carry AT LEAST ONE (visibility/source)
//	ForbiddenTags — NONE: excluded if it carries ANY (deny)
type TagSet struct {
	RequiredTags  []string `json:"required_tags,omitempty"`
	AnyOfTags     []string `json:"any_of_tags,omitempty"`
	ForbiddenTags []string `json:"forbidden_tags,omitempty"`
}

// IsZero reports whether the term constrains nothing.
func (s TagSet) IsZero() bool {
	return len(s.RequiredTags) == 0 && len(s.AnyOfTags) == 0 && len(s.ForbiddenTags) == 0
}

// TagPredicate is the COMPUTED, ready-to-apply form of an access decision: what
// the decision point resolved for one principal on one surface, expressed so that
// both an in-memory store and a SQL store can apply exactly the same rule.
//
// AnyOfClauses are in conjunctive normal form: each inner slice is one OR-set and
// ALL clauses must be satisfied. The kernel never builds these — it receives them
// from the Authorizer and applies them.
//
// Precedence when applied: ForbiddenTags > AnyOfClauses > RequiredTags. A single
// forbidden tag disqualifies a row regardless of any other match.
type TagPredicate struct {
	RequiredTags  []string   `json:"required_tags,omitempty"`
	AnyOfClauses  [][]string `json:"any_of_clauses,omitempty"`
	ForbiddenTags []string   `json:"forbidden_tags,omitempty"`

	// Bypass, when true, admits everything. It marks either the explicit
	// ScopeSystem sentinel (kernel-internal maintenance reads) or an unscoped
	// deployment. It must never be produced by combining two predicates — the
	// kernel does not combine predicates at all.
	Bypass bool `json:"bypass,omitempty"`
}

// ScopeSystem is the explicit, greppable sentinel that bypasses tag filtering for
// kernel-internal / maintenance reads that run on behalf of no principal
// (temporal decay & GC, spreading-activation expansion, episodic indexing). A
// security review can enumerate every use by grepping for this one identifier —
// do NOT generalise it into a role (INV-7).
//
// Treat as read-only; do not mutate its slices.
var ScopeSystem = &TagPredicate{Bypass: true}

// Forbids reports whether tag is in the predicate's ForbiddenTags. A nil receiver
// forbids nothing; callers distinguish "no predicate" at the chokepoint, not here.
func (p *TagPredicate) Forbids(tag string) bool {
	if p == nil {
		return false
	}
	for _, f := range p.ForbiddenTags {
		if f == tag {
			return true
		}
	}
	return false
}

// Allows reports whether a row carrying the given tags satisfies this predicate.
// It is the AUTHORITATIVE row-level test: the pgvector SQL filter is a
// performance-optimized mirror of this logic, and in-memory / fake stores apply it
// directly.
//
// A Bypass predicate allows everything. A NIL receiver allows NOTHING — that is
// the fail-closed backstop for a dropped predicate, not a policy choice: the
// chokepoint refuses an unfiltered read before it ever gets here.
func (p *TagPredicate) Allows(tags []string) bool {
	_, ok := p.Check(tags)
	return ok
}

// Check is Allows plus the reason it failed, so a denial can be explained without
// re-deriving it. The returned AccessDecision carries only Reason and Detail — the
// caller stamps principal, surface, and resource.
func (p *TagPredicate) Check(tags []string) (AccessDecision, bool) {
	if p == nil {
		return AccessDecision{Reason: ReasonNoPrincipal, Detail: "no effective predicate (fail-closed)"}, false
	}
	if p.Bypass {
		return AccessDecision{Allowed: true, Reason: ReasonBypass}, true
	}
	has := func(t string) bool {
		for _, x := range tags {
			if x == t {
				return true
			}
		}
		return false
	}
	for _, f := range p.ForbiddenTags { // deny wins, absolutely
		if has(f) {
			return AccessDecision{Reason: ReasonForbiddenTag, Detail: f}, false
		}
	}
	for _, r := range p.RequiredTags { // every required tag must be present
		if !has(r) {
			return AccessDecision{Reason: ReasonMissingRequiredTag, Detail: r}, false
		}
	}
	for _, clause := range p.AnyOfClauses { // each clause is an OR; all clauses ANDed
		satisfied := false
		for _, a := range clause {
			if has(a) {
				satisfied = true
				break
			}
		}
		if !satisfied {
			return AccessDecision{Reason: ReasonAnyOfUnsatisfied, Detail: strings.Join(clause, "|")}, false
		}
	}
	return AccessDecision{Allowed: true, Reason: ReasonAllowed}, true
}

// IsZero reports whether the predicate constrains nothing and is not a bypass. An
// empty, non-bypass predicate still has to be PRESENT to pass the chokepoint's
// fail-closed nil check.
func (p *TagPredicate) IsZero() bool {
	if p == nil {
		return true
	}
	return !p.Bypass && len(p.RequiredTags) == 0 && len(p.AnyOfClauses) == 0 && len(p.ForbiddenTags) == 0
}

// Unsatisfiable reports whether this predicate can never match any row, with a
// human-readable reason. This is a static contradiction in the DATA, not a policy
// judgment — the kernel detects it so a zero-row result is never silent (INV-3).
// The decision point is expected to detect the same condition at authoring time.
func (p *TagPredicate) Unsatisfiable() (bool, string) {
	if p == nil || p.Bypass {
		return false, ""
	}
	forbidden := make(map[string]struct{}, len(p.ForbiddenTags))
	for _, f := range p.ForbiddenTags {
		forbidden[f] = struct{}{}
	}
	var conflicts []string
	for _, r := range p.RequiredTags {
		if _, bad := forbidden[r]; bad {
			conflicts = append(conflicts, r)
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return true, fmt.Sprintf("Required∩Forbidden={%s}", strings.Join(conflicts, ","))
	}
	for _, clause := range p.AnyOfClauses {
		if len(clause) == 0 {
			continue
		}
		allDenied := true
		for _, a := range clause {
			if _, bad := forbidden[a]; !bad {
				allDenied = false
				break
			}
		}
		if allDenied {
			return true, fmt.Sprintf("AnyOf clause {%s} fully forbidden", strings.Join(clause, ","))
		}
	}
	return false, ""
}
