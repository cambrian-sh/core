package postgres

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/cambrian-sh/core/domain"
)

// RetagDocument changes a document's classification — the document row, every chunk's
// derived tag cache (retagDocumentTx, ADR-0093) AND every memory_fact item extracted
// from the document (ENG-489) — in ONE transaction.
//
// Before ENG-489 a retag moved the document and its chunks but not their facts: a fact
// kept the classification its source chunk had when it was extracted, so after a
// NARROWING retag any reader of memory_fact items that does not go through the source
// chunk (the query plane, FactChain, the premium substrate consultant) still served it
// under the old scope, and after a widening one the facts stayed hidden.
//
// Owner decision 2026-11-15: the facts are rewritten IN PLACE — a deliberate, documented
// exception to the knowledge substrate's append-only rule, for reclassification only
// (ADR-0136 observation; card.core.facts-layer). The item id, values, evidence, assertion
// and validity are untouched; only what the classification determines changes.
func (l *PgDocumentLifecycle) RetagDocument(ctx context.Context, documentID string, tags []string) error {
	if documentID == "" {
		return nil
	}
	err := pgx.BeginFunc(ctx, l.k.pool, func(tx pgx.Tx) error {
		final, err := retagDocumentTx(ctx, tx, documentID, tags)
		if err != nil {
			return err
		}
		_, err = l.k.reclassifyDocumentFactsTx(ctx, tx, documentID, final)
		return err
	})
	return mapError("RetagDocument", err)
}

// reclassifyDocumentFactsTx rewrites, inside tx, the memory_fact items whose
// source_document is documentID to classification: their classification column, their
// classification-derived entity id (<scope_key>/<subject>/<predicate>, domain.
// FactEntityID) and their subject entity reference (domain.FactSubjectRef) — exactly the
// fields domain.RekeyFactItem derives from a classification, nothing else. Both the old
// and the new resolution keys are then re-derived in the same transaction (the old key
// loses the items, the new one gains them), under the same advisory locks PutItem takes.
// It returns the number of items rewritten.
func (s *PgKnowledgeStore) reclassifyDocumentFactsTx(ctx context.Context, tx pgx.Tx, documentID string, classification []string) (int, error) {
	kind := domain.KindMemoryFact
	type item struct {
		id, ns, entity, actor string
		cls                   []string
	}
	rows, err := tx.Query(ctx, `SELECT k.id, k.namespace_id, k.entity_id, k.asserted_by, k.classification
		FROM knowledge_items k JOIN statement_values sv ON sv.item_id = k.id
		WHERE k.kind = $1 AND sv.predicate = $2 AND sv.value_text = $3
		ORDER BY k.id`, kind, domain.FactPredSourceDocument, documentID)
	if err != nil {
		return 0, fmt.Errorf("fact reclassify: select: %w", err)
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.ns, &it.entity, &it.actor, &it.cls); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(items) == 0 {
		return 0, nil
	}

	cls := append([]string{}, classification...)
	type key struct{ ns, entity, actor string }
	touched := map[key]bool{}
	type change struct {
		id, entity, subject string
	}
	var changes []change
	for _, it := range items {
		newEntity, subject, ok := rekeyedFact(it.entity, cls)
		if !ok {
			continue // not a fact key (RekeyFactItem leaves such items alone too)
		}
		if newEntity == it.entity && sameTags(it.cls, cls) {
			continue
		}
		changes = append(changes, change{it.id, newEntity, subject})
		touched[key{it.ns, it.entity, it.actor}] = true
		touched[key{it.ns, newEntity, it.actor}] = true
	}
	if len(changes) == 0 {
		return 0, nil
	}
	keys := make([]key, 0, len(touched))
	for k := range touched {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ns != keys[j].ns {
			return keys[i].ns < keys[j].ns
		}
		if keys[i].entity != keys[j].entity {
			return keys[i].entity < keys[j].entity
		}
		return keys[i].actor < keys[j].actor
	})
	// The per-key advisory locks, in one global order so two retags cannot deadlock.
	for _, k := range keys {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, resolutionLockKey(k.ns, kind, k.entity, k.actor)); err != nil {
			return 0, fmt.Errorf("fact reclassify: lock: %w", err)
		}
	}
	for _, c := range changes {
		if _, err := tx.Exec(ctx, `UPDATE knowledge_items SET classification = $2, entity_id = $3 WHERE id = $1`,
			c.id, cls, c.entity); err != nil {
			return 0, fmt.Errorf("fact reclassify: item %s: %w", c.id, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE statement_values SET value_entity_id = $3
			WHERE item_id = $1 AND predicate = $2 AND value_entity_id IS NOT NULL`,
			c.id, domain.FactPredSubject, c.subject); err != nil {
			return 0, fmt.Errorf("fact reclassify: subject of %s: %w", c.id, err)
		}
	}
	for _, k := range keys {
		if err := s.rederive(ctx, tx, k.ns, kind, k.entity, k.actor); err != nil {
			return 0, fmt.Errorf("fact reclassify: re-derive %s: %w", k.entity, err)
		}
	}
	return len(changes), nil
}

// rekeyedFact is the entity id and subject reference a memory_fact item keyed entity
// gets under classification — computed by domain.RekeyFactItem, the one derivation the
// write chokepoint also uses, so a retag and a fresh write of the same fact agree.
// ok is false for an entity id that is not a fact key.
func rekeyedFact(entity string, classification []string) (newEntity, subject string, ok bool) {
	if len(strings.SplitN(entity, "/", 3)) != 3 {
		return "", "", false
	}
	it := domain.RekeyFactItem(domain.KnowledgeItem{
		Kind: domain.KindMemoryFact, EntityID: entity, Classification: classification,
		Values: []domain.StatementValue{{Predicate: domain.FactPredSubject, Type: "entity"}},
	})
	return it.EntityID, it.Values[0].EntityRef, true
}

// sameTags reports whether a and b hold the same tags, in any order.
func sameTags(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
