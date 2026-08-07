package postgres

import (
	"context"
	"fmt"
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
}

// NewPgQueryPlane composes the plane from the stores it reads through.
func NewPgQueryPlane(pool *pgxpool.Pool, events domain.EventStore,
	knowledge domain.KnowledgeStore, evidence domain.EvidenceStore) *PgQueryPlane {
	return &PgQueryPlane{pool: pool, events: events, knowledge: knowledge, evidence: evidence}
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
	var (
		rows []domain.QueryRow
		err  error
	)
	switch q.Kind {
	case domain.QueryPoint:
		rows, err = p.point(ctx, q, scope)
	case domain.QueryHistory:
		rows, err = p.history(ctx, q, scope, limit)
	case domain.QueryAsOf:
		rows, err = p.asOf(ctx, q, scope, limit)
	case domain.QueryCurrent:
		rows, err = p.current(ctx, q, scope, limit)
	case domain.QueryContradictions:
		rows, err = p.contradictions(ctx, q, scope, limit)
	case domain.QueryAggregate:
		rows, err = p.aggregate(ctx, q, scope)
	case domain.QueryEvents:
		rows, err = p.eventsFor(ctx, q, scope, limit)
	case domain.QueryTraverse:
		rows, err = p.traverse(ctx, q, scope, limit)
	case domain.QueryEvidence:
		rows, err = p.evidenceRow(ctx, q, scope)
	}
	if err != nil {
		return domain.QueryResult{}, err
	}
	return domain.QueryResult{Guarantee: q.Guarantee(), Rows: rows}, nil
}

// evidenceTags batch-resolves evidence ids to their classification. Ids absent
// from the result were not resolvable and their rows must be dropped.
func (p *PgQueryPlane) evidenceTags(ctx context.Context, ids []string) (map[string][]string, error) {
	out := make(map[string][]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := p.pool.Query(ctx,
		`SELECT id, classification FROM evidence WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var tags []string
		if err := rows.Scan(&id, &tags); err != nil {
			return nil, err
		}
		out[id] = tags
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
		if resolvable && scope.Allows(t) {
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
		if resolvable && scope.Allows(t) {
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
	if !scope.Bypass && !scope.Allows(ev.Classification) {
		return nil, nil
	}
	return []domain.QueryRow{{
		"id": string(ev.ID), "source_id": ev.SourceID, "source_key": ev.SourceKey,
		"source_revision": ev.SourceRevision, "source_time": ev.SourceTime,
		"ingested_at": ev.IngestedAt, "content_hash": string(ev.ContentHash),
		"content_bytes": ev.ContentBytes, "classification": ev.Classification,
	}}, nil
}
