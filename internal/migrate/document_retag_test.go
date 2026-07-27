package migrate

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// These exercise the property the whole split was for: a document's classification and
// the derived copies on its chunks move TOGETHER.
//
// They live in this package rather than beside the adapter because they need the real
// migrated schema, and the migration is what defines it. Same disposable-database guard.

func retagFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	freshSchema(ctx, t, pool)
	applyThrough(ctx, t, pool, 6)
	seedLegacyCorpus(ctx, t, pool)
	applyThrough(ctx, t, pool, 7)
}

// retag mirrors PgVectorAdapter.RetagDocument. It is reproduced here rather than
// imported because internal/infrastructure/postgres imports internal/memory, and
// importing it from this package would create a cycle through the migration fixtures.
// The SQL is the part under test.
func retag(ctx context.Context, t *testing.T, pool *pgxpool.Pool, docID string, tags []string, tagsJSON string) error {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`UPDATE documents SET tags = $2, updated_at = NOW() WHERE id = $1`, docID, tags); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE chunks SET metadata = jsonb_set(COALESCE(metadata,'{}'::jsonb), '{tags}', $2::jsonb, true)
		 WHERE document_id = $1`, docID, tagsJSON); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Re-classifying a document must move the document row AND every chunk's cached copy.
// Under the old shape there was no document row to move and the chunks were the only
// record, which is what made a partial update invisible.
func TestRetag_MovesTheDocumentAndEveryChunkTogether(t *testing.T) {
	ctx, pool := splitTestPool(t)
	retagFixture(ctx, t, pool)

	if err := retag(ctx, t, pool, "doc-a", []string{"secrets"}, `["secrets"]`); err != nil {
		t.Fatalf("retag: %v", err)
	}

	var tags []string
	if err := pool.QueryRow(ctx, `SELECT tags FROM documents WHERE id='doc-a'`).Scan(&tags); err != nil {
		t.Fatalf("read document tags: %v", err)
	}
	if len(tags) != 1 || tags[0] != "secrets" {
		t.Errorf("document tags = %v, want [secrets]", tags)
	}

	// Both chunks of doc-a carry the new tag...
	if got := count(ctx, t, pool,
		`SELECT count(*) FROM chunks WHERE document_id='doc-a' AND metadata->'tags' @> '["secrets"]'`); got != 2 {
		t.Errorf("chunks carrying the new tag = %d, want 2", got)
	}
	// ...and none of them still carries the old one. A stale copy left behind is the
	// half-classified document this design exists to make impossible.
	if got := count(ctx, t, pool,
		`SELECT count(*) FROM chunks WHERE document_id='doc-a' AND metadata->'tags' @> '["airline"]'`); got != 0 {
		t.Errorf("%d chunks still carry the OLD tag — the document is half-classified", got)
	}
}

// Re-tagging one document must not touch another's chunks, or the blast radius of a
// classification change would be the whole corpus.
func TestRetag_LeavesOtherDocumentsAlone(t *testing.T) {
	ctx, pool := splitTestPool(t)
	retagFixture(ctx, t, pool)

	// A tag NOTHING else carries, so "doc-b is unchanged" cannot pass by coincidence.
	// doc-b is seeded with ["secrets"]; retagging doc-a to "secrets" would have made
	// this test pass even while leaking.
	if err := retag(ctx, t, pool, "doc-a", []string{"quarantine"}, `["quarantine"]`); err != nil {
		t.Fatalf("retag: %v", err)
	}

	var tags []string
	if err := pool.QueryRow(ctx, `SELECT tags FROM documents WHERE id='doc-b'`).Scan(&tags); err != nil {
		t.Fatalf("read doc-b: %v", err)
	}
	if len(tags) != 1 || tags[0] != "secrets" {
		t.Errorf("doc-b tags changed to %v — a retag leaked across documents", tags)
	}
	if got := count(ctx, t, pool,
		`SELECT count(*) FROM chunks WHERE document_id='doc-b' AND metadata->'tags' @> '["quarantine"]'`); got != 0 {
		t.Errorf("%d of doc-b's chunks picked up doc-a's new tag", got)
	}
	// The parentless agent memory has no document and must be untouched by any retag.
	if got := count(ctx, t, pool,
		`SELECT count(*) FROM chunks WHERE id='mem-1' AND metadata ? 'tags'`); got != 0 {
		t.Errorf("an unparented memory acquired tags from a document retag")
	}
}

// The transaction is the guarantee. If the chunk update fails, the document row must
// not have moved either — otherwise the authoritative copy and the cache disagree,
// which is strictly worse than the old shape because it looks correct.
func TestRetag_RollsBackTheDocumentWhenChunksFail(t *testing.T) {
	ctx, pool := splitTestPool(t)
	retagFixture(ctx, t, pool)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE documents SET tags = $2 WHERE id = $1`, "doc-a", []string{"secrets"}); err != nil {
		t.Fatalf("update document: %v", err)
	}
	// Malformed JSON stands in for any failure of the second statement.
	if _, err := tx.Exec(ctx,
		`UPDATE chunks SET metadata = jsonb_set(COALESCE(metadata,'{}'::jsonb), '{tags}', $2::jsonb, true)
		 WHERE document_id = $1`, "doc-a", "not-valid-json"); err == nil {
		t.Fatal("expected the chunk update to fail")
	}
	_ = tx.Rollback(ctx)

	var tags []string
	if err := pool.QueryRow(ctx, `SELECT tags FROM documents WHERE id='doc-a'`).Scan(&tags); err != nil {
		t.Fatalf("read document: %v", err)
	}
	seen := map[string]bool{}
	for _, tg := range tags {
		seen[tg] = true
	}
	if !seen["airline"] {
		t.Errorf("document tags = %v — the failed retag was not rolled back, so the document row and its chunks now disagree", tags)
	}
}

// Deleting a document must take its chunks with it: an orphaned chunk of a deleted
// document is unreachable data that still answers searches.
func TestRetag_DeletingADocumentCascadesToChunks(t *testing.T) {
	ctx, pool := splitTestPool(t)
	retagFixture(ctx, t, pool)

	if _, err := pool.Exec(ctx, `DELETE FROM documents WHERE id='doc-a'`); err != nil {
		t.Fatalf("delete document: %v", err)
	}
	if got := count(ctx, t, pool, `SELECT count(*) FROM chunks WHERE document_id='doc-a'`); got != 0 {
		t.Errorf("%d chunks survived their document", got)
	}
	// The unparented memory is untouched — it never belonged to that document.
	if got := count(ctx, t, pool, `SELECT count(*) FROM chunks WHERE id='mem-1'`); got != 1 {
		t.Errorf("an unparented memory was deleted with an unrelated document")
	}
}
