package postgres

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// A metadata value json cannot encode used to be discarded in getUpsertBuilder,
// and the row was upserted with NULL metadata — its tags (classification) silently
// gone (T-20261113-persisted-writes-and-builder-resolver-errors). The write must be
// refused before any SQL runs; the adapter here has no pool, so reaching Exec would
// panic instead of returning the error.
func TestSave_UnencodableMetadataIsRefusedBeforeAnyWrite(t *testing.T) {
	p := &PgVectorAdapter{dim: 4}
	doc := &domain.Document{
		ID:           "test-nan-meta",
		Text:         "x",
		DocumentType: domain.DocTypeMnemonicFact,
		Metadata:     map[string]any{"tags": []string{"team:a"}, "score": math.NaN()},
	}
	err := p.Save(context.Background(), doc)
	if err == nil {
		t.Fatal("Save must refuse metadata json cannot encode, not store NULL metadata")
	}
	if !strings.Contains(err.Error(), "test-nan-meta") {
		t.Fatalf("the refusal must name the document, got: %v", err)
	}
	if err := p.SaveBatch(context.Background(), []*domain.Document{doc}); err == nil {
		t.Fatal("SaveBatch must refuse it too")
	}
}

func TestUpsertSQL_EncodableMetadataRenders(t *testing.T) {
	p := &PgVectorAdapter{dim: 4}
	sql, args, err := p.upsertSQL(&domain.Document{
		ID: "test-ok", Text: "x", DocumentType: domain.DocTypeMnemonicFact,
		Metadata: map[string]any{"tags": []string{"team:a"}},
	})
	if err != nil || sql == "" {
		t.Fatalf("upsertSQL: sql=%q args=%d err=%v", sql, len(args), err)
	}
}

// ENG-335: the upsert binds its values instead of interpolating them, so the
// vector and the metadata travel as parameters and two rows of one shape share
// one statement text (one cached prepared statement).
func TestUpsertSQL_BindsValuesAndSharesStatementText(t *testing.T) {
	p := &PgVectorAdapter{dim: 4}
	mk := func(id string, v float32) *domain.Document {
		return &domain.Document{ID: id, Text: "text " + id, DocumentType: domain.DocTypeMnemonicFact,
			Metadata:  map[string]any{"tags": []string{"team:a"}, "document_id": "doc-" + id},
			Embedding: domain.Embedding{Vector: []float32{v, 0.25, 0.5, 0.75}}}
	}
	sqlA, argsA, err := p.upsertSQL(mk("a", 0.125))
	if err != nil {
		t.Fatal(err)
	}
	sqlB, _, err := p.upsertSQL(mk("b", 0.375))
	if err != nil {
		t.Fatal(err)
	}
	if sqlA != sqlB {
		t.Fatalf("two rows of one shape must share one statement text:\n%s\n%s", sqlA, sqlB)
	}
	if !strings.Contains(sqlA, "$1") || strings.Contains(sqlA, "0.125") || strings.Contains(sqlA, "text a") || strings.Contains(sqlA, "team:a") {
		t.Fatalf("values must be bound, not interpolated: %s", sqlA)
	}
	if len(argsA) < 5 {
		t.Fatalf("expected the values as args, got %d", len(argsA))
	}
}
