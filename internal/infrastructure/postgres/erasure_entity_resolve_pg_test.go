package postgres

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ENG-2245 (owner decision 2026-11-19): the erasure selector resolves a source-doc
// ENTITY id ("source_doc:<uri>", what IngestMemory returns) to its document rows
// through the listing's own resolution (entityDocumentRowsSQL, ENG-2184): the entity
// row's document_id, every entity-stamped chunk's document and an embed job staging
// one. Resolve is content-free and unscoped — the enforcing eraser decides each
// candidate under the caller's scope — so every row of the entity is a document
// candidate with Via = the entity id. Erase (a kernel caller that skipped Resolve)
// expands an entity id the same way (expandEntityIDs).
func TestPg_ErasureResolve_ExpandsASourceDocEntityID(t *testing.T) {
	pool, ctx := newOwnerFencePool(t)
	store := NewPgErasureStore(NewPgDocumentLifecycle(NewPgKnowledgeStore(pool, nil)))

	run := fmt.Sprintf("eng2245-%d", time.Now().UnixNano())
	entity := domain.SourceDocEntityPrefix + run + ".pdf"
	rowLatest, rowEarlier, rowStaging, rowOwned, plain :=
		run+".pdf#w=latest", run+".pdf#w=earlier", run+".pdf#w=staging", run+".pdf#w=owned", run+"-plain#w=x"
	bg := context.Background()
	t.Cleanup(func() {
		_, _ = pool.Exec(bg, `DELETE FROM derivation_jobs WHERE document_id LIKE $1 || '%'`, run)
		_, _ = pool.Exec(bg, `DELETE FROM chunks WHERE id LIKE $1 || '%' OR id = $2`, run, entity)
		_, _ = pool.Exec(bg, `DELETE FROM documents WHERE id LIKE $1 || '%'`, run)
	})
	for _, id := range []string{rowLatest, rowEarlier, rowStaging, rowOwned, plain} {
		tags := []string{}
		if id == rowOwned {
			tags = []string{"owner:bob"}
		}
		if _, err := pool.Exec(ctx, `INSERT INTO documents (id, title, source_type, tags, written_by) VALUES ($1, $1, 'operator_ingest', $2, 'alice')`, id, tags); err != nil {
			t.Fatalf("insert document %s: %v", id, err)
		}
	}
	chunk := func(id, documentID, meta string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO chunks (id, document_id, text, metadata) VALUES ($1, $2, 'x', $3::jsonb)`, id, documentID, meta); err != nil {
			t.Fatalf("insert chunk %s: %v", id, err)
		}
	}
	chunk(entity, rowLatest, `{"kind":"source_document","document_id":"`+rowLatest+`"}`)
	chunk(run+"-c1", rowEarlier, `{"source_doc_entity_id":"`+entity+`"}`)
	chunk(run+"-c2", rowOwned, `{"source_doc_entity_id":"`+entity+`"}`)
	chunk(run+"-c3", plain, `{}`)
	if _, err := pool.Exec(ctx, `INSERT INTO derivation_jobs (kind, document_id, chunk_ids, staged, chunks_total)
		VALUES ('embed', $1, '{}', $2::jsonb, 10)`, rowStaging, `{"doc":{"entity_id":"`+entity+`"}}`); err != nil {
		t.Fatalf("insert embed job: %v", err)
	}

	cands, err := store.Resolve(ctx, domain.ErasureSelector{DocumentIDs: []string{entity, plain}})
	if err != nil {
		t.Fatal(err)
	}
	via := map[string]string{}
	for _, c := range cands {
		if c.Kind != domain.ErasureTargetDocument || c.DocumentID != c.ID {
			t.Fatalf("candidate %+v: want a document candidate", c)
		}
		via[c.ID] = c.Via
	}
	want := map[string]string{rowLatest: entity, rowEarlier: entity, rowStaging: entity, rowOwned: entity, plain: plain}
	if len(via) != len(want) {
		t.Fatalf("candidates = %v, want %v", via, want)
	}
	for id, v := range want {
		if via[id] != v {
			t.Fatalf("candidate %s via %q, want %q (all: %v)", id, via[id], v, via)
		}
	}
	// The owner tag travels on the candidate, so the enforcing eraser can hide it.
	for _, c := range cands {
		if c.ID == rowOwned && !slices.Contains(c.Tags, "owner:bob") {
			t.Fatalf("owned row lost its tags: %+v", c)
		}
	}

	// An entity nobody ingested names nothing.
	if cands, err := store.Resolve(ctx, domain.ErasureSelector{DocumentIDs: []string{domain.SourceDocEntityPrefix + run + "-none"}}); err != nil || len(cands) != 0 {
		t.Fatalf("unknown entity = %+v (%v)", cands, err)
	}

	// Erase's own expansion (a kernel caller passes the selector unresolved).
	rows, viaMap, err := expandEntityIDs(ctx, pool, []string{entity, plain, entity})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(rows)
	wantRows := []string{plain, rowEarlier, rowLatest, rowOwned, rowStaging}
	sort.Strings(wantRows)
	if !slices.Equal(rows, wantRows) || viaMap[rowEarlier] != entity || viaMap[plain] != plain {
		t.Fatalf("expandEntityIDs = %v %v, want %v", rows, viaMap, wantRows)
	}
}
