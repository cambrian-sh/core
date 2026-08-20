package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

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
	events    domain.EventStore
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
	links     domain.LinkStore
	entities  domain.EntityStore
	relations *domain.RelationRegistry
}

// NewPgQueryPlane composes the plane from the stores it reads through.
//
// links/entities/relations arrived with five-planes step 2 and are positional
// rather than optional for the reason scope is: every construction site is made
// to decide, and the one deployment that forgets shows up at compile time rather
// than as an entity op that quietly returns nothing.
func NewPgQueryPlane(pool *pgxpool.Pool, events domain.EventStore,
	knowledge domain.KnowledgeStore, evidence domain.EvidenceStore,
	links domain.LinkStore, entities domain.EntityStore,
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
		closure, err = p.identityClosure(ctx, q.EntityID, closureDepth(q))
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
	return row
}

// filterObservations keeps the observations whose evidence classification
// passes the scope, resolving provenance in one batch. Filtering runs BEFORE
// any limit so authorized rows fill the window (no scope-induced starvation
// of the result set on top of the denial itself).
func (p *PgQueryPlane) filterObservations(ctx context.Context, scope *domain.TagPredicate, obs []domain.Observation) ([]domain.Observation, error) {
	if scope.Bypass {
		return obs, nil
	}
	ids := make([]string, 0, len(obs))
	for i := range obs {
		ids = append(ids, string(obs[i].EvidenceID))
	}
	tags, err := p.evidenceTags(ctx, ids)
	if err != nil {
		return nil, err
	}
	kept := obs[:0:0]
	for i := range obs {
		t, resolvable := tags[string(obs[i].EvidenceID)]
		// AllowsRow, not Allows: a substrate row carries parties, and testing it
		// with the party-blind form would deny every party-scoped row (ADR-0121).
		if resolvable && scope.AllowsRow(t.tags, t.parties) {
			kept = append(kept, obs[i])
		}
	}
	return kept, nil
}

// point — ENFORCED via the observation's evidence classification.
func (p *PgQueryPlane) point(ctx context.Context, q domain.KnowledgeQuery, scope *domain.TagPredicate) ([]domain.QueryRow, error) {
	ob, err := p.events.PointLookup(ctx, defaultNamespace, q.EntityID, q.Predicate)
	if err != nil || ob == nil {
		return nil, err
	}
	kept, err := p.filterObservations(ctx, scope, []domain.Observation{*ob})
	if err != nil {
		return nil, err
	}
	if len(kept) == 0 {
		return nil, nil
	}
	return []domain.QueryRow{obsRow(&kept[0])}, nil
}

// history — ENFORCED via each observation's evidence classification.
func (p *PgQueryPlane) history(ctx context.Context, q domain.KnowledgeQuery, scope *domain.TagPredicate, limit int) ([]domain.QueryRow, error) {
	obs, err := p.events.History(ctx, defaultNamespace, q.EntityID, q.Predicate, q.From, q.To)
	if err != nil {
		return nil, err
	}
	obs, err = p.filterObservations(ctx, scope, obs)
	if err != nil {
		return nil, err
	}
	rows := make([]domain.QueryRow, 0, min(len(obs), limit))
	for i := range obs {
		if len(rows) == limit {
			break
		}
		rows = append(rows, obsRow(&obs[i]))
	}
	return rows, nil
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
// ENFORCED via the backing knowledge item's classification: a resolution row
// is emitted only when its item resolves AND passes the scope (fail closed on
// a failed item fetch for non-bypass scopes).
func (p *PgQueryPlane) asOf(ctx context.Context, q domain.KnowledgeQuery, scope *domain.TagPredicate, limit int) ([]domain.QueryRow, error) {
	sqlRows, err := p.pool.Query(ctx, `
		SELECT r.entity_id, r.actor, r.policy, r.item_id, r.reason_code, r.system_from
		FROM resolutions r
		WHERE r.namespace_id=$1 AND r.kind=$2
		  AND r.system_from <= $3 AND (r.system_to IS NULL OR r.system_to > $3)
		  AND r.item_id IS NOT NULL
		  AND ($4 = '' OR r.entity_id = $4)
		ORDER BY r.entity_id, r.actor LIMIT $5`,
		defaultNamespace, q.ItemKind, q.AsOf.UTC(), q.EntityID, limit)
	if err != nil {
		return nil, err
	}
	defer sqlRows.Close()
	type resRow struct {
		entity, actor, policy, itemID, reason string
		sysFrom                               time.Time
	}
	var scanned []resRow
	for sqlRows.Next() {
		var r resRow
		if err := sqlRows.Scan(&r.entity, &r.actor, &r.policy, &r.itemID, &r.reason, &r.sysFrom); err != nil {
			return nil, err
		}
		scanned = append(scanned, r)
	}
	if err := sqlRows.Err(); err != nil {
		return nil, err
	}
	var out []domain.QueryRow
	for _, r := range scanned {
		it, err := p.knowledge.GetItem(ctx, domain.KnowledgeItemID(r.itemID))
		if err != nil || it == nil {
			if scope.Bypass {
				// Bypass keeps the pre-0118 shape: the resolution row without
				// its values, rather than silently narrowing a maintenance read.
				out = append(out, domain.QueryRow{
					"entity_id": r.entity, "actor": r.actor, "policy": r.policy,
					"reason_code": r.reason, "system_from": r.sysFrom, "item_id": r.itemID,
				})
			}
			continue
		}
		if !scope.Bypass && !scope.Allows(it.Classification) {
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

// current — ENFORCED via each resolution's backing item classification.
func (p *PgQueryPlane) current(ctx context.Context, q domain.KnowledgeQuery, scope *domain.TagPredicate, limit int) ([]domain.QueryRow, error) {
	policy := q.Policy
	if policy == "" {
		policy = domain.ResolutionPolicyLatestAssertion
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
		if !scope.Bypass && (r.Item == nil || !scope.Allows(r.Item.Classification)) {
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
func (p *PgQueryPlane) contradictions(ctx context.Context, q domain.KnowledgeQuery, scope *domain.TagPredicate, limit int) ([]domain.QueryRow, error) {
	policy := q.Policy
	if policy == "" {
		policy = domain.ResolutionPolicyLatestAssertion
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
		if !scope.Bypass && (r.Item == nil || !scope.Allows(r.Item.Classification)) {
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
	conds := ""
	var args []any
	n := startIdx
	add := func(cond string, val any) {
		n++
		conds += fmt.Sprintf(" AND "+cond, n)
		args = append(args, val)
	}
	if len(scope.RequiredTags) > 0 {
		add("e.classification @> $%d", scope.RequiredTags)
	}
	if len(scope.ForbiddenTags) > 0 {
		add("NOT (e.classification && $%d)", scope.ForbiddenTags)
	}
	for _, clause := range scope.AnyOfClauses {
		if len(clause) > 0 {
			add("e.classification && $%d", clause)
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
		conds += fmt.Sprintf(
			" AND (NOT (e.classification && $%d) OR e.parties && $%d)", n-1, n)
		args = append(args, scope.PartyScopedTags, identityArg(scope.PartyIdentities))
	}
	return ` AND EXISTS (SELECT 1 FROM evidence e
		WHERE e.id = observations.evidence_id` + conds + `)`, args
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
	if !scope.Bypass {
		var extra []any
		scopeCond, extra = evidenceScopeSQL(scope, len(args))
		args = append(args, extra...)
	}
	var value *float64
	err := p.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT %s::float8 FROM observations
		WHERE namespace_id=$1 AND entity_id=$2 AND predicate=$3
		  AND occurred_at >= $4 AND occurred_at < $5%s`, fns[q.Aggregate], scopeCond),
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
	if scope.Bypass {
		return evs, nil
	}
	ids := make([]string, 0, len(evs))
	for i := range evs {
		ids = append(ids, string(evs[i].EvidenceID))
	}
	tags, err := p.evidenceTags(ctx, ids)
	if err != nil {
		return nil, err
	}
	kept := evs[:0:0]
	for i := range evs {
		t, resolvable := tags[string(evs[i].EvidenceID)]
		// AllowsRow: an event reaches scope through its evidence, which carries
		// parties as well as tags (ADR-0121).
		if resolvable && scope.AllowsRow(t.tags, t.parties) {
			kept = append(kept, evs[i])
		}
	}
	return kept, nil
}

// eventsFor — ENFORCED via each event's evidence classification.
func (p *PgQueryPlane) eventsFor(ctx context.Context, q domain.KnowledgeQuery, scope *domain.TagPredicate, limit int) ([]domain.QueryRow, error) {
	evs, err := p.events.EventsForEntity(ctx, defaultNamespace, q.EntityID, q.From, q.To)
	if err != nil {
		return nil, err
	}
	evs, err = p.filterEvents(ctx, scope, evs)
	if err != nil {
		return nil, err
	}
	var out []domain.QueryRow
	for _, ev := range evs {
		if len(out) == limit {
			break
		}
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
		var next []string
		for _, entity := range frontier {
			evs, err := p.events.EventsForEntity(ctx, defaultNamespace, entity, q.From, q.To)
			if err != nil {
				return nil, err
			}
			evs, err = p.filterEvents(ctx, scope, evs)
			if err != nil {
				return nil, err
			}
			for _, ev := range evs {
				for _, r := range ev.Roles {
					if visited[r.EntityID] {
						continue
					}
					visited[r.EntityID] = true
					if len(out) == limit {
						return out, nil
					}
					out = append(out, domain.QueryRow{
						"from": entity, "to": r.EntityID, "role": r.Role,
						"via_event": string(ev.ID), "event_type": ev.Type, "hop": hop,
					})
					next = append(next, r.EntityID)
				}
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
	if !scope.Bypass && !scope.AllowsRow(ev.Classification, ev.Parties) {
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
// One consequence is deliberate and worth naming: closure MEMBERSHIP may stand
// where the joining link's evidence is unreadable. The membership is metadata —
// "we believe these two ids are one thing" — while the link's basis is content,
// and only content is what scope protects. Such a member comes back with its
// justification fields DROPPED and justified=false, which is an honest "we
// cannot show you why" rather than a silent omission that would make the closure
// look smaller than it is.

// closureMember is one entity the identity closure admitted, carrying the
// justification that admitted it. Empty justification fields mean the caller's
// scope could not see the joining link's evidence (see SCOPE RULE above).
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
func (p *PgQueryPlane) identityClosure(ctx context.Context, entityID string, depth int) (closureResult, error) {
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
		var next []string
		for _, ref := range frontier {
			links, err := p.links.LinksFor(ctx, defaultNamespace, ref, domain.LinkQuery{
				Family: domain.LinkFamilyIdentity,
				State:  domain.LinkStateConfirmed,
			})
			if err != nil {
				return closureResult{}, err
			}
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

// justifiedMembers keeps only the closure members whose joining link's evidence
// the caller can read, and DROPS the rest.
//
// This used to redact the justification and leave membership intact, on the
// reading that membership was harmless metadata. ADR-0128 D12 settled that
// question the other way: an identity closure is itself a disclosure — it tells
// the reader that two systems' records belong to one counterparty, which is
// frequently the sensitive part — so a membership the caller cannot justify is
// not listed at all. Listing it while withholding the reason discloses exactly
// the relationship the rule protects, and does so in a form that looks careful.
func (p *PgQueryPlane) justifiedMembers(ctx context.Context, scope *domain.TagPredicate, members []closureMember) ([]closureMember, error) {
	if scope.Bypass || len(members) == 0 {
		return members, nil
	}
	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.evidence)
	}
	tags, err := p.evidenceTags(ctx, ids)
	if err != nil {
		return nil, err
	}
	kept := members[:0]
	for _, m := range members {
		t, resolvable := tags[m.evidence]
		if resolvable && scope.AllowsRow(t.tags, t.parties) {
			kept = append(kept, m)
		}
	}
	return kept, nil
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
	if scope.Bypass {
		return true, nil
	}
	if firstSeen == "" {
		return false, nil
	}
	tags, err := p.evidenceTags(ctx, []string{firstSeen})
	if err != nil {
		return false, err
	}
	t, resolvable := tags[firstSeen]
	return resolvable && scope.AllowsRow(t.tags, t.parties), nil
}

// filterLinks keeps the links whose evidence classification passes the scope,
// resolving provenance in one batch — the filterEvents/filterObservations shape
// applied to link rows.
//
// Fail closed on unresolvable evidence, as everywhere else on this plane. The one
// case that costs is a `human` link, which the admissibility rule lets carry no
// evidence at all: such a link is visible only to a bypass (operator) scope. That
// is the conservative reading and the right one — a bare human assertion joining
// two ids is exactly the row whose visibility nobody has decided yet.
func (p *PgQueryPlane) filterLinks(ctx context.Context, scope *domain.TagPredicate, links []domain.Link) ([]domain.Link, error) {
	if scope.Bypass {
		return links, nil
	}
	ids := make([]string, 0, len(links))
	for i := range links {
		ids = append(ids, string(links[i].EvidenceID))
	}
	tags, err := p.evidenceTags(ctx, ids)
	if err != nil {
		return nil, err
	}
	kept := links[:0:0]
	for i := range links {
		t, resolvable := tags[string(links[i].EvidenceID)]
		if resolvable && scope.AllowsRow(t.tags, t.parties) {
			kept = append(kept, links[i])
		}
	}
	return kept, nil
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
// closure justification is redacted the same way. The closure itself is not an
// access decision (SCOPE RULE).
//
// Candidates are withheld from every non-bypass caller by construction. A
// candidate is an unreviewed machine proposal, and an agent that reads one as an
// answer has done precisely what the review lane exists to prevent; the operator
// console, which is where review happens, reads with a bypass predicate.
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

	members, err := p.justifiedMembers(ctx, scope, closure.members)
	if err != nil {
		return nil, err
	}
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
	for _, ref := range refs {
		links, err := p.links.LinksFor(ctx, defaultNamespace, ref, domain.LinkQuery{
			State: domain.LinkStateConfirmed, Limit: limit,
		})
		if err != nil {
			return nil, err
		}
		links, err = p.filterLinks(ctx, scope, links)
		if err != nil {
			return nil, err
		}
		for _, l := range links {
			emit(l, ref, false)
		}
		// What CITES this handle (D-W5-6). LinksFor without IncludeIncoming
		// answers only what the ref itself asserted, and an entity is almost
		// never the asserter of its own citations: `referenced_by` runs from
		// the citing record TO the entity, so the card of the most-cited
		// handle in a corpus was the emptiest one on the plane.
		//
		// LINEAGE ONLY, and that is the discipline of this read. Incoming
		// IDENTITY is already reported as closure membership; incoming
		// RELATION is somebody else's assertion about their own record ("my
		// parent is you") and reads correctly only on their card, where a
		// reader can see whose claim it is. Citation is the one direction
		// where the interesting end is the one being pointed at.
		incoming, err := p.links.LinksFor(ctx, defaultNamespace, ref, domain.LinkQuery{
			Family: domain.LinkFamilyLineage, State: domain.LinkStateConfirmed,
			IncludeIncoming: true, Limit: limit,
		})
		if err != nil {
			return nil, err
		}
		incoming, err = p.filterLinks(ctx, scope, incoming)
		if err != nil {
			return nil, err
		}
		for _, l := range incoming {
			emit(l, ref, l.ToRef == ref)
		}
	}

	if scope.Bypass {
		cands, err := p.links.Candidates(ctx, defaultNamespace, limit)
		if err != nil {
			return nil, err
		}
		for _, l := range cands {
			// Candidates is the whole inbox; this shape answers about ONE handle,
			// so the endpoints decide membership rather than a second store method.
			if l.FromRef != subjectRef && l.ToRef != subjectRef {
				continue
			}
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
	// Co-occurrence hops anchored on an ENTITY are deduplicated by the PAIR
	// rather than by the target: one entity binds many events to each other,
	// and a target-keyed check would emit the first pair and swallow every
	// other relationship the same entity holds together.
	seenPair := map[string]bool{}
	frontier := []string{root}
	var out []domain.QueryRow

	state := domain.LinkStateConfirmed
	if scope.Bypass {
		state = ""
	}
	for hop := 1; hop <= depth && len(frontier) > 0; hop++ {
		var next []string
		for _, ref := range frontier {
			if p.links != nil {
				links, err := p.links.LinksFor(ctx, defaultNamespace, ref, domain.LinkQuery{
					Family: domain.LinkFamilyLineage, State: state,
					IncludeIncoming: true, Limit: limit,
				})
				if err != nil {
					return nil, err
				}
				links, err = p.filterLinks(ctx, scope, links)
				if err != nil {
					return nil, err
				}
				for _, l := range links {
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
			}
			// Shared-object hops hold between OCCURRENCES: two events that name
			// the same entity in a role. Both lanes below emit the SAME row
			// shape and the same mechanism label; they differ only in which end
			// of the relationship the walk is standing on.
			switch {
			case strings.HasPrefix(ref, domain.RefPrefixEvent):
				hops, err := p.sharedObjectHops(ctx, scope, ref, limit)
				if err != nil {
					return nil, err
				}
				for _, h := range hops {
					if seen[h.ref] {
						continue
					}
					seen[h.ref] = true
					if len(out) >= limit {
						return out, nil
					}
					out = append(out, domain.QueryRow{
						"row": "hop", "from_ref": ref, "to_ref": h.ref,
						// The verb is the honest description of what
						// co-occurrence establishes: order plus overlap,
						// never cause.
						"relation":  domain.RelationPrecededAndSharesEntities,
						"mechanism": domain.LinkMechanismSharedObject,
						"state":     "", "stored": false,
						"evidence_id": h.evidence, "event_type": h.eventType,
						"occurred_at": h.occurredAt, "shared_entity_id": h.sharedEntity,
						"shared_role": h.sharedRole, "hop": hop,
					})
					next = append(next, h.ref)
				}
			case strings.HasPrefix(ref, domain.RefPrefixEntity):
				// why(entity:X) used to reach this lane only by luck — it
				// needed a STORED lineage link to walk it onto an event first,
				// and an entity whose whole trace is participation has none.
				// So "why is this shipment the way it is" answered nothing,
				// while the same question asked of one of its events answered
				// fully (D-W5-6).
				//
				// The hops run between the EVENTS, not out of the entity: the
				// entity is what they SHARE, and a hop "entity → event" would
				// claim a handle preceded an occurrence, which is not a thing
				// that happened. X rides along as shared_entity_id.
				pairs, err := p.entitySharedObjectHops(ctx, scope, ref, limit)
				if err != nil {
					return nil, err
				}
				for _, h := range pairs {
					key := h.fromRef + "\x00" + h.toRef
					if seenPair[key] {
						continue
					}
					seenPair[key] = true
					if len(out) >= limit {
						return out, nil
					}
					out = append(out, domain.QueryRow{
						"row": "hop", "from_ref": h.fromRef, "to_ref": h.toRef,
						"relation":  domain.RelationPrecededAndSharesEntities,
						"mechanism": domain.LinkMechanismSharedObject,
						"state":     "", "stored": false,
						"evidence_id": h.evidence, "event_type": h.eventType,
						"occurred_at": h.occurredAt, "shared_entity_id": h.sharedEntity,
						"shared_role": h.sharedRole, "hop": hop,
					})
					// BOTH ends enter the walk. The pair was produced by
					// standing on the entity, so neither event has had its own
					// lineage or its own co-occurrences looked at yet.
					for _, end := range []string{h.fromRef, h.toRef} {
						if !seen[end] {
							seen[end] = true
							next = append(next, end)
						}
					}
				}
			}
		}
		frontier = next
	}
	return out, nil
}

// sharedPair is one live, unstored hop between two occurrences, anchored on the
// ENTITY they share rather than on either of them (D-W5-6). Both endpoints are
// named because neither is the ref the walk was standing on.
type sharedPair struct {
	fromRef string
	toRef   string
	// fromEvidence and evidence are the two events' own bases. BOTH are
	// checked against the caller's scope: unlike the event-anchored lane,
	// where the near end is the row the caller asked about and has already
	// been decided, here the caller asked about an entity and neither event
	// has been through a scope check.
	fromEvidence string
	evidence     string
	eventType    string
	occurredAt   time.Time
	sharedEntity string
	sharedRole   string
}

// entitySharedObjectHops pairs the occurrences that name one entity in a role.
//
// The event-anchored lane asks "what came before THIS event"; this one asks
// "what did this entity hold together", and answers with the pairs themselves —
// so an entity that appears in an order, a shipment and an invoice yields the
// three co-occurrences a reader would draw, instead of nothing at all because
// no producer ever stored a link between them.
//
// The strict (occurred_at, id) ordering in the WHERE clause is what makes a
// pair appear ONCE and in the honest direction: without the id tiebreak two
// events stamped the same microsecond would each be reported as preceding the
// other, which is not an ordering, it is two contradictory claims.
//
// ENFORCED on both events' own evidence, fail closed.
func (p *PgQueryPlane) entitySharedObjectHops(ctx context.Context, scope *domain.TagPredicate,
	entityRef string, limit int) ([]sharedPair, error) {
	id, ok := strings.CutPrefix(entityRef, domain.RefPrefixEntity)
	if !ok || id == "" {
		return nil, nil
	}
	sqlRows, err := p.pool.Query(ctx, `
		SELECT DISTINCT e1.id, e2.id, e2.event_type, e2.occurred_at,
		       e1.evidence_id, e2.evidence_id, r1.role
		FROM event_roles r1
		JOIN events e1 ON e1.id = r1.event_id
		JOIN event_roles r2 ON r2.entity_id = r1.entity_id AND r2.event_id <> r1.event_id
		JOIN events e2 ON e2.id = r2.event_id AND e2.namespace_id = e1.namespace_id
		WHERE r1.entity_id = $1 AND e1.namespace_id = $2
		  AND (e2.occurred_at < e1.occurred_at
		       OR (e2.occurred_at = e1.occurred_at AND e2.id < e1.id))
		ORDER BY e2.occurred_at DESC, e1.id, e2.id
		LIMIT $3`, id, defaultNamespace, limit)
	if err != nil {
		return nil, err
	}
	var found []sharedPair
	for sqlRows.Next() {
		var h sharedPair
		var fromID, toID string
		var fromEv, toEv *string
		if err := sqlRows.Scan(&fromID, &toID, &h.eventType, &h.occurredAt,
			&fromEv, &toEv, &h.sharedRole); err != nil {
			sqlRows.Close()
			return nil, err
		}
		h.fromRef = domain.RefPrefixEvent + fromID
		h.toRef = domain.RefPrefixEvent + toID
		if fromEv != nil {
			h.fromEvidence = *fromEv
		}
		if toEv != nil {
			h.evidence = *toEv
		}
		h.sharedEntity = id
		found = append(found, h)
	}
	sqlRows.Close()
	if err := sqlRows.Err(); err != nil {
		return nil, err
	}
	if scope.Bypass || len(found) == 0 {
		return found, nil
	}
	ids := make([]string, 0, len(found)*2)
	for _, h := range found {
		ids = append(ids, h.fromEvidence, h.evidence)
	}
	tags, err := p.evidenceTags(ctx, ids)
	if err != nil {
		return nil, err
	}
	allows := func(evidenceID string) bool {
		t, resolvable := tags[evidenceID]
		return resolvable && scope.AllowsRow(t.tags, t.parties)
	}
	kept := found[:0:0]
	for _, h := range found {
		// A hop is a claim about BOTH events, so a caller who may not read one
		// of them may not be told the other stands next to it.
		if allows(h.fromEvidence) && allows(h.evidence) {
			kept = append(kept, h)
		}
	}
	return kept, nil
}

// sharedHop is one live, unstored hop between two occurrences.
type sharedHop struct {
	ref          string
	evidence     string
	eventType    string
	occurredAt   time.Time
	sharedEntity string
	sharedRole   string
}

// sharedObjectHops finds earlier events sharing a role participant with this one.
//
// Computed rather than stored on purpose: the relation is a JOIN over rows that
// already exist, and materialising it would mean a producer writing O(n²) links
// that say nothing the rows did not already say. The `occurred_at <=` term is
// what makes it a BACKWARD walk — `why` asks what came before, and a hop forward
// in time explains nothing.
//
// ENFORCED on the neighbouring event's own evidence, fail closed.
func (p *PgQueryPlane) sharedObjectHops(ctx context.Context, scope *domain.TagPredicate,
	eventRef string, limit int) ([]sharedHop, error) {
	id, ok := strings.CutPrefix(eventRef, domain.RefPrefixEvent)
	if !ok || id == "" {
		return nil, nil
	}
	sqlRows, err := p.pool.Query(ctx, `
		SELECT DISTINCT e2.id, e2.event_type, e2.occurred_at, e2.evidence_id,
		       r1.entity_id, r1.role
		FROM events e1
		JOIN event_roles r1 ON r1.event_id = e1.id
		JOIN event_roles r2 ON r2.entity_id = r1.entity_id AND r2.event_id <> e1.id
		JOIN events e2 ON e2.id = r2.event_id AND e2.namespace_id = e1.namespace_id
		WHERE e1.id = $1 AND e1.namespace_id = $2 AND e2.occurred_at <= e1.occurred_at
		ORDER BY e2.occurred_at DESC, e2.id
		LIMIT $3`, id, defaultNamespace, limit)
	if err != nil {
		return nil, err
	}
	var found []sharedHop
	for sqlRows.Next() {
		var h sharedHop
		var otherID string
		var evID *string
		if err := sqlRows.Scan(&otherID, &h.eventType, &h.occurredAt, &evID,
			&h.sharedEntity, &h.sharedRole); err != nil {
			sqlRows.Close()
			return nil, err
		}
		h.ref = domain.RefPrefixEvent + otherID
		if evID != nil {
			h.evidence = *evID
		}
		found = append(found, h)
	}
	sqlRows.Close()
	if err := sqlRows.Err(); err != nil {
		return nil, err
	}
	if scope.Bypass || len(found) == 0 {
		return found, nil
	}
	ids := make([]string, 0, len(found))
	for _, h := range found {
		ids = append(ids, h.evidence)
	}
	tags, err := p.evidenceTags(ctx, ids)
	if err != nil {
		return nil, err
	}
	kept := found[:0:0]
	for _, h := range found {
		t, resolvable := tags[h.evidence]
		if resolvable && scope.AllowsRow(t.tags, t.parties) {
			kept = append(kept, h)
		}
	}
	return kept, nil
}
