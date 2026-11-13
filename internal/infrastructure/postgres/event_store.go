package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// PgEventStore is the Postgres adapter for domain.EventStore (ADR-0108).
// Tables come from migration 0014; no DDL here. Every read is exact over
// stored rows — no vectors, no ranking, no model anywhere in this file.
type PgEventStore struct {
	pool  *pgxpool.Pool
	kinds domain.KindSource
}

// ObservationKind is the reserved registry kind observations validate under
// (ADR-0110 D2): observations carry a predicate but no item kind, so their
// value constraints are declared once, under this name.
const ObservationKind = "observation"

// NewPgEventStore wraps an existing pool. kinds is the live ontology or a fixed
// *domain.KindRegistry; nil means no declared kinds.
func NewPgEventStore(pool *pgxpool.Pool, kinds domain.KindSource) *PgEventStore {
	if kinds == nil {
		kinds = (*domain.KindRegistry)(nil)
	}
	return &PgEventStore{pool: pool, kinds: kinds}
}

var _ domain.EventStore = (*PgEventStore)(nil)

// RecordEvent inserts the event and its roles in one transaction, idempotent
// on (namespace, source_ref).
func (s *PgEventStore) RecordEvent(ctx context.Context, ev domain.Event) (domain.EventID, bool, error) {
	if ev.Type == "" || ev.OccurredAt.IsZero() {
		return "", false, fmt.Errorf("event record: type and occurred_at are required")
	}
	if err := checkTimeBasis(ev.TimeBasis); err != nil {
		return "", false, fmt.Errorf("event record: %w", err)
	}
	// A DECLARED event type (KindSpec.EventTypes) must carry its declared roles;
	// an undeclared type passes (ADR-0110 D2 monotonic adoption). The refusal
	// names the constraint and wraps domain.ErrKindRefused.
	ns := orDefaultNamespace(ev.NamespaceID)
	reg := s.kinds.KindsFor(ns)
	if err := reg.ValidateEvent(ev); err != nil {
		return "", false, err
	}
	id := ev.ID
	if id == "" {
		id = domain.EventID("evt_" + uuid.NewString())
	}
	var evidenceID *string
	if ev.EvidenceID != "" {
		e := string(ev.EvidenceID)
		evidenceID = &e
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit

	var gotID string
	err = tx.QueryRow(ctx, `
		INSERT INTO events (id, namespace_id, event_type, occurred_at, evidence_id, source_ref, time_basis, decl_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (namespace_id, source_ref) WHERE source_ref <> '' DO NOTHING
		RETURNING id`,
		string(id), ns, ev.Type, ev.OccurredAt.UTC(), evidenceID, ev.SourceRef, ev.TimeBasis,
		declVersionOrNil(reg.Version()),
	).Scan(&gotID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.QueryRow(ctx,
			`SELECT id FROM events WHERE namespace_id=$1 AND source_ref=$2`,
			ns, ev.SourceRef).Scan(&gotID); err != nil {
			return "", false, fmt.Errorf("event replay lookup: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return "", false, err
		}
		return domain.EventID(gotID), false, nil
	}
	if err != nil {
		return "", false, err
	}
	for _, r := range ev.Roles {
		if r.Role == "" || r.EntityID == "" {
			return "", false, fmt.Errorf("event record: a role needs both role and entity_id")
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO event_roles (event_id, role, entity_id) VALUES ($1,$2,$3)`,
			gotID, r.Role, r.EntityID); err != nil {
			return "", false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return domain.EventID(gotID), true, nil
}

// RecordObservation appends one observation, idempotent on (namespace, source_ref).
func (s *PgEventStore) RecordObservation(ctx context.Context, ob domain.Observation) (bool, error) {
	if ob.EntityID == "" || ob.Predicate == "" || ob.OccurredAt.IsZero() {
		return false, fmt.Errorf("observation record: entity_id, predicate and occurred_at are required")
	}
	if err := checkTimeBasis(ob.TimeBasis); err != nil {
		return false, fmt.Errorf("observation record: %w", err)
	}
	// ADR-0110 D2, under the reserved "observation" kind — per-PREDICATE
	// opt-in: a declared predicate is constrained, an undeclared one passes,
	// so declaring temperature_c cannot break an unrelated stream (monotonic
	// adoption at observation granularity).
	ns := orDefaultNamespace(ob.NamespaceID)
	reg := s.kinds.KindsFor(ns)
	if spec, ok := reg.Spec(ObservationKind); ok {
		if _, declared := spec.Predicates[ob.Predicate]; declared {
			v := ob.Value
			v.Predicate = ob.Predicate
			if err := reg.ValidateValues(ObservationKind, []domain.StatementValue{v}); err != nil {
				return false, err
			}
		}
	}
	date, num, text, ent, ok := typedValueColumns(ob.Value)
	if !ok {
		return false, fmt.Errorf("observation record: unknown value type %q", ob.Value.Type)
	}
	conf := ob.Confidence
	if conf == 0 {
		conf = 1.0
	}
	var evidenceID *string
	if ob.EvidenceID != "" {
		e := string(ob.EvidenceID)
		evidenceID = &e
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO observations (namespace_id, entity_id, predicate, value_type,
			value_date, value_number, value_text, value_entity_id,
			location, occurred_at, confidence, evidence_id, source_ref, time_basis, decl_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT (namespace_id, source_ref, occurred_at) WHERE source_ref <> '' DO NOTHING`,
		ns, ob.EntityID, ob.Predicate, ob.Value.Type,
		date, num, text, ent,
		ob.Location, ob.OccurredAt.UTC(), conf, evidenceID, ob.SourceRef, ob.TimeBasis,
		declVersionOrNil(reg.Version()))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

const observationColumns = `namespace_id, entity_id, predicate, value_type,
	value_date, value_number, value_text, value_entity_id,
	location, occurred_at, confidence, evidence_id, source_ref, time_basis`

// notRetired is the predicate every counted read of a typed row carries
// (ADR-0147 D6): the row's source_ref has no live retirement. alias is the
// table alias, or the table name when the query has none. A row with an empty
// source_ref can never be retired, so it always passes.
func notRetired(alias string) string {
	return ` AND NOT EXISTS (SELECT 1 FROM typed_row_retirements rt
		WHERE rt.namespace_id = ` + alias + `.namespace_id AND rt.source_ref = ` + alias + `.source_ref
		  AND rt.reinstated_at IS NULL)`
}

// checkTimeBasis refuses a time basis the domain does not name, so a typo in a
// writer cannot mint a third clock nobody reads.
func checkTimeBasis(b string) error {
	switch b {
	case "", domain.TimeBasisEvent, domain.TimeBasisReceived:
		return nil
	}
	return fmt.Errorf("unknown time basis %q (want %q or %q)", b, domain.TimeBasisEvent, domain.TimeBasisReceived)
}

func scanObservation(row pgx.Row) (*domain.Observation, error) {
	var ob domain.Observation
	var date *time.Time
	var num *float64
	var text, ent, evidenceID *string
	if err := row.Scan(&ob.NamespaceID, &ob.EntityID, &ob.Predicate, &ob.Value.Type,
		&date, &num, &text, &ent,
		&ob.Location, &ob.OccurredAt, &ob.Confidence, &evidenceID, &ob.SourceRef, &ob.TimeBasis); err != nil {
		return nil, err
	}
	switch {
	case date != nil:
		ob.Value.Date = *date
	case num != nil:
		ob.Value.Number = *num
	case text != nil:
		ob.Value.Text = *text
	case ent != nil:
		ob.Value.EntityRef = *ent
	}
	if evidenceID != nil {
		ob.EvidenceID = domain.EvidenceID(*evidenceID)
	}
	return &ob, nil
}

// PointLookup: the exact latest STORED observation; nil when none — an answer,
// not an error.
func (s *PgEventStore) PointLookup(ctx context.Context, namespace, entityID, predicate string) (*domain.Observation, error) {
	namespace = orDefaultNamespace(namespace)
	ob, err := scanObservation(s.pool.QueryRow(ctx, `
		SELECT `+observationColumns+` FROM observations
		WHERE namespace_id=$1 AND entity_id=$2 AND predicate=$3`+notRetired("observations")+`
		ORDER BY occurred_at DESC, id DESC LIMIT 1`,
		namespace, entityID, predicate))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return ob, err
}

// History: exact range scan, oldest first.
func (s *PgEventStore) History(ctx context.Context, namespace, entityID, predicate string, from, to time.Time) ([]domain.Observation, error) {
	namespace = orDefaultNamespace(namespace)
	rows, err := s.pool.Query(ctx, `
		SELECT `+observationColumns+` FROM observations
		WHERE namespace_id=$1 AND entity_id=$2 AND predicate=$3
		  AND occurred_at >= $4 AND occurred_at < $5`+notRetired("observations")+`
		ORDER BY occurred_at, id`,
		namespace, entityID, predicate, from.UTC(), to.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Observation
	for rows.Next() {
		ob, err := scanObservation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ob)
	}
	return out, rows.Err()
}

// EventsForEntity: every event the entity participated in, roles hydrated.
// It is EventsForEntities with one id, so the two never disagree.
func (s *PgEventStore) EventsForEntity(ctx context.Context, namespace, entityID string, from, to time.Time) ([]domain.Event, error) {
	byEntity, err := s.EventsForEntities(ctx, namespace, []string{entityID}, from, to)
	if err != nil {
		return nil, err
	}
	return byEntity[entityID], nil
}

// EventsForEntities is EventsForEntity over a set of entities in two round
// trips whatever the set's size: one statement for the events (each entity's
// own [from, to) range, oldest first, retired rows excluded — ADR-0147 D6 via
// notRetired) and one for every returned event's roles. The query plane's
// traverse reads a whole frontier through it (T-20261112-traverse-still-reads-
// events-once-per-frontier); before it, a hop cost one statement per frontier
// entity plus one per event.
//
// An event several requested entities took part in is returned under each of
// them, sharing one Roles slice — callers must not mutate it.
func (s *PgEventStore) EventsForEntities(ctx context.Context, namespace string, entityIDs []string, from, to time.Time) (map[string][]domain.Event, error) {
	namespace = orDefaultNamespace(namespace)
	out := map[string][]domain.Event{}
	if len(entityIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT r.entity_id, `+eventColumnsOn("e")+`
		FROM (SELECT DISTINCT event_id, entity_id FROM event_roles WHERE entity_id = ANY($4::text[])) r
		JOIN events e ON e.id = r.event_id
		WHERE e.namespace_id=$1 AND e.occurred_at >= $2 AND e.occurred_at < $3`+notRetired("e")+`
		ORDER BY r.entity_id, e.occurred_at, e.id`,
		namespace, from.UTC(), to.UTC(), entityIDs)
	if err != nil {
		return nil, err
	}
	type keyed struct {
		entity string
		ev     domain.Event
	}
	var all []keyed
	var ids []string
	seen := map[string]bool{}
	for rows.Next() {
		var k keyed
		ev, err := scanTypedEvent(rows, &k.entity)
		if err != nil {
			rows.Close()
			return nil, err
		}
		k.ev = ev
		all = append(all, k)
		if id := string(ev.ID); !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	roles, err := eventRoles(ctx, s.pool, ids)
	if err != nil {
		return nil, err
	}
	for _, k := range all {
		k.ev.Roles = roles[string(k.ev.ID)]
		out[k.entity] = append(out[k.entity], k.ev)
	}
	return out, nil
}

// eventColumnsOn is the event row as scanTypedEvent reads it, over the events table
// aliased alias. Shared with the query plane's fenced events read, so the two
// never scan different columns.
func eventColumnsOn(alias string) string {
	return alias + ".id, " + alias + ".namespace_id, " + alias + ".event_type, " + alias + ".occurred_at, " +
		alias + ".evidence_id, " + alias + ".source_ref, " + alias + ".time_basis"
}

// scanTypedEvent scans eventColumnsOn, after any leading columns the caller selected
// first (lead), into an Event without roles.
func scanTypedEvent(rows pgx.Rows, lead ...any) (domain.Event, error) {
	var ev domain.Event
	var id string
	var evidenceID *string
	dest := append(lead, &id, &ev.NamespaceID, &ev.Type, &ev.OccurredAt, &evidenceID, &ev.SourceRef, &ev.TimeBasis)
	if err := rows.Scan(dest...); err != nil {
		return domain.Event{}, err
	}
	ev.ID = domain.EventID(id)
	if evidenceID != nil {
		ev.EvidenceID = domain.EvidenceID(*evidenceID)
	}
	return ev, nil
}

// eventRoles loads the roles of a set of events in ONE statement, each event's
// roles ordered by role then entity — the order EventsForEntity always returned.
func eventRoles(ctx context.Context, q pgxQuerier, eventIDs []string) (map[string][]domain.EventRole, error) {
	out := map[string][]domain.EventRole{}
	if len(eventIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx,
		`SELECT event_id, role, entity_id FROM event_roles WHERE event_id = ANY($1::text[])
		ORDER BY event_id, role, entity_id`, eventIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var r domain.EventRole
		if err := rows.Scan(&id, &r.Role, &r.EntityID); err != nil {
			return nil, err
		}
		out[id] = append(out[id], r)
	}
	return out, rows.Err()
}

// RetireRows records that the rows written under sourceRefs no longer count
// (ADR-0147 D6, D11). One statement, so a batch is retired whole or not at all.
// A ref that is already retired keeps its first record; one that was reinstated
// is retired again with this call's reason.
func (s *PgEventStore) RetireRows(ctx context.Context, namespace string, sourceRefs []string, reason, retiredBy string) (int, error) {
	switch reason {
	case domain.RetireSuperseded, domain.RetireSourceDeleted:
	default:
		return 0, fmt.Errorf("retire rows: unknown reason %q (want %q or %q)",
			reason, domain.RetireSuperseded, domain.RetireSourceDeleted)
	}
	refs := nonEmpty(sourceRefs) // fact_backfill_lister.go; an empty ref cannot be retired
	if len(refs) == 0 {
		return 0, nil
	}
	namespace = orDefaultNamespace(namespace)
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO typed_row_retirements (namespace_id, source_ref, reason, retired_by)
		SELECT $1, ref, $3, $4 FROM unnest($2::text[]) AS ref
		ON CONFLICT (namespace_id, source_ref) DO UPDATE SET
			reason = EXCLUDED.reason, retired_by = EXCLUDED.retired_by,
			retired_at = now(), reinstated_at = NULL
		WHERE typed_row_retirements.reinstated_at IS NOT NULL`,
		namespace, refs, reason, retiredBy)
	if err != nil {
		return 0, fmt.Errorf("retire rows: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ReinstateRows lifts the live retirement of each ref, keeping the record that
// it was once retired.
func (s *PgEventStore) ReinstateRows(ctx context.Context, namespace string, sourceRefs []string) (int, error) {
	refs := nonEmpty(sourceRefs) // fact_backfill_lister.go; an empty ref cannot be retired
	if len(refs) == 0 {
		return 0, nil
	}
	namespace = orDefaultNamespace(namespace)
	tag, err := s.pool.Exec(ctx, `
		UPDATE typed_row_retirements SET reinstated_at = now()
		WHERE namespace_id = $1 AND source_ref = ANY($2::text[]) AND reinstated_at IS NULL`,
		namespace, refs)
	if err != nil {
		return 0, fmt.Errorf("reinstate rows: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
