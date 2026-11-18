package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ENG-501 (ADR-0137 D5 / ADR-0138 D3) against a real, migrated Postgres: the write
// fence refuses a KG triplet save, a fact item and an extraction marker whose context
// names a document version that ReviseDocument has retired — the new version reuses the
// chunk ids — and lets the current version, a write with no version, and a version of
// another document through. ReviseDocument serializes with an in-flight fenced write on
// the per-document advisory key.
func TestPg_SourceVersionFence_RefusesRetiredVersionWrites(t *testing.T) {
	a, ks, lc, ctx := newLifecycleStores(t)
	const id, writer = "lctest-svf", "alice"
	seedLifecycleDoc(t, a, ks, ctx, id, writer, "version one text", "digest-v1", 1)
	chunk := id + "-chunk-1"
	v1 := domain.WithSourceVersion(ctx, domain.SourceVersion{DocumentID: id, ContentDigest: "digest-v1"})
	v2 := domain.WithSourceVersion(ctx, domain.SourceVersion{DocumentID: id, ContentDigest: "digest-v2"})

	// Positive control: the current version writes.
	if err := a.SaveChunkTriplets(v1, chunk, []domain.ChunkTriplet{{H: "v1head", R: "r", T: "t", Weight: 1}}); err != nil {
		t.Fatalf("current version's triplets: %v", err)
	}

	// A revise holding the advisory key waits for an in-flight fenced write of the document.
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := erasureFenceTx(v1, tx, id, ""); err != nil {
		t.Fatalf("fence of the current version: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		out, err := lc.ReviseDocument(ctx, domain.DocumentRevision{ID: id, Writer: writer, ContentDigest: "digest-v2", Text: "version two text"})
		if err == nil && !out.Retired {
			err = errors.New("revise did not retire version 1")
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("ReviseDocument finished while a fenced write held the document (err=%v): it must take the advisory key", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ReviseDocument: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ReviseDocument did not finish after the fenced write committed")
	}

	// The new version saves its chunk under the SAME id.
	if err := a.SaveBatch(ctx, []*domain.Document{{ID: chunk, Text: "version two text", DocumentType: domain.DocTypeMnemonicFact,
		Embedding: domain.Embedding{Vector: unitVec(1), Size: eventTestDim, Model: "lc-model"},
		Metadata:  map[string]any{"document_id": id, "source_agent_id": writer}, CreatedAt: time.Now().UTC()}}); err != nil {
		t.Fatalf("SaveBatch v2: %v", err)
	}

	// The retired version's writes are refused, with the version sentinel (not erasure).
	err = a.SaveChunkTriplets(v1, chunk, []domain.ChunkTriplet{{H: "v1late", R: "r", T: "t", Weight: 1}})
	if !errors.Is(err, domain.ErrSourceVersionRetired) || errors.Is(err, domain.ErrSourceErased) {
		t.Fatalf("retired version's triplets: want ErrSourceVersionRetired, got %v", err)
	}
	fact := func(v string) domain.KnowledgeItem {
		return domain.Fact{Subject: "alice", Attribute: "owns", Value: v, SubjectKey: "alice", AttributeKey: "owns",
			ValueNorm: domain.NormalizeFactText(v), AssertedAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
			Actor: writer, SourceChunkID: chunk, SourceDocumentID: id, Quote: v}.ToItem(lcNS)
	}
	if _, _, err := ks.PutItem(v1, fact("v1late fact")); !errors.Is(err, domain.ErrSourceVersionRetired) {
		t.Fatalf("retired version's fact: want ErrSourceVersionRetired, got %v", err)
	}
	marker := domain.ExtractionMarker{NamespaceID: lcNS, Extractor: domain.KindMemoryFact, DocumentID: id, ContentHash: "chunk-ids-hash", Items: 1}
	if err := ks.MarkExtracted(v1, marker); !errors.Is(err, domain.ErrSourceVersionRetired) {
		t.Fatalf("retired version's marker: want ErrSourceVersionRetired, got %v", err)
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM chunk_triplets WHERE chunk_id = $1 AND h LIKE 'v1%'`, chunk); n != 0 {
		t.Fatalf("%d triplets of the retired version on the new version's chunk", n)
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM statement_values WHERE value_text LIKE '%v1late%'`); n != 0 {
		t.Fatalf("%d fact values of the retired version", n)
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM knowledge_extractions WHERE document_id = $1`, id); n != 0 {
		t.Fatalf("%d extraction markers of the retired version", n)
	}

	// The current version, a write with no version, and another document's version pass.
	if err := a.SaveChunkTriplets(v2, chunk, []domain.ChunkTriplet{{H: "v2head", R: "r", T: "t", Weight: 1}}); err != nil {
		t.Fatalf("current version's triplets: %v", err)
	}
	if _, _, err := ks.PutItem(v2, fact("v2 fact")); err != nil {
		t.Fatalf("current version's fact: %v", err)
	}
	if err := ks.MarkExtracted(v2, marker); err != nil {
		t.Fatalf("current version's marker: %v", err)
	}
	if err := a.SaveChunkTriplets(ctx, chunk, []domain.ChunkTriplet{{H: "unversioned", R: "r", T: "t", Weight: 1}}); err != nil {
		t.Fatalf("a write with no version is not fenced by version: %v", err)
	}
	other := domain.WithSourceVersion(ctx, domain.SourceVersion{DocumentID: "lctest-someone-else", ContentDigest: "x"})
	if err := a.SaveChunkTriplets(other, chunk, []domain.ChunkTriplet{{H: "otherdoc", R: "r", T: "t", Weight: 1}}); err != nil {
		t.Fatalf("a version of another document does not apply: %v", err)
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM chunk_triplets WHERE chunk_id = $1 AND h = 'v2head'`, chunk); n != 1 {
		t.Fatalf("current version's triplet rows = %d, want 1", n)
	}
}
