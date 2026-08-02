package postgres

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// PgKnowledgeStore is the Postgres adapter for domain.KnowledgeStore
// (ADR-0106). Tables come from migration 0012; no DDL here.
type PgKnowledgeStore struct {
	pool *pgxpool.Pool
	// reg validates declared kinds and resolves policies to authorities
	// (ADR-0110). A constructor argument, not a setter: a store that can gain
	// a registry mid-flight can disagree with itself between two writes.
	reg *domain.KindRegistry
}

// NewPgKnowledgeStore wraps an existing pool. reg may be nil (no declared
// kinds; every policy resolves to latest_assertion).
func NewPgKnowledgeStore(pool *pgxpool.Pool, reg *domain.KindRegistry) *PgKnowledgeStore {
	return &PgKnowledgeStore{pool: pool, reg: reg}
}

var _ domain.KnowledgeStore = (*PgKnowledgeStore)(nil)

// PutItem appends one item and re-derives the resolution for its key from the
// FULL item set via domain.ResolveLatestAssertion — never by comparing against
// "the prior row", which is the arrival-order bug the memo forbids (§13).
func (s *PgKnowledgeStore) PutItem(ctx context.Context, item domain.KnowledgeItem) (domain.KnowledgeItemID, bool, error) {
	if item.Kind == "" || item.EntityID == "" {
		return "", false, fmt.Errorf("knowledge put: kind and entity_id are required")
	}
	if item.AssertedAt.IsZero() {
		// The resolver orders by AssertedAt; a zero value would silently lose
		// to everything and make the "latest" word un-assertable.
		return "", false, fmt.Errorf("knowledge put: asserted_at is required")
	}
	// ADR-0110 D2: a DECLARED kind's values must fit its declaration; the
	// refusal names the constraint and carries domain.ErrKindRefused so async
	// callers can tell permanent from transient.
	if err := s.reg.ValidateValues(item.Kind, item.Values); err != nil {
		return "", false, err
	}
	ns := item.NamespaceID
	if ns == "" {
		ns = "default"
	}
	id := item.ID
	if id == "" {
		id = domain.KnowledgeItemID("ki_" + uuid.NewString())
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit

	// One writer at a time per resolution key: the recompute below reads the
	// full item set and swaps the current version, so two concurrent writers
	// must serialize or the second could close a version it never read. An
	// advisory xact lock keyed on the resolution key is simpler and less
	// deadlock-prone than juggling row locks across two tables.
	h := fnv.New64a()
	fmt.Fprintf(h, "%s|%s|%s|%s", ns, item.Kind, item.EntityID, item.AssertedBy)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(h.Sum64())); err != nil { //nolint:gosec // deliberate wraparound: the lock key only needs stability
		return "", false, err
	}

	var evidenceID *string
	if item.EvidenceID != "" {
		e := string(item.EvidenceID)
		evidenceID = &e
	}
	tags := item.Classification
	if tags == nil {
		tags = []string{}
	}
	var gotID string
	err = tx.QueryRow(ctx, `
		INSERT INTO knowledge_items (id, namespace_id, kind, evidence_id, entity_id,
			asserted_by, asserted_at, source_ref, negation, classification,
			valid_from, valid_to, schema_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,1)
		ON CONFLICT (namespace_id, kind, entity_id, source_ref) WHERE source_ref <> ''
		DO NOTHING
		RETURNING id`,
		string(id), ns, item.Kind, evidenceID, item.EntityID,
		item.AssertedBy, item.AssertedAt.UTC(), item.SourceRef, item.Negation, tags,
		nullTime(item.ValidFrom), nullTime(item.ValidTo),
	).Scan(&gotID)

	if errors.Is(err, pgx.ErrNoRows) {
		// Replay: the assertion is already recorded. Nothing changed, so the
		// resolution cannot have changed either — return the existing identity.
		if err := tx.QueryRow(ctx, `
			SELECT id FROM knowledge_items
			WHERE namespace_id=$1 AND kind=$2 AND entity_id=$3 AND source_ref=$4`,
			ns, item.Kind, item.EntityID, item.SourceRef,
		).Scan(&gotID); err != nil {
			return "", false, fmt.Errorf("knowledge replay lookup: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return "", false, err
		}
		return domain.KnowledgeItemID(gotID), false, nil
	}
	if err != nil {
		return "", false, err
	}

	for _, v := range item.Values {
		if err := insertValue(ctx, tx, gotID, v); err != nil {
			return "", false, err
		}
	}

	if err := s.rederive(ctx, tx, ns, item.Kind, item.EntityID, item.AssertedBy); err != nil {
		return "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return domain.KnowledgeItemID(gotID), true, nil
}

func insertValue(ctx context.Context, tx pgx.Tx, itemID string, v domain.StatementValue) error {
	var date *time.Time
	var num *float64
	var text, ent *string
	switch v.Type {
	case "date":
		d := v.Date.UTC()
		date = &d
	case "number":
		n := v.Number
		num = &n
	case "text":
		s := v.Text
		text = &s
	case "entity":
		e := v.EntityRef
		ent = &e
	default:
		return fmt.Errorf("statement value %q: unknown type %q", v.Predicate, v.Type)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO statement_values (item_id, predicate, value_type,
			value_date, value_number, value_text, value_entity_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		itemID, v.Predicate, v.Type, date, num, text, ent)
	return err
}

// rederive recomputes the key's resolution from ALL its items and versions the
// row when the answer changed: close the current version, insert the new one.
// The policy comes from the kind's declaration and the resolver from the
// authority registry (ADR-0110 D3) — an undeclared kind derives under
// latest_assertion, exactly as before the registry existed.
func (s *PgKnowledgeStore) rederive(ctx context.Context, tx pgx.Tx, ns, kind, entity, actor string) error {
	policy := domain.ResolutionPolicyLatestAssertion
	if spec, ok := s.reg.Spec(kind); ok {
		policy = spec.Policy
	}
	authority, ok := s.reg.Authority(policy)
	if !ok {
		// NewKindRegistry refuses undeclared policies at boot, so this is a
		// wiring bug, not a data condition — refuse rather than silently
		// deriving under a different policy than the kind declared.
		return fmt.Errorf("kind %q: no authority registered for policy %q", kind, policy)
	}
	rows, err := tx.Query(ctx, `
		SELECT id, asserted_at, source_ref, negation, valid_from
		FROM knowledge_items
		WHERE namespace_id=$1 AND kind=$2 AND entity_id=$3 AND asserted_by=$4`,
		ns, kind, entity, actor)
	if err != nil {
		return err
	}
	var items []domain.KnowledgeItem
	for rows.Next() {
		var it domain.KnowledgeItem
		var itemID string
		var validFrom *time.Time
		if err := rows.Scan(&itemID, &it.AssertedAt, &it.SourceRef, &it.Negation, &validFrom); err != nil {
			rows.Close()
			return err
		}
		it.ID = domain.KnowledgeItemID(itemID)
		if validFrom != nil {
			it.ValidFrom = *validFrom
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// A key with no items left (erasure took them all) closes its current
	// version and gets NO replacement: an empty belief row would read as "we
	// currently believe nothing about this" when the truth is "there is no
	// longer anything to believe over".
	if len(items) == 0 {
		_, err := tx.Exec(ctx, `
			UPDATE resolutions SET system_to = now()
			WHERE namespace_id=$1 AND kind=$2 AND entity_id=$3 AND actor=$4 AND policy=$5
			  AND system_to IS NULL`,
			ns, kind, entity, actor, policy)
		return err
	}

	winner, reason := authority.Resolve(items)
	var winnerID *string
	if winner != nil {
		w := string(winner.ID)
		winnerID = &w
	}

	// Compare against the current version; only a CHANGED answer creates one.
	var curItem *string
	var curReason string
	err = tx.QueryRow(ctx, `
		SELECT item_id, reason_code FROM resolutions
		WHERE namespace_id=$1 AND kind=$2 AND entity_id=$3 AND actor=$4 AND policy=$5
		  AND system_to IS NULL`,
		ns, kind, entity, actor, policy,
	).Scan(&curItem, &curReason)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// no current version
	case err != nil:
		return err
	default:
		if equalPtr(curItem, winnerID) && curReason == reason {
			return nil // unchanged — no new version
		}
		if _, err := tx.Exec(ctx, `
			UPDATE resolutions SET system_to = now()
			WHERE namespace_id=$1 AND kind=$2 AND entity_id=$3 AND actor=$4 AND policy=$5
			  AND system_to IS NULL`,
			ns, kind, entity, actor, policy); err != nil {
			return err
		}
	}
	var validFrom *time.Time
	if winner != nil && !winner.ValidFrom.IsZero() {
		v := winner.ValidFrom
		validFrom = &v
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO resolutions (namespace_id, kind, entity_id, actor, policy,
			item_id, reason_code, valid_from)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		ns, kind, entity, actor, policy,
		winnerID, reason, validFrom)
	return err
}

// GetItem returns one item with its values.
func (s *PgKnowledgeStore) GetItem(ctx context.Context, id domain.KnowledgeItemID) (*domain.KnowledgeItem, error) {
	it, err := scanItem(ctx, s.pool, `WHERE ki.id = $1`, string(id))
	if err != nil {
		return nil, err
	}
	if len(it) != 1 {
		return nil, fmt.Errorf("knowledge get %s: not found", id)
	}
	return &it[0], nil
}

// CurrentResolutions lists current beliefs for a kind under a policy, hydrated
// with winning items. Negated keys (item_id NULL) are omitted.
func (s *PgKnowledgeStore) CurrentResolutions(ctx context.Context, namespace, kind, policy string) ([]domain.Resolution, error) {
	if namespace == "" {
		namespace = "default"
	}
	rows, err := s.pool.Query(ctx, `
		SELECT r.entity_id, r.actor, r.item_id, r.reason_code, r.system_from
		FROM resolutions r
		WHERE r.namespace_id=$1 AND r.kind=$2 AND r.policy=$3
		  AND r.system_to IS NULL AND r.item_id IS NOT NULL
		ORDER BY r.id`,
		namespace, kind, policy)
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
		r.NamespaceID, r.Kind, r.Policy = namespace, kind, policy
		r.Item = &domain.KnowledgeItem{ID: domain.KnowledgeItemID(itemID)}
		out = append(out, r)
		ids = append(ids, itemID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return out, nil
	}

	items, err := scanItem(ctx, s.pool, `WHERE ki.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[domain.KnowledgeItemID]domain.KnowledgeItem, len(items))
	for _, it := range items {
		byID[it.ID] = it
	}
	for i := range out {
		if it, ok := byID[out[i].Item.ID]; ok {
			cp := it
			out[i].Item = &cp
		}
	}
	return out, nil
}

// scanItem loads items + values for an arbitrary WHERE clause on alias `ki`.
func scanItem(ctx context.Context, pool *pgxpool.Pool, where string, arg any) ([]domain.KnowledgeItem, error) {
	rows, err := pool.Query(ctx, `
		SELECT ki.id, ki.namespace_id, ki.kind, ki.evidence_id, ki.entity_id,
			ki.asserted_by, ki.asserted_at, ki.source_ref, ki.negation,
			ki.classification, ki.valid_from, ki.valid_to
		FROM knowledge_items ki `+where, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []domain.KnowledgeItem
	for rows.Next() {
		var it domain.KnowledgeItem
		var itemID string
		var evidenceID *string
		var validFrom, validTo *time.Time
		if err := rows.Scan(&itemID, &it.NamespaceID, &it.Kind, &evidenceID, &it.EntityID,
			&it.AssertedBy, &it.AssertedAt, &it.SourceRef, &it.Negation,
			&it.Classification, &validFrom, &validTo); err != nil {
			return nil, err
		}
		it.ID = domain.KnowledgeItemID(itemID)
		if evidenceID != nil {
			it.EvidenceID = domain.EvidenceID(*evidenceID)
		}
		if validFrom != nil {
			it.ValidFrom = *validFrom
		}
		if validTo != nil {
			it.ValidTo = *validTo
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range items {
		vrows, err := pool.Query(ctx, `
			SELECT predicate, value_type, value_date, value_number, value_text, value_entity_id
			FROM statement_values WHERE item_id = $1 ORDER BY predicate`,
			string(items[i].ID))
		if err != nil {
			return nil, err
		}
		for vrows.Next() {
			var v domain.StatementValue
			var date *time.Time
			var num *float64
			var text, ent *string
			if err := vrows.Scan(&v.Predicate, &v.Type, &date, &num, &text, &ent); err != nil {
				vrows.Close()
				return nil, err
			}
			switch {
			case date != nil:
				v.Date = *date
			case num != nil:
				v.Number = *num
			case text != nil:
				v.Text = *text
			case ent != nil:
				v.EntityRef = *ent
			}
			items[i].Values = append(items[i].Values, v)
		}
		vrows.Close()
		if err := vrows.Err(); err != nil {
			return nil, err
		}
	}
	return items, nil
}

// EraseItems implements the compliance erasure (ADR-0106 addendum): delete the
// matching items, their values (FK cascade) and every resolution version that
// referenced them, then re-derive the projection for keys that only lost SOME
// of their items — a surviving speaker's belief must not vanish because a
// different speaker's data was erased.
func (s *PgKnowledgeStore) EraseItems(ctx context.Context, namespace, kind string, sel domain.KnowledgeErasure) (int, error) {
	if kind == "" {
		return 0, fmt.Errorf("knowledge erase: kind is required")
	}
	if len(sel.Entities) == 0 && sel.ValueEntityRef == "" {
		return 0, nil // an empty selector matches nothing, by contract
	}
	if namespace == "" {
		namespace = "default"
	}
	entities := sel.Entities
	if entities == nil {
		entities = []string{}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit

	// The victims, and the (entity, actor) keys they belong to — collected
	// BEFORE deletion, because afterwards there is nothing left to ask.
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT ki.id, ki.entity_id, ki.asserted_by
		FROM knowledge_items ki
		LEFT JOIN statement_values sv ON sv.item_id = ki.id AND sv.value_type = 'entity'
		WHERE ki.namespace_id = $1 AND ki.kind = $2
		  AND (ki.entity_id = ANY($3) OR ($4 <> '' AND sv.value_entity_id = $4))`,
		namespace, kind, entities, sel.ValueEntityRef)
	if err != nil {
		return 0, err
	}
	var ids []string
	type rkey struct{ entity, actor string }
	keys := map[rkey]bool{}
	for rows.Next() {
		var id, entity, actor string
		if err := rows.Scan(&id, &entity, &actor); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		keys[rkey{entity, actor}] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, tx.Commit(ctx)
	}

	// Resolution history first (it references the items), then the items —
	// statement_values goes with them via ON DELETE CASCADE.
	if _, err := tx.Exec(ctx, `DELETE FROM resolutions WHERE item_id = ANY($1)`, ids); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM knowledge_items WHERE id = ANY($1)`, ids)
	if err != nil {
		return 0, err
	}
	for k := range keys {
		if err := s.rederive(ctx, tx, namespace, kind, k.entity, k.actor); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func equalPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
