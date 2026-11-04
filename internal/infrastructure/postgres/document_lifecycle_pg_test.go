package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvector "github.com/pgvector/pgvector-go/pgx"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0138 D2–D4 against a real, migrated Postgres (PG_TEST_DSN; schema from `orchestrator
// migrate up` with CAMBRIAN_EMBEDDER__DIMENSIONS=1536, as in CI). Every row uses the
// "lctest" substrate namespace and the "lctest-" id prefix, deleted before and after.

const lcNS = "lctest"

func newLifecycleStores(t *testing.T) (*PgVectorAdapter, *PgKnowledgeStore, *PgDocumentLifecycle, context.Context) {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse PG_TEST_DSN: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error { return pgxvector.RegisterTypes(ctx, conn) }
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name = 'document_versions'`).Scan(&n); err != nil || n == 0 {
		t.Skipf("document_versions missing (run `orchestrator migrate up` first): n=%d err=%v", n, err)
	}
	clean := func() {
		c := context.Background()
		for _, q := range []string{
			`DELETE FROM resolutions WHERE namespace_id = 'lctest'`,
			`DELETE FROM knowledge_assessments WHERE namespace_id = 'lctest'`,
			`DELETE FROM knowledge_items WHERE namespace_id = 'lctest'`,
			`DELETE FROM knowledge_extractions WHERE document_id LIKE 'lctest-%'`,
			`DELETE FROM knowledge_extraction_windows WHERE document_id LIKE 'lctest-%'`,
			`DELETE FROM chunk_triplets WHERE chunk_id LIKE 'lctest-%'`,
			`DELETE FROM chunk_pagerank WHERE chunk_id LIKE 'lctest-%'`,
			`DELETE FROM document_edges WHERE source_id LIKE 'lctest-%' OR target_id LIKE 'lctest-%'`,
			`DELETE FROM document_versions WHERE document_id LIKE 'lctest-%'`,
			`DELETE FROM chunks WHERE id LIKE 'lctest-%'`,
			`DELETE FROM documents WHERE id LIKE 'lctest-%'`,
		} {
			_, _ = pool.Exec(c, q)
		}
	}
	clean()
	t.Cleanup(clean)
	reg, err := domain.NewKindRegistry([]domain.KindSpec{domain.MemoryFactKindSpec()},
		[]domain.ResolutionAuthority{domain.ValidTimeSupersessionAuthority{}})
	if err != nil {
		t.Fatal(err)
	}
	ks := NewPgKnowledgeStore(pool, reg)
	return &PgVectorAdapter{pool: pool, dim: eventTestDim, projectionWrite: true, projModelID: "lc-model"}, ks, NewPgDocumentLifecycle(ks), ctx
}

// seedLifecycleDoc stores document id with n chunks written by writer, plus one of every
// derivative the cascade must reach: KG triplets, PageRank, a section, structure and
// Hebbian edges, a memory_fact item keyed into chunk 1, an extraction marker and a
// journal row. A second, unrelated document "lctest-other" gets the same kinds of rows.
func seedLifecycleDoc(t *testing.T, a *PgVectorAdapter, ks *PgKnowledgeStore, ctx context.Context, id, writer, text, digest string, n int) {
	t.Helper()
	if _, err := a.SaveDocument(ctx, domain.SourceDocument{ID: id, Title: id, Text: text, WrittenBy: writer, ContentDigest: digest}); err != nil {
		t.Fatalf("SaveDocument: %v", err)
	}
	var docs []*domain.Document
	for i := 1; i <= n; i++ {
		cid := id + "-chunk-" + string(rune('0'+i))
		docs = append(docs, &domain.Document{ID: cid, Text: text, DocumentType: domain.DocTypeMnemonicFact,
			Embedding: domain.Embedding{Vector: unitVec(i), Size: eventTestDim, Model: "lc-model"},
			Metadata:  map[string]any{"document_id": id, "source_agent_id": writer}, CreatedAt: time.Now().UTC()})
	}
	if err := a.SaveBatch(ctx, docs); err != nil {
		t.Fatalf("SaveBatch: %v", err)
	}
	c1 := id + "-chunk-1"
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO chunk_triplets (chunk_id, h, r, t) VALUES ($1, 'alice', 'owns', 'boat')`, []any{c1}},
		{`INSERT INTO chunk_pagerank (chunk_id, score) VALUES ($1, 0.5)`, []any{c1}},
		{`INSERT INTO document_sections (id, document_id, title) VALUES ($1, $2, 'S')`, []any{id + "-sec-1", id}},
		{`INSERT INTO document_edges (source_id, target_id, edge_type) VALUES ($1, $2, 'PART_OF')`, []any{c1, id + "-sec-1"}},
		{`INSERT INTO document_edges (source_id, target_id, edge_type) VALUES ('lctest-other-chunk-1', $1, 'co_activated')`, []any{c1}},
		{`INSERT INTO knowledge_extractions (namespace_id, extractor, document_id, content_hash) VALUES ($1, 'memory_fact', $2, 'h')`, []any{lcNS, id}},
		{`INSERT INTO knowledge_extraction_windows (namespace_id, extractor, document_id, content_hash, extractor_version, passage_start, passage_end)
			VALUES ($1, 'memory_fact', $2, 'h', 'v', 0, 1)`, []any{lcNS, id}},
	} {
		if _, err := a.pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed %q: %v", q.sql, err)
		}
	}
	f := domain.Fact{Subject: "alice", Attribute: "owns", Value: "boat " + id, SubjectKey: "alice", AttributeKey: "owns",
		ValueNorm: domain.NormalizeFactText("boat " + id), AssertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Actor: writer, SourceChunkID: c1, SourceDocumentID: id, Quote: "boat"}
	if _, _, err := ks.PutItem(ctx, f.ToItem(lcNS)); err != nil {
		t.Fatalf("PutItem: %v", err)
	}
}

func countRows(t *testing.T, a *PgVectorAdapter, ctx context.Context, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := a.pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// derivativeCounts is what the cascade must reach, for one document.
func derivativeCounts(t *testing.T, a *PgVectorAdapter, ctx context.Context, id string) map[string]int {
	return map[string]int{
		"chunks":     countRows(t, a, ctx, `SELECT count(*) FROM chunks WHERE document_id = $1`, id),
		"triplets":   countRows(t, a, ctx, `SELECT count(*) FROM chunk_triplets WHERE chunk_id LIKE $1`, id+"-chunk-%"),
		"pagerank":   countRows(t, a, ctx, `SELECT count(*) FROM chunk_pagerank WHERE chunk_id LIKE $1`, id+"-chunk-%"),
		"sections":   countRows(t, a, ctx, `SELECT count(*) FROM document_sections WHERE document_id = $1`, id),
		"edges":      countRows(t, a, ctx, `SELECT count(*) FROM document_edges WHERE source_id LIKE $1 OR target_id LIKE $1`, id+"-%"),
		"facts":      countRows(t, a, ctx, `SELECT count(*) FROM knowledge_items ki JOIN statement_values sv ON sv.item_id = ki.id AND sv.predicate = 'source_document' WHERE ki.namespace_id = $1 AND sv.value_text = $2`, lcNS, id),
		"markers":    countRows(t, a, ctx, `SELECT count(*) FROM knowledge_extractions WHERE document_id = $1`, id),
		"journal":    countRows(t, a, ctx, `SELECT count(*) FROM knowledge_extraction_windows WHERE document_id = $1`, id),
		"documents":  countRows(t, a, ctx, `SELECT count(*) FROM documents WHERE id = $1`, id),
		"chunk_embs": countRows(t, a, ctx, `SELECT count(*) FROM chunk_embeddings WHERE chunk_id LIKE $1`, id+"-chunk-%"),
	}
}

// ADR-0138 D3/D4 (T-20261103-re-ingesting-a-changed-document-never-retires,
// T-20261103-forgetting-a-document-leaves-its-memory-fact): a changed re-ingest retires
// the stored version — chunks (incl. the tail a shorter version would have left),
// triplets, PageRank, sections, edges, fact items, markers and journal rows — and leaves
// a content-free version row; an unrelated document is untouched; an unchanged re-ingest
// retires nothing.
func TestPg_DocumentLifecycle_ChangedReingestRetiresEveryDerivative(t *testing.T) {
	a, ks, lc, ctx := newLifecycleStores(t)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-doc", "agent:a", "v1 text", "d1", 3)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-other", "agent:b", "other text", "o1", 2)
	before := derivativeCounts(t, a, ctx, "lctest-doc")
	for k, v := range before {
		if v == 0 {
			t.Fatalf("seed: %s = 0 (%v)", k, before)
		}
	}
	otherBefore := derivativeCounts(t, a, ctx, "lctest-other")

	// Unchanged: nothing retired.
	out, err := lc.ReviseDocument(ctx, domain.DocumentRevision{ID: "lctest-doc", Writer: "agent:a", ContentDigest: "d1", Text: "v1 text"})
	if err != nil || out.Retired || !out.Previous.Exists || out.Version != 1 {
		t.Fatalf("unchanged: %+v %v", out, err)
	}
	if got := derivativeCounts(t, a, ctx, "lctest-doc"); got["chunks"] != 3 || got["facts"] != 1 {
		t.Fatalf("an unchanged re-ingest deleted rows: %v", got)
	}

	// Changed: everything derived from v1 goes; the documents row stays at version 2.
	out, err = lc.ReviseDocument(ctx, domain.DocumentRevision{ID: "lctest-doc", Writer: "agent:a", ContentDigest: "d2", Text: "v2"})
	if err != nil || !out.Retired || out.Version != 2 || out.Previous.Version != 1 {
		t.Fatalf("changed: %+v %v", out, err)
	}
	after := derivativeCounts(t, a, ctx, "lctest-doc")
	for k, v := range after {
		want := 0
		if k == "documents" {
			want = 1
		}
		if k == "edges" {
			// the Hebbian edge from the OTHER document's chunk TO ours is ours to drop too
			want = 0
		}
		if v != want {
			t.Errorf("after retirement %s = %d, want %d", k, v, want)
		}
	}
	if out.Counts["chunks"] != 3 || out.Counts["knowledge_items"] != 1 || out.Counts["chunk_triplets"] != 1 {
		t.Errorf("counts: %v", out.Counts)
	}
	var version int
	var digest, writer string
	if err := a.pool.QueryRow(ctx, `SELECT version, content_digest, written_by FROM documents WHERE id = 'lctest-doc'`).Scan(&version, &digest, &writer); err != nil {
		t.Fatal(err)
	}
	if version != 2 || digest != "d2" || writer != "agent:a" {
		t.Errorf("document row: version=%d digest=%q writer=%q", version, digest, writer)
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM document_versions WHERE document_id = 'lctest-doc' AND version = 1 AND content_digest = 'd1' AND written_by = 'agent:a'`); n != 1 {
		t.Errorf("version row for v1: %d", n)
	}
	// The version row is content-free: it has no text column at all, and its counts are
	// table names to integers.
	if n := countRows(t, a, ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'document_versions' AND column_name IN ('text','body','embedding')`); n != 0 {
		t.Errorf("document_versions carries content columns")
	}

	// The unrelated document lost only the Hebbian edge it shared with the retired chunk.
	otherAfter := derivativeCounts(t, a, ctx, "lctest-other")
	for k, v := range otherBefore {
		want := v
		if k == "edges" {
			want = v - 1
		}
		if otherAfter[k] != want {
			t.Errorf("unrelated document %s: %d -> %d", k, v, otherAfter[k])
		}
	}
}

// ADR-0138 Q1: another writer of a held id is refused and nothing changes; a writer-less
// ingest neither claims nor is refused; a new id is a new document.
func TestPg_DocumentLifecycle_AnotherWriterIsRefused(t *testing.T) {
	a, ks, lc, ctx := newLifecycleStores(t)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-held", "agent:a", "mine", "d1", 2)
	_, err := lc.ReviseDocument(ctx, domain.DocumentRevision{ID: "lctest-held", Writer: "agent:b", ContentDigest: "d2", Text: "theirs"})
	if !errors.Is(err, domain.ErrDocumentOwnedByAnotherWriter) {
		t.Fatalf("want ErrDocumentOwnedByAnotherWriter, got %v", err)
	}
	if got := derivativeCounts(t, a, ctx, "lctest-held"); got["chunks"] != 2 || got["facts"] != 1 {
		t.Fatalf("a refused revision changed rows: %v", got)
	}
	out, err := lc.ReviseDocument(ctx, domain.DocumentRevision{ID: "lctest-held", ContentDigest: "d1", Text: "mine"})
	if err != nil || out.Retired {
		t.Fatalf("writer-less same-content: %+v %v", out, err)
	}
	out, err = lc.ReviseDocument(ctx, domain.DocumentRevision{ID: "lctest-new", Writer: "agent:b", ContentDigest: "x"})
	if err != nil || out.Previous.Exists || out.Retired || out.Version != 1 {
		t.Fatalf("new document: %+v %v", out, err)
	}
}

// ADR-0138 D2: a row written before 0026 has no written_by; its writer of record is the
// source_agent_id its chunks carry, which DocumentRecord reports and the refusal uses.
// SaveDocument never replaces a recorded writer.
func TestPg_DocumentLifecycle_LegacyWriterOfRecordFromChunks(t *testing.T) {
	a, _, lc, ctx := newLifecycleStores(t)
	if _, err := a.SaveDocument(ctx, domain.SourceDocument{ID: "lctest-legacy", Text: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveBatch(ctx, []*domain.Document{{ID: "lctest-legacy-chunk-1", Text: "old", DocumentType: domain.DocTypeMnemonicFact,
		Embedding: domain.Embedding{Vector: unitVec(7), Size: eventTestDim},
		Metadata:  map[string]any{"document_id": "lctest-legacy", "source_agent_id": "agent:old"}, CreatedAt: time.Now().UTC()}}); err != nil {
		t.Fatal(err)
	}
	rec, err := lc.DocumentRecord(ctx, "lctest-legacy")
	if err != nil || !rec.Exists || rec.WrittenBy != "agent:old" || rec.ContentDigest != "" || rec.Version != 1 {
		t.Fatalf("legacy record: %+v %v", rec, err)
	}
	if _, err := lc.ReviseDocument(ctx, domain.DocumentRevision{ID: "lctest-legacy", Writer: "agent:new", Text: "old"}); !errors.Is(err, domain.ErrDocumentOwnedByAnotherWriter) {
		t.Fatalf("legacy row of another writer: %v", err)
	}
	// Same writer, same text, no digest stored: unchanged (nothing retired).
	out, err := lc.ReviseDocument(ctx, domain.DocumentRevision{ID: "lctest-legacy", Writer: "agent:old", ContentDigest: "dd", Text: "old"})
	if err != nil || out.Retired {
		t.Fatalf("legacy unchanged: %+v %v", out, err)
	}
	// SaveDocument records digest and writer, and a later writer-less save keeps both.
	if _, err := a.SaveDocument(ctx, domain.SourceDocument{ID: "lctest-legacy", Text: "old", WrittenBy: "agent:old", ContentDigest: "dd"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SaveDocument(ctx, domain.SourceDocument{ID: "lctest-legacy", Text: "old"}); err != nil {
		t.Fatal(err)
	}
	rec, _ = lc.DocumentRecord(ctx, "lctest-legacy")
	if rec.WrittenBy != "agent:old" || rec.ContentDigest != "dd" {
		t.Fatalf("after saves: %+v", rec)
	}
	// A binary against a legacy (digest-less) row cannot be proven equal: retired.
	if _, err := a.pool.Exec(ctx, `UPDATE documents SET content_digest = NULL WHERE id = 'lctest-legacy'`); err != nil {
		t.Fatal(err)
	}
	out, err = lc.ReviseDocument(ctx, domain.DocumentRevision{ID: "lctest-legacy", Writer: "agent:old", ContentDigest: "bin", Text: "old", HasData: true})
	if err != nil || !out.Retired {
		t.Fatalf("legacy binary: %+v %v", out, err)
	}
}

// ADR-0137 S2: EraseItemsTx's EvidenceIDs selector erases the items of the named
// evidence, inside the caller's transaction (rolled back: nothing changes).
func TestPg_EraseItemsTx_EvidenceSelectorAndCallerTransaction(t *testing.T) {
	a, ks, _, ctx := newLifecycleStores(t)
	if _, err := a.pool.Exec(ctx, `INSERT INTO evidence (id, namespace_id, source_id, source_key, content_hash)
		VALUES ('lctest-ev1', 'default', 'lctest', 'lctest-evdoc', 'h') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = a.pool.Exec(context.Background(), `DELETE FROM evidence WHERE id = 'lctest-ev1'`) })
	f := domain.Fact{Subject: "s", Attribute: "p", Value: "v", SubjectKey: "s", AttributeKey: "p", ValueNorm: "v",
		AssertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Actor: "agent:a", SourceChunkID: "lctest-evdoc-chunk-1", EvidenceID: "lctest-ev1"}
	if _, _, err := ks.PutItem(ctx, f.ToItem(lcNS)); err != nil {
		t.Fatal(err)
	}
	sel := domain.KnowledgeErasure{EvidenceIDs: []string{"lctest-ev1"}}
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n, err := ks.EraseItemsTx(ctx, tx, lcNS, domain.KindMemoryFact, sel)
	if err != nil || n != 1 {
		t.Fatalf("EraseItemsTx = %d %v", n, err)
	}
	_ = tx.Rollback(ctx)
	if c := countRows(t, a, ctx, `SELECT count(*) FROM knowledge_items WHERE namespace_id = $1 AND evidence_id = 'lctest-ev1'`, lcNS); c != 1 {
		t.Fatalf("rolled-back erase removed the item (%d left)", c)
	}
	if n, err := ks.EraseItems(ctx, lcNS, domain.KindMemoryFact, sel); err != nil || n != 1 {
		t.Fatalf("EraseItems = %d %v", n, err)
	}
	if n, err := ks.EraseItems(ctx, lcNS, domain.KindMemoryFact, domain.KnowledgeErasure{ValuePredicate: domain.FactPredSourceDocument}); err != nil || n != 0 {
		t.Fatalf("a predicate without values must match nothing: %d %v", n, err)
	}
}
