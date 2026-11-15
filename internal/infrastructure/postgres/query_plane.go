package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz/sqlfence"
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
	relations domain.RelationSource
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
	relations domain.RelationSource) *PgQueryPlane {
	if relations == nil {
		relations = (*domain.RelationRegistry)(nil)
	}
	return &PgQueryPlane{
		pool: pool, events: events, knowledge: knowledge, evidence: evidence,
		links: links, entities: entities, relations: relations,
	}
}

var _ domain.QueryPlane = (*PgQueryPlane)(nil)

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
	if scope.Unfiltered() && !q.IncludeRetired {
		ob, err := p.events.PointLookup(ctx, defaultNamespace, q.EntityID, q.Predicate)
		if err != nil || ob == nil {
			return nil, err
		}
		return []domain.QueryRow{obsRow(ob)}, nil
	}
	args := []any{defaultNamespace, q.EntityID, q.Predicate}
	scopeCond, extra := sqlfence.EvidenceExists(scope, "observations.evidence_id", len(args))
	args = append(args, extra...)
	ob, err := scanObservation(p.pool.QueryRow(ctx, `
		SELECT `+observationColumns+` FROM observations
		WHERE namespace_id=$1 AND entity_id=$2 AND predicate=$3`+scopeCond+retiredCond(q, "observations")+`
		ORDER BY occurred_at DESC, id DESC LIMIT 1`, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows := []domain.QueryRow{obsRow(ob)}
	return rows, p.markRetired(ctx, q, []string{ob.SourceRef}, rows)
}

// retiredCond is the retirement predicate of a typed-row read: notRetired, or
// nothing when the operator asked for retired rows too (IncludeRetired,
// ENG-1045). The scope fence is never part of it — retired rows stay fenced.
func retiredCond(q domain.KnowledgeQuery, alias string) string {
	if q.IncludeRetired {
		return ""
	}
	return notRetired(alias)
}

// markRetired stamps the rows whose source_ref has a LIVE retirement with
// retired:true, retired_reason, retired_by and retired_at, in one statement
// (refs[i] is rows[i]'s source_ref). A no-op unless IncludeRetired: without the
// flag no retired row can be in the answer.
func (p *PgQueryPlane) markRetired(ctx context.Context, q domain.KnowledgeQuery, refs []string, rows []domain.QueryRow) error {
	if !q.IncludeRetired || len(rows) == 0 {
		return nil
	}
	type retirement struct {
		reason, by string
		at         time.Time
	}
	sqlRows, err := p.pool.Query(ctx, `
		SELECT source_ref, reason, retired_by, retired_at FROM typed_row_retirements
		WHERE namespace_id = $1 AND source_ref = ANY($2::text[]) AND reinstated_at IS NULL`,
		defaultNamespace, refs)
	if err != nil {
		return fmt.Errorf("query: retirements: %w", err)
	}
	defer sqlRows.Close()
	live := map[string]retirement{}
	for sqlRows.Next() {
		var ref string
		var r retirement
		if err := sqlRows.Scan(&ref, &r.reason, &r.by, &r.at); err != nil {
			return err
		}
		live[ref] = r
	}
	if err := sqlRows.Err(); err != nil {
		return err
	}
	for i, row := range rows {
		r, ok := live[refs[i]]
		if refs[i] == "" || !ok {
			continue
		}
		row["retired"] = true
		row["retired_reason"] = r.reason
		row["retired_by"] = r.by
		row["retired_at"] = r.at
	}
	return nil
}

// history — ENFORCED in SQL via each observation's evidence classification
// (sqlfence.EvidenceExists, point's rendering), and the LIMIT applies AFTER the fence:
// the page is the first `limit` rows the caller may read, oldest first, and
// the range is never loaded whole only to be cut in Go
// (T-20261112-substrate-query-shapes-apply-limit-before-the). Retired rows
// (ADR-0147) are excluded as in EventStore.History.
func (p *PgQueryPlane) history(ctx context.Context, q domain.KnowledgeQuery, scope *domain.TagPredicate, limit int) ([]domain.QueryRow, error) {
	args := []any{defaultNamespace, q.EntityID, q.Predicate, q.From.UTC(), q.To.UTC()}
	scopeCond, extra := sqlfence.EvidenceExists(scope, "observations.evidence_id", len(args))
	args = append(args, extra...)
	args = append(args, limit)
	sqlRows, err := p.pool.Query(ctx, `
		SELECT `+observationColumns+` FROM observations
		WHERE namespace_id=$1 AND entity_id=$2 AND predicate=$3
		  AND occurred_at >= $4 AND occurred_at < $5`+scopeCond+retiredCond(q, "observations")+`
		ORDER BY occurred_at, id LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, err
	}
	rows := []domain.QueryRow{}
	var refs []string
	for sqlRows.Next() {
		ob, err := scanObservation(sqlRows)
		if err != nil {
			sqlRows.Close()
			return nil, err
		}
		rows = append(rows, obsRow(ob))
		refs = append(refs, ob.SourceRef)
	}
	sqlRows.Close()
	if err := sqlRows.Err(); err != nil {
		return nil, err
	}
	return rows, p.markRetired(ctx, q, refs, rows)
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
// → sqlfence.TextArray, the TagPredicate.Allows mirror the catalog uses; owner
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

// current — ENFORCED in SQL via each resolution's backing item classification
// (sqlArgs.item → sqlfence.TextArray, the TagPredicate.Allows mirror; owner
// fence under Bypass too). Policy defaults to latest_assertion; Actor keeps that
// actor's own rows only.
//
// The entity, actor, fence and LIMIT are all in the query (ENG-833): this used
// to load every current resolution of the kind through
// KnowledgeStore.CurrentResolutions and filter and count in Go, so a narrow
// question about one entity paid for the whole kind, and the scope check was an
// inline scope.Allows copy in the adapter (AGENTS.md §2.5).
func (p *PgQueryPlane) current(ctx context.Context, q domain.KnowledgeQuery, scope *domain.TagPredicate, limit int) ([]domain.QueryRow, error) {
	policy := q.PolicyOrDefault()
	if err := p.refuseActorOnCrossActorPolicy(ctx, q, policy); err != nil {
		return nil, err
	}
	var entities []string
	if q.EntityID != "" {
		entities = []string{q.EntityID}
	}
	res, err := p.currentResolutions(ctx, q.ItemKind, policy, entities, q.Actor, scope, limit)
	if err != nil {
		return nil, err
	}
	out := make([]domain.QueryRow, 0, len(res))
	for _, r := range res {
		out = append(out, domain.QueryRow{
			"entity_id": r.EntityID, "actor": r.Actor, "policy": r.Policy,
			"reason_code": r.ReasonCode, "values": itemValues(r.Item),
		})
	}
	return out, nil
}

// currentResolutions reads the CURRENT resolutions of one kind under one policy
// that scope may read, hydrated with their items, in resolution order. entities
// (when non-empty) and actor (when non-empty) narrow in SQL; limit <= 0 means no
// LIMIT. A resolution whose item the fence rejects — or that has no item — is
// not returned for any filtered scope.
func (p *PgQueryPlane) currentResolutions(ctx context.Context, kind, policy string, entities []string,
	actor string, scope *domain.TagPredicate, limit int) ([]domain.Resolution, error) {
	a := &sqlArgs{}
	where := currentResolutionsWhere(a, kind, policy, entities, actor, scope)
	sqlText := `
		SELECT r.entity_id, r.actor, r.item_id, r.reason_code, r.system_from
		FROM resolutions r LEFT JOIN knowledge_items ki ON ki.id = r.item_id
		WHERE ` + where + `
		ORDER BY r.id`
	if limit > 0 {
		sqlText += ` LIMIT ` + a.add(limit)
	}
	rows, err := p.pool.Query(ctx, sqlText, a.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Resolution
	var ids []string
	for rows.Next() {
		var r domain.Resolution
		var itemID string
		if err := rows.Scan(&r.EntityID, &r.Actor, &itemID, &r.ReasonCode, &r.SystemFrom); err != nil {
			return nil, err
		}
		r.NamespaceID, r.Kind, r.Policy = defaultNamespace, kind, policy
		r.Item = &domain.KnowledgeItem{ID: domain.KnowledgeItemID(itemID)}
		out = append(out, r)
		ids = append(ids, itemID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if len(ids) == 0 {
		return out, nil
	}
	items, err := loadItems(ctx, p.pool, `WHERE ki.id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("current: load items: %w", err)
	}
	byID := make(map[domain.KnowledgeItemID]*domain.KnowledgeItem, len(items))
	for i := range items {
		byID[items[i].ID] = &items[i]
	}
	kept := out[:0]
	for _, r := range out {
		// An item erased between the two reads is dropped rather than answered
		// with no values: the fence admitted the row on that item's labels.
		if it, ok := byID[r.Item.ID]; ok {
			r.Item = it
			kept = append(kept, r)
		}
	}
	return kept, nil
}

// currentResolutionsWhere renders the WHERE of a current-resolutions read: the
// kind, the policy, the open (system_to IS NULL) non-negated rows, the optional
// entity and actor narrowing, and the item fence (sqlArgs.item, alias "ki").
// Pure, so its shape is unit-tested without a database.
func currentResolutionsWhere(a *sqlArgs, kind, policy string, entities []string, actor string,
	scope *domain.TagPredicate) string {
	where := `r.namespace_id=` + a.add(defaultNamespace) + ` AND r.kind=` + a.add(kind) +
		` AND r.policy=` + a.add(policy) + ` AND r.system_to IS NULL AND r.item_id IS NOT NULL`
	switch len(entities) {
	case 0:
	case 1:
		where += ` AND r.entity_id = ` + a.add(entities[0])
	default:
		where += ` AND r.entity_id = ANY(` + a.add(entities) + `)`
	}
	if actor != "" {
		where += ` AND r.actor = ` + a.add(actor)
	}
	return where + a.item(scope, "ki")
}

// contradictionKeysSQL selects the (entity, predicate) pairs on which the
// visible current resolutions disagree: two or more distinct values among the
// sides scope may read. The fence applies to every side BEFORE grouping, so a
// disagreement is never reported because of a side the caller cannot read. A
// value is compared on its typed columns, which is equality on the value
// itemValues renders. Pure, so its shape is unit-tested without a database.
func contradictionKeysSQL(a *sqlArgs, kind, policy, entity, predicate, actor string,
	scope *domain.TagPredicate, limit int) string {
	var entities []string
	if entity != "" {
		entities = []string{entity}
	}
	where := currentResolutionsWhere(a, kind, policy, entities, "", scope)
	sides := `
		SELECT r.entity_id, r.actor, sv.predicate,
			concat_ws('|', sv.value_type, sv.value_text, sv.value_number::text,
				sv.value_date::text, sv.value_entity_id) AS val
		FROM resolutions r
		LEFT JOIN knowledge_items ki ON ki.id = r.item_id
		JOIN statement_values sv ON sv.item_id = r.item_id
		WHERE ` + where
	if predicate != "" {
		sides += ` AND sv.predicate = ` + a.add(predicate)
	}
	having := `count(DISTINCT val) >= 2`
	if actor != "" {
		// Only the disagreements this actor is part of; the other sides stay.
		having += ` AND bool_or(actor = ` + a.add(actor) + `)`
	}
	return `WITH sides AS (` + sides + `)
		SELECT entity_id, predicate FROM sides
		GROUP BY entity_id, predicate
		HAVING ` + having + `
		ORDER BY entity_id, predicate
		LIMIT ` + a.add(limit)
}

// contradictions surfaces entities whose CURRENT resolutions disagree across
// actors — the disagreement returned, never resolved away (memo §8).
//
// ENFORCED in SQL via each side's backing item classification, BEFORE grouping:
// a contradiction row must not leak one side's value because the other side was
// readable. The disagreeing (entity, predicate) pairs, and the LIMIT, are
// decided in the query (ENG-833); only the sides of those entities are loaded.
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
	a := &sqlArgs{}
	keySQL := contradictionKeysSQL(a, q.ItemKind, policy, q.EntityID, q.Predicate, q.Actor, scope, limit)
	krows, err := p.pool.Query(ctx, keySQL, a.args...)
	if err != nil {
		return nil, err
	}
	type key struct{ entity, predicate string }
	var keys []key
	entitySet := map[string]bool{}
	var entities []string
	for krows.Next() {
		var k key
		if err := krows.Scan(&k.entity, &k.predicate); err != nil {
			krows.Close()
			return nil, err
		}
		keys = append(keys, k)
		if !entitySet[k.entity] {
			entitySet[k.entity] = true
			entities = append(entities, k.entity)
		}
	}
	krows.Close()
	if err := krows.Err(); err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, nil
	}
	// The sides of exactly those entities, under the same fence.
	res, err := p.currentResolutions(ctx, q.ItemKind, policy, entities, "", scope, 0)
	if err != nil {
		return nil, err
	}
	views := map[key]map[string]any{}
	for _, r := range res {
		for pred, val := range itemValues(r.Item) {
			k := key{r.EntityID, pred}
			if views[k] == nil {
				views[k] = map[string]any{}
			}
			views[k][r.Actor] = val
		}
	}
	out := make([]domain.QueryRow, 0, len(keys))
	for _, k := range keys {
		v := views[k]
		if len(v) < 2 {
			// A side erased between the two reads: no longer a disagreement.
			continue
		}
		out = append(out, domain.QueryRow{
			"entity_id": k.entity, "predicate": k.predicate, "actors": v,
		})
	}
	return out, nil
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
		scopeCond, extra = sqlfence.EvidenceExists(scope, "observations.evidence_id", len(args))
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
// (sqlfence.EvidenceExists, the rendering point/aggregate use), with the LIMIT AFTER
// the fence: the page is the first `limit` events the caller may read, oldest
// first, and the range is never loaded whole only to be cut in Go
// (T-20261112-substrate-query-shapes-apply-limit-before-the). The event
// selection is EventsForEntity's — same range, same order, retired rows
// excluded (notRetired, ADR-0147) — and the page's roles load in one statement.
func (p *PgQueryPlane) eventsFor(ctx context.Context, q domain.KnowledgeQuery, scope *domain.TagPredicate, limit int) ([]domain.QueryRow, error) {
	args := []any{defaultNamespace, q.From.UTC(), q.To.UTC(), q.EntityID}
	// The events are aliased ev, not e: sqlfence.EvidenceExists' EXISTS names the
	// evidence table e and would shadow an events alias of that name.
	scopeCond, extra := sqlfence.EvidenceExists(scope, "ev.evidence_id", len(args))
	args = append(args, extra...)
	args = append(args, limit)
	sqlRows, err := p.pool.Query(ctx, `
		SELECT `+eventColumnsOn("ev")+`
		FROM events ev
		WHERE ev.namespace_id=$1 AND ev.occurred_at >= $2 AND ev.occurred_at < $3
		  AND EXISTS (SELECT 1 FROM event_roles r WHERE r.event_id = ev.id AND r.entity_id = $4)`+
		retiredCond(q, "ev")+latestRevision("ev")+scopeCond+`
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
	var refs []string
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
		refs = append(refs, ev.SourceRef)
	}
	return out, p.markRetired(ctx, q, refs, out)
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
	if errors.Is(err, pgx.ErrNoRows) {
		// An absent id answers exactly like a hidden one below — no rows — or the
		// shape becomes an existence oracle: a scoped caller could tell "does not
		// exist" (an error) from "exists but you may not see it" (empty), the
		// distinction the entity ops are built to deny
		// (T-20261112-substrate-read-path-leaks-and-gaps-evidence, item 1).
		return nil, nil
	}
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
