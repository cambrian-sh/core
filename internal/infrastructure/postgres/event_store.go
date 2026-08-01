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
	pool *pgxpool.Pool
	reg  *domain.KindRegistry
}

// ObservationKind is the reserved registry kind observations validate under
// (ADR-0110 D2): observations carry a predicate but no item kind, so their
// value constraints are declared once, under this name.
const ObservationKind = "observation"

// NewPgEventStore wraps an existing pool. reg may be nil (no declared kinds).
func NewPgEventStore(pool *pgxpool.Pool, reg *domain.KindRegistry) *PgEventStore {
	return &PgEventStore{pool: pool, reg: reg}
}

var _ domain.EventStore = (*PgEventStore)(nil)

// RecordEvent inserts the event and its roles in one transaction, idempotent
// on (namespace, source_ref).
func (s *PgEventStore) RecordEvent(ctx context.Context, ev domain.Event) (domain.EventID, bool, error) {
	if ev.Type == "" || ev.OccurredAt.IsZero() {
		return "", false, fmt.Errorf("event record: type and occurred_at are required")
	}
	ns := ev.NamespaceID
	if ns == "" {
		ns = "default"
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
		INSERT INTO events (id, namespace_id, event_type, occurred_at, evidence_id, source_ref)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (namespace_id, source_ref) WHERE source_ref <> '' DO NOTHING
		RETURNING id`,
		string(id), ns, ev.Type, ev.OccurredAt.UTC(), evidenceID, ev.SourceRef,
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
	// ADR-0110 D2, under the reserved "observation" kind — per-PREDICATE
	// opt-in: a declared predicate is constrained, an undeclared one passes,
	// so declaring temperature_c cannot break an unrelated stream (monotonic
	// adoption at observation granularity).
	if spec, ok := s.reg.Spec(ObservationKind); ok {
		if _, declared := spec.Predicates[ob.Predicate]; declared {
			v := ob.Value
			v.Predicate = ob.Predicate
			if err := s.reg.ValidateValues(ObservationKind, []domain.StatementValue{v}); err != nil {
				return false, err
			}
		}
	}
	ns := ob.NamespaceID
	if ns == "" {
		ns = "default"
	}
	var date *time.Time
	var num *float64
	var text, ent *string
	switch ob.Value.Type {
	case "date":
		d := ob.Value.Date.UTC()
		date = &d
	case "number":
		n := ob.Value.Number
		num = &n
	case "text":
		t := ob.Value.Text
		text = &t
	case "entity":
		e := ob.Value.EntityRef
		ent = &e
	default:
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
			location, occurred_at, confidence, evidence_id, source_ref)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (namespace_id, source_ref, occurred_at) WHERE source_ref <> '' DO NOTHING`,
		ns, ob.EntityID, ob.Predicate, ob.Value.Type,
		date, num, text, ent,
		ob.Location, ob.OccurredAt.UTC(), conf, evidenceID, ob.SourceRef)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

const observationColumns = `namespace_id, entity_id, predicate, value_type,
	value_date, value_number, value_text, value_entity_id,
	location, occurred_at, confidence, evidence_id, source_ref`

func scanObservation(row pgx.Row) (*domain.Observation, error) {
	var ob domain.Observation
	var date *time.Time
	var num *float64
	var text, ent, evidenceID *string
	if err := row.Scan(&ob.NamespaceID, &ob.EntityID, &ob.Predicate, &ob.Value.Type,
		&date, &num, &text, &ent,
		&ob.Location, &ob.OccurredAt, &ob.Confidence, &evidenceID, &ob.SourceRef); err != nil {
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
	if namespace == "" {
		namespace = "default"
	}
	ob, err := scanObservation(s.pool.QueryRow(ctx, `
		SELECT `+observationColumns+` FROM observations
		WHERE namespace_id=$1 AND entity_id=$2 AND predicate=$3
		ORDER BY occurred_at DESC, id DESC LIMIT 1`,
		namespace, entityID, predicate))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return ob, err
}

// History: exact range scan, oldest first.
func (s *PgEventStore) History(ctx context.Context, namespace, entityID, predicate string, from, to time.Time) ([]domain.Observation, error) {
	if namespace == "" {
		namespace = "default"
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+observationColumns+` FROM observations
		WHERE namespace_id=$1 AND entity_id=$2 AND predicate=$3
		  AND occurred_at >= $4 AND occurred_at < $5
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
func (s *PgEventStore) EventsForEntity(ctx context.Context, namespace, entityID string, from, to time.Time) ([]domain.Event, error) {
	if namespace == "" {
		namespace = "default"
	}
	rows, err := s.pool.Query(ctx, `
		SELECT e.id, e.namespace_id, e.event_type, e.occurred_at, e.evidence_id, e.source_ref
		FROM events e
		WHERE e.namespace_id=$1 AND e.occurred_at >= $2 AND e.occurred_at < $3
		  AND EXISTS (SELECT 1 FROM event_roles r WHERE r.event_id = e.id AND r.entity_id = $4)
		ORDER BY e.occurred_at, e.id`,
		namespace, from.UTC(), to.UTC(), entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Event
	for rows.Next() {
		var ev domain.Event
		var id string
		var evidenceID *string
		if err := rows.Scan(&id, &ev.NamespaceID, &ev.Type, &ev.OccurredAt, &evidenceID, &ev.SourceRef); err != nil {
			return nil, err
		}
		ev.ID = domain.EventID(id)
		if evidenceID != nil {
			ev.EvidenceID = domain.EvidenceID(*evidenceID)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		rrows, err := s.pool.Query(ctx,
			`SELECT role, entity_id FROM event_roles WHERE event_id=$1 ORDER BY role, entity_id`,
			string(out[i].ID))
		if err != nil {
			return nil, err
		}
		for rrows.Next() {
			var r domain.EventRole
			if err := rrows.Scan(&r.Role, &r.EntityID); err != nil {
				rrows.Close()
				return nil, err
			}
			out[i].Roles = append(out[i].Roles, r)
		}
		rrows.Close()
		if err := rrows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
