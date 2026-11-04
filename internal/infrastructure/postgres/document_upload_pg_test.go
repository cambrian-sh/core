package postgres

import (
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0138 Amendment B1 against a real, migrated Postgres (PG_TEST_DSN; 0026 + 0027).
// Same fixture and "lctest-" prefix as document_lifecycle_pg_test.go.

// B1.1: the exact-duplicate lookup is per writer, needs the stored version to still
// hold content, and is deterministic (oldest first).
func TestPg_DocumentLifecycle_FindByContentIsPerWriterAndNeedsContent(t *testing.T) {
	a, ks, lc, ctx := newLifecycleStores(t)
	const digest = "b1digest00000000000000000000000000000000000000000000000000000001"
	seedLifecycleDoc(t, a, ks, ctx, "lctest-upl-old", "op:a", "same bytes", digest, 1)
	time.Sleep(10 * time.Millisecond) // created_at orders the two copies
	seedLifecycleDoc(t, a, ks, ctx, "lctest-upl-new", "op:a", "same bytes", digest, 1)
	// A documents row with the digest but no chunk (a failed earlier write) never counts.
	if _, err := a.SaveDocument(ctx, domain.SourceDocument{ID: "lctest-upl-empty", Title: "e", Text: "same bytes",
		WrittenBy: "op:c", ContentDigest: digest}); err != nil {
		t.Fatalf("SaveDocument: %v", err)
	}

	rec, err := lc.FindByContent(ctx, "op:a", digest)
	if err != nil || !rec.Exists || rec.ID != "lctest-upl-old" || rec.WrittenBy != "op:a" || rec.Version != 1 {
		t.Fatalf("same writer: %+v, %v", rec, err)
	}
	if rec.CreatedAt.IsZero() || rec.UpdatedAt.IsZero() || rec.Title != "lctest-upl-old" {
		t.Errorf("record lacks title/times: %+v", rec)
	}
	for _, c := range []struct{ writer, digest string }{
		{"op:b", digest},         // another writer's identical bytes are not a duplicate
		{"op:c", digest},         // a row with no content is not a duplicate
		{"op:a", "other-digest"}, // other content
		{"", digest},             // writer-less: never
	} {
		if rec, err := lc.FindByContent(ctx, c.writer, c.digest); err != nil || rec.Exists {
			t.Errorf("%+v: want no match, got %+v, %v", c, rec, err)
		}
	}
}

// B1.1: the association of an upload with its session and scope is one row per place —
// a repeat counts deliveries — and recording it changes nothing about the document.
func TestPg_DocumentLifecycle_RecordAssociationIsIdempotentPerPlace(t *testing.T) {
	a, ks, lc, ctx := newLifecycleStores(t)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-upl-a", "op:a", "text", "dg-a", 1)
	tagsBefore := countRows(t, a, ctx, `SELECT cardinality(tags) FROM documents WHERE id = 'lctest-upl-a'`)
	for _, as := range []domain.DocumentAssociation{
		{DocumentID: "lctest-upl-a", Writer: "op:a", SessionID: "s1", Scope: []string{"finance"}, UploadName: "r.pdf"},
		{DocumentID: "lctest-upl-a", Writer: "op:a", SessionID: "s1", Scope: []string{"finance"}, UploadName: "R.pdf"},
		{DocumentID: "lctest-upl-a", Writer: "op:a", SessionID: "s2", Scope: []string{"hr"}, UploadName: "copy.pdf"},
		{DocumentID: "lctest-upl-a", Writer: "op:a"}, // no session, empty scope
	} {
		if err := lc.RecordAssociation(ctx, as); err != nil {
			t.Fatalf("RecordAssociation %+v: %v", as, err)
		}
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM document_associations WHERE document_id = 'lctest-upl-a'`); n != 3 {
		t.Errorf("rows: %d, want 3", n)
	}
	if n := countRows(t, a, ctx, `SELECT deliveries FROM document_associations
		WHERE document_id = 'lctest-upl-a' AND session_id = 's1' AND scope = ARRAY['finance']`); n != 2 {
		t.Errorf("deliveries: %d, want 2", n)
	}
	if n := countRows(t, a, ctx, `SELECT cardinality(tags) FROM documents WHERE id = 'lctest-upl-a'`); n != tagsBefore {
		t.Errorf("an association changed the document's tags: %d -> %d", tagsBefore, n)
	}
	// The FK: an association for a document that does not exist is refused.
	if err := lc.RecordAssociation(ctx, domain.DocumentAssociation{DocumentID: "lctest-upl-missing", Writer: "op:a"}); err == nil {
		t.Error("an association of a missing document was accepted")
	}
	// It cascades with the document (ADR-0137's forget deletes documents rows).
	if _, err := a.pool.Exec(ctx, `DELETE FROM chunks WHERE document_id = 'lctest-upl-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.pool.Exec(ctx, `DELETE FROM document_sections WHERE document_id = 'lctest-upl-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.pool.Exec(ctx, `DELETE FROM documents WHERE id = 'lctest-upl-a'`); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM document_associations WHERE document_id = 'lctest-upl-a'`); n != 0 {
		t.Errorf("associations survived their document: %d", n)
	}
}

// B1.2 rides D3: two versions under ONE upload id — the second ReviseDocument retires
// the first (the existing path, no second implementation), and the record reports v2.
func TestPg_DocumentLifecycle_ReuploadUnderTheUploadIDIsANewVersion(t *testing.T) {
	a, ks, lc, ctx := newLifecycleStores(t)
	const id = "lctest-operator_ingest://report.pdf#w=0123456789abcdef"
	seedLifecycleDoc(t, a, ks, ctx, id, "op:a", "v1", "dg-v1", 2)
	out, err := lc.ReviseDocument(ctx, domain.DocumentRevision{ID: id, Writer: "op:a", ContentDigest: "dg-v2", Text: "v2"})
	if err != nil || !out.Retired || out.Version != 2 {
		t.Fatalf("revise: %+v, %v", out, err)
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM chunks WHERE document_id = $1`, id); n != 0 {
		t.Errorf("old version chunks left: %d", n)
	}
	rec, err := lc.DocumentRecord(ctx, id)
	if err != nil || rec.Version != 2 || rec.ContentDigest != "dg-v2" || rec.WrittenBy != "op:a" {
		t.Errorf("record after the re-upload: %+v, %v", rec, err)
	}
	// Another operator uploading the same name gets another id (rule U); were it to
	// name this id, the D3 writer check refuses it.
	if _, err := lc.ReviseDocument(ctx, domain.DocumentRevision{ID: id, Writer: "op:b", ContentDigest: "dg-x"}); err == nil {
		t.Error("another writer revised the upload")
	}
}
