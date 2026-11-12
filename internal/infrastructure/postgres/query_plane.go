package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// PgQueryPlane executes the closed query AST (ADR-0111) by composing the
// existing typed stores and adding exactly the four read shapes they lack.
// Validation runs FIRST; an invalid query never touches a store.
//
// ADR-0118 D2: every shape enforces the caller's scope predicate, matched to
// what its rows actually carry. Evidence and knowledge items hold their own
// classification; events and observations reach it only through evidence_id,
// so those shapes resolve provenance and filter — and a row whose evidence
// cannot be resolved is DROPPED for non-bypass scopes (fail closed;
// observations.evidence_id has no FK, so absence must not become visibility).
// Aggregates cannot be post-filtered and gain the predicate in SQL instead.
// Every method below is labelled with how it enforces — the
// EnforcingVectorStore discipline: adding a shape breaks this file until
// someone decides.
type PgQueryPlane struct {
	pool      *pgxpool.Pool
	events    walkEventStore
	knowledge domain.KnowledgeStore
	evidence  domain.EvidenceStore
	// The identity plane (five-planes step 2; FIVE-PLANES-BUILD.md). All three
	// may be nil: a deployment without migration 0016 has no links, and the two
	// new shapes then answer emptily rather than erroring — "this kernel holds no
	// assertions about identity" is a true answer, and a 500 is not.
	//
	// relations is how the closure learns WHICH verbs it may walk. The kernel
	// never names one: the Zero-Hardcode Rule applies to vocabulary as hard here
	// as it does to routing, and a hard-coded "same_as" would make the registry
	// decorative.
	links     walkLinkStore
	entities  domain.EntityStore
	relations *domain.RelationRegistry
}

// walkLinkStore is the link port plus the set-based read the graph walks need
// (why, the identity closure): a whole frontier's links in one round trip, Limit
// and Offset applied per ref. Kept here, as a consumer-side composition of the
// domain port (AGENTS §3.1), rather than on domain.LinkStore: it is how THIS
// plane reads, and no other LinkStore consumer has a frontier.
// PgLinkStore.LinksFor is LinksForRefs with one ref, so the two never disagree.
type walkLinkStore interface {
	domain.LinkStore
	LinksForRefs(ctx context.Context, namespace string, refs []string, opts domain.LinkQuery) (map[string][]domain.Link, error)
	// CandidatesForRefs is the review inbox (Candidates' membership and order)
	// restricted to the candidates touching each ref, Offset/Limit per ref — how
	// the entity card reads one subject's candidates without reading the inbox.
	CandidatesForRefs(ctx context.Context, namespace string, refs []string, offset, limit int) (map[string][]domain.Link, error)
}

var _ walkLinkStore = (*PgLinkStore)(nil)

// walkEventStore is the event port plus the set-based read traverse needs: a
// whole frontier's events in one round trip (roles in a second), each entity's
// range exactly as EventsForEntity reads it. The walkLinkStore pattern: a
// consumer-side composition of the domain port (AGENTS §3.1), because only this
// plane has a frontier. PgEventStore.EventsForEntity is EventsForEntities with
// one id, so the two never disagree.
type walkEventStore interface {
	domain.EventStore
	EventsForEntities(ctx context.Context, namespace string, entityIDs []string, from, to time.Time) (map[string][]domain.Event, error)
}

var _ walkEventStore = (*PgEventStore)(nil)

// NewPgQueryPlane composes the plane from the stores it reads through.
//
// links/entities/relations arrived with five-planes step 2 and are positional
// rather than optional for the reason scope is: every construction site is made
// to decide, and the one deployment that forgets shows up at compile time rather
// than as an entity op that quietly returns nothing.
func NewPgQueryPlane(pool *pgxpool.Pool, events walkEventStore,
	knowledge domain.KnowledgeStore, evidence domain.EvidenceStore,
	links walkLinkStore, entities domain.EntityStore,
	relations *domain.RelationRegistry) *PgQueryPlane {
	return &PgQueryPlane{
		pool: pool, events: events, knowledge: knowledge, evidence: evidence,
		links: links, entities: entities, relations: relations,
	}
}

var _ domain.QueryPlane = (*PgQueryPlane)(nil)

const defaultNamespace = "default"

// Query validates, then dispatches. Every answer carries its §14 guarantee.
//
// Scope is a REQUIRED positional parameter: nil denies (ErrQueryScopeMissing),
// bypass reads unrestricted, an unsatisfiable predicate proceeds and returns
// zero rows (the EnforcingVectorStore convention — unsatisfiable is a valid,
// empty scope, not an error).
func (p *PgQueryPlane) Query(ctx context.Context, q domain.KnowledgeQuery, scope *domain.TagPredicate) (domain.QueryResult, error) {
	if scope == nil {
		return domain.QueryResult{}, domain.ErrQueryScopeMissing
	}
	if err := q.Validate(); err != nil {
		return domain.QueryResult{}, err
	}
	limit := q.Limit
	if limit == 0 {
		limit = 100
	}
	// The identity closure is resolved BEFORE any shape runs, because two
	// different things want it: `entity` reports the closure as part of its
	// answer, and ExpandAliases uses it to widen the SUBJECT of an ordinary
	// entity-scoped shape. Resolving it once means the guards are enforced once.
	var (
		closure closureResult
		err     error
	)
	if q.Kind == domain.QueryEntity || (q.ExpandAliases && q.SupportsAliasExpansion()) {
		// Scoped: the closure walks only links this caller can read, so the
		// widened subjects, ClosureSize, the fan-out flags and the set-cap
		// refusal never count an alias the caller could not justify (no hidden
		// counts; T-20261107-link-review-inbox-and-entity-op-candidates-skip).
		closure, err = p.identityClosure(ctx, q.EntityID, closureDepth(q), scope)
		if err != nil {
			return domain.QueryResult{}, err
		}
	}
	subjects := []string{q.EntityID}
	if q.ExpandAliases && q.SupportsAliasExpansion() {
		for _, m := range closure.members {
			subjects = append(subjects, m.entityID)
		}
	}

	var rows []domain.QueryRow
	switch q.Kind {
	case domain.QueryPoint:
		rows, err = perSubject(subjects, q, func(sq domain.KnowledgeQuery) ([]domain.QueryRow, error) {
			return p.point(ctx, sq, scope)
		})
	case domain.QueryHistory:
		rows, err = perSubject(subjects, q, func(sq domain.KnowledgeQuery) ([]domain.QueryRow, error) {
			return p.history(ctx, sq, scope, limit)
		})
	case domain.QueryAsOf:
		rows, err = p.asOf(ctx, q, scope, limit)
	case domain.QueryCurrent:
		rows, err = p.current(ctx, q, scope, limit)
	case domain.QueryContradictions:
		rows, err = p.contradictions(ctx, q, scope, limit)
	case domain.QueryAggregate:
		// One row PER SUBJECT under expansion, never one merged figure: count
		// would sum correctly and avg would not, and a plane that silently
		// returns the mean of four means has invented a statistic.
		rows, err = perSubject(subjects, q, func(sq domain.KnowledgeQuery) ([]domain.QueryRow, error) {
			return p.aggregate(ctx, sq, scope)
		})
	case domain.QueryEvents:
		rows, err = perSubject(subjects, q, func(sq domain.KnowledgeQuery) ([]domain.QueryRow, error) {
			return p.eventsFor(ctx, sq, scope, limit)
		})
	case domain.QueryTraverse:
		rows, err = perSubject(subjects, q, func(sq domain.KnowledgeQuery) ([]domain.QueryRow, error) {
			return p.traverse(ctx, sq, scope, limit)
		})
	case domain.QueryEvidence:
		rows, err = p.evidenceRow(ctx, q, scope)
	case domain.QueryEntity:
		rows, err = p.entityOp(ctx, q, scope, closure, limit)
	case domain.QueryWhy:
		rows, err = p.why(ctx, q, scope, limit)
	}
	if err != nil {
		return domain.QueryResult{}, err
	}
	// The limit bounds the ANSWER, not each alias's share of it: a per-subject
	// limit applied N times would return N times what the caller asked for.
	if len(rows) > limit {
		rows = rows[:limit]
	}
	res := domain.QueryResult{Guarantee: q.Guarantee(), Rows: rows}
	if q.ExpandAliases && q.SupportsAliasExpansion() {
		// Reported only when the answer was actually DRAWN OVER the aliases.
		// `entity` lists its closure as rows either way, and labelling an
		// unexpanded answer "across 3 aliases" would claim a widening that did
		// not happen.
		res.ClosureSize = len(closure.members)
		res.Guarantee += domain.ClosureNote(len(closure.members))
	}
	return res, nil
}

// perSubject runs one entity-scoped shape once per subject and stamps `via_ref`
// on every row an ALIAS produced, so a reader can always tell which id an answer
// actually came from. subjects[0] is the asked-for entity and is never stamped.
func perSubject(subjects []string, q domain.KnowledgeQuery,
	run func(domain.KnowledgeQuery) ([]domain.QueryRow, error)) ([]domain.QueryRow, error) {
	var out []domain.QueryRow
	for i, subject := range subjects {
		sq := q
		sq.EntityID = subject
		rows, err := run(sq)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			for _, r := range rows {
				r["via_ref"] = domain.EntityRef(subject)
			}
		}
		out = append(out, rows...)
	}
	return out, nil
}

// closureDepth resolves the CLOSURE walk depth.
//
// Hops is honoured only for `entity`, where it means nothing else. On `traverse`
// Hops already means the co-participation walk's depth, and letting one number
// drive two different walks is how a caller ends up tuning the wrong one; every
// other shape takes the default.
func closureDepth(q domain.KnowledgeQuery) int {
	d := 0
	if q.Kind == domain.QueryEntity {
		d = q.Hops
	}
	if d <= 0 {
		d = domain.ClosureDefaultDepth
	}
	if d > domain.ClosureMaxDepth {
		d = domain.ClosureMaxDepth
	}
	return d
}

// identityArg renders a reader's identities for the SQL overlap test.
//
// A nil slice would be sent as NULL, and `parties && NULL` is NULL rather than
// false — which in a WHERE clause is not-true and therefore still denies, but by
// three-valued-logic accident rather than by intent. An empty array denies for
// the reason we mean: this reader is a party to nothing.
func identityArg(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

// evidenceAccess is one evidence row's access-relevant state: what it IS and who
// it is ABOUT (ADR-0121). Named apart from the evidenceRow QUERY SHAPE below,
// which is a different thing that renders a row for the wire.
type evidenceAccess struct {
	tags    []string
	parties []string
}

// evidenceTags batch-resolves evidence ids to their classification AND parties.
// Ids absent from the result were not resolvable and their rows must be dropped.
func (p *PgQueryPlane) evidenceTags(ctx context.Context, ids []string) (map[string]evidenceAccess, error) {
	out := make(map[string]evidenceAccess, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := p.pool.Query(ctx,
		`SELECT id, classification, parties FROM evidence WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var row evidenceAccess
		if err := rows.Scan(&id, &row.tags, &row.parties); err != nil {
			return nil, err
		}
		out[id] = row
	}
	return out, rows.Err()
}

// obsRow renders one observation. The value slot is a TYPED UNION selected by
// value_type; only the selected member is emitted — an unset Go time.Time
// serializes as year 1 and reads as data, which is exactly the confusion a
// wire row must not create. Empty location is likewise omitted (absent, not
// blank).
func obsRow(ob *domain.Observation) domain.QueryRow {
	row := domain.QueryRow{
		"entity_id": ob.EntityID, "predicate": ob.Predicate,
		"value_type": ob.Value.Type, "occurred_at": ob.OccurredAt,
		"confidence": ob.Confidence, "evidence_id": string(ob.EvidenceID),
	}
	switch ob.Value.Type {
	case "date":
		row["value_date"] = ob.Value.Date
	case "number":
		row["value_number"] = ob.Value.Number
	case "text":
		row["value_text"] = ob.Value.Text
	case "entity":
		row["value_entity_id"] = ob.Value.EntityRef
	}
	if ob.Location != "" {
		row["location"] = ob.Location
	}
	// Which clock occurred_at is (ADR-0147 D8). Only when the writer said: an
	// absent key means "not recorded", which is every row written before 0037.
	if ob.TimeBasis != "" {
		row["time_basis"] = ob.TimeBasis
	}
	return row
}

// point — ENFORCED in SQL: the answer is the latest observation the caller may
// READ, not the latest observation filtered afterwards.
//
// The order matters. Taking the newest row and then filtering it meant a newer
// row from someone else's owner:X evidence turned a reader's ordinary answer
// into "not found" — so the presence of an answer told the reader whether a
// newer private row existed (an existence oracle, ADR-0128 D12;
// T-20261107-substratelane-pointlookup-history). Only ScopeMaintenance, which
// filters nothing, keeps the store's own PointLookup.
func (p *PgQueryPlane) point(ctx context.Context, q domain.KnowledgeQuery, scope *domain.TagPredicate) ([]domain.QueryRow, error) {
	if scope.Unfiltered() {
		ob, err := p.events.PointLookup(ctx, defaultNamespace, q.EntityID, q.Predicate)
		if err != nil || ob == nil {
			return nil, err
		}
		return []domain.QueryRow{obsRow(ob)}, nil
	}
	args := []any{defaultNamespace, q.EntityID, q.Predicate}
	scopeCond, extra := evidenceScopeSQL(scope, len(args))
	args = append(args, extra...)
	ob, err := scanObservation(p.pool.QueryRow(ctx, `
		SELECT `+observationColumns+` FROM observations
		WHERE namespace_id=$1 AND entity_id=$2 AND predicate=$3`+scopeCond+notRetired("observations")+`
		ORDER BY occurred_at DESC, id DESC LIMIT 1`, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []domain.QueryRow{obsRow(ob)}, nil
}

// history — ENFORCED in SQL via each observation's evidence classification
// (evidenceScopeSQL, point's rendering), and the LIMIT applies AFTER the fence:
// the page is the first `limit` rows the caller may read, oldest first, and
// the range is never loaded whole only to be cut in Go
// (T-20261112-substrate-query-shapes-apply-limit-before-the). Retired rows
// (ADR-0147) are excluded as in EventStore.History.
func (p *PgQueryPlane) history(ctx context.Context, q domain.KnowledgeQuery, scope *domain.TagPredicate, limit int) ([]domain.QueryRow, error) {
	args := []any{defaultNamespace, q.EntityID, q.Predicate, q.From.UTC(), q.To.UTC()}
	scopeCond, extra := evidenceScopeSQL(scope, len(args))
	args = append(args, extra...)
	args = append(args, limit)
	sqlRows, err := p.pool.Query(ctx, `
		SELECT `+observationColumns+` FROM observations
		WHERE namespace_id=$1 AND entity_id=$2 AND predicate=$3
		  AND occurred_at >= $4 AND occurred_at < $5`+scopeCond+notRetired("observations")+`
		ORDER BY occurred_at, id LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer sqlRows.Close()
	rows := []domain.QueryRow{}
	for sqlRows.Next() {
		ob, err := scanObservation(sqlRows)
		if err != nil {
			return nil, err
		}
		rows = append(rows, obsRow(ob))
	}
	return rows, sqlRows.Err()
}

func itemValues(it *domain.KnowledgeItem) map[string]any {
	vals := map[string]any{}
	if it == nil {
		return vals
	}
	for _, v := range it.Values {
		switch v.Type {
		case "date":
			vals[v.Predicate] = v.Date
		case "number":
			vals[v.Predicate] = v.Number
		case "text":
			vals[v.Predicate] = v.Text
		case "entity":
			vals[v.Predicate] = v.EntityRef
		}
	}
	return vals
}

// asOf answers "what did our records hold at time T" from the resolutions
// version history — the one §14 row that is exact AND complete, because it is
// a question about our own records rather than the world.
//
// ENFORCED in SQL via the backing knowledge item's classification (sqlArgs.item
// → textArrayScopeSQL, the TagPredicate.Allows mirror the catalog uses; owner
// fence under Bypass too), and the LIMIT applies AFTER the fence, so a narrow
// caller's page is the first `limit` rows it may read rather than whatever was
// left of a page cut first (T-20261112-query-plane-as-of-applies-sql-limit-
// before-the). A row whose item does not resolve fails closed for every
// filtered scope. Items and their values load in one batch, and a failed read
// FAILS the query — it used to drop the row, which made "exact and complete"
// false on any transient error.
//
// POLICY AND ACTOR (owner decision 2026-11-12, ADR-0111 amendment): as_of reads
// the rows of ONE policy, defaulting exactly as current does
// (KnowledgeQuery.PolicyOrDefault), so as_of(now) answers what current answers.
// It used to read every policy, which for an entity-scoped kind mixed the '*'
// cross-actor winner with each actor's own latest in one answer. Actor narrows to
// that actor's own rows; an actor on a cross-actor policy is refused
// (refuseActorOnCrossActorPolicy).
func (p *PgQueryPlane) asOf(ctx context.Context, q domain.KnowledgeQuery, scope *domain.TagPredicate, limit int) ([]domain.QueryRow, error) {
	policy := q.PolicyOrDefault()
	if err := p.refuseActorOnCrossActorPolicy(ctx, q, policy); err != nil {
		return nil, err
	}
	a := &sqlArgs{}
	where := `r.namespace_id=` + a.add(defaultNamespace) + ` AND r.kind=` + a.add(q.ItemKind) +
		` AND r.policy=` + a.add(policy)
	at := a.add(q.AsOf.UTC())
	where += ` AND r.system_from <= ` + at + ` AND (r.system_to IS NULL OR r.system_to > ` + at + `)
		  AND r.item_id IS NOT NULL`
	if q.EntityID != "" {
		where += ` AND r.entity_id = ` + a.add(q.EntityID)
	}
	if q.Actor != "" {
		where += ` AND r.actor = ` + a.add(q.Actor)
	}
	where += a.item(scope, "ki")
	sqlRows, err := p.pool.Query(ctx, `
		SELECT r.entity_id, r.actor, r.policy, r.item_id, r.reason_code, r.system_from
		FROM resolutions r LEFT JOIN knowledge_items ki ON ki.id = r.item_id
		WHERE `+where+`
		ORDER BY r.entity_id, r.actor, r.id LIMIT `+a.add(limit), a.args...)
	if err != nil {
		return nil, err
	}
	defer sqlRows.Close()
	type resRow struct {
		entity, actor, policy, itemID, reason string
		sysFrom                               time.Time
	}
	var scanned []resRow
	var ids []string
	for sqlRows.Next() {
		var r resRow
		if err := sqlRows.Scan(&r.entity, &r.actor, &r.policy, &r.itemID, &r.reason, &r.sysFrom); err != nil {
			return nil, err
		}
		scanned = append(scanned, r)
		ids = append(ids, r.itemID)
	}
	if err := sqlRows.Err(); err != nil {
		return nil, err
	}
	sqlRows.Close()
	items := map[string]*domain.KnowledgeItem{}
	if len(ids) > 0 {
		loaded, err := loadItems(ctx, p.pool, `WHERE ki.id = ANY($1)`, ids)
		if err != nil {
			return nil, fmt.Errorf("as_of: load items: %w", err)
		}
		for i := range loaded {
			items[string(loaded[i].ID)] = &loaded[i]
		}
	}
	var out []domain.QueryRow
	for _, r := range scanned {
		it := items[r.itemID]
		if it == nil {
			if scope.Unfiltered() {
				// Maintenance keeps the pre-0118 shape: the resolution row without
				// its values, rather than silently narrowing a maintenance read.
				// A plain Bypass no longer does: an item it cannot classify may
				// be another principal's owner-scoped row (owner fence) — the SQL
				// fence already required the item for every filtered scope, so
				// only an item erased between the two reads lands here.
				out = append(out, domain.QueryRow{
					"entity_id": r.entity, "actor": r.actor, "policy": r.policy,
					"reason_code": r.reason, "system_from": r.sysFrom, "item_id": r.itemID,
				})
			}
			continue
		}
		out = append(out, domain.QueryRow{
			"entity_id": r.entity, "actor": r.actor, "policy": r.policy,
			"reason_code": r.reason, "system_from": r.sysFrom, "item_id": r.itemID,
			"values": itemValues(it), "asserted_by": it.AssertedBy, "asserted_at": it.AssertedAt,
		})
	}
	return out, nil
}

// refuseActorOnCrossActorPolicy refuses an Actor on a policy whose resolution
// rows for this kind are cross-actor ('*', an entity-scoped kind's winner). Those
// rows are derived from EVERY actor's claims, so "this actor's own claims,
// resolved under that policy" is not something the store holds; answering with
// the actor's latest_assertion rows, or with nothing, would each be a different
// question answered silently (ADR-0111, ADR-0110 D2 "cannot express safely").
// Decided from the rows themselves, so it needs no kind registry and holds for a
// kind declared by deployment data.
func (p *PgQueryPlane) refuseActorOnCrossActorPolicy(ctx context.Context, q domain.KnowledgeQuery, policy string) error {
	if q.Actor == "" {
		return nil
	}
	var crossActor bool
	if err := p.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM resolutions
			WHERE namespace_id=$1 AND kind=$2 AND policy=$3 AND actor=$4)`,
		defaultNamespace, q.ItemKind, policy, domain.ResolutionActorAll).Scan(&crossActor); err != nil {
		return fmt.Errorf("%s: policy rows: %w", q.Kind, err)
	}
	if crossActor {
		return fmt.Errorf("%s: actor %q with policy %q: kind %q resolves that policy ACROSS actors, "+
			"so one actor's own claims under it are not stored — leave policy empty for the actor's "+
			"latest, or drop actor for the cross-actor winner: %w",
			q.Kind, q.Actor, policy, q.ItemKind, domain.ErrCannotExpress)
	}
	return nil
}

// current — ENFORCED via each resolution's backing item classification.
// Policy defaults to latest_assertion; Actor keeps that actor's own rows only.
func (p *PgQueryPlane) current(ctx context.Context, q domain.KnowledgeQuery, scope *domain.TagPredicate, limit int) ([]domain.QueryRow, error) {
	policy := q.PolicyOrDefault()
	if err := p.refuseActorOnCrossActorPolicy(ctx, q, policy); err != nil {
		return nil, err
	}
	res, err := p.knowledge.CurrentResolutions(ctx, defaultNamespace, q.ItemKind, policy)
	if err != nil {
		return nil, err
	}
	var out []domain.QueryRow
	for _, r := range res {
		if len(out) == limit {
			break
		}
		if q.EntityID != "" && r.EntityID != q.EntityID {
			continue
		}
		if q.Actor != "" && r.Actor != q.Actor {
			continue
		}
		if r.Item == nil || !scope.Allows(r.Item.Classification) { // owner fence holds under Bypass
			continue
		}
		out = append(out, domain.QueryRow{
			"entity_id": r.EntityID, "actor": r.Actor, "policy": r.Policy,
			"reason_code": r.ReasonCode, "values": itemValues(r.Item),
		})
	}
	return out, nil
}

// contradictions surfaces entities whose CURRENT resolutions disagree across
// actors — the disagreement returned, never resolved away (memo §8).
//
// ENFORCED via each side's backing item classification, BEFORE grouping: a
// contradiction row must not leak one side's value because the other side was
// readable.
//
// Actor keeps only the disagreements that actor is part of (it holds one of the
// sides); the other sides are still every actor's, because a disagreement with
// one side removed is not a disagreement. Actor on a cross-actor policy is
// refused, as on current.
func (p *PgQueryPlane) contradictions(ctx context.Context, q domain.KnowledgeQuery, scope *domain.TagPredicate, limit int) ([]domain.QueryRow, error) {
	policy := q.PolicyOrDefault()
	if err := p.refuseActorOnCrossActorPolicy(ctx, q, policy); err != nil {
		return nil, err
	}
	res, err := p.knowledge.CurrentResolutions(ctx, defaultNamespace, q.ItemKind, policy)
	if err != nil {
		return nil, err
	}
	type side struct {
		actor string
		value any
	}
	byEntityPred := map[string]map[string][]side{}
	for _, r := range res {
		if q.EntityID != "" && r.EntityID != q.EntityID {
			continue
		}
		if r.Item == nil || !scope.Allows(r.Item.Classification) { // owner fence holds under Bypass
			continue
		}
		for pred, val := range itemValues(r.Item) {
			if q.Predicate != "" && pred != q.Predicate {
				continue
			}
			if byEntityPred[r.EntityID] == nil {
				byEntityPred[r.EntityID] = map[string][]side{}
			}
			byEntityPred[r.EntityID][pred] = append(byEntityPred[r.EntityID][pred], side{r.Actor, val})
		}
	}
	var out []domain.QueryRow
	for entity, preds := range byEntityPred {
		for pred, sides := range preds {
			if len(sides) < 2 {
				continue
			}
			distinct := map[string]bool{}
			views := map[string]any{}
			for _, s := range sides {
				distinct[fmt.Sprint(s.value)] = true
				views[s.actor] = s.value
			}
			if len(distinct) < 2 {
				continue
			}
			if _, involved := views[q.Actor]; q.Actor != "" && !involved {
				continue
			}
			if len(out) == limit {
				return out, nil
			}
			out = append(out, domain.QueryRow{
				"entity_id": entity, "predicate": pred, "actors": views,
			})
		}
	}
	return out, nil
}

// evidenceScopeSQL renders the scope predicate as an EXISTS join against the
// evidence table's native TEXT[] classification — required ⊆ (@>), forbidden
// disjoint (NOT &&), each any-of clause overlapping (&&). Args are appended
// after the caller's, starting at $startIdx+1.
func evidenceScopeSQL(scope *domain.TagPredicate, startIdx int) (string, []any) {
	return evidenceScopeSQLOn(scope, "observations.evidence_id", startIdx)
}

// evidenceScopeSQLOn is evidenceScopeSQL for any column that references
// evidence.id (observations, events, the outbox). It is the ONE SQL rendering
// of "may this reader see the row this evidence backs", shared by the aggregate
// shape and the names-and-counts catalog (query_plane_catalog.go) so the two can
// never disagree. "" for ScopeMaintenance, the only unfiltered predicate.
func evidenceScopeSQLOn(scope *domain.TagPredicate, refCol string, startIdx int) (string, []any) {
	if scope.Unfiltered() {
		return "", nil
	}
	conds, args := evidenceRowConds(scope, "e", startIdx)
	return ` AND EXISTS (SELECT 1 FROM evidence e
		WHERE e.id = ` + refCol + conds + `)`, args
}

// evidenceRowConds renders the scope as " AND …" conditions over an evidence
// row aliased `alias` — the body of evidenceScopeSQLOn, also used directly when
// the evidence table itself is the one being read. "" for ScopeMaintenance.
//
// It is TagPredicate.CheckRow's mirror (the row carries parties). With a valid
// own-rows term (ADR-0146, TagPredicate.OwnRowTerm) the conditions become
// " AND ((main) OR (own_row AND ownrows))": own_row is "the classification contains
// owner:<the reader's single owner>"; ownrows is this rendering of the own-rows
// predicate, whose fence (held = that owner) adds "and no other owner tag".
func evidenceRowConds(scope *domain.TagPredicate, alias string, startIdx int) (string, []any) {
	if scope.Unfiltered() {
		return "", nil
	}
	conds, args, n := evidenceRowTerms(scope, alias, startIdx)
	own := scope.OwnRowTerm()
	if own == nil {
		return " AND " + strings.Join(conds, " AND "), args
	}
	n++
	ownRow := fmt.Sprintf(alias+".classification @> ARRAY[$%d::text]", n)
	args = append(args, domain.OwnerTag(scope.Owners[0]))
	oconds, oargs, _ := evidenceRowTerms(own, alias, n)
	args = append(args, oargs...)
	return " AND ((" + strings.Join(conds, " AND ") + ") OR (" + ownRow + " AND " +
		strings.Join(oconds, " AND ") + "))", args
}

// evidenceRowTerms renders one predicate's own terms (never its OwnRows) as
// conditions to AND over the evidence row `alias`, numbering args from
// $startIdx+1; it returns the last index used. Never empty: the owner fence is
// always first.
func evidenceRowTerms(scope *domain.TagPredicate, alias string, startIdx int) ([]string, []any, int) {
	var conds []string
	var args []any
	n := startIdx
	add := func(cond string, val any) {
		n++
		conds = append(conds, fmt.Sprintf(cond, n))
		args = append(args, val)
	}
	// The owner fence (domain/owner_scope.go): every predicate but
	// ScopeMaintenance, Bypass included.
	add(ownerFenceTextArray(alias+".classification"), scope.HeldOwnerTags())
	if scope.Bypass {
		return conds, args, n
	}
	col := alias + ".classification"
	if len(scope.RequiredTags) > 0 {
		add(col+" @> $%d", scope.RequiredTags)
	}
	if len(scope.ForbiddenTags) > 0 {
		add("NOT ("+col+" && $%d)", scope.ForbiddenTags)
	}
	for _, clause := range scope.AnyOfClauses {
		if len(clause) > 0 {
			add(col+" && $%d", clause)
		}
	}
	// Party-scoping (ADR-0121 D1): a material implication — if the row carries a
	// party-scoped tag, the reader must be one of its parties.
	//
	// It can only ever REMOVE rows: a row without any party-scoped tag satisfies
	// the left disjunct and is untouched, and a row with one is admitted only by
	// the extra condition. Nothing here admits a row the tag terms above rejected,
	// which is what keeps INV-1 (no path widens a scope) true of this clause.
	//
	// With no identities the right disjunct is `parties && '{}'` — always false —
	// so every party-scoped row is denied. That is D6's fail-closed reading, and
	// it is why "party to nothing" and "we could not resolve you" agree here.
	if len(scope.PartyScopedTags) > 0 {
		n += 2
		conds = append(conds, fmt.Sprintf(
			"(NOT ("+col+" && $%d) OR "+alias+".parties && $%d)", n-1, n))
		args = append(args, scope.PartyScopedTags, identityArg(scope.PartyIdentities))
	}
	return conds, args, n
}

// aggregate — ENFORCED in SQL: summed rows cannot be post-filtered, so under a
// non-bypass scope the predicate joins to evidence classification inside the
// query itself.
func (p *PgQueryPlane) aggregate(ctx context.Context, q domain.KnowledgeQuery, scope *domain.TagPredicate) ([]domain.QueryRow, error) {
	// The function set is CLOSED and mapped here — never interpolated from the
	// request, which is what keeps this an AST and not SQL smuggling.
	fns := map[string]string{
		"count": "count(*)",
		"avg":   "avg(value_number)",
		"min":   "min(value_number)",
		"max":   "max(value_number)",
	}
	from, to := q.From, q.To
	if from.IsZero() {
		from = time.Unix(0, 0)
	}
	if to.IsZero() {
		to = time.Now().UTC().Add(24 * time.Hour)
	}
	args := []any{defaultNamespace, q.EntityID, q.Predicate, from.UTC(), to.UTC()}
	scopeCond := ""
	if !scope.Unfiltered() {
		var extra []any
		scopeCond, extra = evidenceScopeSQL(scope, len(args))
		args = append(args, extra...)
	}
	var value *float64
	err := p.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT %s::float8 FROM observations
		WHERE namespace_id=$1 AND entity_id=$2 AND predicate=$3
		  AND occurred_at >= $4 AND occurred_at < $5%s%s`, fns[q.Aggregate], scopeCond, notRetired("observations")),
		args...).Scan(&value)
	if err != nil {
		return nil, err
	}
	row := domain.QueryRow{"entity_id": q.EntityID, "predicate": q.Predicate, "aggregate": q.Aggregate}
	if value != nil {
		row["value"] = *value
	}
	return []domain.QueryRow{row}, nil
}

// filterEvents keeps events whose evidence classification passes the scope,
// resolving provenance in one batch (fail closed on unresolvable evidence).
func (p *PgQueryPlane) filterEvents(ctx context.Context, scope *domain.TagPredicate, evs []domain.Event) ([]domain.Event, error) {
	// AllowsRow (inside guardNodes): an event reaches scope through its
	// evidence, which carries parties as well as tags (ADR-0121).
	return guardNodes(ctx, p, scope, evs, eventEvidence)
}

func eventEvidence(ev domain.Event) []string { return []string{string(ev.EvidenceID)} }

// eventsFor — ENFORCED in SQL via each event's evidence classification
// (evidenceScopeSQLOn, the rendering point/aggregate use), with the LIMIT AFTER
// the fence: the page is the first `limit` events the caller may read, oldest
// first, and the range is never loaded whole only to be cut in Go
// (T-20261112-substrate-query-shapes-apply-limit-before-the). The event
// selection is EventsForEntity's — same range, same order, retired rows
// excluded (notRetired, ADR-0147) — and the page's roles load in one statement.
func (p *PgQueryPlane) eventsFor(ctx context.Context, q domain.KnowledgeQuery, scope *domain.TagPredicate, limit int) ([]domain.QueryRow, error) {
	args := []any{defaultNamespace, q.From.UTC(), q.To.UTC(), q.EntityID}
	// The events are aliased ev, not e: evidenceScopeSQLOn's EXISTS names the
	// evidence table e and would shadow an events alias of that name.
	scopeCond, extra := evidenceScopeSQLOn(scope, "ev.evidence_id", len(args))
	args = append(args, extra...)
	args = append(args, limit)
	sqlRows, err := p.pool.Query(ctx, `
		SELECT `+eventColumnsOn("ev")+`
		FROM events ev
		WHERE ev.namespace_id=$1 AND ev.occurred_at >= $2 AND ev.occurred_at < $3
		  AND EXISTS (SELECT 1 FROM event_roles r WHERE r.event_id = ev.id AND r.entity_id = $4)`+
		notRetired("ev")+scopeCond+`
		ORDER BY ev.occurred_at, ev.id LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, err
	}
	var evs []domain.Event
	var ids []string
	for sqlRows.Next() {
		ev, err := scanTypedEvent(sqlRows)
		if err != nil {
			sqlRows.Close()
			return nil, err
		}
		evs = append(evs, ev)
		ids = append(ids, string(ev.ID))
	}
	sqlRows.Close()
	if err := sqlRows.Err(); err != nil {
		return nil, err
	}
	rolesBy, err := eventRoles(ctx, p.pool, ids)
	if err != nil {
		return nil, err
	}
	var out []domain.QueryRow
	for _, ev := range evs {
		ev.Roles = rolesBy[string(ev.ID)]
		roles := map[string]string{}
		for _, r := range ev.Roles {
			roles[r.Role] = r.EntityID
		}
		out = append(out, domain.QueryRow{
			"event_id": string(ev.ID), "event_type": ev.Type,
			"occurred_at": ev.OccurredAt, "roles": roles,
			"evidence_id": string(ev.EvidenceID),
		})
	}
	return out, nil
}

// traverse walks co-participation edges breadth-first, hop-capped and
// visited-set bounded — high-fan-out boundedness is a §17 contract row.
//
// ENFORCED via each via-event's evidence classification, and traversal DOES
// NOT CONTINUE through unauthorized events (ADR-0118 D2): a forbidden edge is
// not a bridge, otherwise reachability leaks what the rows would not.
func (p *PgQueryPlane) traverse(ctx context.Context, q domain.KnowledgeQuery, scope *domain.TagPredicate, limit int) ([]domain.QueryRow, error) {
	visited := map[string]bool{q.EntityID: true}
	frontier := []string{q.EntityID}
	var out []domain.QueryRow
	for hop := 1; hop <= q.Hops && len(frontier) > 0; hop++ {
		// ONE events read and ONE guard batch per hop for the whole frontier
		// (T-20261112-traverse-still-reads-events-once-per-frontier), walked in
		// frontier order, each entity's events oldest first — the order the
		// per-entity reads produced, so the answer is unchanged. Every event is
		// still judged on its own evidence by the walk's one node guard.
		byEntity, err := p.events.EventsForEntities(ctx, defaultNamespace, frontier, q.From, q.To)
		if err != nil {
			return nil, err
		}
		var batch []keyedNode[domain.Event]
		for _, entity := range frontier {
			for _, ev := range byEntity[entity] {
				batch = append(batch, keyedNode[domain.Event]{key: entity, node: ev})
			}
		}
		kept, err := guardNodes(ctx, p, scope, batch,
			func(x keyedNode[domain.Event]) []string { return eventEvidence(x.node) })
		if err != nil {
			return nil, err
		}
		var next []string
		for _, x := range kept {
			ev := x.node
			for _, r := range ev.Roles {
				if visited[r.EntityID] {
					continue
				}
				visited[r.EntityID] = true
				if len(out) == limit {
					return out, nil
				}
				out = append(out, domain.QueryRow{
					"from": x.key, "to": r.EntityID, "role": r.Role,
					"via_event": string(ev.ID), "event_type": ev.Type, "hop": hop,
				})
				next = append(next, r.EntityID)
			}
		}
		frontier = next
	}
	return out, nil
}

// evidenceRow — ENFORCED on the evidence row's own classification: an
// unauthorized inspection returns zero rows, never the classification list.
func (p *PgQueryPlane) evidenceRow(ctx context.Context, q domain.KnowledgeQuery, scope *domain.TagPredicate) ([]domain.QueryRow, error) {
	ev, err := p.evidence.Get(ctx, domain.EvidenceID(q.EvidenceID))
	if err != nil {
		return nil, err
	}
	// AllowsRow: this shape reads the evidence row itself, so its parties are in
	// hand and a party-scoped tag can be answered properly rather than denied for
	// want of the information (ADR-0121).
	if !scope.AllowsRow(ev.Classification, ev.Parties) {
		return nil, nil
	}
	return []domain.QueryRow{{
		"id": string(ev.ID), "source_id": ev.SourceID, "source_key": ev.SourceKey,
		"source_revision": ev.SourceRevision, "source_time": ev.SourceTime,
		"ingested_at": ev.IngestedAt, "content_hash": string(ev.ContentHash),
		"content_bytes": ev.ContentBytes, "classification": ev.Classification,
	}}, nil
}

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
	verbs := p.relations.ClosureVerbs(domain.ClosureIdentity)
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
			}, linkEvidence)
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
	kept, err := guardNodes(ctx, p, scope, []string{firstSeen},
		func(id string) []string { return []string{id} })
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
	return guardNodes(ctx, p, scope, links, linkEvidence)
}

func linkEvidence(l domain.Link) []string { return []string{string(l.EvidenceID)} }

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
		}, linkEvidence)
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
		}, linkEvidence)
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

	if scope.Bypass {
		// The subject's slice of the review inbox, filtered to the subject IN SQL
		// before the limit and paged past owner-fenced rows by pageVisible — so a
		// subject's candidates are never lost behind other subjects'
		// higher-confidence ones (they were, when this read the whole inbox to
		// `limit` and filtered here). Same membership and order as the inbox
		// (PgLinkStore openCandidate / candidateOrder): a confirmed candidate is
		// answered and does not come back.
		cands, err := pageVisible(ctx, p, scope, []string{subjectRef}, limit,
			func(keys []string, offset, n int) (map[string][]domain.Link, error) {
				return p.links.CandidatesForRefs(ctx, defaultNamespace, keys, offset, n)
			}, linkEvidence)
		if err != nil {
			return nil, err
		}
		for _, l := range cands[subjectRef] {
			out = append(out, linkRow(l, "candidate"))
		}
	}
	return gate(out), nil
}

// whyRef promotes a bare entity id to a typed ref and leaves an already-typed one
// alone. Both forms are accepted because the two callers differ: a console walks
// back from an event it is looking at, an agent from the entity it was asked
// about, and making either prefix by hand is a footgun with no upside.
func whyRef(id string) string {
	for _, prefix := range []string{
		domain.RefPrefixEntity, domain.RefPrefixEvent,
		domain.RefPrefixDecision, domain.RefPrefixEvidence,
	} {
		if strings.HasPrefix(id, prefix) {
			return id
		}
	}
	return domain.EntityRef(id)
}

// why walks lineage BACKWARD from a typed ref, hop by hop, labelling every hop
// with the mechanism that produced it.
//
// Two sources feed the walk and they are deliberately distinguishable on the
// wire. STORED lineage links (stored=true) were asserted by somebody, so they
// carry state, producer and evidence. LIVE shared-object hops (stored=false) are
// computed at read time from event_roles — two occurrences sharing a participant
// — and nobody asserted them; emitting those with a state would claim a review
// that never happened.
//
// ENFORCED per row: a link is filtered on its own evidence, an event on its
// event's evidence, and the walk DOES NOT CONTINUE through a row the caller
// cannot read (the traverse precedent — a forbidden edge is not a bridge, or
// reachability leaks what the rows themselves would not).
//
// Candidate lineage is withheld from non-bypass callers for entityOp's reason: an
// unreviewed proposal must not become a step in somebody's causal story.
func (p *PgQueryPlane) why(ctx context.Context, q domain.KnowledgeQuery,
	scope *domain.TagPredicate, limit int) ([]domain.QueryRow, error) {
	depth := q.Hops
	if depth <= 0 {
		depth = domain.ClosureDefaultDepth
	}
	if depth > domain.ClosureMaxDepth {
		return nil, domain.ClosureDepthRefusal(depth)
	}
	root := whyRef(q.EntityID)
	seen := map[string]bool{root: true}
	// Related rows anchored on an ENTITY are deduplicated by the PAIR rather
	// than by the target: one entity binds many events to each other, and a
	// target-keyed check would emit the first pair and swallow every other
	// relationship the same entity holds together.
	seenPair := map[string]bool{}
	seenHub := map[string]bool{}
	frontier := []string{root}
	var out []domain.QueryRow
	cp := correlationParamsFor(q)
	rootExpanded := false

	state := domain.LinkStateConfirmed
	if scope.Bypass {
		state = ""
	}
	for hop := 1; hop <= depth && len(frontier) > 0; hop++ {
		// The walk STANDS on every frontier ref before it expands it, and
		// standing on an event is reading it: its lineage and its related rows
		// (which carry its own role) are claims about that event. So each event
		// ref is decided on its OWN evidence first — the root as much as one
		// reached through a readable link at hop >= 2 — and one the caller may
		// not read (or that ADR-0147 retired) is not expanded at all. A hidden
		// event therefore answers exactly as an absent id does (ADR-0128 D12).
		readable, err := p.readableEventRefs(ctx, scope, frontier)
		if err != nil {
			return nil, err
		}
		var expand, eventRefs, entityRefs []string
		for _, ref := range frontier {
			switch {
			case strings.HasPrefix(ref, domain.RefPrefixEvent):
				if readable != nil && !readable[ref] {
					continue
				}
				eventRefs = append(eventRefs, ref)
			case strings.HasPrefix(ref, domain.RefPrefixEntity):
				entityRefs = append(entityRefs, ref)
			}
			expand = append(expand, ref)
		}
		if hop == 1 && len(expand) == 1 {
			rootExpanded = true
		}
		// ONE read per lane per hop for the whole frontier, each node guarded
		// (guardNodes) and paged until every ref has `limit` rows the caller may
		// read (filter before limit). Rows are emitted in frontier order, stored
		// links before related rows.
		var linksBy map[string][]domain.Link
		if p.links != nil {
			linksBy, err = pageVisible(ctx, p, scope, expand, limit,
				func(keys []string, offset, n int) (map[string][]domain.Link, error) {
					return p.links.LinksForRefs(ctx, defaultNamespace, keys, domain.LinkQuery{
						Family: domain.LinkFamilyLineage, State: state,
						IncludeIncoming: true, Limit: n, Offset: offset,
					})
				}, linkEvidence)
			if err != nil {
				return nil, err
			}
		}
		// ADR-0148 D1/D2: related rows only when the caller asked for them.
		var relBy, pairBy map[string][]relatedRow
		var hubsBy map[string][]hubFlag
		if q.IncludeCorrelation {
			if relBy, hubsBy, err = p.relatedFromEvents(ctx, scope, eventRefs, cp); err != nil {
				return nil, err
			}
			pairHubs := map[string][]hubFlag{}
			if pairBy, pairHubs, err = p.relatedFromEntities(ctx, scope, entityRefs, cp, limit); err != nil {
				return nil, err
			}
			for k, v := range pairHubs {
				if hubsBy == nil {
					hubsBy = map[string][]hubFlag{}
				}
				hubsBy[k] = append(hubsBy[k], v...)
			}
		}

		var next []string
		for _, ref := range expand {
			for _, l := range linksBy[ref] {
				other := l.ToRef
				if other == ref {
					other = l.FromRef
				}
				if other == ref || seen[other] {
					continue
				}
				seen[other] = true
				if len(out) >= limit {
					return out, nil
				}
				row := linkRow(l, "hop")
				row["hop"] = hop
				row["stored"] = true
				out = append(out, row)
				next = append(next, other)
			}
			switch {
			case strings.HasPrefix(ref, domain.RefPrefixEvent):
				for _, h := range relBy[ref] {
					if seen[h.toRef] {
						continue
					}
					seen[h.toRef] = true
					if len(out) >= limit {
						return out, nil
					}
					out = append(out, h.row(hop))
					next = append(next, h.toRef)
				}
			case strings.HasPrefix(ref, domain.RefPrefixEntity):
				// why(entity:X) with include_correlation: the pairs of events X
				// took part in (D-W5-6). The rows run between the EVENTS; X is
				// what they share and rides along as shared_entity_id.
				for _, h := range pairBy[ref] {
					key := h.fromRef + "\x00" + h.toRef
					if seenPair[key] {
						continue
					}
					seenPair[key] = true
					if len(out) >= limit {
						return out, nil
					}
					out = append(out, h.row(hop))
					// BOTH ends enter the walk: neither event has had its own
					// lineage or related rows looked at yet.
					for _, end := range []string{h.fromRef, h.toRef} {
						if !seen[end] {
							seen[end] = true
							next = append(next, end)
						}
					}
				}
			}
			// D5: a participant over the hub threshold was not followed; say
			// so once, where a reader can see why an obvious relation is absent.
			for _, hb := range hubsBy[ref] {
				if seenHub[hb.entity] {
					continue
				}
				seenHub[hb.entity] = true
				if len(out) >= limit {
					return out, nil
				}
				out = append(out, domain.QueryRow{
					"row": "flagged", "ref": domain.EntityRef(hb.entity), "via_ref": ref,
					"guard": "why_hub_participant", "limit": domain.CorrelationHubThreshold,
					"events": hb.events,
				})
			}
		}
		frontier = next
	}
	if len(out) == 0 && !q.IncludeCorrelation && rootExpanded {
		return p.whyHint(ctx, scope, root, cp, limit)
	}
	return out, nil
}

// whyHint is ADR-0148's answer to an empty flag-off walk: if the root has
// related events the caller could see with include_correlation, say how many
// rather than answer with silence (owner decision 2026-11-12). It is computed by
// the very lane the flag would run, at hop 1, so N is exactly the number of
// related rows (event root) or distinct related events (entity root) the caller
// would get — every one of them caller-visible, fenced on its own evidence. The
// root has already passed readableEventRefs, so a hidden root never reaches
// here and still answers as an absent id.
//
// Wire shape: one row {row:"hint", hint:"related_available", related_count:N,
// flag:"include_correlation", text:"no stored lineage; N related events
// available with include_correlation"}. A row rather than a new response field:
// the rows are the contract's extensible part (ADR-0148 §4), and a client that
// does not know the shape still renders it as a row.
func (p *PgQueryPlane) whyHint(ctx context.Context, scope *domain.TagPredicate,
	root string, cp correlationParams, limit int) ([]domain.QueryRow, error) {
	n := 0
	switch {
	case strings.HasPrefix(root, domain.RefPrefixEvent):
		rel, _, err := p.relatedFromEvents(ctx, scope, []string{root}, cp)
		if err != nil {
			return nil, err
		}
		n = len(rel[root])
	case strings.HasPrefix(root, domain.RefPrefixEntity):
		pairs, _, err := p.relatedFromEntities(ctx, scope, []string{root}, cp, limit)
		if err != nil {
			return nil, err
		}
		distinct := map[string]bool{}
		for _, h := range pairs[root] {
			distinct[h.fromRef], distinct[h.toRef] = true, true
		}
		n = len(distinct)
	}
	if n == 0 {
		return nil, nil
	}
	return []domain.QueryRow{{
		"row": "hint", "hint": "related_available", "related_count": n,
		"flag": "include_correlation",
		"text": fmt.Sprintf("no stored lineage; %d related events available with include_correlation", n),
	}}, nil
}

// ─── Related rows (ADR-0148): windowed, hub-damped, ranked co-occurrence ─────
//
// A related row says two occurrences share a participant and happened close
// together. It is computed at read time from event_roles, never stored, and it
// is NOT a cause: `why` returns it only under include_correlation, labelled
// row:"related" with a score, apart from the stored lineage hops.
//
// Parameters (accepted by the owner 2026-11-12, ADR-0148 §3):
//   - window W: the related event lies in [anchor - W, anchor] (default 72 h);
//   - hub damping: a shared participant x weighs w(x) = 1/ln(e + df(x)), where
//     df(x) counts the CALLER-VISIBLE, non-retired events x takes part in within
//     the window; x with df > CorrelationHubThreshold is not followed and is
//     reported as a flagged row;
//   - score = Σ_x w(x) · exp(-Δt/τ), τ = W/3, ordered score DESC, then
//     occurred_at DESC, then id;
//   - at most CorrelationMaxPerEvent related rows per anchoring event;
//   - roles, when given, must match the participant's role in BOTH events.
//
// FENCE. df is a COUNT, and a count over rows the caller cannot read would
// leak them (ADR-0128 D12), so df and the candidate set are fenced in SQL with
// evidenceScopeSQLOn — the one SQL rendering of the row test, the aggregate
// shape's precedent ("summed rows cannot be post-filtered"). Every emitted row
// then still passes guardNodes on its own events' evidence, the walk's one node
// guard. Retired rows (ADR-0147) are skipped everywhere a row is counted.

// relatedRow is one related (computed, unstored) row between two occurrences.
type relatedRow struct {
	fromRef, toRef         string
	fromEvidence, evidence string
	eventType              string
	occurredAt             time.Time
	sharedEntity           string
	sharedRole             string
	score                  float64
}

func (h relatedRow) row(hop int) domain.QueryRow {
	return domain.QueryRow{
		"row": "related", "from_ref": h.fromRef, "to_ref": h.toRef,
		// The verb is the honest description of what co-occurrence
		// establishes: order plus overlap, never cause.
		"relation":  domain.RelationPrecededAndSharesEntities,
		"mechanism": domain.LinkMechanismSharedObject,
		"state":     "", "stored": false,
		"evidence_id": h.evidence, "event_type": h.eventType,
		"occurred_at": h.occurredAt, "shared_entity_id": h.sharedEntity,
		"shared_role": h.sharedRole, "hop": hop,
		"score": math.Round(h.score*1000) / 1000,
	}
}

// hubFlag is a shared participant the lane refused to follow.
type hubFlag struct {
	entity string
	events int
}

type correlationParams struct {
	window time.Duration
	roles  []string
}

func correlationParamsFor(q domain.KnowledgeQuery) correlationParams {
	roles := q.CorrelationRoles
	if roles == nil {
		roles = []string{} // bound as '{}', never NULL: cardinality(NULL) is NULL
	}
	return correlationParams{window: q.CorrelationWindowOrDefault(), roles: roles}
}

// hubWeight is w(x) = 1/ln(e + df).
func hubWeight(df int) float64 { return 1 / math.Log(math.E+float64(df)) }

// recencyDecay is exp(-Δt/τ) with τ = window/3.
func recencyDecay(dt, window time.Duration) float64 {
	tau := window.Seconds() / 3
	if tau <= 0 || dt <= 0 {
		return 1
	}
	return math.Exp(-dt.Seconds() / tau)
}

// relatedFromEvents finds, for each anchoring event of a frontier (already
// decided readable by why's gate) in one statement, the earlier events that
// share a participant with it inside the window, scored and capped.
func (p *PgQueryPlane) relatedFromEvents(ctx context.Context, scope *domain.TagPredicate,
	eventRefs []string, cp correlationParams) (map[string][]relatedRow, map[string][]hubFlag, error) {
	var ids []string
	for _, ref := range eventRefs {
		if id, ok := strings.CutPrefix(ref, domain.RefPrefixEvent); ok && id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil, nil
	}
	args := []any{ids, defaultNamespace, cp.window.Seconds(), cp.roles, domain.CorrelationHubThreshold}
	// The counted events are aliased oe, not e: evidenceScopeSQLOn's EXISTS
	// names the evidence table e, and would shadow an events alias of that name.
	dfFence, dfArgs := evidenceScopeSQLOn(scope, "oe.evidence_id", len(args))
	args = append(args, dfArgs...)
	candFence, candArgs := evidenceScopeSQLOn(scope, "e2.evidence_id", len(args))
	args = append(args, candArgs...)
	rows, err := p.pool.Query(ctx, `
		WITH a AS (
			SELECT e1.id AS aid, e1.occurred_at AS at1, e1.evidence_id AS aev,
			       r1.entity_id AS x, r1.role AS role1
			FROM events e1 JOIN event_roles r1 ON r1.event_id = e1.id
			WHERE e1.id = ANY($1) AND e1.namespace_id = $2`+notRetired("e1")+`
			  AND (cardinality($4::text[]) = 0 OR r1.role = ANY($4::text[]))
		), d AS (
			SELECT a.aid, a.x, count(DISTINCT oe.id) AS df
			FROM (SELECT DISTINCT aid, at1, x FROM a) a
			JOIN event_roles r ON r.entity_id = a.x
			JOIN events oe ON oe.id = r.event_id AND oe.namespace_id = $2
			 AND oe.occurred_at >= a.at1 - make_interval(secs => $3::float8)
			 AND oe.occurred_at <= a.at1`+notRetired("oe")+dfFence+`
			GROUP BY a.aid, a.x
		)
		SELECT a.aid, a.at1, a.aev, a.x, a.role1, d.df,
		       c.id, c.event_type, c.occurred_at, c.evidence_id
		FROM a JOIN d ON d.aid = a.aid AND d.x = a.x
		LEFT JOIN LATERAL (
			SELECT DISTINCT e2.id, e2.event_type, e2.occurred_at, e2.evidence_id
			FROM event_roles r2
			JOIN events e2 ON e2.id = r2.event_id AND e2.namespace_id = $2
			WHERE d.df <= $5 AND r2.entity_id = a.x AND r2.event_id <> a.aid
			  AND (cardinality($4::text[]) = 0 OR r2.role = ANY($4::text[]))
			  AND e2.occurred_at >= a.at1 - make_interval(secs => $3::float8)
			  AND e2.occurred_at <= a.at1`+notRetired("e2")+candFence+`
		) c ON true
		ORDER BY a.aid, a.x, a.role1, c.id`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	type shared struct {
		df   int
		role string
	}
	type cand struct {
		anchor, anchorEv string
		at1              time.Time
		id, typ, ev      string
		at               time.Time
		xs               map[string]shared
	}
	cands := map[string]*cand{} // anchor \x00 e2
	var order []string
	hubs := map[string][]hubFlag{}
	hubSeen := map[string]bool{}
	for rows.Next() {
		var aid, x, role1 string
		var aev, cid, ctyp, cev *string
		var at1 time.Time
		var cat *time.Time
		var df int
		if err := rows.Scan(&aid, &at1, &aev, &x, &role1, &df, &cid, &ctyp, &cat, &cev); err != nil {
			return nil, nil, err
		}
		anchor := domain.RefPrefixEvent + aid
		if df > domain.CorrelationHubThreshold {
			if !hubSeen[anchor+"\x00"+x] {
				hubSeen[anchor+"\x00"+x] = true
				hubs[anchor] = append(hubs[anchor], hubFlag{entity: x, events: df})
			}
			continue
		}
		if cid == nil {
			continue
		}
		key := anchor + "\x00" + *cid
		c := cands[key]
		if c == nil {
			c = &cand{anchor: anchor, at1: at1, id: *cid, at: *cat, xs: map[string]shared{}}
			if aev != nil {
				c.anchorEv = *aev
			}
			if ctyp != nil {
				c.typ = *ctyp
			}
			if cev != nil {
				c.ev = *cev
			}
			cands[key] = c
			order = append(order, key)
		}
		if s, ok := c.xs[x]; !ok || role1 < s.role {
			c.xs[x] = shared{df: df, role: role1}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	var all []relatedRow
	for _, key := range order {
		c := cands[key]
		decay := recencyDecay(c.at1.Sub(c.at), cp.window)
		var score float64
		best, bestW := "", -1.0
		for x, s := range c.xs {
			w := hubWeight(s.df)
			score += w * decay
			if w > bestW || (w == bestW && x < best) {
				best, bestW = x, w
			}
		}
		all = append(all, relatedRow{
			fromRef: c.anchor, toRef: domain.RefPrefixEvent + c.id,
			fromEvidence: c.anchorEv, evidence: c.ev, eventType: c.typ, occurredAt: c.at,
			sharedEntity: best, sharedRole: c.xs[best].role, score: score,
		})
	}
	// The near end was decided by why's gate; the far end is decided here, on
	// its own evidence, by the walk's one node guard.
	kept, err := guardNodes(ctx, p, scope, all, func(h relatedRow) []string { return []string{h.evidence} })
	if err != nil {
		return nil, nil, err
	}
	return capRelated(kept, func(h relatedRow) string { return h.fromRef },
		domain.CorrelationMaxPerEvent), hubs, nil
}

// relatedFromEntities pairs, for each entity of a frontier in one statement,
// the occurrences the entity took part in that lie within the window of each
// other (D-W5-6, bounded by ADR-0148). The later event is the anchor: it keeps
// at most CorrelationMaxPerEvent partners, and each entity keeps at most
// `limit` pairs, best score first.
func (p *PgQueryPlane) relatedFromEntities(ctx context.Context, scope *domain.TagPredicate,
	entityRefs []string, cp correlationParams, limit int) (map[string][]relatedRow, map[string][]hubFlag, error) {
	var ids []string
	for _, ref := range entityRefs {
		if id, ok := strings.CutPrefix(ref, domain.RefPrefixEntity); ok && id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil, nil
	}
	args := []any{ids, defaultNamespace, cp.window.Seconds(), cp.roles,
		domain.CorrelationHubThreshold, domain.CorrelationMaxPerEvent, limit}
	fence, fenceArgs := evidenceScopeSQLOn(scope, "oe.evidence_id", len(args)) // oe: see relatedFromEvents
	args = append(args, fenceArgs...)
	// df for a pair's anchor is the windowed count of the entity's visible
	// events up to that anchor: a RANGE window over occurred_at.
	rows, err := p.pool.Query(ctx, `
		WITH ev AS (
			SELECT r.entity_id AS x, oe.id, oe.event_type, oe.occurred_at, oe.evidence_id,
			       array_agg(DISTINCT r.role ORDER BY r.role) AS roles
			FROM event_roles r
			JOIN events oe ON oe.id = r.event_id AND oe.namespace_id = $2
			WHERE r.entity_id = ANY($1)`+notRetired("oe")+fence+`
			GROUP BY r.entity_id, oe.id, oe.event_type, oe.occurred_at, oe.evidence_id
		), w AS (
			SELECT ev.*, count(*) OVER (PARTITION BY x ORDER BY occurred_at
			       RANGE BETWEEN make_interval(secs => $3::float8) PRECEDING AND CURRENT ROW) AS df
			FROM ev
		), hubs AS (
			SELECT x, max(df) AS df FROM w GROUP BY x HAVING max(df) > $5
		), pairs AS (
			SELECT w1.x, w1.id AS from_id, w1.evidence_id AS from_ev, w1.occurred_at AS at1,
			       w1.df, w1.roles AS roles1, c.id AS to_id, c.event_type, c.occurred_at,
			       c.evidence_id AS to_ev
			FROM w w1
			JOIN LATERAL (
				SELECT w2.id, w2.event_type, w2.occurred_at, w2.evidence_id FROM w w2
				WHERE w2.x = w1.x
				  AND (w2.occurred_at < w1.occurred_at
				       OR (w2.occurred_at = w1.occurred_at AND w2.id < w1.id))
				  AND w2.occurred_at >= w1.occurred_at - make_interval(secs => $3::float8)
				  AND (cardinality($4::text[]) = 0 OR w2.roles && $4::text[])
				ORDER BY w2.occurred_at DESC, w2.id DESC
				LIMIT $6) c ON true
			WHERE w1.df <= $5 AND (cardinality($4::text[]) = 0 OR w1.roles && $4::text[])
		), ranked AS (
			SELECT pairs.*, (1.0 / ln(exp(1.0) + df)) *
			       exp(-extract(epoch FROM (at1 - occurred_at)) / ($3::float8 / 3.0)) AS score
			FROM pairs
		), top AS (
			SELECT ranked.*, row_number() OVER (PARTITION BY x
			       ORDER BY score DESC, occurred_at DESC, from_id, to_id) AS rn
			FROM ranked
		)
		SELECT 'pair', x, from_id, from_ev, to_id, event_type, occurred_at, to_ev,
		       roles1, score::float8, df::int
		FROM top WHERE rn <= $7
		UNION ALL
		SELECT 'hub', x, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, df::int FROM hubs
		ORDER BY 1, 2, 10 DESC NULLS LAST, 7 DESC NULLS LAST, 3, 5`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := map[string][]relatedRow{}
	hubs := map[string][]hubFlag{}
	var all []relatedRow
	for rows.Next() {
		var kind, x string
		var fromID, fromEv, toID, typ, toEv *string
		var at *time.Time
		var roles []string
		var score *float64
		var df int
		if err := rows.Scan(&kind, &x, &fromID, &fromEv, &toID, &typ, &at, &toEv, &roles, &score, &df); err != nil {
			return nil, nil, err
		}
		ref := domain.RefPrefixEntity + x
		if kind == "hub" {
			hubs[ref] = append(hubs[ref], hubFlag{entity: x, events: df})
			continue
		}
		h := relatedRow{
			fromRef: domain.RefPrefixEvent + *fromID, toRef: domain.RefPrefixEvent + *toID,
			occurredAt: *at, sharedEntity: x, sharedRole: firstRole(roles, cp.roles), score: *score,
		}
		if fromEv != nil {
			h.fromEvidence = *fromEv
		}
		if toEv != nil {
			h.evidence = *toEv
		}
		if typ != nil {
			h.eventType = *typ
		}
		all = append(all, h)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	// A pair is a claim about BOTH events, so a caller who may not read one of
	// them may not be told the other stands next to it.
	kept, err := guardNodes(ctx, p, scope, all, func(h relatedRow) []string {
		return []string{h.fromEvidence, h.evidence}
	})
	if err != nil {
		return nil, nil, err
	}
	for _, h := range kept {
		ref := domain.EntityRef(h.sharedEntity)
		out[ref] = append(out[ref], h)
	}
	return out, hubs, nil
}

// firstRole is the role a row reports for the shared participant: the first,
// in order, that the role filter admits (any role when there is no filter).
func firstRole(roles, filter []string) string {
	for _, r := range roles {
		if len(filter) == 0 {
			return r
		}
		for _, f := range filter {
			if r == f {
				return r
			}
		}
	}
	return ""
}

// capRelated orders related rows by score DESC, occurred_at DESC, target id,
// per anchor, and keeps at most `per` for each anchor.
func capRelated(rows []relatedRow, anchorOf func(relatedRow) string, per int) map[string][]relatedRow {
	byAnchor := map[string][]relatedRow{}
	for _, h := range rows {
		byAnchor[anchorOf(h)] = append(byAnchor[anchorOf(h)], h)
	}
	for k, hs := range byAnchor {
		sort.SliceStable(hs, func(i, j int) bool {
			if hs[i].score != hs[j].score {
				return hs[i].score > hs[j].score
			}
			if !hs[i].occurredAt.Equal(hs[j].occurredAt) {
				return hs[i].occurredAt.After(hs[j].occurredAt)
			}
			return hs[i].toRef < hs[j].toRef
		})
		if len(hs) > per {
			hs = hs[:per]
		}
		byAnchor[k] = hs
	}
	return byAnchor
}

// readableEventRefs decides which event: refs in refs the caller may stand on,
// each on that event's OWN evidence, through filterEvents — the same
// evidenceTags/AllowsRow test (owner fence included) every other event read
// applies, not a second rendering of the policy. Fail closed: an event that does
// not exist, has no evidence, or whose evidence does not resolve is unreadable,
// which is what makes a hidden event indistinguishable from an absent id.
//
// A nil map means "no filtering" (ScopeMaintenance, the one Unfiltered
// predicate), mirroring filterEvents' own short-circuit; non-event refs are
// never in the map and the caller does not consult it for them.
func (p *PgQueryPlane) readableEventRefs(ctx context.Context, scope *domain.TagPredicate,
	refs []string) (map[string]bool, error) {
	if scope.Unfiltered() {
		return nil, nil
	}
	readable := map[string]bool{}
	var ids []string
	for _, ref := range refs {
		if id, ok := strings.CutPrefix(ref, domain.RefPrefixEvent); ok && id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return readable, nil
	}
	rows, err := p.pool.Query(ctx,
		// Retired rows (ADR-0147) do not count: a retired event is not stood on.
		`SELECT id, evidence_id FROM events WHERE id = ANY($1) AND namespace_id = $2`+notRetired("events"),
		ids, defaultNamespace)
	if err != nil {
		return nil, err
	}
	var evs []domain.Event
	for rows.Next() {
		var id string
		var evID *string
		if err := rows.Scan(&id, &evID); err != nil {
			rows.Close()
			return nil, err
		}
		ev := domain.Event{ID: domain.EventID(id)}
		if evID != nil {
			ev.EvidenceID = domain.EvidenceID(*evID)
		}
		evs = append(evs, ev)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	kept, err := p.filterEvents(ctx, scope, evs)
	if err != nil {
		return nil, err
	}
	for _, ev := range kept {
		readable[domain.RefPrefixEvent+string(ev.ID)] = true
	}
	return readable, nil
}

// ─── The node guard: one scope decision for every graph walk ─────────────────
//
// why, traverse and the identity closure each used to fence their nodes with a
// hand-rolled copy of the same evidenceTags/AllowsRow loop, and the copies
// drifted: one lane checked only the far end of a hop, so why(event:<hidden>)
// was an existence oracle (T-20261112-query-plane-why-never-fences-the-root-
// event-or). guardNodes is now the only loop. Every node a walk emits or stands
// on — a link, an event, a co-occurrence hop, an entity's minting evidence —
// passes through it, directly or through filterLinks/filterEvents, which are
// one-line wrappers over it.

// guardNodes keeps the nodes the scope may read, resolving every node's
// evidence in one batch (evidenceTags) and judging each with AllowsRow — the
// same test point/history/events apply in SQL (evidenceRowConds is CheckRow's
// mirror), owner fence and party scoping included (ADR-0121), never a second
// rendering of the policy (AGENTS §2.5).
//
// A node names the evidence it stands on, and is admitted only if it names at
// least one and the scope may read EVERY one (a hop between two events is a
// claim about both). Fail closed: an empty or unresolvable evidence id hides
// the node, for a plain Bypass too — only ScopeMaintenance (Unfiltered) admits
// everything, as everywhere else on this plane. Order is preserved.
func guardNodes[T any](ctx context.Context, p *PgQueryPlane, scope *domain.TagPredicate,
	nodes []T, evidenceOf func(T) []string) ([]T, error) {
	if scope.Unfiltered() || len(nodes) == 0 {
		return nodes, nil
	}
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, evidenceOf(n)...)
	}
	tags, err := p.evidenceTags(ctx, ids)
	if err != nil {
		return nil, err
	}
	kept := nodes[:0:0]
	for _, n := range nodes {
		ev := evidenceOf(n)
		ok := len(ev) > 0
		for _, id := range ev {
			t, resolvable := tags[id]
			if !resolvable || !scope.AllowsRow(t.tags, t.parties) {
				ok = false
				break
			}
		}
		if ok {
			kept = append(kept, n)
		}
	}
	return kept, nil
}

// walkMaxPages bounds filter-before-limit paging in the walk shapes: a lane
// reads at most this many pages of `want` rows per key before it settles for
// what it has. The first page is exactly the read the walk always made, so a
// caller whose scope hides nothing never reads a second one; the bound only
// caps how far a narrow caller's lane digs past rows it may not see.
const walkMaxPages = 8

// keyedNode is one node of a frontier read, remembered with the key it was read
// for so a whole page can be guarded in one batch.
type keyedNode[T any] struct {
	key  string
	node T
}

// pageVisible is filter-before-limit for one lane of a walk: for every key it
// reads pages of `want` rows (fetch: one statement for all keys still short),
// guards them (guardNodes), and keeps going for a key until it holds `want`
// nodes the caller may read, its rows run out (a short page), or walkMaxPages
// pages were read. Without it a page the store cut BEFORE the scope was known
// gave a narrow caller a short or empty answer while visible rows existed.
func pageVisible[T any](ctx context.Context, p *PgQueryPlane, scope *domain.TagPredicate,
	keys []string, want int,
	fetch func(keys []string, offset, n int) (map[string][]T, error),
	evidenceOf func(T) []string) (map[string][]T, error) {
	out := map[string][]T{}
	if want <= 0 {
		return out, nil
	}
	seen := map[string]bool{}
	var pending []string
	for _, k := range keys {
		if !seen[k] {
			seen[k] = true
			pending = append(pending, k)
		}
	}
	for page := 0; page < walkMaxPages && len(pending) > 0; page++ {
		raw, err := fetch(pending, page*want, want)
		if err != nil {
			return nil, err
		}
		var batch []keyedNode[T]
		for _, k := range pending {
			for _, n := range raw[k] {
				batch = append(batch, keyedNode[T]{key: k, node: n})
			}
		}
		kept, err := guardNodes(ctx, p, scope, batch,
			func(x keyedNode[T]) []string { return evidenceOf(x.node) })
		if err != nil {
			return nil, err
		}
		for _, x := range kept {
			if len(out[x.key]) < want {
				out[x.key] = append(out[x.key], x.node)
			}
		}
		var still []string
		for _, k := range pending {
			if len(raw[k]) >= want && len(out[k]) < want {
				still = append(still, k)
			}
		}
		pending = still
	}
	return out, nil
}
