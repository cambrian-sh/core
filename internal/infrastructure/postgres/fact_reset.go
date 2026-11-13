package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/objects"
)

var _ domain.FactResetter = (*PgKnowledgeStore)(nil)

// ErrFactResetNoSelector refuses a reset with no selector: deleting every fact needs
// FactResetFilter.All, never an empty filter.
var ErrFactResetNoSelector = errors.New("fact reset: no selector (set a namespace prefix, a principal, or All)")

// ResetFacts is ADR-0136 A2-Q3's HARD RESET of the fact layer for a re-run
// (cmd/facts-reset). It deletes, in ONE transaction, for the memory_fact items asserted
// by the selected principals (knowledge_items.asserted_by):
//
//  1. the assessments whose input_item_ids overlap them (their verdicts cite the items);
//  2. the resolution versions that cite them (resolutions.item_id has NO cascade) or sit
//     on their keys (namespace, entity_id, actor);
//  3. the items themselves — statement_values and knowledge_item_embeddings cascade;
//  4. the memory_fact extraction markers of the documents those principals wrote (the
//     backfill lister's own writer selection), so a backfill extracts them again, and
//     their two-phase extraction journal rows (knowledge_extraction_windows, 0025);
//  5. unless KeepLearned, every LEARNED memory_fact predicate profile row (owner answer
//     A2-Q2: profiles are namespace-wide; declared and override rows are kept).
//
// Then, after the commit, it re-derives every SURVIVING key whose inputs changed: keys
// of items cited by a deleted assessment, and keys under a predicate whose learned
// profile was deleted (their derivation read that cardinality).
//
// This is NOT ADR-0137 erasure. Erasure leaves content-free `erased` sentinels so a
// closed chain stays closed and never resurrects a prior value; a reset removes the
// facts, their verdicts and their markers outright, as if extraction had never run for
// the selection. It is a maintenance operation for re-running an experiment, run with
// no ingest writing into the selection.
//
// DryRun runs the same statements and rolls the transaction back: the counts are exact
// and nothing changes (no re-derive runs; Rederived counts the keys that would).
func (s *PgKnowledgeStore) ResetFacts(ctx context.Context, f domain.FactResetFilter) (domain.FactResetReport, error) {
	var rep domain.FactResetReport
	if !f.All && f.Namespace == "" && f.Principal == "" {
		return rep, ErrFactResetNoSelector
	}
	kind := domain.KindMemoryFact

	// The item selection (asserted_by) and the document selection (writer) take their
	// own argument lists.
	var itemArgs []any
	itemArg := func(v any) string {
		itemArgs = append(itemArgs, v)
		return fmt.Sprintf("$%d", len(itemArgs))
	}
	itemWhere := "kind = " + itemArg(kind)
	if f.Principal != "" {
		itemWhere += " AND asserted_by = " + itemArg(f.Principal)
	}
	if f.Namespace != "" {
		itemWhere += " AND asserted_by LIKE " + itemArg(likePrefix(f.Namespace)) + ` ESCAPE '\'`
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return rep, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit

	// The selected items and their keys.
	type nsKey struct {
		ns string
		k  resolutionKey
	}
	var ids, keyNS, keyEntity, keyActor []string
	seenKey := map[nsKey]bool{}
	rows, err := tx.Query(ctx, `SELECT id, namespace_id, entity_id, asserted_by FROM knowledge_items WHERE `+itemWhere, itemArgs...)
	if err != nil {
		return rep, fmt.Errorf("fact reset: select items: %w", err)
	}
	for rows.Next() {
		var id, ns, entity, actor string
		if err := rows.Scan(&id, &ns, &entity, &actor); err != nil {
			rows.Close()
			return rep, err
		}
		ids = append(ids, id)
		k := nsKey{ns, resolutionKey{entity, actor}}
		if !seenKey[k] {
			seenKey[k] = true
			keyNS, keyEntity, keyActor = append(keyNS, ns), append(keyEntity, entity), append(keyActor, actor)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}
	deleted := make(map[string]bool, len(ids))
	for _, id := range ids {
		deleted[id] = true
	}

	// 1. Assessments citing the items; remember the SURVIVING items they also cite.
	survivors := map[string]bool{}
	if len(ids) > 0 {
		rows, err := tx.Query(ctx, `DELETE FROM knowledge_assessments WHERE kind = $1 AND input_item_ids && $2::text[]
			RETURNING input_item_ids`, kind, ids)
		if err != nil {
			return rep, fmt.Errorf("fact reset: delete assessments: %w", err)
		}
		for rows.Next() {
			var inputs []string
			if err := rows.Scan(&inputs); err != nil {
				rows.Close()
				return rep, err
			}
			rep.Assessments++
			for _, in := range inputs {
				if !deleted[in] {
					survivors[in] = true
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return rep, err
		}
	}

	// 2. Resolutions citing the items or on their keys, then 3. the items.
	if len(ids) > 0 {
		tag, err := tx.Exec(ctx, `DELETE FROM resolutions r WHERE r.kind = $1 AND (r.item_id = ANY($2::text[])
			OR (r.namespace_id, r.entity_id, r.actor) IN (SELECT * FROM unnest($3::text[], $4::text[], $5::text[])))`,
			kind, ids, keyNS, keyEntity, keyActor)
		if err != nil {
			return rep, fmt.Errorf("fact reset: delete resolutions: %w", err)
		}
		rep.Resolutions = int(tag.RowsAffected())
		tag, err = tx.Exec(ctx, `DELETE FROM knowledge_items WHERE id = ANY($1::text[])`, ids)
		if err != nil {
			return rep, fmt.Errorf("fact reset: delete items: %w", err)
		}
		rep.Items = int(tag.RowsAffected())
		// The deleted resolutions were inputs of their entities' object rows
		// (ADR-0149 D3); recompute those rows in this transaction.
		byNS := map[string][]string{}
		for i := range keyNS {
			byNS[keyNS[i]] = append(byNS[keyNS[i]], keyEntity[i])
		}
		nss := make([]string, 0, len(byNS))
		for ns := range byNS {
			nss = append(nss, ns)
		}
		sort.Strings(nss)
		for _, ns := range nss {
			if err := objects.ApplyStateTx(ctx, tx, s.kinds, ns, byNS[ns]...); err != nil {
				return rep, fmt.Errorf("fact reset: object rows: %w", err)
			}
		}
	}

	// 4. The extraction markers of the selected writers' documents.
	var docArgs []any
	docArg := func(v any) string {
		docArgs = append(docArgs, v)
		return fmt.Sprintf("$%d", len(docArgs))
	}
	docSel := ` WHERE k.extractor = ` + docArg(FactBackfillExtractor)
	if conds := documentWriterConds(f.Principal, f.Namespace, docArg); len(conds) > 0 {
		docSel += ` AND k.document_id IN (SELECT DISTINCT d.id FROM documents d JOIN chunks c ON c.document_id = d.id WHERE ` +
			strings.Join(conds, " AND ") + `)`
	}
	tag, err := tx.Exec(ctx, `DELETE FROM knowledge_extractions k`+docSel, docArgs...)
	if err != nil {
		return rep, fmt.Errorf("fact reset: delete extraction markers: %w", err)
	}
	rep.Markers = int(tag.RowsAffected())
	// The two-phase backfill's extraction journal (0025) of the same documents: its rows
	// name items deleted above, and a following backfill must extract again.
	if _, err := tx.Exec(ctx, `DELETE FROM knowledge_extraction_windows k`+docSel, docArgs...); err != nil {
		return rep, fmt.Errorf("fact reset: delete extraction journal: %w", err)
	}

	// 5. Learned profiles (all versions), remembering which (namespace, predicate) lost one.
	type nsPred struct{ ns, pred string }
	var lostNS, lostPred []string
	if !f.KeepLearned {
		rows, err := tx.Query(ctx, `DELETE FROM predicate_profiles WHERE kind = $1 AND source = 'learned'
			RETURNING namespace_id, predicate`, kind)
		if err != nil {
			return rep, fmt.Errorf("fact reset: delete learned profiles: %w", err)
		}
		seen := map[nsPred]bool{}
		for rows.Next() {
			var p nsPred
			if err := rows.Scan(&p.ns, &p.pred); err != nil {
				rows.Close()
				return rep, err
			}
			rep.Profiles++
			if !seen[p] {
				seen[p] = true
				lostNS, lostPred = append(lostNS, p.ns), append(lostPred, p.pred)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return rep, err
		}
	}

	// The surviving keys whose derivation inputs changed.
	survivorIDs := make([]string, 0, len(survivors))
	for id := range survivors {
		survivorIDs = append(survivorIDs, id)
	}
	var redo []nsKey
	if len(survivorIDs) > 0 || len(lostPred) > 0 {
		rows, err := tx.Query(ctx, `SELECT DISTINCT namespace_id, entity_id, asserted_by FROM knowledge_items
			WHERE kind = $1 AND (id = ANY($2::text[])
				OR (namespace_id, split_part(entity_id, '/', 3)) IN (SELECT * FROM unnest($3::text[], $4::text[])))`,
			kind, survivorIDs, lostNS, lostPred)
		if err != nil {
			return rep, fmt.Errorf("fact reset: keys to re-derive: %w", err)
		}
		for rows.Next() {
			var k nsKey
			if err := rows.Scan(&k.ns, &k.k.entity, &k.k.actor); err != nil {
				rows.Close()
				return rep, err
			}
			redo = append(redo, k)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return rep, err
		}
	}
	sort.Slice(redo, func(i, j int) bool {
		if redo[i].ns != redo[j].ns {
			return redo[i].ns < redo[j].ns
		}
		return redo[i].k.less(redo[j].k)
	})
	rep.Rederived = len(redo)

	if f.DryRun {
		return rep, nil // the deferred Rollback discards every statement above
	}
	if err := tx.Commit(ctx); err != nil {
		return rep, fmt.Errorf("fact reset: commit: %w", err)
	}
	for i, k := range redo {
		if err := s.rederiveOne(ctx, k.ns, kind, k.k); err != nil {
			return rep, fmt.Errorf("fact reset: committed; re-derive stopped after %d of %d keys: %w", i, len(redo), err)
		}
	}
	return rep, nil
}
