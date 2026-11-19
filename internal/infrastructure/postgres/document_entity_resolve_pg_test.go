package postgres

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ENG-2184 (owner decision 2026-11-19): the document listing's id lookup accepts a
// source-doc ENTITY id ("source_doc:<uri>", what IngestMemory returns and every
// MemoryWrittenOp carries) and resolves it to the document rows it stands for, through
// the links the ingest wrote — never by re-deriving a row id from the URI:
//
//   - the entity row's own document_id (the latest writer's row);
//   - a chunk stamped with the entity (metadata.source_doc_entity_id) — an earlier
//     writer's embedded row;
//   - an embed job staging the entity (derivation_jobs.staged.doc.entity_id) — a row
//     accepted for background embedding whose chunks are not written yet.
//
// Several writers are several rows, newest first. The read scope still applies: a row
// owned by someone else (owner:<id>) is resolved only for a reader holding that owner.
// The file name sorts before migration_guard_test.go (see fence_owner_scope_pg_test.go).
func TestPg_ListDocuments_ResolvesASourceDocEntityID(t *testing.T) {
	pool, ctx := newOwnerFencePool(t)
	p := &PgVectorAdapter{pool: pool, dim: eventTestDim}

	run := fmt.Sprintf("eng2184-%d", time.Now().UnixNano())
	entity := domain.SourceDocEntityPrefix + run + ".pdf"
	other := domain.SourceDocEntityPrefix + run + "-other.pdf"
	rowLatest, rowEarlier, rowStaging, rowOwned, rowUnrelated :=
		run+".pdf#w=latest", run+".pdf#w=earlier", run+".pdf#w=staging", run+".pdf#w=owned", run+"-other.pdf#w=x"
	bg := context.Background()
	t.Cleanup(func() {
		_, _ = pool.Exec(bg, `DELETE FROM derivation_jobs WHERE document_id LIKE $1 || '%'`, run)
		_, _ = pool.Exec(bg, `DELETE FROM chunks WHERE id LIKE $1 || '%' OR id IN ($2, $3)`, run, entity, other)
		_, _ = pool.Exec(bg, `DELETE FROM documents WHERE id LIKE $1 || '%'`, run)
	})

	base := time.Now().UTC().Add(-time.Hour)
	doc := func(id string, age time.Duration, tags ...string) {
		t.Helper()
		if tags == nil {
			tags = []string{}
		}
		if _, err := pool.Exec(ctx, `INSERT INTO documents (id, title, source_type, tags, created_at, updated_at)
			VALUES ($1, $1, 'operator_ingest', $2, $3, $3)`, id, tags, base.Add(age)); err != nil {
			t.Fatalf("insert document %s: %v", id, err)
		}
	}
	doc(rowEarlier, 1*time.Minute)
	doc(rowStaging, 2*time.Minute)
	doc(rowOwned, 3*time.Minute, "owner:bob")
	doc(rowLatest, 4*time.Minute)
	doc(rowUnrelated, 5*time.Minute)

	chunk := func(id, documentID string, meta string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO chunks (id, document_id, text, metadata) VALUES ($1, $2, 'x', $3::jsonb)`,
			id, documentID, meta); err != nil {
			t.Fatalf("insert chunk %s: %v", id, err)
		}
	}
	// The entity row (minted at ingest) points at the latest writer's row.
	chunk(entity, rowLatest, `{"kind":"source_document","document_id":"`+rowLatest+`"}`)
	// An earlier writer's embedded chunk, and bob's personal row's chunk.
	chunk(run+"-c1", rowEarlier, `{"source_doc_entity_id":"`+entity+`"}`)
	chunk(run+"-c2", rowOwned, `{"source_doc_entity_id":"`+entity+`"}`)
	// Another entity's rows never answer for this one.
	chunk(other, rowUnrelated, `{"kind":"source_document","document_id":"`+rowUnrelated+`"}`)
	// A row accepted for background embedding: only its embed job names the entity.
	if _, err := pool.Exec(ctx, `INSERT INTO derivation_jobs (kind, document_id, chunk_ids, staged, chunks_total)
		VALUES ('embed', $1, '{}', $2::jsonb, 10)`, rowStaging, `{"doc":{"entity_id":"`+entity+`"}}`); err != nil {
		t.Fatalf("insert embed job: %v", err)
	}

	ids := func(docs []domain.DocumentSummary) []string {
		out := make([]string, 0, len(docs))
		for _, d := range docs {
			out = append(out, d.ID)
		}
		return out
	}

	// The operator plane's listing (owner fence, no owners held): every unowned row of
	// the entity, newest first; bob's row is not resolved.
	page, next, total, err := p.ListDocuments(ctx, domain.DocumentFilter{IDPrefix: entity})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids(page), []string{rowLatest, rowStaging, rowEarlier}; !slices.Equal(got, want) {
		t.Fatalf("entity rows = %v, want %v (newest first, owned row fenced)", got, want)
	}
	if total != 3 || next != "" {
		t.Fatalf("total = %d next = %q, want 3 and no cursor", total, next)
	}

	// Limit 1 is the entity's single answer: the most recently written readable row.
	page, _, _, err = p.ListDocuments(ctx, domain.DocumentFilter{IDPrefix: entity, Limit: 1})
	if err != nil || len(page) != 1 || page[0].ID != rowLatest {
		t.Fatalf("limit 1 = %v (%v), want [%s]", ids(page), err, rowLatest)
	}

	// A reader holding bob's owner resolves his row too (owner fence, §2.4/§2.5) ...
	page, _, _, err = p.ListDocuments(ctx, domain.DocumentFilter{IDPrefix: entity, Owners: []string{"bob"}})
	if err != nil || !slices.Contains(ids(page), rowOwned) {
		t.Fatalf("bob's listing = %v (%v), want his row included", ids(page), err)
	}
	// ... and a principal-serving scope that forbids nothing but holds no owner does not.
	page, _, _, err = p.ListDocuments(ctx, domain.DocumentFilter{IDPrefix: entity, Scope: &domain.TagPredicate{}})
	if err != nil || slices.Contains(ids(page), rowOwned) || len(page) != 3 {
		t.Fatalf("scoped listing = %v (%v), want the 3 unowned rows", ids(page), err)
	}

	// The other entity resolves only to its own row; an unknown entity to nothing.
	if page, _, _, err = p.ListDocuments(ctx, domain.DocumentFilter{IDPrefix: other}); err != nil || !slices.Equal(ids(page), []string{rowUnrelated}) {
		t.Fatalf("other entity = %v (%v)", ids(page), err)
	}
	if page, _, total, err = p.ListDocuments(ctx, domain.DocumentFilter{IDPrefix: domain.SourceDocEntityPrefix + run + "-none"}); err != nil || len(page) != 0 || total != 0 {
		t.Fatalf("unknown entity = %v total %d (%v)", ids(page), total, err)
	}

	// A plain row-id prefix is the prefix match it always was.
	if page, _, _, err = p.ListDocuments(ctx, domain.DocumentFilter{IDPrefix: run + ".pdf#w=l"}); err != nil || !slices.Equal(ids(page), []string{rowLatest}) {
		t.Fatalf("row-id prefix = %v (%v)", ids(page), err)
	}
}
