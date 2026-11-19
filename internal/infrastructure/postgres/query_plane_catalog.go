package postgres

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz/sqlfence"
)

// The substrate's names-and-counts surface (domain.SubstrateCatalog) and the
// identity plane's review inbox, both served by the query plane so that every
// substrate read a principal can reach goes through ONE fenced path
// (T-20261107-substratelane-pointlookup-history,
// T-20261107-ask-knowledge-catalog-reads-entity-ids-and,
// T-20261107-link-review-inbox-and-entity-op-candidates-skip).
//
// Before this file the premium explore surface (Overview, ListEntities) and the
// ask former's grounding catalog ran their own SQL over the substrate tables with
// no predicate at all, and the operator review inbox listed every candidate. Each
// of those returned names derived from someone's owner:X evidence. The fix is not
// a second copy of the fence in premium (AGENTS.md §2.5): it is these reads, here,
// built on the same sqlfence.TypedRowExists / sqlfence.TextArray renderings the
// aggregate shape and the item stores already use.
//
// How each table reaches scope — the same as the AST shapes:
//   - evidence:              its own classification + parties (sqlfence.Evidence)
//   - observations, events:  through evidence_id, narrowed by their own row
//     classification (sqlfence.TypedRowExists, ADR-0158 D2)
//   - evidence_outbox:       through evidence_id (sqlfence.EvidenceExists)
//   - event_roles:           through its event
//   - knowledge_items:       its own classification (sqlfence.TextArray)
//   - resolutions:           through the item it resolves to; a resolution with
//     no item (a negation) has nothing to judge and is dropped
//     for every scope but ScopeMaintenance (fail closed)

const (
	catalogDefaultLimit = 50
	catalogMaxLimit     = domain.MaxQueryLimit
)

// Catalog returns the names-and-counts surface BOUND to scope. A nil scope
// yields a catalog whose every method refuses with ErrQueryScopeMissing — the
// QueryPlane rule: nil denies; unrestricted is said explicitly.
func (p *PgQueryPlane) Catalog(scope *domain.TagPredicate) domain.SubstrateCatalog {
	return pgCatalog{plane: p, scope: scope}
}

// ReviewCandidates is the identity plane's review inbox (contract 0098
// ListLinkCandidates) read through the plane, so every candidate is judged on
// every evidence row it rests on. The operator plane reads at domain.ScopeSystem,
// which still applies the owner fence: a candidate proposed from someone's
// owner:X evidence is not listed, and the inbox does not say how many were
// withheld.
//
// The fence is IN the query, before LIMIT (viaLink = sqlfence.LinkExists, the
// SQL mirror of filterLinks), so a page holds `limit` readable candidates
// whenever that many exist (ENG-1754, owner decision 2026-11-19 — as was done
// for as_of). Rejected: reading `limit` candidates and walking them through
// filterLinks — every fenced candidate ahead in candidateOrder shortened the
// page, and a full page of fenced ones hid the readable inbox entirely.
// Candidates on PgLinkStore stays the unfenced inbox for maintenance callers.
func (p *PgQueryPlane) ReviewCandidates(ctx context.Context, namespace string, limit int, scope *domain.TagPredicate) ([]domain.Link, error) {
	if scope == nil {
		return nil, domain.ErrQueryScopeMissing
	}
	if p == nil || p.links == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = defaultIdentityLimit
	}
	a := &sqlArgs{}
	q := `SELECT ` + linkColumns + ` FROM links l
		WHERE l.namespace_id=` + a.add(orDefaultNamespace(namespace)) + `::text AND ` + openCandidate +
		a.viaLink(scope, "l") + `
		ORDER BY ` + candidateOrder + `
		LIMIT ` + a.add(limit) + `::int`
	rows, err := p.pool.Query(ctx, q, a.args...)
	if err != nil {
		return nil, err
	}
	return collectLinks(rows)
}

type pgCatalog struct {
	plane *PgQueryPlane
	scope *domain.TagPredicate
}

var _ domain.SubstrateCatalog = pgCatalog{}

// sqlArgs numbers positional args as conditions are appended, so the fence
// renderings (which take a start index) compose with the caller's own filters.
type sqlArgs struct{ args []any }

func (a *sqlArgs) add(v any) string {
	a.args = append(a.args, v)
	return "$" + strconv.Itoa(len(a.args))
}

// inNamespace keeps a typed-row read (observations, events) to the namespace
// every query-plane shape reads (defaultNamespace). The catalog used to count and
// list typed rows across every namespace — a studio:<ingress> namespace included
// — while the shapes an operator then ran answered from 'default' only, so the
// catalog promised rows no query could reach (ENG-833, owner decision
// 2026-11-14). col is the alias-qualified namespace column.
func (a *sqlArgs) inNamespace(col string) string {
	return " AND " + col + " = " + a.add(defaultNamespace)
}

// viaEvidence fences a row that references evidence through refCol and carries
// no row classification of its own (the evidence outbox; an entity's minting
// evidence until ADR-0158 stage 5). A typed row uses viaTypedRow.
func (a *sqlArgs) viaEvidence(scope *domain.TagPredicate, refCol string) string {
	cond, extra := sqlfence.EvidenceExists(scope, refCol, len(a.args))
	a.args = append(a.args, extra...)
	return cond
}

// viaTypedRow fences a typed row (event, observation, link) that references
// evidence through refCol and carries its own row classification in rowCol:
// fence(E) AND fence(E ∪ R) (sqlfence.TypedRowExists, ADR-0158 D2), so the row's
// tags can only hide it, never widen it, and an empty R reads as viaEvidence.
func (a *sqlArgs) viaTypedRow(scope *domain.TagPredicate, refCol, rowCol string) string {
	cond, extra := sqlfence.TypedRowExists(scope, refCol, rowCol, len(a.args))
	a.args = append(a.args, extra...)
	return cond
}

// viaLink fences a link row aliased alias on EVERY evidence row it rests on —
// its cited evidence as a typed row and each supporting evidence
// (sqlfence.LinkExists, ENG-2049): the SQL mirror of filterLinks.
func (a *sqlArgs) viaLink(scope *domain.TagPredicate, alias string) string {
	cond, extra := sqlfence.LinkExists(scope, alias, len(a.args))
	a.args = append(a.args, extra...)
	return cond
}

// evidenceRow fences the evidence table itself, aliased alias.
func (a *sqlArgs) evidenceRow(scope *domain.TagPredicate, alias string) string {
	cond, extra := sqlfence.Evidence(alias).Where(scope, len(a.args))
	a.args = append(a.args, extra...)
	return cond
}

// item fences a knowledge_items row aliased alias. A LEFT-joined item that is
// absent (a negation resolution) fails closed for every filtered scope.
func (a *sqlArgs) item(scope *domain.TagPredicate, alias string) string {
	if scope.Unfiltered() {
		return ""
	}
	cond, extra := sqlfence.TextArray(alias+".classification").Where(scope, len(a.args))
	a.args = append(a.args, extra...)
	return " AND " + alias + ".id IS NOT NULL" + cond
}

func (c pgCatalog) check() error {
	if c.scope == nil {
		return domain.ErrQueryScopeMissing
	}
	if c.plane == nil || c.plane.pool == nil {
		return fmt.Errorf("substrate catalog: no store")
	}
	return nil
}

func boundLimit(n int) int {
	if n <= 0 {
		return catalogDefaultLimit
	}
	if n > catalogMaxLimit {
		return catalogMaxLimit
	}
	return n
}

func likeAny(subs []string) []string {
	out := make([]string, 0, len(subs))
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	for _, s := range subs {
		if s != "" {
			out = append(out, "%"+esc.Replace(s)+"%")
		}
	}
	return out
}

// Overview — ENFORCED per table as listed in the file comment. Every figure is
// over readable rows only; nothing here counts what the reader cannot see.
func (c pgCatalog) Overview(ctx context.Context) (domain.SubstrateOverview, error) {
	var out domain.SubstrateOverview
	if err := c.check(); err != nil {
		return out, err
	}
	pool, scope := c.plane.pool, c.scope
	count := func(dest *int64, build func(a *sqlArgs) string) error {
		a := &sqlArgs{}
		q := build(a)
		if err := pool.QueryRow(ctx, q, a.args...).Scan(dest); err != nil {
			return fmt.Errorf("substrate catalog: overview: %w", err)
		}
		return nil
	}
	counts := []struct {
		dest  *int64
		build func(a *sqlArgs) string
	}{
		{&out.EvidenceCount, func(a *sqlArgs) string {
			return `SELECT count(*) FROM evidence ev WHERE TRUE` + a.evidenceRow(scope, "ev")
		}},
		{&out.EventCount, func(a *sqlArgs) string {
			return `SELECT count(*) FROM events WHERE TRUE` + a.inNamespace("events.namespace_id") +
				a.viaTypedRow(scope, "events.evidence_id", "events.classification") + notRetired("events") + latestRevision("events")
		}},
		{&out.ObservationCount, func(a *sqlArgs) string {
			return `SELECT count(*) FROM observations WHERE TRUE` + a.inNamespace("observations.namespace_id") +
				a.viaTypedRow(scope, "observations.evidence_id", "observations.classification") + notRetired("observations")
		}},
		{&out.KnowledgeItemCount, func(a *sqlArgs) string {
			return `SELECT count(*) FROM knowledge_items ki WHERE TRUE` + a.item(scope, "ki")
		}},
		{&out.LiveResolutionCount, func(a *sqlArgs) string {
			return `SELECT count(*) FROM resolutions r JOIN knowledge_items ki ON ki.id = r.item_id
				WHERE r.system_to IS NULL` + a.item(scope, "ki")
		}},
		{&out.EntityCount, func(a *sqlArgs) string {
			return `SELECT count(DISTINCT entity_id) FROM observations WHERE TRUE` +
				a.inNamespace("observations.namespace_id") +
				a.viaTypedRow(scope, "observations.evidence_id", "observations.classification") + notRetired("observations")
		}},
		{&out.ActorCount, func(a *sqlArgs) string {
			return `SELECT count(DISTINCT r.actor) FROM resolutions r
				LEFT JOIN knowledge_items ki ON ki.id = r.item_id
				WHERE r.system_to IS NULL` + a.item(scope, "ki")
		}},
		// Pending means UNPROCESSED: outbox rows are retained after processing,
		// so the bare count is "everything ever". Fenced through the evidence
		// row, like everything else — a backlog figure that counted someone's
		// private deliveries would be a hidden count.
		//
		// Pending excludes DEAD items (migration 0039): pending is work that will
		// still happen — due, backing off, or held by a paused ingress — and a dead
		// item never drains by itself, so counting it made "pending > 0" mean
		// "wait" for something no wait would fix. Dead items are their own figure
		// (owner decision 2026-11-13,
		// T-20261113-substrate-overview-has-no-separate-dead-letter); the two
		// partition the unprocessed rows, so pending + dead is the old number.
		{&out.OutboxPending, func(a *sqlArgs) string {
			return `SELECT count(*) FROM evidence_outbox o WHERE o.processed_at IS NULL AND o.dead_at IS NULL` +
				a.viaEvidence(scope, "o.evidence_id")
		}},
		{&out.OutboxDead, func(a *sqlArgs) string {
			return `SELECT count(*) FROM evidence_outbox o WHERE o.processed_at IS NULL AND o.dead_at IS NOT NULL` +
				a.viaEvidence(scope, "o.evidence_id")
		}},
	}
	for _, ct := range counts {
		if err := count(ct.dest, ct.build); err != nil {
			return out, err
		}
	}

	a := &sqlArgs{}
	// Retired rows (ADR-0147 D6) are left out of every count and window here:
	// a superseded mapping revision must not double what the overview reports.
	obsCond := a.inNamespace("observations.namespace_id") +
		a.viaTypedRow(scope, "observations.evidence_id", "observations.classification") + notRetired("observations")
	evCond := a.inNamespace("events.namespace_id") +
		a.viaTypedRow(scope, "events.evidence_id", "events.classification") + notRetired("events")
	var earliest, latest *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT min(occurred_at), max(occurred_at) FROM (
			SELECT occurred_at FROM observations WHERE TRUE`+obsCond+`
			UNION ALL
			SELECT occurred_at FROM events WHERE TRUE`+evCond+`
		) t`, a.args...).Scan(&earliest, &latest); err != nil {
		return out, fmt.Errorf("substrate catalog: overview window: %w", err)
	}
	if earliest != nil {
		out.Earliest = *earliest
	}
	if latest != nil {
		out.Latest = *latest
	}

	// count(DISTINCT ev.id), not count(*): the lateral unnest yields one row PER
	// TAG, and the earlier count(*) reported a three-tag source three times over.
	a = &sqlArgs{}
	srcRows, err := pool.Query(ctx, `
		SELECT ev.source_id, count(DISTINCT ev.id), max(ev.ingested_at),
		       COALESCE(array_agg(DISTINCT tag) FILTER (WHERE tag IS NOT NULL), '{}')
		FROM evidence ev LEFT JOIN LATERAL unnest(ev.classification) AS tag ON true
		WHERE TRUE`+a.evidenceRow(scope, "ev")+`
		GROUP BY ev.source_id ORDER BY max(ev.ingested_at) DESC LIMIT 50`, a.args...)
	if err != nil {
		return out, fmt.Errorf("substrate catalog: overview sources: %w", err)
	}
	defer srcRows.Close()
	for srcRows.Next() {
		var s domain.CatalogSource
		if err := srcRows.Scan(&s.SourceID, &s.EvidenceCount, &s.LastSeen, &s.Classification); err != nil {
			return out, err
		}
		out.Sources = append(out.Sources, s)
	}
	if err := srcRows.Err(); err != nil {
		return out, err
	}

	a = &sqlArgs{}
	kindRows, err := pool.Query(ctx, `
		SELECT ki.kind, count(*) FROM knowledge_items ki WHERE TRUE`+a.item(scope, "ki")+`
		GROUP BY ki.kind ORDER BY count(*) DESC, ki.kind LIMIT 50`, a.args...)
	if err != nil {
		return out, fmt.Errorf("substrate catalog: overview kinds: %w", err)
	}
	defer kindRows.Close()
	for kindRows.Next() {
		var k domain.CatalogKindCount
		if err := kindRows.Scan(&k.Kind, &k.Count); err != nil {
			return out, err
		}
		out.Kinds = append(out.Kinds, k)
	}
	return out, kindRows.Err()
}

// Entities — ENFORCED: an entity is listed only through a row the scope can
// read (an observation or event through its evidence, a live resolution through
// its item), and its counts and predicates come from readable observations only.
func (c pgCatalog) Entities(ctx context.Context, f domain.EntityFilter) ([]domain.CatalogEntity, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	if f.EventType != "" && f.Predicate != "" {
		return nil, fmt.Errorf("substrate catalog: entities: predicate and event type cannot be combined")
	}
	scope := c.scope
	a := &sqlArgs{}
	idFilter := func(col string) string {
		cond := ""
		if len(f.IDs) > 0 {
			cond += " AND " + col + " = ANY(" + a.add(f.IDs) + ")"
		}
		if pats := likeAny(f.Substrings); len(pats) > 0 {
			cond += " AND " + col + " ILIKE ANY(" + a.add(pats) + ")"
		} else if len(f.Substrings) > 0 {
			cond += " AND FALSE"
		}
		return cond
	}
	var parts []string
	if f.EventType != "" {
		parts = append(parts, `SELECT er.entity_id, max(ev.occurred_at) AS last_seen
			FROM events ev JOIN event_roles er ON er.event_id = ev.id
			WHERE ev.event_type = `+a.add(f.EventType)+idFilter("er.entity_id")+
			a.inNamespace("ev.namespace_id")+
			a.viaTypedRow(scope, "ev.evidence_id", "ev.classification")+notRetired("ev")+latestRevision("ev")+`
			GROUP BY er.entity_id`)
	} else {
		pred := ""
		if f.Predicate != "" {
			pred = " AND o.predicate = " + a.add(f.Predicate)
		}
		parts = append(parts, `SELECT o.entity_id, max(o.occurred_at) AS last_seen
			FROM observations o WHERE TRUE`+pred+idFilter("o.entity_id")+
			a.inNamespace("o.namespace_id")+
			a.viaTypedRow(scope, "o.evidence_id", "o.classification")+notRetired("o")+`
			GROUP BY o.entity_id`)
		if f.IncludeResolutions && f.Predicate == "" {
			parts = append(parts, `SELECT r.entity_id, max(r.system_from) AS last_seen
				FROM resolutions r LEFT JOIN knowledge_items ki ON ki.id = r.item_id
				WHERE r.system_to IS NULL`+idFilter("r.entity_id")+a.item(scope, "ki")+`
				GROUP BY r.entity_id`)
		}
	}
	limit := boundLimit(f.Limit)
	rows, err := c.plane.pool.Query(ctx, `
		SELECT entity_id, max(last_seen) FROM (`+strings.Join(parts, " UNION ALL ")+`) u
		GROUP BY entity_id ORDER BY 2 DESC, 1 LIMIT `+a.add(limit), a.args...)
	if err != nil {
		return nil, fmt.Errorf("substrate catalog: entities: %w", err)
	}
	var out []domain.CatalogEntity
	for rows.Next() {
		var e domain.CatalogEntity
		if err := rows.Scan(&e.ID, &e.LastSeen); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	// Counts and predicate names, over READABLE observations of the listed ids.
	ids := make([]string, 0, len(out))
	for _, e := range out {
		ids = append(ids, e.ID)
	}
	a = &sqlArgs{}
	prow, err := c.plane.pool.Query(ctx, `
		SELECT o.entity_id, count(*), array_agg(DISTINCT o.predicate)
		FROM observations o WHERE o.entity_id = ANY(`+a.add(ids)+`)`+
		a.inNamespace("o.namespace_id")+
		a.viaTypedRow(scope, "o.evidence_id", "o.classification")+notRetired("o")+`
		GROUP BY o.entity_id`, a.args...)
	if err != nil {
		return nil, fmt.Errorf("substrate catalog: entity predicates: %w", err)
	}
	defer prow.Close()
	byID := make(map[string]int, len(out))
	for i := range out {
		byID[out[i].ID] = i
	}
	for prow.Next() {
		var id string
		var n int64
		var preds []string
		if err := prow.Scan(&id, &n, &preds); err != nil {
			return nil, err
		}
		i, ok := byID[id]
		if !ok {
			continue
		}
		sort.Strings(preds)
		if f.PredicateLimit > 0 && len(preds) > f.PredicateLimit {
			preds = preds[:f.PredicateLimit]
		}
		out[i].ObservationCount = n
		out[i].Predicates = preds
	}
	return out, prow.Err()
}

// distinctNames runs one "SELECT DISTINCT name … ORDER BY 1 LIMIT n" read.
func (c pgCatalog) distinctNames(ctx context.Context, what string, build func(a *sqlArgs) string, limit int) ([]string, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	a := &sqlArgs{}
	q := build(a) + ` ORDER BY 1 LIMIT ` + a.add(boundLimit(limit))
	rows, err := c.plane.pool.Query(ctx, q, a.args...)
	if err != nil {
		return nil, fmt.Errorf("substrate catalog: %s: %w", what, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Predicates — ENFORCED through each observation's evidence.
func (c pgCatalog) Predicates(ctx context.Context, limit int) ([]string, error) {
	return c.distinctNames(ctx, "predicates", func(a *sqlArgs) string {
		return `SELECT DISTINCT o.predicate FROM observations o WHERE TRUE` +
			a.inNamespace("o.namespace_id") +
			a.viaTypedRow(c.scope, "o.evidence_id", "o.classification") + notRetired("o")
	}, limit)
}

// EventTypes — ENFORCED through each event's evidence.
func (c pgCatalog) EventTypes(ctx context.Context, limit int) ([]string, error) {
	return c.distinctNames(ctx, "event types", func(a *sqlArgs) string {
		return `SELECT DISTINCT ev.event_type FROM events ev WHERE ev.event_type <> ''` +
			a.inNamespace("ev.namespace_id") +
			a.viaTypedRow(c.scope, "ev.evidence_id", "ev.classification") + notRetired("ev")
	}, limit)
}

// ItemKinds — ENFORCED through each live resolution's item.
func (c pgCatalog) ItemKinds(ctx context.Context, limit int) ([]string, error) {
	return c.distinctNames(ctx, "item kinds", func(a *sqlArgs) string {
		return `SELECT DISTINCT r.kind FROM resolutions r
			LEFT JOIN knowledge_items ki ON ki.id = r.item_id
			WHERE r.system_to IS NULL` + a.item(c.scope, "ki")
	}, limit)
}

// Actors — ENFORCED through each live resolution's item.
func (c pgCatalog) Actors(ctx context.Context, entityIDs []string, limit int) ([]string, error) {
	return c.distinctNames(ctx, "actors", func(a *sqlArgs) string {
		ent := ""
		if len(entityIDs) > 0 {
			ent = " AND r.entity_id = ANY(" + a.add(entityIDs) + ")"
		}
		return `SELECT DISTINCT r.actor FROM resolutions r
			LEFT JOIN knowledge_items ki ON ki.id = r.item_id
			WHERE r.system_to IS NULL` + ent + a.item(c.scope, "ki")
	}, limit)
}
