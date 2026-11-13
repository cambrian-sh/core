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

	// Owners are the principal ids this reader ACTS AS for the owner fence
	// (owner_scope.go, ADR-0141 position 3): a row carrying owner:X is admitted
	// only when X is listed. Resolved by the decision point from the reader
	// (never from a policy term), and applied by the kernel as data. Empty denies
	// every owner-scoped row — fail closed. Unlike every other term it applies
	// under Bypass too.
	Owners []string `json:"owners,omitempty"`
	// AllOwners lifts the owner fence. Honoured ONLY together with Bypass — i.e.
	// only on ScopeMaintenance, which no decision point ever hands a principal.
	AllOwners bool `json:"all_owners,omitempty"`

	// Bypass, when true, admits everything EXCEPT other principals' owner-scoped
	// rows (the owner fence survives Bypass; only ScopeMaintenance lifts it). It
	// marks either the explicit ScopeSystem sentinel (kernel-internal reads, the
	// operator plane) or an unscoped deployment. It must never be produced by
	// combining two predicates — the kernel does not combine predicates at all.
	Bypass bool `json:"bypass,omitempty"`

	// LedgerRuns is the run-scope grant (ADR-0145 D11;
	// T-20261112-staged-planner-s-ledger-reads-return-found): the ledger entries
	// of the runs named here are readable under the run's own kernel scope (the
	// owner fence for the run's owner only — the scope the DAG executor reads the
	// same run under, runLedgerScope), in ADDITION to what the rest of this
	// predicate admits. It is a kernel-held fact, set only by the kernel for the
	// staged planner of the Execute that owns those runs — never by a decision
	// point, a header or a payload — and applied ONLY by authz.EnforcingLedger.
	// Every other read path ignores it, and since it can only add ledger rows,
	// ignoring it fails closed. Not projectable to a TagSet (ToTagSet drops it,
	// which narrows).
	LedgerRuns *LedgerRunGrant `json:"ledger_runs,omitempty"`

	// OwnRows is the predicate for rows whose owner tags are exactly
	// {owner:Owners[0]} — the reader's OWN rows (ADR-0146, the own-rows rule).
	// Set only by the decision point: for an agent reading for a conversation
	// owner O it is the agent's whole predicate intersected with O's RESTRICTIVE
	// terms only (O's ForbiddenTags incl. ADR-0091 closure, party scoping — never
	// O's RequiredTags / AnyOfClauses); for a person reading directly it is their
	// predicate without its positive terms. nil = no exemption.
	//
	// It is a COMPLETE second predicate, never a set of terms to subtract from
	// this one: the main predicate is always at least as narrow as OwnRows, so an
	// enforcement point that does not know this field applies the main predicate
	// and fails closed. A row is admitted when the main predicate admits it OR it
	// is an own row (OwnRowTerm) that OwnRows admits — the Go form (Check,
	// CheckRow) and every SQL mirror compute exactly that disjunction.
	//
	// The kernel never builds or widens it; it applies it as data, and only
	// through OwnRowTerm, which refuses a malformed one (Bypass, AllOwners,
	// LedgerRuns, a nested OwnRows, or Owners different from this predicate's
	// single owner) by returning nil — the main predicate alone, fail closed.
	// Not projectable to a TagSet (ToTagSet / TagSetFromPredicate drop it, which
	// narrows).
	OwnRows *TagPredicate `json:"own_rows,omitempty"`
}

// OwnRowTerm returns the own-rows predicate (ADR-0146) when it may be applied,
// nil otherwise. It is the ONE gate every enforcement point — Check, CheckRow
// and the SQL mirrors — goes through, so the fail-closed conditions (D4) live in
// one place:
//
//   - a nil, Bypass or AllOwners outer predicate has no own-rows term (Bypass
//     already admits everything but the fence; nothing may widen maintenance);
//   - the outer predicate must act for exactly ONE owner, non-empty;
//   - the inner predicate must not be Bypass, AllOwners, carry LedgerRuns or a
//     nested OwnRows, and its Owners must be exactly the outer's single owner.
func (p *TagPredicate) OwnRowTerm() *TagPredicate {
	if p == nil || p.Bypass || p.AllOwners || p.OwnRows == nil {
		return nil
	}
	if len(p.Owners) != 1 || p.Owners[0] == "" {
		return nil
	}
	o := p.OwnRows
	if o.Bypass || o.AllOwners || o.LedgerRuns != nil || o.OwnRows != nil {
		return nil
	}
	if len(o.Owners) != 1 || o.Owners[0] != p.Owners[0] {
		return nil
	}
	return o
}

// IsOwnRow reports whether a row carrying tags is one of this reader's own rows
// under ADR-0146: it carries owner:<Owners[0]> and NO other owner-namespace tag.
// A row with no owner tag is never an own row; mixed owners never are. False
// whenever OwnRowTerm is nil.
func (p *TagPredicate) IsOwnRow(tags []string) bool {
	if p.OwnRowTerm() == nil {
		return false
	}
	mine := OwnerTag(p.Owners[0])
	own := false
	for _, t := range tags {
		if !IsOwnerTag(t) {
			continue
		}
		if t != mine {
			return false
		}
		own = true
	}
	return own
}

// ownRowsFor is the own-rows predicate a row carrying tags may additionally be
// admitted by, nil when the row is not an own row or there is no valid term.
func (p *TagPredicate) ownRowsFor(tags []string) *TagPredicate {
	if !p.IsOwnRow(tags) {
		return nil
	}
	return p.OwnRowTerm()
}

// ownRowDetail marks an allow the own-rows rule produced, so an explanation
// can say why a row the main predicate refuses was admitted.
const ownRowDetail = "own row (ADR-0146)"

// LedgerRunGrant names the runs a staged planner owns. Roots are root run ids
// (RootRunID): an entry is granted when RootRunID(entry.RunID) is one of them, so
// every coverage stage ("-s<n>") and plan generation ("-g<n>") of the planner's
// own Execute is covered and no other Execute's run is. A nested delegated run
// (a worker's own Execute) has its own root and is NOT covered: its result
// reaches the parent run as the delegating step's output, recorded under the
// parent's run id. Owner is the run's conversation owner, the only owner:<id>
// rows the grant can admit ("" admits none). An empty grant grants nothing.
type LedgerRunGrant struct {
	Roots []string `json:"roots,omitempty"`
	Owner string   `json:"owner,omitempty"`
}

// Covers reports whether runID belongs to one of the granted runs. A nil grant,
// an empty run id or an empty root covers nothing (fail closed).
func (g *LedgerRunGrant) Covers(runID string) bool {
	if g == nil || runID == "" {
		return false
	}
	root := RootRunID(runID)
	for _, r := range g.Roots {
		if r != "" && r == root {
			return true
		}
	}
	return false
}

// ScopeSystem is the explicit, greppable sentinel that bypasses tag filtering for
// (everything but the owner fence — see ScopeMaintenance, owner_scope.go)
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
//
// A row the main terms refuse is admitted when it is the reader's own row
// (IsOwnRow) and the own-rows predicate (OwnRows, ADR-0146) admits it; the
// refusal returned otherwise is the main predicate's.
func (p *TagPredicate) Check(tags []string) (AccessDecision, bool) {
	dec, ok := p.checkMain(tags)
	if ok {
		return dec, ok
	}
	if own := p.ownRowsFor(tags); own != nil {
		if odec, ook := own.checkMain(tags); ook {
			odec.Detail = ownRowDetail
			return odec, true
		}
	}
	return dec, false
}

// checkMain is Check over this predicate's own terms only, never OwnRows.
func (p *TagPredicate) checkMain(tags []string) (AccessDecision, bool) {
	if p == nil {
		return AccessDecision{Reason: ReasonNoPrincipal, Detail: "no effective predicate (fail-closed)"}, false
	}
	// The owner fence first, and BEFORE Bypass: it is the one term a bypass does
	// not waive (owner_scope.go). Detail is generic on purpose — naming the owner
	// would tell the reader whose private material exists.
	if p.foreignOwnerTag(tags) != "" {
		return AccessDecision{Reason: ReasonNotOwner, Detail: "owner-scoped row"}, false
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
//
// The own-rows rule (ADR-0146) applies exactly as in Check: the main terms, or —
// for the reader's own row — the own-rows predicate, each with its party test.
func (p *TagPredicate) CheckRow(tags, parties []string) (AccessDecision, bool) {
	dec, ok := p.checkRowMain(tags, parties)
	if ok {
		return dec, ok
	}
	if own := p.ownRowsFor(tags); own != nil {
		if odec, ook := own.checkRowMain(tags, parties); ook {
			odec.Detail = ownRowDetail
			return odec, true
		}
	}
	return dec, false
}

// checkRowMain is CheckRow over this predicate's own terms only, never OwnRows.
func (p *TagPredicate) checkRowMain(tags, parties []string) (AccessDecision, bool) {
	// The tag terms first and unchanged, so a row denied by a label is denied for
	// that reason rather than for a relationship — the explanation an operator
	// gets should name the first thing that is actually wrong.
	dec, ok := p.checkMain(tags)
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
	for i, clause := range p.AnyOfClauses {
		// An EMPTY clause is an empty OR-set: nothing can satisfy it, so Check denies
		// every row and every SQL rendering renders it FALSE (owner decision
		// 2026-11-13, T-20261113-sql-fence-mirrors-skip-an-empty-any-of-clause). No
		// decision point should ever emit one, so the reason says so: the caller's
		// unsatisfiable-predicate log line then names it as an upstream bug.
		if len(clause) == 0 {
			return true, fmt.Sprintf("AnyOf clause #%d is empty (a resolver emitted an empty OR-set — likely an upstream bug)", i)
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
