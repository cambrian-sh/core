package evidence_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/evidence"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// ENG-1611 against the real Postgres store and migration 0065: a fetched body
// archived with DerivedFrom gets its parent's classification and parties and the
// derived_from link; one that would widen them is refused and writes nothing.
func TestPg_DerivedEvidenceInheritsItsParentsClassification(t *testing.T) {
	dsn, _ := pgscratch.Migrated(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	clean := func() {
		for _, q := range []string{
			`DELETE FROM evidence_outbox WHERE evidence_id IN (SELECT id FROM evidence WHERE source_id = 'derivedpg:crm')`,
			`DELETE FROM evidence WHERE source_id = 'derivedpg:crm'`,
		} {
			if _, err := pool.Exec(context.Background(), q); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)
	store := postgres.NewPgEvidenceStore(pool)
	ing, err := evidence.NewIngestor(evidence.NewTestBlobs(), store)
	if err != nil {
		t.Fatal(err)
	}

	parent, _, err := ing.Ingest(ctx, domain.RawEvidence{SourceID: "derivedpg:crm", SourceKey: "acct-7", SourceRevision: "r1",
		Bytes: []byte("account 7"), Classification: []string{"finance", "confidential"}, Parties: []string{"customer:7"}})
	if err != nil {
		t.Fatal(err)
	}
	child, inserted, err := ing.Ingest(ctx, domain.RawEvidence{SourceID: "derivedpg:crm", SourceKey: "acct-7/fetch:n1:i1",
		SourceRevision: "r1", Bytes: []byte("fetched page"), DerivedFrom: parent})
	if err != nil || !inserted {
		t.Fatalf("derived ingest: inserted=%v err=%v", inserted, err)
	}
	got, err := store.Get(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Classification, []string{"finance", "confidential"}) ||
		!reflect.DeepEqual(got.Parties, []string{"customer:7"}) || got.DerivedFrom != parent {
		t.Fatalf("derived row = classification %v parties %v derived_from %q", got.Classification, got.Parties, got.DerivedFrom)
	}

	var n int
	_, _, err = ing.Ingest(ctx, domain.RawEvidence{SourceID: "derivedpg:crm", SourceKey: "acct-7/fetch:n1:i2",
		SourceRevision: "r1", Bytes: []byte("other page"), Classification: []string{"public"}, DerivedFrom: parent})
	if !errors.Is(err, domain.ErrDerivedEvidence) {
		t.Fatalf("widening derived ingest: err = %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM evidence WHERE source_id = 'derivedpg:crm'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows = %d (%v), want 2: a refused derived delivery wrote a row", n, err)
	}
}
