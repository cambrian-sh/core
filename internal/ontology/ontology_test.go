package ontology

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// seedDoc is the fixture boot declarations the unit and Pg tests share.
func seedDoc() domain.OntologyDoc {
	return domain.OntologyDoc{
		Kinds: []domain.KindSpec{
			{Kind: "order", Predicates: map[string]domain.ValueSpec{
				"status": {Type: "text", Enum: []string{"open", "shipped", "lost"}},
				"total":  {Type: "number"},
			}},
			domain.MemoryFactKindSpec(),
		},
		Relations: []domain.RelationSpec{{Name: "placed_by", Family: domain.LinkFamilyRelation}},
	}
}

func authorities() []domain.ResolutionAuthority {
	return []domain.ResolutionAuthority{domain.ValidTimeSupersessionAuthority{}}
}

// memOntology builds an Ontology with no database, for the parts of Apply that
// decide before touching one (classification, refusal, validation, dry run).
func memOntology(t *testing.T) *Ontology {
	t.Helper()
	st, err := build(seedDoc(), 1, nil, authorities())
	if err != nil {
		t.Fatal(err)
	}
	o := &Ontology{authorities: authorities(), seed: seedDoc()}
	o.cur.Store(st)
	return o
}

func TestApply_RefusesBeforeTouchingTheDatabase(t *testing.T) {
	t.Parallel()
	o := memOntology(t)
	ctx := context.Background()

	// Breaking: a type change.
	res, err := o.Apply(ctx, "ns1", domain.OntologyDoc{Kinds: []domain.KindSpec{{Kind: "order",
		Predicates: map[string]domain.ValueSpec{"status": {Type: "number"}, "total": {Type: "number"}}}}}, "user:op", false)
	if !errors.Is(err, domain.ErrOntologyChangeRefused) || res.Class != domain.ChangeBreaking || res.Applied {
		t.Fatalf("breaking change: res=%+v err=%v", res, err)
	}
	if !strings.Contains(err.Error(), "kind order.status: value type text -> number") {
		t.Fatalf("the refusal must name the breaking change: %v", err)
	}
	// Does not validate: an enum on a number.
	_, err = o.Apply(ctx, "ns1", domain.OntologyDoc{Kinds: []domain.KindSpec{{Kind: "invoice",
		Predicates: map[string]domain.ValueSpec{"n": {Type: "number", Enum: []string{"a"}}}}}}, "user:op", false)
	if !errors.Is(err, domain.ErrOntologyChangeRefused) || !strings.Contains(err.Error(), "enum is only valid on a text") {
		t.Fatalf("invalid change: %v", err)
	}
	// The base namespace is code's, not the operator's.
	if _, err := o.Apply(ctx, domain.OntologyBaseNamespace, domain.OntologyDoc{}, "user:op", false); !errors.Is(err, domain.ErrOntologyChangeRefused) {
		t.Fatalf("base namespace: %v", err)
	}
	if _, err := o.Apply(ctx, "ns1", domain.OntologyDoc{}, " ", false); !errors.Is(err, domain.ErrOntologyChangeRefused) {
		t.Fatalf("no author: %v", err)
	}
	// Dry run of a tightening classifies and applies nothing.
	res, err = o.Apply(ctx, "ns1", domain.OntologyDoc{Relations: []domain.RelationSpec{{Name: "placed_by",
		Family: domain.LinkFamilyRelation, Cardinality: domain.CardinalityOne}}}, "user:op", true)
	if err != nil || res.Class != domain.ChangeTightening || res.Applied || res.Version != 1 {
		t.Fatalf("dry run: res=%+v err=%v", res, err)
	}
	// No difference: nothing to do.
	res, err = o.Apply(ctx, "", seedDoc(), "user:op", false)
	if err != nil || len(res.Changes) != 0 || res.Applied || res.Namespace != DefaultNamespace {
		t.Fatalf("no-op: res=%+v err=%v", res, err)
	}
	if o.Current("ns1") != o.Current("") {
		t.Fatal("nothing was applied, so ns1 still reads the base snapshot")
	}
}

// The one mutation is an atomic swap: readers never block and never see a
// half-built state, and a reader holding a snapshot keeps it (-race).
func TestSwap_ConcurrentReadersSeeWholeSnapshots(t *testing.T) {
	t.Parallel()
	o := memOntology(t)
	held := o.Current("ns")
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s := o.Current("ns")
				if s.Kinds.Version() != s.Version || s.Relations.Version() != s.Version {
					t.Error("a snapshot's registries disagree with its version")
					return
				}
				_ = o.KindsFor("ns").EntityScoped("order")
				_ = o.RelationsFor("other").Verbs()
			}
		}()
	}
	for v := int64(2); v < 200; v++ {
		snap, err := domain.NewOntologySnapshot("ns", v, seedDoc(), authorities())
		if err != nil {
			t.Fatal(err)
		}
		o.swap("ns", snap)
	}
	close(stop)
	wg.Wait()
	if o.Current("ns").Version != 199 {
		t.Fatalf("last swap lost: %d", o.Current("ns").Version)
	}
	if held.Version != 1 {
		t.Fatal("a held snapshot changed under its reader")
	}
	if o.Current("other").Version != 1 {
		t.Fatal("a swap in one namespace moved another")
	}
}

// build replays a namespace's rows over the seed in order, replacing by name.
func TestBuild_ReplaysRowsInOrder(t *testing.T) {
	t.Parallel()
	enum := func(vals ...string) domain.KindSpec {
		return domain.KindSpec{Kind: "order", Predicates: map[string]domain.ValueSpec{
			"status": {Type: "text", Enum: vals}, "total": {Type: "number"}}}
	}
	rows := []row{
		{version: 3, namespace: "a", doc: domain.OntologyDoc{Kinds: []domain.KindSpec{enum("open")}}},
		{version: 4, namespace: "b", doc: domain.OntologyDoc{Relations: []domain.RelationSpec{{Name: "ships_to", Family: domain.LinkFamilyRelation}}}},
		{version: 5, namespace: "a", doc: domain.OntologyDoc{Kinds: []domain.KindSpec{enum("open", "shipped")}}},
	}
	st, err := build(seedDoc(), 2, rows, authorities())
	if err != nil {
		t.Fatal(err)
	}
	a, b := st.ns["a"], st.ns["b"]
	if a.Version != 5 || b.Version != 4 || st.base.Version != 2 {
		t.Fatalf("versions a=%d b=%d base=%d", a.Version, b.Version, st.base.Version)
	}
	spec, _ := a.Kinds.Spec("order")
	if got := spec.Predicates["status"].Enum; len(got) != 2 {
		t.Fatalf("a's later row must win: %v", got)
	}
	if _, ok := b.Relations.Spec("ships_to"); !ok {
		t.Fatal("b's relation missing")
	}
	if _, ok := st.base.Relations.Spec("ships_to"); ok {
		t.Fatal("a namespace row leaked into the base")
	}
	// A row that no longer validates over the seed refuses, naming the namespace.
	bad := []row{{version: 3, namespace: "c", doc: domain.OntologyDoc{Relations: []domain.RelationSpec{{Name: "x", Family: "nope"}}}}}
	if _, err := build(seedDoc(), 2, bad, authorities()); err == nil || !strings.Contains(err.Error(), `namespace "c"`) {
		t.Fatalf("invalid replay: %v", err)
	}
}

func TestViolations_RefusesNilScope(t *testing.T) {
	t.Parallel()
	if _, _, err := memOntology(t).Violations(context.Background(), nil, "", 0); !errors.Is(err, errNilScope) {
		t.Fatalf("nil scope: %v", err)
	}
}
