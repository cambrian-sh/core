package postgres

// The identity plane read shapes of PgQueryPlane: the alias closure and the entity
// subject (moved from query_plane.go, ADR-0149 P0; no logic change).

import (
	"context"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// ─── The identity plane's read shapes (five-planes step 2) ───────────────────
//
// SCOPE RULE, stated once and relied on by everything below.
//
// A link is metadata ABOUT two refs; it is not the rows those refs stand for.
// So the two questions the plane answers are kept apart:
//
//  1. May the caller READ this row?  Decided by the row's OWN classification
//     and parties, resolved through the existing evidenceTags/AllowsRow path —
//     exactly as point/history/events already decide it. A link is a row like
//     any other and is judged on ITS evidence.
//  2. Which SUBJECT is being asked about?  Widened by the closure, and by
//     nothing else.
//
// The closure therefore NEVER widens access. Expanding "customer/C-1042" to its
// confirmed alias "crm/ACME" changes which entity ids the query asks about; each
// row that comes back is still authorized on its own terms, so a stranger who
// could read nothing before a confirmation can read nothing after it. That is
// the ADR-0118 access rule restated for links, and it is the property the
// non-widening test pins.
//
// Closure MEMBERSHIP is itself content (ADR-0128 D12): "these two systems'
// records are one counterparty" is frequently the sensitive part. So the walk
// itself is scoped (identityClosure): a member is admitted only through a link
// the caller can read, a hop does not continue through one it cannot, and a
// member the caller cannot justify is not listed, not counted, and not used to
// widen a subject. An earlier version listed such members with their
// justification dropped; that disclosed exactly the relationship the rule
// protects, in a form that looked careful.
//
// The same holds for candidates (entityOp) and for the operator review inbox
// (ReviewCandidates): a link is judged on its own evidence, and the operator
// plane's plain Bypass still applies the owner fence — only ScopeMaintenance
// reads every link, and no principal-serving read uses it.

// closureMember is one entity the identity closure admitted, carrying the
// justification that admitted it. Every member was admitted through a link the
// caller's scope can read (see SCOPE RULE above), so justified is always true;
// the field stays on the wire so a consumer never has to infer it.
type closureMember struct {
	entityID  string
	viaRef    string
	hop       int
	linkID    string
	relation  string
	mechanism string
	evidence  string
	justified bool
}

// closureResult is the closure and the entities the guards refused to expand.
type closureResult struct {
	members []closureMember
	// flagged holds refs whose fan-out under one mechanism exceeded
	// ClosureMaxLinksPerEntityPerMechanism. They are REPORTED, never silently
	// dropped: an id that one producer claims is sixteen other things is the
	// single most useful row in a review queue.
	flagged []string
}

// identityClosure expands an entity across CONFIRMED, unretracted identity links
// whose verb the registry declares Closure="identity".
//
// The verb set comes from the registry, never from a literal here. A deployment
// that declares no identity-closure verb gets no expansion at all, which is the
// correct behaviour and not a bug to work around: closure is a claim about what a
// verb MEANS, and only a deployment can make it.
//
// Guards, in the order they bite:
//   - depth    — the caller's, defaulted, hard-capped at ClosureMaxDepth
//   - fan-out  — an entity over ClosureMaxLinksPerEntityPerMechanism links of one
//     mechanism is FLAGGED and contributes no expansion
//   - set size — over ClosureMaxEntities the whole query is REFUSED, loudly,
//     rather than truncated into something that looks like an answer
//
// SCOPED: every hop reads only the links whose evidence the caller may read
// (filterLinks), and the walk does not continue through one it may not — the
// traverse precedent: a forbidden edge is not a bridge. Every guard therefore
// counts READABLE links only. Counting the unfiltered set would leak through the
// guards themselves: a flagged row would say "more than 16 links you cannot
// see", and a set-cap refusal would say "at least N aliases exist".
func (p *PgQueryPlane) identityClosure(ctx context.Context, entityID string, depth int, scope *domain.TagPredicate) (closureResult, error) {
	var out closureResult
	if p.links == nil || entityID == "" {
		return out, nil
	}
	verbs := p.relations.RelationsFor(defaultNamespace).ClosureVerbs(domain.ClosureIdentity)
	if len(verbs) == 0 {
		return out, nil
	}
	walkable := make(map[string]bool, len(verbs))
	for _, v := range verbs {
		walkable[v] = true
	}

	seed := domain.EntityRef(entityID)
	seen := map[string]bool{seed: true}
	frontier := []string{seed}
	for hop := 1; hop <= depth && len(frontier) > 0; hop++ {
		// One read for the whole frontier, guarded and paged until each ref has
		// the store's default page of READABLE links (filter before limit): a
		// page cut before the scope was applied would let a caller's hidden
		// links crowd its readable aliases out of the closure.
		linksBy, err := pageVisible(ctx, p, scope, frontier, defaultIdentityLimit,
			func(keys []string, offset, n int) (map[string][]domain.Link, error) {
				return p.links.LinksForRefs(ctx, defaultNamespace, keys, domain.LinkQuery{
					Family: domain.LinkFamilyIdentity,
					State:  domain.LinkStateConfirmed,
					Limit:  n, Offset: offset,
				})
			}, linkRefs)
		if err != nil {
			return closureResult{}, err
		}
		var next []string
		for _, ref := range frontier {
			links := linksBy[ref]
			perMechanism := map[string]int{}
			for _, l := range links {
				if walkable[l.Relation] {
					perMechanism[l.Mechanism]++
				}
			}
			overFanOut := false
			for _, n := range perMechanism {
				if n > domain.ClosureMaxLinksPerEntityPerMechanism {
					overFanOut = true
				}
			}
			if overFanOut {
				// Flag and EXCLUDE. Following it would spend the set cap on one
				// producer's bad batch and refuse the query for everybody else's
				// benefit; excluding it leaves the rest of the closure usable and
				// puts the offending id where a reviewer will see it.
				out.flagged = append(out.flagged, ref)
				continue
			}
			for _, l := range links {
				if !walkable[l.Relation] {
					continue
				}
				other := l.ToRef
				if other == ref {
					other = l.FromRef
				}
				if other == ref || seen[other] {
					continue
				}
				local, ok := strings.CutPrefix(other, domain.RefPrefixEntity)
				if !ok || local == "" {
					// An identity link to a non-entity ref is not an alias.
					continue
				}
				seen[other] = true
				if len(out.members)+1 > domain.ClosureMaxEntities {
					return closureResult{}, domain.ClosureSetRefusal(len(out.members) + 1)
				}
				out.members = append(out.members, closureMember{
					entityID: local, viaRef: ref, hop: hop, linkID: l.ID,
					relation: l.Relation, mechanism: l.Mechanism,
					evidence: string(l.EvidenceID), justified: true,
				})
				next = append(next, other)
			}
		}
		frontier = next
	}
	return out, nil
}

// mayKnowEntityExists answers the question ADR-0128 D12 introduced: is this
// caller allowed to learn that this handle exists at all?
//
// The basis is the evidence that MINTED the entity — the record whose arrival
// disclosed that the thing exists. A caller who can read that record already
// knows, and withholding the handle from them would refuse an answer they are
// entitled to. A caller who cannot read it learns nothing here.
//
// Fail closed on an unresolvable or absent id, as everywhere else on this plane.
// An entity with no recorded first-seen evidence is not thereby public; it is
// simply not disclosed by this route, and the caller may still reach it through
// a row they can read (see the gate in entityOp).
func (p *PgQueryPlane) mayKnowEntityExists(ctx context.Context, scope *domain.TagPredicate, firstSeen string) (bool, error) {
	if scope.Unfiltered() {
		return true, nil
	}
	if firstSeen == "" {
		return false, nil
	}
	// The minting evidence only: the mint carries no row classification yet
	// (entities.first_seen_classification is ADR-0158 stage 5).
	kept, err := guardNodes(ctx, p, scope, []string{firstSeen},
		func(id string) []guardRef { return []guardRef{{evidence: id}} })
	return len(kept) == 1, err
}

// filterLinks keeps the links whose evidence classification passes the scope,
// resolving provenance in one batch — the filterEvents shape applied to link
// rows.
//
// Fail closed on unresolvable evidence, as everywhere else on this plane — for
// a plain Bypass (the operator plane, ScopeSystem) too, which still owes the
// owner fence. The one case that costs is a `human` link, which the
// admissibility rule lets carry no evidence at all: such a link is visible only
// to ScopeMaintenance (scope.Unfiltered), i.e. to no principal-serving read. That
// is the conservative reading and the right one — a bare human assertion joining
// two ids is exactly the row whose visibility nobody has decided yet. (A
// ConfirmLink row inherits its candidate's evidence, so a confirmed candidate
// stays visible to whoever could read the candidate.)
func (p *PgQueryPlane) filterLinks(ctx context.Context, scope *domain.TagPredicate, links []domain.Link) ([]domain.Link, error) {
	return guardNodes(ctx, p, scope, links, linkRefs)
}

// linkRefs is what a link stands on: its evidence and its own row
// classification (ADR-0158 D2).
func linkRefs(l domain.Link) []guardRef {
	return []guardRef{{evidence: string(l.EvidenceID), rowTags: l.Classification}}
}

// linkRow renders one assertion for the wire. `row` discriminates the shapes an
// `entity` answer mixes, so a consumer never has to guess from key presence.
// Optional times are OMITTED when unset rather than emitted as year 1 — the
// obsRow convention, for the same reason: a zero time reads as data.
func linkRow(l domain.Link, kind string) domain.QueryRow {
	row := domain.QueryRow{
		"row": kind, "link_id": l.ID, "family": l.Family,
		"from_ref": l.FromRef, "to_ref": l.ToRef, "relation": l.Relation,
		"state": l.State, "mechanism": l.Mechanism, "producer": l.Producer,
		"confidence": l.Confidence, "evidence_id": string(l.EvidenceID),
		"asserted_by": l.AssertedBy, "asserted_at": l.AssertedAt,
		"source_ref": l.SourceRef,
	}
	if l.ValidFrom != nil {
		row["valid_from"] = *l.ValidFrom
	}
	if l.ValidTo != nil {
		row["valid_to"] = *l.ValidTo
	}
	if l.RetractedAt != nil {
		row["retracted_at"] = *l.RetractedAt
	}
	return row
}

// entityOp answers "what do we hold about this handle": the entity row, its
// identity closure, the confirmed links touching it, and — for an operator only —
// the candidates awaiting review.
//
// ENFORCED per row: every link is filtered on its OWN evidence (filterLinks), and
// the closure was walked over readable links only (SCOPE RULE), so an alias row
// appears only when the caller can read the link that admitted it.
//
// Candidates are withheld from every non-bypass caller by construction. A
// candidate is an unreviewed machine proposal, and an agent that reads one as an
// answer has done precisely what the review lane exists to prevent; the operator
// console, which is where review happens, reads with a bypass predicate. Even
// there each candidate passes filterLinks: a plain Bypass still applies the owner
// fence, so a candidate drawn from someone's owner:X evidence is not shown.
func (p *PgQueryPlane) entityOp(ctx context.Context, q domain.KnowledgeQuery,
	scope *domain.TagPredicate, closure closureResult, limit int) ([]domain.QueryRow, error) {
	out := []domain.QueryRow{}

	subject := domain.QueryRow{
		"row": "entity", "entity_id": q.EntityID, "minted": false,
		"namespace_id": defaultNamespace,
	}
	if kind, ok := domain.EntityKindFromID(q.EntityID); ok {
		subject["entity_kind"] = kind
	}
	var firstSeen string
	if p.entities != nil {
		e, found, err := p.entities.GetEntity(ctx, defaultNamespace, q.EntityID)
		if err != nil {
			return nil, err
		}
		if found {
			firstSeen = string(e.FirstSeenEvidence)
			subject["minted"] = true
			subject["entity_kind"] = e.Kind
			subject["namespace_id"] = e.NamespaceID
			subject["created_at"] = e.CreatedAt
			subject["first_seen_evidence"] = firstSeen
		}
	}
	// `minted` is REPORTED rather than turned into an empty answer: an id with
	// links but no handle is a real state — a producer wrote assertions about
	// something nothing ever minted — and hiding it would hide the defect.
	out = append(out, subject)

	// ADR-0128 D12: existence is protected content, not metadata. A caller who
	// can reach no row of this entity must not be able to tell it apart from an
	// id that was never minted, so the whole answer — subject row included — is
	// withheld rather than emptied.
	//
	// Two ways to earn the subject: read the evidence that minted it, or reach
	// any row about it below. The second matters because an entity is routinely
	// minted from a record a reader cannot see and then cited by records they
	// can; refusing those would over-refuse in the name of a rule about
	// under-refusing.
	subjectVisible, err := p.mayKnowEntityExists(ctx, scope, firstSeen)
	if err != nil {
		return nil, err
	}
	gate := func(rows []domain.QueryRow) []domain.QueryRow {
		if subjectVisible {
			return rows
		}
		for _, r := range rows {
			switch r["row"] {
			case "alias", "link", "candidate":
				// Reachable content: the caller already knows this handle is
				// real, so naming it discloses nothing further.
				return rows
			}
		}
		// Indistinguishable from a fabricated id, which is the point. The
		// distinction survives in the audit record, where an operator
		// diagnosing a complaint needs it and the caller cannot see it.
		return []domain.QueryRow{}
	}

	members := closure.members
	for _, m := range members {
		row := domain.QueryRow{
			"row": "alias", "entity_id": m.entityID, "hop": m.hop,
			"justified": m.justified,
		}
		if m.justified {
			row["via_ref"] = m.viaRef
			row["link_id"] = m.linkID
			row["relation"] = m.relation
			row["mechanism"] = m.mechanism
			row["evidence_id"] = m.evidence
		}
		out = append(out, row)
	}
	for _, ref := range closure.flagged {
		out = append(out, domain.QueryRow{
			"row": "flagged", "ref": ref,
			"guard": "closure_max_links_per_entity_per_mechanism",
			"limit": domain.ClosureMaxLinksPerEntityPerMechanism,
		})
	}

	if p.links == nil {
		return gate(out), nil
	}
	// Links for the subject, plus each alias's when the caller asked to expand.
	subjectRef := domain.EntityRef(q.EntityID)
	refs := []string{subjectRef}
	if q.ExpandAliases {
		for _, m := range members {
			refs = append(refs, domain.EntityRef(m.entityID))
		}
	}
	// One link is reported ONCE however many reads reach it. The two queries
	// below overlap by construction — a lineage link the subject asserted comes
	// back from both — and a card that listed it twice would read as two
	// citations where there is one.
	seenLink := make(map[string]bool)
	emit := func(l domain.Link, ref string, incoming bool) {
		if l.ID != "" {
			if seenLink[l.ID] {
				return
			}
			seenLink[l.ID] = true
		}
		row := linkRow(l, "link")
		if ref != subjectRef {
			row["via_ref"] = ref
		}
		if incoming {
			// Stamped only on the incoming half, so the ABSENCE of the key
			// keeps meaning what it meant before this field existed: a link
			// this ref asserted, or a symmetric verb with no direction to
			// assert in.
			row["direction"] = "incoming"
		}
		out = append(out, row)
	}
	// Each lane is ONE read for every ref, guarded and paged until each ref has
	// `limit` links the caller may read (pageVisible — filter before limit,
	// T-20261112-substrate-query-shapes-apply-limit-before-the): a page the
	// store cut before the scope was known let a caller's hidden links crowd its
	// readable ones off the card. The first page is exactly the read this shape
	// always made, so a caller whose scope hides nothing reads nothing more.
	outBy, err := pageVisible(ctx, p, scope, refs, limit,
		func(keys []string, offset, n int) (map[string][]domain.Link, error) {
			return p.links.LinksForRefs(ctx, defaultNamespace, keys, domain.LinkQuery{
				State: domain.LinkStateConfirmed, Limit: n, Offset: offset,
			})
		}, linkRefs)
	if err != nil {
		return nil, err
	}
	// What CITES this handle (D-W5-6). A read without IncludeIncoming answers
	// only what the ref itself asserted, and an entity is almost never the
	// asserter of its own citations: `referenced_by` runs from the citing
	// record TO the entity, so the card of the most-cited handle in a corpus
	// was the emptiest one on the plane.
	//
	// LINEAGE ONLY, and that is the discipline of this read. Incoming IDENTITY
	// is already reported as closure membership; incoming RELATION is somebody
	// else's assertion about their own record ("my parent is you") and reads
	// correctly only on their card, where a reader can see whose claim it is.
	// Citation is the one direction where the interesting end is the one being
	// pointed at.
	inBy, err := pageVisible(ctx, p, scope, refs, limit,
		func(keys []string, offset, n int) (map[string][]domain.Link, error) {
			return p.links.LinksForRefs(ctx, defaultNamespace, keys, domain.LinkQuery{
				Family: domain.LinkFamilyLineage, State: domain.LinkStateConfirmed,
				IncludeIncoming: true, Limit: n, Offset: offset,
			})
		}, linkRefs)
	if err != nil {
		return nil, err
	}
	for _, ref := range refs {
		for _, l := range outBy[ref] {
			emit(l, ref, false)
		}
		for _, l := range inBy[ref] {
			emit(l, ref, l.ToRef == ref)
		}
	}

	if seesReviewCandidates(ctx, scope) {
		// ENG-1536: operators only (seesReviewCandidates). The subject's slice of the review inbox, filtered to the subject IN SQL
		// before the limit and paged past owner-fenced rows by pageVisible — so a
		// subject's candidates are never lost behind other subjects'
		// higher-confidence ones (they were, when this read the whole inbox to
		// `limit` and filtered here). Same membership and order as the inbox
		// (PgLinkStore openCandidate / candidateOrder): a confirmed candidate is
		// answered and does not come back.
		cands, err := pageVisible(ctx, p, scope, []string{subjectRef}, limit,
			func(keys []string, offset, n int) (map[string][]domain.Link, error) {
				return p.links.CandidatesForRefs(ctx, defaultNamespace, keys, offset, n)
			}, linkRefs)
		if err != nil {
			return nil, err
		}
		for _, l := range cands[subjectRef] {
			out = append(out, linkRow(l, "candidate"))
		}
	}
	return gate(out), nil
}
