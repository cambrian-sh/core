package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// PgIdentityScanSource is domain.IdentityScanSource over Postgres: the background
// identity producers' scans (owner decision 2026-11-14, ENG-1393), moved here from
// premium's raw SQL so the substrate's read rules are applied by the substrate.
//
// Every query renders the same two rules the query plane does, through the same
// helpers: notRetired (ADR-0147 D6) on every typed-row read, and the sqlfence
// access predicate on every row, keyed by the evidence it rests on: viaTypedRow
// for a typed row (its own row classification can only narrow, ADR-0158 D2),
// viaEvidence for an entity's minting evidence, evidenceRow for evidence itself. Neither is optional and neither is the caller's to apply.
type PgIdentityScanSource struct {
	pool *pgxpool.Pool
}

var _ domain.IdentityScanSource = (*PgIdentityScanSource)(nil)

// NewPgIdentityScanSource returns the scan source over pool.
func NewPgIdentityScanSource(pool *pgxpool.Pool) *PgIdentityScanSource {
	return &PgIdentityScanSource{pool: pool}
}

// errIdentityScanNamespace refuses a scan with no namespace: every substrate row
// lives in one, and an unconfined scan is a read across tenants.
var errIdentityScanNamespace = errors.New("identity scan: namespace is required")

func (s *PgIdentityScanSource) check(scope *domain.TagPredicate, namespace string) error {
	if scope == nil {
		return domain.ErrQueryScopeMissing
	}
	if strings.TrimSpace(namespace) == "" {
		return errIdentityScanNamespace
	}
	if s == nil || s.pool == nil {
		return errors.New("identity scan: no store")
	}
	return nil
}

func identityScanErr(what string, err error) error {
	return fmt.Errorf("identity scan: %s: %w", what, err)
}

// AliasObservations implements domain.IdentityScanSource. value_text only: an
// alias is a name, and a number or a date is not one however its predicate is
// spelled.
func (s *PgIdentityScanSource) AliasObservations(ctx context.Context, scope *domain.TagPredicate, namespace string, predicates []string, afterID int64, limit int) ([]domain.IdentityAliasRow, error) {
	if err := s.check(scope, namespace); err != nil {
		return nil, err
	}
	if len(predicates) == 0 {
		return nil, nil
	}
	a := &sqlArgs{}
	q := `SELECT o.id, o.entity_id, o.value_text, COALESCE(o.evidence_id, ''), o.occurred_at
		 FROM observations o
		 WHERE o.namespace_id=` + a.add(namespace) + `::text
		   AND o.predicate = ANY(` + a.add(predicates) + `::text[])
		   AND o.value_text IS NOT NULL
		   AND o.id > ` + a.add(afterID) + `::bigint` +
		notRetired("o") + a.viaTypedRow(scope, "o.evidence_id", "o.classification") + `
		 ORDER BY o.id
		 LIMIT ` + a.add(limit) + `::int`
	rows, err := s.pool.Query(ctx, q, a.args...)
	if err != nil {
		return nil, identityScanErr("alias observations", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.IdentityAliasRow, error) {
		var o domain.IdentityAliasRow
		err := r.Scan(&o.ID, &o.EntityID, &o.Value, &o.EvidenceID, &o.OccurredAt)
		return o, err
	})
}

// EntitiesSince implements domain.IdentityScanSource. An entity is fenced by the
// evidence that minted it (the query plane's mayKnowEntityExists rule): a handle
// with no first-seen evidence is visible only to ScopeMaintenance.
func (s *PgIdentityScanSource) EntitiesSince(ctx context.Context, scope *domain.TagPredicate, namespace string, after domain.IdentityScanPosition, limit int) ([]domain.IdentityEntityRow, error) {
	if err := s.check(scope, namespace); err != nil {
		return nil, err
	}
	a := &sqlArgs{}
	q := `SELECT en.id, en.kind, en.created_at, COALESCE(en.first_seen_evidence, '')
		 FROM entities en
		 WHERE en.namespace_id=` + a.add(namespace) + `::text
		   AND (en.created_at, en.id) > (` + a.add(after.At) + `::timestamptz, ` + a.add(after.ID) + `::text)` +
		a.viaEvidence(scope, "en.first_seen_evidence") + `
		 ORDER BY en.created_at, en.id
		 LIMIT ` + a.add(limit) + `::int`
	return s.entities(ctx, "entities", q, a.args)
}

// PartiesForEntity implements domain.IdentityScanSource. Three routes reach an
// evidence row from a handle and all three count: it was minted by that delivery,
// an observation about it cites it, or it played a role in an event citing it.
// The evidence row itself is fenced, so a party is only ever read off evidence the
// scope may see; and the observation and event routes count only rows the scope
// may see (the typed-row fence, ADR-0158 D2), so a row its mapping restricted
// lends its evidence's parties to no reader it is hidden from.
func (s *PgIdentityScanSource) PartiesForEntity(ctx context.Context, scope *domain.TagPredicate, namespace, entityID string, limit int) ([]domain.IdentityPartyRow, error) {
	if err := s.check(scope, namespace); err != nil {
		return nil, err
	}
	a := &sqlArgs{}
	ns, ent := a.add(namespace), a.add(entityID)
	q := `WITH ev AS (
			SELECT en.first_seen_evidence AS id FROM entities en
			  WHERE en.namespace_id=` + ns + `::text AND en.id=` + ent + `::text AND en.first_seen_evidence IS NOT NULL
			UNION
			SELECT o.evidence_id FROM observations o
			  WHERE o.namespace_id=` + ns + `::text AND o.entity_id=` + ent + `::text AND o.evidence_id IS NOT NULL` + notRetired("o") +
		a.viaTypedRow(scope, "o.evidence_id", "o.classification") + `
			UNION
			SELECT x.evidence_id FROM events x
			  JOIN event_roles r ON r.event_id = x.id
			  WHERE x.namespace_id=` + ns + `::text AND r.entity_id=` + ent + `::text AND x.evidence_id IS NOT NULL` + notRetired("x") +
		a.viaTypedRow(scope, "x.evidence_id", "x.classification") + `
		)
		SELECT DISTINCT p.party, e.id
		FROM evidence e
		JOIN ev ON ev.id = e.id
		CROSS JOIN LATERAL unnest(e.parties) AS p(party)
		WHERE e.namespace_id=` + ns + `::text` + a.evidenceRow(scope, "e") + `
		ORDER BY p.party, e.id
		LIMIT ` + a.add(limit) + `::int`
	rows, err := s.pool.Query(ctx, q, a.args...)
	if err != nil {
		return nil, identityScanErr("an entity's parties", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.IdentityPartyRow, error) {
		var p domain.IdentityPartyRow
		err := r.Scan(&p.Party, &p.EvidenceID)
		return p, err
	})
}

// identityPartyEvidenceScanLimit bounds how much evidence one party's reverse
// lookup reads: the newest rows, where a party's current handles appear.
const identityPartyEvidenceScanLimit = 500

// EntitiesWithParty implements domain.IdentityScanSource: the mirror of
// PartiesForEntity, walked from the party. The candidate evidence rows are fenced
// before they are joined, an observation or event route counts only a row the
// scope may see (ADR-0158 D2), and the entities returned are fenced by their own
// first-seen evidence like every other entity read here.
func (s *PgIdentityScanSource) EntitiesWithParty(ctx context.Context, scope *domain.TagPredicate, namespace, party, kind string, limit int) ([]domain.IdentityEntityRow, error) {
	if err := s.check(scope, namespace); err != nil {
		return nil, err
	}
	a := &sqlArgs{}
	ns := a.add(namespace)
	q := `WITH ev AS (
			SELECT e.id FROM evidence e
			WHERE e.namespace_id=` + ns + `::text AND e.parties && ARRAY[` + a.add(party) + `::text]` + a.evidenceRow(scope, "e") + `
			ORDER BY e.ingested_at DESC
			LIMIT ` + a.add(identityPartyEvidenceScanLimit) + `::int
		), ent AS (
			SELECT en.id AS entity_id FROM entities en
			  JOIN ev ON ev.id = en.first_seen_evidence
			  WHERE en.namespace_id=` + ns + `::text
			UNION
			SELECT o.entity_id FROM observations o
			  JOIN ev ON ev.id = o.evidence_id
			  WHERE o.namespace_id=` + ns + `::text` + notRetired("o") +
		a.viaTypedRow(scope, "o.evidence_id", "o.classification") + `
			UNION
			SELECT r.entity_id FROM event_roles r
			  JOIN events x ON x.id = r.event_id
			  JOIN ev ON ev.id = x.evidence_id
			  WHERE x.namespace_id=` + ns + `::text` + notRetired("x") +
		a.viaTypedRow(scope, "x.evidence_id", "x.classification") + `
		)
		SELECT en.id, en.kind, en.created_at, COALESCE(en.first_seen_evidence, '')
		FROM entities en
		JOIN ent ON ent.entity_id = en.id
		WHERE en.namespace_id=` + ns + `::text AND en.kind=` + a.add(kind) + `::text` +
		a.viaEvidence(scope, "en.first_seen_evidence") + `
		ORDER BY en.created_at, en.id
		LIMIT ` + a.add(limit) + `::int`
	return s.entities(ctx, "entities by party", q, a.args)
}

func (s *PgIdentityScanSource) entities(ctx context.Context, what, q string, args []any) ([]domain.IdentityEntityRow, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, identityScanErr(what, err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.IdentityEntityRow, error) {
		var e domain.IdentityEntityRow
		err := r.Scan(&e.ID, &e.Kind, &e.CreatedAt, &e.FirstSeenEvidence)
		return e, err
	})
	if err != nil {
		return nil, identityScanErr(what, err)
	}
	return out, nil
}

// EventsSince implements domain.IdentityScanSource.
func (s *PgIdentityScanSource) EventsSince(ctx context.Context, scope *domain.TagPredicate, namespace string, after domain.IdentityScanPosition, limit int) ([]domain.IdentityEventRow, error) {
	if err := s.check(scope, namespace); err != nil {
		return nil, err
	}
	a := &sqlArgs{}
	q := `SELECT x.id, x.event_type, x.occurred_at, COALESCE(x.evidence_id, '')
		 FROM events x
		 WHERE x.namespace_id=` + a.add(namespace) + `::text
		   AND (x.occurred_at, x.id) > (` + a.add(after.At) + `::timestamptz, ` + a.add(after.ID) + `::text)` +
		notRetired("x") + a.viaTypedRow(scope, "x.evidence_id", "x.classification") + `
		 ORDER BY x.occurred_at, x.id
		 LIMIT ` + a.add(limit) + `::int`
	return s.events(ctx, "events", q, a.args)
}

// RoleEntities implements domain.IdentityScanSource. The event is read under the
// same rules as any other (namespace, retirement, fence), so a retired or
// invisible event has no participants to report.
func (s *PgIdentityScanSource) RoleEntities(ctx context.Context, scope *domain.TagPredicate, namespace, eventID string, limit int) ([]string, error) {
	if err := s.check(scope, namespace); err != nil {
		return nil, err
	}
	a := &sqlArgs{}
	q := `SELECT r.entity_id FROM event_roles r
		 JOIN events x ON x.id = r.event_id
		 WHERE x.namespace_id=` + a.add(namespace) + `::text AND r.event_id=` + a.add(eventID) + `::text` +
		notRetired("x") + a.viaTypedRow(scope, "x.evidence_id", "x.classification") + `
		 ORDER BY r.role, r.entity_id
		 LIMIT ` + a.add(limit) + `::int`
	rows, err := s.pool.Query(ctx, q, a.args...)
	if err != nil {
		return nil, identityScanErr("event roles", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// PriorEventsSharingEntity implements domain.IdentityScanSource. Strictly before
// the position by the SAME (time, id) comparison the scan orders by, so an event
// never correlates with itself and two events at one instant still have a defined
// order.
func (s *PgIdentityScanSource) PriorEventsSharingEntity(ctx context.Context, scope *domain.TagPredicate, namespace string, entities []string, before domain.IdentityScanPosition, since time.Time, limit int) ([]domain.IdentityEventRow, error) {
	if err := s.check(scope, namespace); err != nil {
		return nil, err
	}
	if len(entities) == 0 {
		return nil, nil
	}
	a := &sqlArgs{}
	q := `SELECT DISTINCT x.id, x.event_type, x.occurred_at, COALESCE(x.evidence_id, '')
		 FROM events x
		 JOIN event_roles r ON r.event_id = x.id
		 WHERE x.namespace_id=` + a.add(namespace) + `::text
		   AND r.entity_id = ANY(` + a.add(entities) + `::text[])
		   AND x.occurred_at >= ` + a.add(since) + `::timestamptz
		   AND (x.occurred_at, x.id) < (` + a.add(before.At) + `::timestamptz, ` + a.add(before.ID) + `::text)` +
		notRetired("x") + a.viaTypedRow(scope, "x.evidence_id", "x.classification") + `
		 ORDER BY x.occurred_at DESC, x.id DESC
		 LIMIT ` + a.add(limit) + `::int`
	return s.events(ctx, "prior events", q, a.args)
}

func (s *PgIdentityScanSource) events(ctx context.Context, what, q string, args []any) ([]domain.IdentityEventRow, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, identityScanErr(what, err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.IdentityEventRow, error) {
		var e domain.IdentityEventRow
		err := r.Scan(&e.ID, &e.EventType, &e.OccurredAt, &e.EvidenceID)
		return e, err
	})
	if err != nil {
		return nil, identityScanErr(what, err)
	}
	return out, nil
}

const identityScanLinkColumns = `l.id, l.namespace_id, l.family, l.from_ref, l.to_ref, l.relation, l.state, l.mechanism,
		        l.producer, l.confidence, COALESCE(l.evidence_id, ''), l.asserted_by, l.asserted_at,
		        l.recorded_at, l.valid_from, l.valid_to, l.source_ref`

// ConfirmedIdentityLinks implements domain.IdentityScanSource. A link is fenced
// on its own evidence (the query plane's filterLinks rule), so an evidence-less
// human link is visible only to ScopeMaintenance.
func (s *PgIdentityScanSource) ConfirmedIdentityLinks(ctx context.Context, scope *domain.TagPredicate, namespace string, limit int) ([]domain.Link, error) {
	if err := s.check(scope, namespace); err != nil {
		return nil, err
	}
	a := &sqlArgs{}
	q := `SELECT ` + identityScanLinkColumns + `
		 FROM links l
		 WHERE l.namespace_id=` + a.add(namespace) + `::text AND l.family=` + a.add(domain.LinkFamilyIdentity) + `::text
		   AND l.state=` + a.add(domain.LinkStateConfirmed) + `::text AND l.retracted_at IS NULL` +
		a.viaTypedRow(scope, "l.evidence_id", "l.classification") + `
		 ORDER BY l.id
		 LIMIT ` + a.add(limit) + `::int`
	return s.links(ctx, "confirmed identity links", q, a.args)
}

// CandidateLinksSince implements domain.IdentityScanSource. recorded_at, not
// asserted_at: the latter is the source's clock, and a backfill writes rows behind
// everything already scanned.
func (s *PgIdentityScanSource) CandidateLinksSince(ctx context.Context, scope *domain.TagPredicate, namespace string, mechanisms []string, after domain.IdentityScanPosition, limit int) ([]domain.Link, error) {
	if err := s.check(scope, namespace); err != nil {
		return nil, err
	}
	a := &sqlArgs{}
	q := `SELECT ` + identityScanLinkColumns + `
		 FROM links l
		 WHERE l.namespace_id=` + a.add(namespace) + `::text AND l.state=` + a.add(domain.LinkStateCandidate) + `::text
		   AND l.retracted_at IS NULL
		   AND l.mechanism = ANY(` + a.add(mechanisms) + `::text[])
		   AND (l.recorded_at, l.id) > (` + a.add(after.At) + `::timestamptz, ` + a.add(after.ID) + `::text)` +
		a.viaTypedRow(scope, "l.evidence_id", "l.classification") + `
		 ORDER BY l.recorded_at, l.id
		 LIMIT ` + a.add(limit) + `::int`
	return s.links(ctx, "candidate links", q, a.args)
}

func (s *PgIdentityScanSource) links(ctx context.Context, what, q string, args []any) ([]domain.Link, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, identityScanErr(what, err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.Link, error) {
		var l domain.Link
		var evidence string
		err := r.Scan(&l.ID, &l.NamespaceID, &l.Family, &l.FromRef, &l.ToRef,
			&l.Relation, &l.State, &l.Mechanism, &l.Producer, &l.Confidence, &evidence,
			&l.AssertedBy, &l.AssertedAt, &l.RecordedAt, &l.ValidFrom, &l.ValidTo, &l.SourceRef)
		l.EvidenceID = domain.EvidenceID(evidence)
		return l, err
	})
	if err != nil {
		return nil, identityScanErr(what, err)
	}
	return out, nil
}

// EntityExists implements domain.IdentityScanSource.
func (s *PgIdentityScanSource) EntityExists(ctx context.Context, scope *domain.TagPredicate, namespace, id string) (bool, error) {
	if err := s.check(scope, namespace); err != nil {
		return false, err
	}
	a := &sqlArgs{}
	q := `SELECT EXISTS (SELECT 1 FROM entities en
		 WHERE en.namespace_id=` + a.add(namespace) + `::text AND en.id=` + a.add(id) + `::text` +
		a.viaEvidence(scope, "en.first_seen_evidence") + `)`
	var exists bool
	if err := s.pool.QueryRow(ctx, q, a.args...).Scan(&exists); err != nil {
		return false, identityScanErr("entity exists", err)
	}
	return exists, nil
}

// EntitiesByLocal implements domain.IdentityScanSource. A LIKE with the local
// part escaped: a local id comes off a source's text and may carry % or _, and an
// unescaped pattern would turn "OPS_412" into a wildcard. Exactly one hit is a
// resolution; anything else is an ambiguity for a human.
func (s *PgIdentityScanSource) EntitiesByLocal(ctx context.Context, scope *domain.TagPredicate, namespace, local string, limit int) ([]string, error) {
	if err := s.check(scope, namespace); err != nil {
		return nil, err
	}
	a := &sqlArgs{}
	q := `SELECT en.id FROM entities en
		 WHERE en.namespace_id=` + a.add(namespace) + `::text AND en.id LIKE ` + a.add("%/"+escapeLikeLiteral(local)) + `::text ESCAPE '\'` +
		a.viaEvidence(scope, "en.first_seen_evidence") + `
		 ORDER BY en.id
		 LIMIT ` + a.add(limit) + `::int`
	rows, err := s.pool.Query(ctx, q, a.args...)
	if err != nil {
		return nil, identityScanErr("entities by local id", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// escapeLikeLiteral makes source text safe inside a LIKE pattern with ESCAPE '\':
// the escape character itself is doubled first, then % and _ are escaped. (The
// premium scanner this replaced mapped `\` to `\`, so a local id ending in a
// backslash escaped the next pattern character.)
func escapeLikeLiteral(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
