package postgres

// Related rows (ADR-0148) of PgQueryPlane's why shape (moved from query_plane.go,
// ADR-0149 P0; no logic change).

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz/sqlfence"
)

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
// sqlfence.EvidenceExists — the one SQL rendering of the row test, the aggregate
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
	// The counted events are aliased oe, not e: sqlfence.EvidenceExists' EXISTS
	// names the evidence table e, and would shadow an events alias of that name.
	dfFence, dfArgs := sqlfence.EvidenceExists(scope, "oe.evidence_id", len(args))
	args = append(args, dfArgs...)
	candFence, candArgs := sqlfence.EvidenceExists(scope, "e2.evidence_id", len(args))
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
	fence, fenceArgs := sqlfence.EvidenceExists(scope, "oe.evidence_id", len(args)) // oe: see relatedFromEvents
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
