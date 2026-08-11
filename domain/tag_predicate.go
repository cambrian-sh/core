package domain

import (
	"errors"
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

// ErrPartyTermNotCarryable is returned by ToTagSet when a predicate carries a
// party term (ADR-0121 D1b).
//
// TagSet is the AUTHORED three-set term and deliberately gains no party field: a
// party term originates on a policy, not on a caller, and widening the carrier
// would create three more places to write, transport and forget it. So the
// invariant is that no projection LOSES one — enforced by refusing rather than
// by remembering.
//
// The failure this prevents is the one ADR-0121 D1a exists for, arriving through
// a carrier instead of a rule: a truncating projection keeps the broad tag grant
// and drops the restriction, which is "restriction lost, permission kept" — a
// widening, and INV-1 broken by a struct literal.
var ErrPartyTermNotCarryable = errors.New(
	"domain: this predicate is party-scoped and a TagSet cannot carry that term; " +
		"projecting it would keep the tag grant and silently drop the restriction")

// ToTagSet projects a computed predicate back into the authored three-set form,
// refusing when the predicate carries a party term.
//
// AnyOfClauses flatten only when there is at most one clause: a TagSet holds ONE
// any-of set, so two clauses (which are ANDed) cannot be represented and would
// widen if merged into one OR.
func (p *TagPredicate) ToTagSet() (TagSet, error) {
	if p == nil {
		return TagSet{}, nil
	}
	if len(p.PartyScopedTags) > 0 {
		return TagSet{}, fmt.Errorf("%w (tags: %s)",
			ErrPartyTermNotCarryable, strings.Join(p.PartyScopedTags, ", "))
	}
	if len(p.AnyOfClauses) > 1 {
		return TagSet{}, fmt.Errorf(
			"domain: this predicate has %d any-of clauses and a TagSet holds one; "+
				"merging them would turn an AND of ORs into a single OR, which widens",
			len(p.AnyOfClauses))
	}
	out := TagSet{RequiredTags: p.RequiredTags, ForbiddenTags: p.ForbiddenTags}
	if len(p.AnyOfClauses) == 1 {
		out.AnyOfTags = p.AnyOfClauses[0]
	}
	return out, nil
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

	// PartyScopedTags qualifies tags with "…and only the rows you are a party to"
	// (ADR-0121 D1). For a row carrying one of these tags, the reader must also
	// appear in the row's parties.
	//
	// A RESTRICTION, and it takes ADR-0087's restriction rules: it accumulates by
	// union, is never removable, and survives Block Inheritance. Folding it like
	// RequiredTags would make Block Inheritance a privilege escalation, because
	// rule 3 there strips permissions and keeps denies (D1a).
	//
	// It can only ever remove rows. A row the tag terms did not admit is not
	// admitted by this, which is what keeps the algebra an intersection and
	// INV-1 ("no code path widens an effective scope") true.
	PartyScopedTags []string `json:"party_scoped_tags,omitempty"`
	// PartyIdentities is WHO THE READER IS — the entity identities this principal
	// holds, resolved by the decision point while composing (D3).
	//
	// The kernel never derives these. It receives them the way it receives every
	// other term: as data it applies without interpreting. Empty, with any
	// party-scoped tag in play, denies every row carrying that tag — fail closed
	// (D6), because "party to nothing" and "we could not tell" must not differ in
	// the direction of access.
	PartyIdentities []string `json:"party_identities,omitempty"`

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
	// Party-scoped tags, tested against NO parties — see CheckRow for the
	// substrate's row-aware form. A resource with no parties (a tool, a skill, an
	// agent, a document) cannot have the reader as one, so a party-scoped tag it
	// carries denies. That is fail-closed AND correct rather than merely safe: if
	// a policy says "only the rows you are a party to", a thing nobody can be a
	// party to is not one of them.
	for _, t := range p.PartyScopedTags {
		if has(t) {
			return AccessDecision{Reason: ReasonNotAParty, Detail: t}, false
		}
	}
	return AccessDecision{Allowed: true, Reason: ReasonAllowed}, true
}

// AllowsRow is Allows for a resource that HAS parties — a substrate row.
func (p *TagPredicate) AllowsRow(tags, parties []string) bool {
	_, ok := p.CheckRow(tags, parties)
	return ok
}

// CheckRow is Check for a substrate row, which carries parties as well as tags
// (ADR-0121).
//
// Separate from Check rather than a wider Check, deliberately. Only substrate
// rows have parties; the other three securable kinds do not, and giving them a
// parameter they must pass as nil is an invitation to pass nil from a call site
// that DID have parties available. Two names, and the wrong one fails closed.
func (p *TagPredicate) CheckRow(tags, parties []string) (AccessDecision, bool) {
	// The tag terms first and unchanged, so a row denied by a label is denied for
	// that reason rather than for a relationship — the explanation an operator
	// gets should name the first thing that is actually wrong.
	dec, ok := p.Check(tags)
	if p == nil || p.Bypass {
		return dec, ok
	}
	if !ok && dec.Reason != ReasonNotAParty {
		return dec, ok
	}
	for _, t := range p.PartyScopedTags {
		carries := false
		for _, x := range tags {
			if x == t {
				carries = true
				break
			}
		}
		if !carries {
			continue // the qualifier says nothing about a row without the tag
		}
		if !overlaps(parties, p.PartyIdentities) {
			return AccessDecision{Reason: ReasonNotAParty, Detail: t}, false
		}
	}
	return AccessDecision{Allowed: true, Reason: ReasonAllowed}, true
}

// overlaps reports whether the two sets share a member. Both are small — a row's
// parties and one reader's identities — so the nested scan beats building a map.
func overlaps(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// IsZero reports whether the predicate constrains nothing and is not a bypass. An
// empty, non-bypass predicate still has to be PRESENT to pass the chokepoint's
// fail-closed nil check.
func (p *TagPredicate) IsZero() bool {
	if p == nil {
		return true
	}
	// PartyScopedTags counts. A predicate whose only term is party-scoping
	// constrains a great deal, and omitting it here is not cosmetic: the empty
	// answer is consumed at `querymemory.go`'s
	// `case !pred.Bypass && !pred.IsZero()`, so a party-filtered zero-row result
	// would be classed as "policy did not shape this outcome" and INV-3's
	// policy_note would never be emitted — the silent empty ADR-0121 D6 exists to
	// prevent, reintroduced by a helper.
	//
	// PartyIdentities deliberately does NOT count: identities alone constrain
	// nothing, and a predicate carrying only "who you are" is genuinely empty.
	return !p.Bypass && len(p.RequiredTags) == 0 && len(p.AnyOfClauses) == 0 &&
		len(p.ForbiddenTags) == 0 && len(p.PartyScopedTags) == 0
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
