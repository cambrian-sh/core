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
func (p *PgQueryPlane) Query(ctx context.Context, q domain.KnowledgeQuery) (domain.QueryResult, error) {
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
		rows, err = p.point(ctx, q)
	case domain.QueryHistory:
		rows, err = p.history(ctx, q, limit)
	case domain.QueryAsOf:
		rows, err = p.asOf(ctx, q, limit)
	case domain.QueryCurrent:
		rows, err = p.current(ctx, q, limit)
	case domain.QueryContradictions:
		rows, err = p.contradictions(ctx, q, limit)
	case domain.QueryAggregate:
		rows, err = p.aggregate(ctx, q)
	case domain.QueryEvents:
		rows, err = p.eventsFor(ctx, q, limit)
	case domain.QueryTraverse:
		rows, err = p.traverse(ctx, q, limit)
	case domain.QueryEvidence:
		rows, err = p.evidenceRow(ctx, q)
	}
	if err != nil {
		return domain.QueryResult{}, err
	}
	return domain.QueryResult{Guarantee: q.Guarantee(), Rows: rows}, nil
}

func obsRow(ob *domain.Observation) domain.QueryRow {
	return domain.QueryRow{
		"entity_id": ob.EntityID, "predicate": ob.Predicate,
		"value_type": ob.Value.Type, "value_date": ob.Value.Date,
		"value_number": ob.Value.Number, "value_text": ob.Value.Text,
		"value_entity_id": ob.Value.EntityRef,
		"location":        ob.Location, "occurred_at": ob.OccurredAt,
		"confidence": ob.Confidence, "evidence_id": string(ob.EvidenceID),
	}
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

func (p *PgQueryPlane) point(ctx context.Context, q domain.KnowledgeQuery) ([]domain.QueryRow, error) {
	ob, err := p.events.PointLookup(ctx, defaultNamespace, q.EntityID, q.Predicate)
	if err != nil || ob == nil {
		return nil, err
	}
	return []domain.QueryRow{obsRow(ob)}, nil
}

func (p *PgQueryPlane) history(ctx context.Context, q domain.KnowledgeQuery, limit int) ([]domain.QueryRow, error) {
	obs, err := p.events.History(ctx, defaultNamespace, q.EntityID, q.Predicate, q.From, q.To)
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

// asOf answers "what did our records hold at time T" from the resolutions
// version history — the one §14 row that is exact AND complete, because it is
// a question about our own records rather than the world.
func (p *PgQueryPlane) asOf(ctx context.Context, q domain.KnowledgeQuery, limit int) ([]domain.QueryRow, error) {
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
	var out []domain.QueryRow
	var ids []domain.KnowledgeItemID
	for sqlRows.Next() {
		var entity, actor, policy, itemID, reason string
		var sysFrom time.Time
		if err := sqlRows.Scan(&entity, &actor, &policy, &itemID, &reason, &sysFrom); err != nil {
			return nil, err
		}
		out = append(out, domain.QueryRow{
			"entity_id": entity, "actor": actor, "policy": policy,
			"reason_code": reason, "system_from": sysFrom, "item_id": itemID,
		})
		ids = append(ids, domain.KnowledgeItemID(itemID))
	}
	if err := sqlRows.Err(); err != nil {
		return nil, err
	}
	for i, id := range ids {
		it, err := p.knowledge.GetItem(ctx, id)
		if err == nil {
			out[i]["values"] = itemValues(it)
			out[i]["asserted_by"] = it.AssertedBy
			out[i]["asserted_at"] = it.AssertedAt
		}
	}
	return out, nil
}

func (p *PgQueryPlane) current(ctx context.Context, q domain.KnowledgeQuery, limit int) ([]domain.QueryRow, error) {
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
		out = append(out, domain.QueryRow{
			"entity_id": r.EntityID, "actor": r.Actor, "policy": r.Policy,
			"reason_code": r.ReasonCode, "values": itemValues(r.Item),
		})
	}
	return out, nil
}

// contradictions surfaces entities whose CURRENT resolutions disagree across
// actors — the disagreement returned, never resolved away (memo §8).
func (p *PgQueryPlane) contradictions(ctx context.Context, q domain.KnowledgeQuery, limit int) ([]domain.QueryRow, error) {
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

func (p *PgQueryPlane) aggregate(ctx context.Context, q domain.KnowledgeQuery) ([]domain.QueryRow, error) {
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
	var value *float64
	err := p.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT %s::float8 FROM observations
		WHERE namespace_id=$1 AND entity_id=$2 AND predicate=$3
		  AND occurred_at >= $4 AND occurred_at < $5`, fns[q.Aggregate]),
		defaultNamespace, q.EntityID, q.Predicate, from.UTC(), to.UTC()).Scan(&value)
	if err != nil {
		return nil, err
	}
	row := domain.QueryRow{"entity_id": q.EntityID, "predicate": q.Predicate, "aggregate": q.Aggregate}
	if value != nil {
		row["value"] = *value
	}
	return []domain.QueryRow{row}, nil
}

func (p *PgQueryPlane) eventsFor(ctx context.Context, q domain.KnowledgeQuery, limit int) ([]domain.QueryRow, error) {
	evs, err := p.events.EventsForEntity(ctx, defaultNamespace, q.EntityID, q.From, q.To)
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
func (p *PgQueryPlane) traverse(ctx context.Context, q domain.KnowledgeQuery, limit int) ([]domain.QueryRow, error) {
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

func (p *PgQueryPlane) evidenceRow(ctx context.Context, q domain.KnowledgeQuery) ([]domain.QueryRow, error) {
	ev, err := p.evidence.Get(ctx, domain.EvidenceID(q.EvidenceID))
	if err != nil {
		return nil, err
	}
	return []domain.QueryRow{{
		"id": string(ev.ID), "source_id": ev.SourceID, "source_key": ev.SourceKey,
		"source_revision": ev.SourceRevision, "source_time": ev.SourceTime,
		"ingested_at": ev.IngestedAt, "content_hash": string(ev.ContentHash),
		"content_bytes": ev.ContentBytes, "classification": ev.Classification,
	}}, nil
}
