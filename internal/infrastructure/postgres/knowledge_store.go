package postgres

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// PgKnowledgeStore is the Postgres adapter for domain.KnowledgeStore
// (ADR-0106). Tables come from migrations 0012 and 0022; no DDL here.
//
// ADR-0136 A1 widened it for INPUT-TAKING authorities (domain.InputResolutionAuthority,
// the memory_fact kind's valid_time_supersession): their derivation reads the key's full
// items WITH their values and stated valid_to, the stored assessments citing them, the
// other side of cross-key verdicts, and the predicate's cardinality profile
// (knowledge_assessment_store.go). Kinds under latest_assertion derive exactly as before.
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

// pgxQuerier is what both a pool and a transaction offer for reads.
type pgxQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// resolutionLockKey is the advisory-lock key of one resolution key. Every writer that
// re-derives a key takes it first (PutItem, PutAssessment, RederivePredicate,
// EraseItems), so two writers of one key serialize.
func resolutionLockKey(ns, kind, entity, actor string) int64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s|%s|%s|%s", ns, kind, entity, actor)
	return int64(h.Sum64()) //nolint:gosec // deliberate wraparound: the lock key only needs stability
}

// PutItem appends one item and re-derives the resolution for its key from the
// FULL item set via the kind's authority — never by comparing against "the prior
// row", which is the arrival-order bug the memo forbids (§13).
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
	entityScoped := s.reg.EntityScoped(item.Kind)
	if entityScoped && item.AssertedBy == domain.ResolutionActorAll {
		// "*" is the entity row's actor column; an actor of that name would be
		// indistinguishable from the cross-actor belief.
		return "", false, fmt.Errorf("knowledge put: kind %q: actor %q is reserved for the entity-scoped resolution: %w",
			item.Kind, domain.ResolutionActorAll, domain.ErrKindRefused)
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
	//
	// An ENTITY-scoped kind locks the entity key instead: its cross-actor row
	// reads every actor's items, so every writer of the entity must serialize, and
	// the per-actor rows it also maintains are derived under the same lock.
	lockActor := item.AssertedBy
	if entityScoped {
		lockActor = domain.ResolutionActorAll
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, resolutionLockKey(ns, item.Kind, item.EntityID, lockActor)); err != nil {
		return "", false, err
	}

	// Lifecycle rules (KindSpec.Lifecycle) are checked against the key's CURRENT
	// resolution, read inside the lock so no concurrent writer can move the state
	// between the check and the write. Read before the insert, checked only for a
	// NEW item: a replay changes nothing, whatever the state has become since.
	var current *domain.KnowledgeItem
	if s.reg.HasLifecycle(item.Kind) {
		policy, _, perr := s.policyFor(item.Kind)
		if perr != nil {
			return "", false, perr
		}
		current, err = currentResolvedItem(ctx, tx, ns, item.Kind, item.EntityID, lockActor, policy)
		if err != nil {
			return "", false, fmt.Errorf("knowledge put: read current state: %w", err)
		}
	}

	gotID, inserted, err := insertItemTx(ctx, tx, ns, id, item)
	if err != nil {
		return "", false, err
	}
	if !inserted {
		// Replay: the assertion is already recorded. Nothing changed, so the
		// resolution cannot have changed either — return the existing identity.
		if err := tx.Commit(ctx); err != nil {
			return "", false, err
		}
		return domain.KnowledgeItemID(gotID), false, nil
	}
	if err := s.reg.ValidateTransition(item.Kind, current, item); err != nil {
		return "", false, err // the deferred rollback discards the insert
	}
	if err := s.rederive(ctx, tx, ns, item.Kind, item.EntityID, item.AssertedBy); err != nil {
		return "", false, err
	}
	if entityScoped {
		if err := s.rederive(ctx, tx, ns, item.Kind, item.EntityID, domain.ResolutionActorAll); err != nil {
			return "", false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return domain.KnowledgeItemID(gotID), true, nil
}

// currentResolvedItem loads the item the key's CURRENT resolution names under
// policy; nil when there is no current resolution or its latest word was a negation.
func currentResolvedItem(ctx context.Context, tx pgx.Tx, ns, kind, entity, actor, policy string) (*domain.KnowledgeItem, error) {
	var itemID *string
	err := tx.QueryRow(ctx, `
		SELECT item_id FROM resolutions
		WHERE namespace_id=$1 AND kind=$2 AND entity_id=$3 AND actor=$4 AND policy=$5
		  AND system_to IS NULL`,
		ns, kind, entity, actor, policy).Scan(&itemID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && itemID == nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	items, err := loadItems(ctx, tx, `WHERE ki.id = $1`, *itemID)
	if err != nil || len(items) == 0 {
		return nil, err
	}
	return &items[0], nil
}

// insertItemTx inserts one item and its values on tx. inserted=false (and the existing
// id) when (namespace, kind, entity, source_ref) already exists.
func insertItemTx(ctx context.Context, tx pgx.Tx, ns string, id domain.KnowledgeItemID, item domain.KnowledgeItem) (string, bool, error) {
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
	err := tx.QueryRow(ctx, `
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
		if err := tx.QueryRow(ctx, `
			SELECT id FROM knowledge_items
			WHERE namespace_id=$1 AND kind=$2 AND entity_id=$3 AND source_ref=$4`,
			ns, item.Kind, item.EntityID, item.SourceRef,
		).Scan(&gotID); err != nil {
			return "", false, fmt.Errorf("knowledge replay lookup: %w", err)
		}
		return gotID, false, nil
	}
	if err != nil {
		return "", false, err
	}
	for _, v := range item.Values {
		if err := insertValue(ctx, tx, gotID, v); err != nil {
			return "", false, err
		}
	}
	return gotID, true, nil
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

// policyFor returns the policy and authority of a kind.
func (s *PgKnowledgeStore) policyFor(kind string) (string, domain.ResolutionAuthority, error) {
	policy := domain.ResolutionPolicyLatestAssertion
	if spec, ok := s.reg.Spec(kind); ok {
		policy = spec.Policy
	}
	authority, ok := s.reg.Authority(policy)
	if !ok {
		// NewKindRegistry refuses undeclared policies at boot, so this is a
		// wiring bug, not a data condition — refuse rather than silently
		// deriving under a different policy than the kind declared.
		return "", nil, fmt.Errorf("kind %q: no authority registered for policy %q", kind, policy)
	}
	return policy, authority, nil
}

// rederive recomputes the key's resolution from ALL its items and versions the
// row when the answer changed: close the current version, insert the new one.
// The policy comes from the kind's declaration and the resolver from the
// authority registry (ADR-0110 D3) — an undeclared kind derives under
// latest_assertion, exactly as before the registry existed. An input-taking
// authority (ADR-0136 A1) derives from the widened read (deriveKeys).
//
// An ENTITY-scoped kind (KindSpec.ResolutionScope) has two row families: the
// cross-actor row (actor = domain.ResolutionActorAll) derived under the kind's
// policy from EVERY actor's items, and one row per actor under latest_assertion —
// each speaker's current word, so `current` and `contradictions` still show what
// everyone said. NewKindRegistry refuses entity scope for latest_assertion and for
// input-taking authorities, so the two families never share a policy.
func (s *PgKnowledgeStore) rederive(ctx context.Context, tx pgx.Tx, ns, kind, entity, actor string) error {
	policy, authority, err := s.policyFor(kind)
	if err != nil {
		return err
	}
	spec, _ := s.reg.Spec(kind)
	allActors := false
	if spec.ResolutionScope == domain.ResolutionScopeEntity {
		if actor == domain.ResolutionActorAll {
			allActors = true
		} else {
			policy, authority = domain.ResolutionPolicyLatestAssertion, domain.LatestAssertionAuthority{}
		}
	}
	if ia, ok := authority.(domain.InputResolutionAuthority); ok {
		k := resolutionKey{entity: entity, actor: actor}
		derived, err := s.deriveKeys(ctx, tx, ia, ns, kind, []resolutionKey{k}, nil)
		if err != nil {
			return err
		}
		d, ok := derived[k]
		if !ok || len(d.items) == 0 {
			return closeResolution(ctx, tx, ns, kind, entity, actor, policy)
		}
		var winnerID *string
		var validFrom, validTo *time.Time
		if w := d.outcome.Winner; w != nil {
			id := string(w.ID)
			winnerID = &id
			for _, iv := range d.outcome.Chain {
				if iv.ItemID == w.ID {
					validFrom, validTo = iv.From, iv.To
				}
			}
		}
		return versionResolution(ctx, tx, ns, kind, entity, actor, policy, winnerID, d.outcome.Reason, validFrom, validTo)
	}

	rows, err := tx.Query(ctx, `
		SELECT id, asserted_by, asserted_at, source_ref, negation, valid_from
		FROM knowledge_items
		WHERE namespace_id=$1 AND kind=$2 AND entity_id=$3 AND ($5 OR asserted_by=$4)`,
		ns, kind, entity, actor, allActors)
	if err != nil {
		return err
	}
	var items []domain.KnowledgeItem
	for rows.Next() {
		var it domain.KnowledgeItem
		var itemID string
		var validFrom *time.Time
		if err := rows.Scan(&itemID, &it.AssertedBy, &it.AssertedAt, &it.SourceRef, &it.Negation, &validFrom); err != nil {
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
		return closeResolution(ctx, tx, ns, kind, entity, actor, policy)
	}

	var winner *domain.KnowledgeItem
	var reason string
	if ka, ok := authority.(domain.KindAwareAuthority); ok {
		winner, reason = ka.ResolveKind(spec, items)
	} else {
		winner, reason = authority.Resolve(items)
	}
	if winner == nil && reason == domain.ReasonNoDesignated {
		// Nobody eligible has spoken: there is no belief, not a "nothing" belief.
		return closeResolution(ctx, tx, ns, kind, entity, actor, policy)
	}
	var winnerID *string
	var validFrom *time.Time
	if winner != nil {
		w := string(winner.ID)
		winnerID = &w
		if !winner.ValidFrom.IsZero() {
			v := winner.ValidFrom
			validFrom = &v
		}
	}
	return versionResolution(ctx, tx, ns, kind, entity, actor, policy, winnerID, reason, validFrom, nil)
}

func closeResolution(ctx context.Context, tx pgx.Tx, ns, kind, entity, actor, policy string) error {
	_, err := tx.Exec(ctx, `
		UPDATE resolutions SET system_to = now()
		WHERE namespace_id=$1 AND kind=$2 AND entity_id=$3 AND actor=$4 AND policy=$5
		  AND system_to IS NULL`,
		ns, kind, entity, actor, policy)
	return err
}

// versionResolution writes a new current version when the answer changed — the
// existing system-time discipline: a changed answer closes the old version and inserts
// a new one; an unchanged one writes nothing.
func versionResolution(ctx context.Context, tx pgx.Tx, ns, kind, entity, actor, policy string,
	winnerID *string, reason string, validFrom, validTo *time.Time) error {
	var curItem *string
	var curReason string
	var curTo *time.Time
	err := tx.QueryRow(ctx, `
		SELECT item_id, reason_code, valid_to FROM resolutions
		WHERE namespace_id=$1 AND kind=$2 AND entity_id=$3 AND actor=$4 AND policy=$5
		  AND system_to IS NULL`,
		ns, kind, entity, actor, policy,
	).Scan(&curItem, &curReason, &curTo)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// no current version
	case err != nil:
		return err
	default:
		if equalPtr(curItem, winnerID) && curReason == reason && equalTimePtr(curTo, validTo) {
			return nil // unchanged — no new version
		}
		if err := closeResolution(ctx, tx, ns, kind, entity, actor, policy); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO resolutions (namespace_id, kind, entity_id, actor, policy,
			item_id, reason_code, valid_from, valid_to)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		ns, kind, entity, actor, policy,
		winnerID, reason, utcTimePtr(validFrom), utcTimePtr(validTo))
	return err
}

// GetItem returns one item with its values.
func (s *PgKnowledgeStore) GetItem(ctx context.Context, id domain.KnowledgeItemID) (*domain.KnowledgeItem, error) {
	it, err := loadItems(ctx, s.pool, `WHERE ki.id = $1`, string(id))
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

	items, err := loadItems(ctx, s.pool, `WHERE ki.id = ANY($1)`, ids)
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

// loadItems loads items + values for an arbitrary WHERE clause on alias `ki`, on a pool
// or a transaction. Values come in ONE batched query, not one per item.
func loadItems(ctx context.Context, q pgxQuerier, where string, args ...any) ([]domain.KnowledgeItem, error) {
	rows, err := q.Query(ctx, `
		SELECT ki.id, ki.namespace_id, ki.kind, ki.evidence_id, ki.entity_id,
			ki.asserted_by, ki.asserted_at, ki.source_ref, ki.negation,
			ki.classification, ki.valid_from, ki.valid_to
		FROM knowledge_items ki `+where, args...)
	if err != nil {
		return nil, err
	}
	var items []domain.KnowledgeItem
	for rows.Next() {
		var it domain.KnowledgeItem
		var itemID string
		var evidenceID *string
		var validFrom, validTo *time.Time
		if err := rows.Scan(&itemID, &it.NamespaceID, &it.Kind, &evidenceID, &it.EntityID,
			&it.AssertedBy, &it.AssertedAt, &it.SourceRef, &it.Negation,
			&it.Classification, &validFrom, &validTo); err != nil {
			rows.Close()
			return nil, err
		}
		it.ID = domain.KnowledgeItemID(itemID)
		it.AssertedAt = it.AssertedAt.UTC()
		if evidenceID != nil {
			it.EvidenceID = domain.EvidenceID(*evidenceID)
		}
		if validFrom != nil {
			it.ValidFrom = validFrom.UTC()
		}
		if validTo != nil {
			it.ValidTo = validTo.UTC()
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return items, nil
	}
	ids := make([]string, len(items))
	pos := make(map[string]int, len(items))
	for i := range items {
		ids[i] = string(items[i].ID)
		pos[ids[i]] = i
	}
	vrows, err := q.Query(ctx, `
		SELECT item_id, predicate, value_type, value_date, value_number, value_text, value_entity_id
		FROM statement_values WHERE item_id = ANY($1) ORDER BY item_id, predicate`, ids)
	if err != nil {
		return nil, err
	}
	defer vrows.Close()
	for vrows.Next() {
		var itemID string
		var v domain.StatementValue
		var date *time.Time
		var num *float64
		var text, ent *string
		if err := vrows.Scan(&itemID, &v.Predicate, &v.Type, &date, &num, &text, &ent); err != nil {
			return nil, err
		}
		switch {
		case date != nil:
			v.Date = date.UTC()
		case num != nil:
			v.Number = *num
		case text != nil:
			v.Text = *text
		case ent != nil:
			v.EntityRef = *ent
		}
		if i, ok := pos[itemID]; ok {
			items[i].Values = append(items[i].Values, v)
		}
	}
	return items, vrows.Err()
}

// EraseItems implements the compliance erasure (ADR-0106 addendum): delete the
// matching items, their values (FK cascade), their item embeddings (FK cascade), every
// resolution version that referenced them and every assessment that cited them, then
// re-derive the projection for keys that only lost SOME of their items — a surviving
// speaker's belief must not vanish because a different speaker's data was erased.
//
// ADR-0136 A1-Q4 / ADR-0137 Q5: for memory_fact, every surviving item an erased item had
// closed (in its own key or, through a contradictory assessment, in another) receives a
// content-free sentinel (domain.FactErasedSentinel), so it stays closed on re-derivation
// instead of becoming current again.
//
// The selector matches items ABOUT the named entities, items carrying an entity value
// equal to ValueEntityRef, items derived from the named evidence rows (EvidenceIDs,
// ADR-0137 S2) and items carrying a text value ValueTexts under ValuePredicate (ADR-0138
// D4: the document cascade's selector when evidence capture is off).
func (s *PgKnowledgeStore) EraseItems(ctx context.Context, namespace, kind string, sel domain.KnowledgeErasure) (int, error) {
	if kind == "" {
		return 0, fmt.Errorf("knowledge erase: kind is required")
	}
	if sel.IsEmpty() {
		return 0, nil // an empty selector matches nothing, by contract
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit
	n, err := s.EraseItemsTx(ctx, tx, namespace, kind, sel)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return n, nil
}

// EraseItemsTx is EraseItems inside the CALLER's transaction (ADR-0137 D4 step 7, S2),
// so an erasure that also deletes chunks, triplets and documents (ADR-0138 D4) commits
// or rolls back as one unit. Same rules as EraseItems; the caller commits.
func (s *PgKnowledgeStore) EraseItemsTx(ctx context.Context, tx pgx.Tx, namespace, kind string, sel domain.KnowledgeErasure) (int, error) {
	if kind == "" {
		return 0, fmt.Errorf("knowledge erase: kind is required")
	}
	if sel.IsEmpty() {
		return 0, nil // an empty selector matches nothing, by contract
	}
	if namespace == "" {
		namespace = "default"
	}
	entities := sel.Entities
	if entities == nil {
		entities = []string{}
	}
	evidenceIDs := sel.EvidenceIDs
	if evidenceIDs == nil {
		evidenceIDs = []string{}
	}
	valueTexts := sel.ValueTexts
	if valueTexts == nil || sel.ValuePredicate == "" {
		valueTexts = []string{}
	}

	// The victims, and the (entity, actor) keys they belong to — collected
	// BEFORE deletion, because afterwards there is nothing left to ask.
	victims, err := loadItems(ctx, tx, `
		WHERE ki.id IN (SELECT DISTINCT k2.id FROM knowledge_items k2
			LEFT JOIN statement_values sv ON sv.item_id = k2.id AND sv.value_type = 'entity'
			WHERE k2.namespace_id = $1 AND k2.kind = $2
			  AND (k2.entity_id = ANY($3) OR ($4 <> '' AND sv.value_entity_id = $4)
			       OR k2.evidence_id = ANY($5)
			       OR EXISTS (SELECT 1 FROM statement_values st
			                  WHERE st.item_id = k2.id AND st.predicate = $6 AND st.value_text = ANY($7))))`,
		namespace, kind, entities, sel.ValueEntityRef, evidenceIDs, sel.ValuePredicate, valueTexts)
	if err != nil {
		return 0, err
	}
	if len(victims) == 0 {
		return 0, nil
	}
	var ids []string
	keys := map[resolutionKey]bool{}
	entityScoped := s.reg.EntityScoped(kind)
	for _, v := range victims {
		ids = append(ids, string(v.ID))
		keys[resolutionKey{v.EntityID, v.AssertedBy}] = true
		if entityScoped {
			// The cross-actor row read this item too.
			keys[resolutionKey{v.EntityID, domain.ResolutionActorAll}] = true
		}
	}
	// Assessments citing an erased item go too: they name it and carry the model's
	// verbatim answer about it. Keys of the OTHER items they cited lose an input, so
	// they are re-derived as well.
	crows, err := tx.Query(ctx, `
		SELECT DISTINCT ki.entity_id, ki.asserted_by FROM knowledge_assessments a
		JOIN knowledge_items ki ON ki.id = ANY(a.input_item_ids)
		WHERE a.input_item_ids && $1::text[] AND ki.kind = $2 AND ki.namespace_id = $3`, ids, kind, namespace)
	if err != nil {
		return 0, err
	}
	for crows.Next() {
		var e, a string
		if err := crows.Scan(&e, &a); err != nil {
			crows.Close()
			return 0, err
		}
		keys[resolutionKey{e, a}] = true
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		return 0, err
	}
	sortedKeys := make([]resolutionKey, 0, len(keys))
	for k := range keys {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Slice(sortedKeys, func(i, j int) bool { return sortedKeys[i].less(sortedKeys[j]) })
	for _, k := range sortedKeys {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, resolutionLockKey(namespace, kind, k.entity, k.actor)); err != nil {
			return 0, err
		}
	}

	// ADR-0136 A1-Q4 / ADR-0137 Q5: an authority that keeps chains closed across an
	// erasure gets, BEFORE anything is deleted, the derivation of every affected key; each
	// surviving item an erased one had closed receives a content-free sentinel at the
	// closing instant. Other kinds erase exactly as before.
	var sentinels []domain.KnowledgeItem
	if _, authority, err := s.policyFor(kind); err == nil {
		sa, okS := authority.(domain.SentinelOnEraseAuthority)
		ia, okI := authority.(domain.InputResolutionAuthority)
		if okS && okI {
			victimByID := make(map[domain.KnowledgeItemID]domain.KnowledgeItem, len(victims))
			for _, v := range victims {
				victimByID[v.ID] = v
			}
			derived, err := s.deriveKeys(ctx, tx, ia, namespace, kind, sortedKeys, nil)
			if err != nil {
				return 0, fmt.Errorf("knowledge erase: derive before erase: %w", err)
			}
			for _, k := range sortedKeys {
				d, ok := derived[k]
				if !ok {
					continue
				}
				byID := map[domain.KnowledgeItemID]domain.KnowledgeItem{}
				for _, it := range d.items {
					byID[it.ID] = it
				}
				for _, iv := range d.outcome.Chain {
					erased, closedByVictim := victimByID[iv.SupersededBy]
					closed, survives := byID[iv.ItemID]
					if !closedByVictim || !survives || iv.To == nil {
						continue
					}
					if _, isVictim := victimByID[iv.ItemID]; isVictim {
						continue
					}
					if sent, ok := sa.ErasedSentinel(erased, closed, *iv.To); ok {
						sentinels = append(sentinels, sent)
					}
				}
			}
		}
	}

	if _, err := tx.Exec(ctx, `DELETE FROM knowledge_assessments WHERE input_item_ids && $1::text[]`, ids); err != nil {
		return 0, err
	}
	// Resolution history first (it references the items), then the items —
	// statement_values and knowledge_item_embeddings go with them via ON DELETE CASCADE.
	if _, err := tx.Exec(ctx, `DELETE FROM resolutions WHERE item_id = ANY($1)`, ids); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM knowledge_items WHERE id = ANY($1)`, ids)
	if err != nil {
		return 0, err
	}
	for _, sent := range sentinels {
		if _, _, err := insertItemTx(ctx, tx, namespace, sent.ID, sent); err != nil {
			return 0, fmt.Errorf("knowledge erase: sentinel: %w", err)
		}
	}
	for _, k := range sortedKeys {
		if err := s.rederive(ctx, tx, namespace, kind, k.entity, k.actor); err != nil {
			return 0, err
		}
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

func utcTimePtr(t *time.Time) *time.Time {
	if t == nil {
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

func equalTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
