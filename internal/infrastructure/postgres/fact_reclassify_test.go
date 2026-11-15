package postgres

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ENG-489 (owner decision 2026-11-15): a document retag rewrites its memory_fact items
// IN PLACE — classification, the classification-derived entity key and the subject
// reference — in the same transaction as the document row and chunk tag cache.

func TestRekeyedFact_MatchesTheWriteChokepointDerivation(t *testing.T) {
	t.Parallel()
	old := domain.FactEntityID(domain.FactScopeKey([]string{"public"}), "user", "home_city")
	entity, subject, ok := rekeyedFact(old, []string{"secret"})
	scope := domain.FactScopeKey([]string{"secret"})
	if !ok || entity != domain.FactEntityID(scope, "user", "home_city") || subject != domain.FactSubjectRef(scope, "user") {
		t.Fatalf("rekeyedFact = %q %q %v, want the RekeyFactItem key under the new scope", entity, subject, ok)
	}
	if _, _, ok := rekeyedFact("not-a-fact-key", []string{"x"}); ok {
		t.Fatal("a non-fact entity id must be left alone")
	}
}

// itemSnapshot is everything in a fact item that a reclassification must NOT change:
// the item row without classification/entity_id, and its statement values without the
// subject reference.
func itemSnapshot(t *testing.T, ks *PgKnowledgeStore, ctx context.Context, id string) string {
	t.Helper()
	var b strings.Builder
	var ns, kind, actor, sourceRef string
	var evidence *string
	var assertedAt, createdAt time.Time
	var neg bool
	var vf, vt *time.Time
	var schema int
	if err := ks.pool.QueryRow(ctx, `SELECT namespace_id, kind, evidence_id, asserted_by, asserted_at, source_ref,
		negation, valid_from, valid_to, schema_version, created_at FROM knowledge_items WHERE id = $1`, id).
		Scan(&ns, &kind, &evidence, &actor, &assertedAt, &sourceRef, &neg, &vf, &vt, &schema, &createdAt); err != nil {
		t.Fatalf("snapshot %s: %v", id, err)
	}
	fmt.Fprintf(&b, "%s|%s|%s|%s|%s|%s|%v|%s|%s|%d|%s\n", ns, kind, deref(evidence), actor, assertedAt, sourceRef, neg, deref(vf), deref(vt), schema, createdAt)
	rows, err := ks.pool.Query(ctx, `SELECT predicate, value_type, value_date, value_number, value_text, value_entity_id
		FROM statement_values WHERE item_id = $1 AND predicate <> $2 ORDER BY predicate`, id, domain.FactPredSubject)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var pred, typ string
		var d *time.Time
		var n *float64
		var txt, ent *string
		if err := rows.Scan(&pred, &typ, &d, &n, &txt, &ent); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s|%s|%s|%s|%s|%s\n", pred, typ, deref(d), deref(n), deref(txt), deref(ent))
	}
	return b.String()
}

// deref renders a nullable column value (never the pointer itself).
func deref[T any](p *T) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprint(*p)
}

func TestPg_RetagReclassifiesTheDocumentsFactsInPlace(t *testing.T) {
	ks, _, ctx := newFactSubstrate(t)
	const ns = "default" // the query plane reads the default namespace
	reader := NewPgFactItemReader(ks, ns, "a1-model", eventTestDim)
	const doc, chunk = "a1test-eng489-doc", "a1test-eng489-doc-chunk-1"
	cleanup := func() {
		c := context.Background()
		for _, q := range []string{
			`DELETE FROM resolutions WHERE kind = 'memory_fact' AND entity_id LIKE '%/eng489_attr'`,
			`DELETE FROM knowledge_items WHERE kind = 'memory_fact' AND entity_id LIKE '%/eng489_attr'`,
			`DELETE FROM documents WHERE id = '` + doc + `'`,
		} {
			_, _ = ks.pool.Exec(c, q)
		}
	}
	cleanup()
	t.Cleanup(cleanup)
	if _, err := ks.pool.Exec(ctx, `INSERT INTO documents (id, source_type, tags) VALUES ($1, 'a1src', ARRAY['public'])`, doc); err != nil {
		t.Fatal(err)
	}
	mk := func(value string, from string) domain.KnowledgeItem {
		f := domain.Fact{
			Subject: "user", Attribute: "eng489 attr", Value: value, SubjectKey: "user",
			AttributeKey: domain.NormalizeFactAttribute("eng489 attr"), ValueNorm: domain.NormalizeFactText(value),
			ValidFrom: a1Day(from), AssertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Actor: "alice",
			SourceChunkID: chunk, SourceDocumentID: doc, TurnIndex: 0, Quote: value, Tags: []string{"public"},
		}
		return f.ToItem(ns)
	}
	older, newer := mk("lisbon", "2024-01-01"), mk("porto", "2025-01-01")
	putAll(t, ks, ctx, older, newer)
	oldKey := older.EntityID
	if !strings.HasSuffix(oldKey, "/eng489_attr") {
		t.Fatalf("fixture key %q", oldKey)
	}
	before := map[string]string{}
	for _, it := range []domain.KnowledgeItem{older, newer} {
		before[string(it.ID)] = itemSnapshot(t, ks, ctx, string(it.ID))
	}

	plane := NewPgQueryPlane(ks.pool, NewPgEventStore(ks.pool, nil), ks, nil, nil, nil, nil)
	current := func(entity string, scope *domain.TagPredicate) []domain.QueryRow {
		t.Helper()
		res, err := plane.Query(ctx, domain.KnowledgeQuery{Kind: domain.QueryCurrent, ItemKind: domain.KindMemoryFact,
			Policy: domain.PolicyValidTimeSupersession, EntityID: entity}, scope)
		if err != nil {
			t.Fatalf("query plane current: %v", err)
		}
		return res.Rows
	}
	noSecret := &domain.TagPredicate{ForbiddenTags: []string{"secret"}}
	if rows := current(oldKey, noSecret); len(rows) == 0 {
		t.Fatal("precondition: the public fact is current on the query plane")
	}

	// Narrowing retag: public -> secret, through the lifecycle (one transaction).
	if err := NewPgDocumentLifecycle(ks).RetagDocument(ctx, doc, []string{"secret"}); err != nil {
		t.Fatalf("RetagDocument: %v", err)
	}
	scope := domain.FactScopeKey([]string{"secret"})
	newKey := domain.FactEntityID(scope, "user", "eng489_attr")

	// 1. The items carry the new classification and key; nothing else changed.
	for _, it := range []domain.KnowledgeItem{older, newer} {
		var cls []string
		var entity, subject string
		if err := ks.pool.QueryRow(ctx, `SELECT k.classification, k.entity_id, sv.value_entity_id FROM knowledge_items k
			JOIN statement_values sv ON sv.item_id = k.id AND sv.predicate = $2 WHERE k.id = $1`,
			string(it.ID), domain.FactPredSubject).Scan(&cls, &entity, &subject); err != nil {
			t.Fatal(err)
		}
		sort.Strings(cls)
		if !reflect.DeepEqual(cls, []string{"secret"}) || entity != newKey || subject != domain.FactSubjectRef(scope, "user") {
			t.Fatalf("item %s after retag: cls=%v entity=%s subject=%s", it.ID, cls, entity, subject)
		}
		if got := itemSnapshot(t, ks, ctx, string(it.ID)); got != before[string(it.ID)] {
			t.Fatalf("item %s changed beyond its classification:\nbefore %s\nafter  %s", it.ID, before[string(it.ID)], got)
		}
	}

	// 2. Query plane (what the premium consultant reads): the old key has no current
	// belief; the new key's belief is hidden from a reader that may not see "secret" and
	// visible to one that may.
	if rows := current(oldKey, &domain.TagPredicate{Bypass: true}); len(rows) != 0 {
		t.Fatalf("the old key must have no current belief after the retag: %+v", rows)
	}
	if rows := current(newKey, noSecret); len(rows) != 0 {
		t.Fatalf("a narrowed fact leaked through the query plane: %+v", rows)
	}
	if rows := current(newKey, &domain.TagPredicate{Bypass: true}); len(rows) == 0 {
		t.Fatal("the reclassified fact must be current under its new key")
	}

	// 3. FactChain and FactsByID read the new classification.
	moved := domain.FactFromItem(newer)
	moved.EntityID, moved.ScopeKey = newKey, scope
	if chain, err := reader.FactChain(ctx, moved, noSecret); err != nil || len(chain) != 0 {
		t.Fatalf("FactChain under a narrowed scope: %v %v", chain, err)
	}
	if chain, err := reader.FactChain(ctx, moved, &domain.TagPredicate{Bypass: true}); err != nil || len(chain) != 2 {
		t.Fatalf("FactChain under the new key: %d facts, err %v; want both values", len(chain), err)
	}
	if got, err := reader.FactsByID(ctx, []domain.KnowledgeItemID{older.ID, newer.ID}, noSecret); err != nil || len(got) != 0 {
		t.Fatalf("FactsByID under a narrowed scope: %v %v", got, err)
	}

	// 4. The document row and its tag moved in the same operation.
	var tags []string
	if err := ks.pool.QueryRow(ctx, `SELECT tags FROM documents WHERE id = $1`, doc).Scan(&tags); err != nil || !reflect.DeepEqual(tags, []string{"secret"}) {
		t.Fatalf("document tags %v err %v", tags, err)
	}

	// 5. Idempotent: the same retag again rewrites nothing.
	if n, err := func() (int, error) {
		tx, err := ks.pool.Begin(ctx)
		if err != nil {
			return 0, err
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		return ks.reclassifyDocumentFactsTx(ctx, tx, doc, []string{"secret"})
	}(); err != nil || n != 0 {
		t.Fatalf("a repeated retag rewrote %d items (err %v), want 0", n, err)
	}
}
