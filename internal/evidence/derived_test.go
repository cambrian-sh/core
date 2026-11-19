package evidence

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ENG-1611: material a pipeline node fetched carries exactly its parent's
// classification and parties; nothing derived can widen them.

func derivedFixture(t *testing.T) (*Ingestor, *fakeStore, domain.EvidenceID) {
	t.Helper()
	store := newFakeStore()
	ing, err := NewIngestor(newFakeBlobs(), store)
	if err != nil {
		t.Fatal(err)
	}
	parent, _, err := ing.Ingest(context.Background(), Raw{SourceID: "crm", SourceKey: "acct-7", SourceRevision: "r1",
		Bytes: []byte("account 7"), Classification: []string{"finance", "confidential"}, Parties: []string{"customer:7"}})
	if err != nil {
		t.Fatal(err)
	}
	return ing, store, parent
}

func TestIngest_DerivedEvidenceInheritsTheParentsLabels(t *testing.T) {
	ing, store, parent := derivedFixture(t)
	for name, raw := range map[string]Raw{
		"no labels of its own": {SourceKey: "acct-7/fetch:n1:i1"},
		"repeats the parent's": {SourceKey: "acct-7/fetch:n1:i2", Classification: []string{"confidential", "finance"}, Parties: []string{"customer:7"}},
	} {
		raw.SourceID, raw.SourceRevision, raw.Bytes, raw.DerivedFrom = "crm", "r1", []byte("page"), parent
		id, inserted, err := ing.Ingest(context.Background(), raw)
		if err != nil || !inserted {
			t.Fatalf("%s: %v", name, err)
		}
		got, _ := store.Get(context.Background(), id)
		if !reflect.DeepEqual(got.Classification, []string{"finance", "confidential"}) ||
			!reflect.DeepEqual(got.Parties, []string{"customer:7"}) || got.DerivedFrom != parent {
			t.Fatalf("%s: derived row = %+v", name, got)
		}
	}
}

func TestIngest_DerivedEvidenceThatCouldWidenIsRefused(t *testing.T) {
	ing, store, parent := derivedFixture(t)
	before := len(store.rows)
	for name, raw := range map[string]Raw{
		"narrower classification": {Classification: []string{"finance"}, DerivedFrom: parent},
		"other classification":    {Classification: []string{"public"}, DerivedFrom: parent},
		"other parties":           {Parties: []string{"customer:8"}, DerivedFrom: parent},
		"missing parent":          {DerivedFrom: "ev_gone"},
		"other namespace":         {NamespaceID: "tenant-b", DerivedFrom: parent},
	} {
		raw.SourceID, raw.SourceKey, raw.SourceRevision, raw.Bytes = "crm", "acct-7/fetch:"+name, "r1", []byte("page")
		if _, _, err := ing.Ingest(context.Background(), raw); !errors.Is(err, domain.ErrDerivedEvidence) {
			t.Errorf("%s: err = %v, want ErrDerivedEvidence", name, err)
		}
	}
	if len(store.rows) != before {
		t.Fatalf("a refused derived delivery wrote a row")
	}
}
