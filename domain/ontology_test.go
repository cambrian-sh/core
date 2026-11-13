package domain

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"pgregory.net/rapid"
)

func fp(v float64) *float64 { return &v }

// ADR-0149 D2: every class of change, one row each.
func TestClassifyOntologyChange_Table(t *testing.T) {
	t.Parallel()
	order := func(mut func(*KindSpec)) KindSpec {
		k := KindSpec{Kind: "order", Predicates: map[string]ValueSpec{
			"status": {Type: "text", Enum: []string{"open", "shipped"}},
			"total":  {Type: "number", Min: fp(0), Max: fp(100)},
			"note":   {Type: "text"},
		}}
		if mut != nil {
			mut(&k)
		}
		return k
	}
	placed := func(mut func(*RelationSpec)) RelationSpec {
		r := RelationSpec{Name: "placed_by", Family: LinkFamilyRelation, FromKind: "order"}
		if mut != nil {
			mut(&r)
		}
		return r
	}
	cur := OntologyDoc{Kinds: []KindSpec{order(nil)}, Relations: []RelationSpec{placed(nil)}}

	cases := []struct {
		name    string
		next    OntologyDoc
		class   OntologyChangeClass
		subject string
		detail  string
	}{
		{"new kind", OntologyDoc{Kinds: []KindSpec{order(nil), {Kind: "customer", Predicates: map[string]ValueSpec{"n": {Type: "text"}}}}, Relations: cur.Relations},
			ChangeAdditive, "kind customer", "new kind"},
		{"new optional predicate", doc(order(func(k *KindSpec) { k.Predicates["eta"] = ValueSpec{Type: "date"} }), placed(nil)),
			ChangeAdditive, "kind order.eta", "new optional predicate"},
		{"new required predicate", doc(order(func(k *KindSpec) { k.Predicates["eta"] = ValueSpec{Type: "date", Required: true} }), placed(nil)),
			ChangeTightening, "kind order.eta", "new required predicate"},
		{"predicate now required", doc(order(func(k *KindSpec) { k.Predicates["note"] = ValueSpec{Type: "text", Required: true} }), placed(nil)),
			ChangeTightening, "kind order.note", "now required"},
		{"enum widened", doc(order(func(k *KindSpec) {
			k.Predicates["status"] = ValueSpec{Type: "text", Enum: []string{"open", "shipped", "lost"}}
		}), placed(nil)), ChangeAdditive, "kind order.status", "enum widened"},
		{"enum narrowed", doc(order(func(k *KindSpec) { k.Predicates["status"] = ValueSpec{Type: "text", Enum: []string{"open"}} }), placed(nil)),
			ChangeTightening, "kind order.status", "enum narrowed"},
		{"enum declared", doc(order(func(k *KindSpec) { k.Predicates["note"] = ValueSpec{Type: "text", Enum: []string{"a"}} }), placed(nil)),
			ChangeTightening, "kind order.note", "enum declared"},
		{"enum removed", doc(order(func(k *KindSpec) { k.Predicates["status"] = ValueSpec{Type: "text"} }), placed(nil)),
			ChangeAdditive, "kind order.status", "enum removed"},
		{"min raised", doc(order(func(k *KindSpec) { k.Predicates["total"] = ValueSpec{Type: "number", Min: fp(5), Max: fp(100)} }), placed(nil)),
			ChangeTightening, "kind order.total", "minimum 0 -> 5"},
		{"max raised", doc(order(func(k *KindSpec) { k.Predicates["total"] = ValueSpec{Type: "number", Min: fp(0), Max: fp(200)} }), placed(nil)),
			ChangeAdditive, "kind order.total", "maximum 100 -> 200"},
		{"bound removed", doc(order(func(k *KindSpec) { k.Predicates["total"] = ValueSpec{Type: "number", Max: fp(100)} }), placed(nil)),
			ChangeAdditive, "kind order.total", "minimum removed"},
		{"type changed", doc(order(func(k *KindSpec) { k.Predicates["note"] = ValueSpec{Type: "number"} }), placed(nil)),
			ChangeBreaking, "kind order.note", "value type text -> number"},
		{"predicate removed", doc(order(func(k *KindSpec) { delete(k.Predicates, "note") }), placed(nil)),
			ChangeBreaking, "kind order.note", "predicate removed"},
		{"policy changed", doc(order(func(k *KindSpec) { k.CommitClass = "x" }), placed(nil)),
			ChangeBreaking, "kind order", "resolution, lifecycle"},
		{"kind removed", OntologyDoc{Relations: cur.Relations}, ChangeBreaking, "kind order", "kind removed"},
		{"new relation", OntologyDoc{Kinds: cur.Kinds, Relations: []RelationSpec{placed(nil), {Name: "ships_to", Family: LinkFamilyRelation}}},
			ChangeAdditive, "relation ships_to", "new relation"},
		{"relation removed", OntologyDoc{Kinds: cur.Kinds}, ChangeBreaking, "relation placed_by", "relation removed"},
		{"to kind declared", doc(order(nil), placed(func(r *RelationSpec) { r.ToKind = "customer" })),
			ChangeTightening, "relation placed_by", `to_kind "" -> "customer"`},
		{"from kind changed", doc(order(nil), placed(func(r *RelationSpec) { r.FromKind = "invoice" })),
			ChangeTightening, "relation placed_by", `from_kind "order" -> "invoice"`},
		{"from kind dropped", doc(order(nil), placed(func(r *RelationSpec) { r.FromKind = "" })),
			ChangeAdditive, "relation placed_by", "no longer required"},
		{"cardinality one", doc(order(nil), placed(func(r *RelationSpec) { r.Cardinality = CardinalityOne })),
			ChangeTightening, "relation placed_by", "cardinality many -> one"},
		{"relation required", doc(order(nil), placed(func(r *RelationSpec) { r.Required = true })),
			ChangeTightening, "relation placed_by", "now required"},
		{"inverse declared", doc(order(nil), placed(func(r *RelationSpec) { r.Inverse = "placed" })),
			ChangeAdditive, "relation placed_by", "inverse"},
		{"family changed", doc(order(nil), placed(func(r *RelationSpec) { r.Family = LinkFamilyLineage })),
			ChangeBreaking, "relation placed_by", "family, symmetry or closure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyOntologyChange(cur, tc.next)
			if c := MaxChangeClass(got); c != tc.class {
				t.Fatalf("class = %s, want %s (changes %+v)", c, tc.class, got)
			}
			found := false
			for _, c := range got {
				if c.Subject == tc.subject && strings.Contains(c.Detail, tc.detail) && c.Class == tc.class {
					found = true
				}
			}
			if !found {
				t.Fatalf("no %s change %q on %q in %+v", tc.class, tc.detail, tc.subject, got)
			}
		})
	}
	if got := ClassifyOntologyChange(cur, cur); len(got) != 0 {
		t.Fatalf("an identical document is no change, got %+v", got)
	}
	// The default policy spelt out is the same policy.
	spelt := doc(order(func(k *KindSpec) { k.Policy = ResolutionPolicyLatestAssertion }), placed(nil))
	if got := ClassifyOntologyChange(cur, spelt); len(got) != 0 {
		t.Fatalf("latest_assertion spelt out is no change, got %+v", got)
	}
}

func doc(k KindSpec, r RelationSpec) OntologyDoc {
	return OntologyDoc{Kinds: []KindSpec{k}, Relations: []RelationSpec{r}}
}

func TestOntologyDoc_OverlayReplacesByNameAndKeepsBase(t *testing.T) {
	t.Parallel()
	base := OntologyDoc{
		Kinds:     []KindSpec{{Kind: "a", Predicates: map[string]ValueSpec{"x": {Type: "text"}}}, {Kind: "b", Predicates: map[string]ValueSpec{"y": {Type: "text"}}}},
		Relations: []RelationSpec{{Name: "r1", Family: LinkFamilyRelation}},
	}
	change := OntologyDoc{
		Kinds:     []KindSpec{{Kind: "b", Predicates: map[string]ValueSpec{"y": {Type: "text"}, "z": {Type: "date"}}}, {Kind: "c", Predicates: map[string]ValueSpec{"w": {Type: "text"}}}},
		Relations: []RelationSpec{{Name: "r1", Family: LinkFamilyRelation, ToKind: "a"}},
	}
	got := base.Overlay(change)
	if len(got.Kinds) != 3 || got.Kinds[0].Kind != "a" || got.Kinds[1].Kind != "b" || got.Kinds[2].Kind != "c" {
		t.Fatalf("kinds = %+v", got.Kinds)
	}
	if _, ok := got.Kinds[1].Predicates["z"]; !ok {
		t.Fatal("b was not replaced")
	}
	if len(got.Relations) != 1 || got.Relations[0].ToKind != "a" {
		t.Fatalf("relations = %+v", got.Relations)
	}
	if len(base.Kinds[1].Predicates) != 1 || base.Relations[0].ToKind != "" {
		t.Fatal("Overlay modified its receiver")
	}
	// A base that declares a name twice is NOT deduplicated: the registry refuses it.
	dup := OntologyDoc{Kinds: []KindSpec{base.Kinds[0], base.Kinds[0]}}
	if _, err := NewOntologySnapshot("ns", 1, dup.Overlay(OntologyDoc{}), nil); err == nil {
		t.Fatal("a duplicate kind in the base must still refuse")
	}
}

// The persisted form must be lossless: a document survives JSON and its
// canonical form ignores order.
func TestOntologyDoc_CanonicalAndRoundTrip(t *testing.T) {
	t.Parallel()
	d := OntologyDoc{
		Kinds: []KindSpec{
			{Kind: "z", Predicates: map[string]ValueSpec{"n": {Type: "number", Min: fp(1), Required: true}}},
			{Kind: "a", Policy: ResolutionPolicyDesignatedActor, ResolutionScope: ResolutionScopeEntity,
				Designated: &DesignatedActors{Principals: []string{"user:x"}},
				Predicates: map[string]ValueSpec{"s": {Type: "text", Enum: []string{"o", "c"}}},
				Lifecycle:  &KindLifecycle{StatePredicate: "s", Initial: []string{"o"}, Transitions: map[string][]string{"o": {"c"}}},
				EventTypes: []EventTypeSpec{{Type: "opened", Roles: []string{"by"}}}, CommitClass: "cc"},
		},
		Relations: []RelationSpec{{Name: "r", Family: LinkFamilyRelation, FromKind: "a", ToKind: "z", Cardinality: CardinalityOne, Required: true, Inverse: "ri"}},
	}
	raw, err := d.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	var back OntologyDoc
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	again, _ := back.Canonical()
	if string(again) != string(raw) {
		t.Fatalf("canonical form is not stable:\n%s\n%s", raw, again)
	}
	reordered := OntologyDoc{Kinds: []KindSpec{d.Kinds[1], d.Kinds[0]}, Relations: d.Relations}
	r2, _ := reordered.Canonical()
	if string(r2) != string(raw) {
		t.Fatal("canonical form depends on spec order")
	}
	if !reflect.DeepEqual(back.Relations, d.Relations) {
		t.Fatalf("relations did not round-trip: %+v", back.Relations)
	}
}

func TestNewOntologySnapshot_StampsVersionAndRefusesLikeTheRegistries(t *testing.T) {
	t.Parallel()
	snap, err := NewOntologySnapshot("default", 7, OntologyDoc{
		Kinds:     []KindSpec{{Kind: "k", Predicates: map[string]ValueSpec{"p": {Type: "text"}}}},
		Relations: []RelationSpec{{Name: "r", Family: LinkFamilyRelation}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Version != 7 || snap.Kinds.Version() != 7 || snap.Relations.Version() != 7 {
		t.Fatalf("version not stamped: %d %d %d", snap.Version, snap.Kinds.Version(), snap.Relations.Version())
	}
	if snap.Kinds.KindsFor("anything") != snap.Kinds || snap.Relations.RelationsFor("x") != snap.Relations {
		t.Fatal("a registry is its own fixed source")
	}
	var nilK *KindRegistry
	var nilR *RelationRegistry
	if nilK.Version() != 0 || nilR.Version() != 0 {
		t.Fatal("a nil registry has version 0")
	}
	if _, err := NewOntologySnapshot("default", 1, OntologyDoc{Kinds: []KindSpec{{Kind: "k", Predicates: map[string]ValueSpec{"p": {Type: "blob"}}}}}, nil); err == nil {
		t.Fatal("an invalid kind must refuse")
	}
	if _, err := NewOntologySnapshot("default", 1, OntologyDoc{Relations: []RelationSpec{{Name: RelationSameAs, Family: LinkFamilyIdentity}}}, nil); err == nil {
		t.Fatal("a redeclared seed verb must refuse")
	}
}

// Required binds the written item, not a negation.
func TestKindRegistry_ValidateItemRequired(t *testing.T) {
	t.Parallel()
	reg, err := NewKindRegistry([]KindSpec{{Kind: "order", Predicates: map[string]ValueSpec{
		"customer": {Type: "entity", Required: true}, "note": {Type: "text"}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ok := KnowledgeItem{Kind: "order", Values: []StatementValue{{Predicate: "customer", Type: "entity", EntityRef: "customer/1"}}}
	if err := reg.ValidateItem(ok); err != nil {
		t.Fatalf("carrying the required predicate: %v", err)
	}
	missing := KnowledgeItem{Kind: "order", Values: []StatementValue{{Predicate: "note", Type: "text", Text: "x"}}}
	err = reg.ValidateItem(missing)
	if !errors.Is(err, ErrKindRefused) || !strings.Contains(err.Error(), `requires predicate "customer"`) {
		t.Fatalf("missing required predicate: %v", err)
	}
	if err := reg.ValidateItem(KnowledgeItem{Kind: "order", Negation: true}); err != nil {
		t.Fatalf("a negation carries no values: %v", err)
	}
	if err := reg.ValidateItem(KnowledgeItem{Kind: "undeclared"}); err != nil {
		t.Fatalf("an undeclared kind passes (monotonic adoption): %v", err)
	}
	bad := KnowledgeItem{Kind: "order", Values: []StatementValue{{Predicate: "customer", Type: "text", Text: "c"}}}
	if err := reg.ValidateItem(bad); !errors.Is(err, ErrKindRefused) {
		t.Fatalf("ValidateItem must still check value types: %v", err)
	}
}

// Property (ADR-0149 D2): any sequence of ADDITIVE changes keeps every write the
// starting declarations admitted admissible. Additive changes are generated from
// the classifier's own vocabulary (new kind, new optional predicate, enum
// widened, bound loosened, requirement dropped), and the property is checked
// against the classifier too: every generated change must classify additive.
func TestProperty_AdditiveChangesKeepValidWritesValid(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		start := OntologyDoc{Kinds: []KindSpec{{Kind: "order", Predicates: map[string]ValueSpec{
			"status": {Type: "text", Enum: []string{"open", "shipped"}},
			"total":  {Type: "number", Min: fp(0), Max: fp(100)},
			"ref":    {Type: "text", Required: true},
		}}}}
		// Writes valid under start.
		var writes []KnowledgeItem
		n := rapid.IntRange(1, 6).Draw(t, "writes")
		for i := 0; i < n; i++ {
			writes = append(writes, KnowledgeItem{Kind: "order", Values: []StatementValue{
				{Predicate: "ref", Type: "text", Text: "r"},
				{Predicate: "status", Type: "text", Text: rapid.SampledFrom([]string{"open", "shipped"}).Draw(t, "status")},
				{Predicate: "total", Type: "number", Number: rapid.Float64Range(0, 100).Draw(t, "total")},
			}})
		}
		cur := start
		steps := rapid.IntRange(1, 8).Draw(t, "steps")
		for i := 0; i < steps; i++ {
			k := cloneKind(cur.Kinds[0])
			switch rapid.IntRange(0, 5).Draw(t, "op") {
			case 0:
				k.Predicates[rapid.StringMatching(`x[a-z]{1,4}`).Draw(t, "pred")] = ValueSpec{Type: "date"}
			case 1:
				// Widening an enum is additive; adding one where there is none is
				// not (it closes an open text predicate), so only widen.
				s := k.Predicates["status"]
				if len(s.Enum) > 0 {
					s.Enum = append(append([]string(nil), s.Enum...), rapid.StringMatching(`[a-z]{1,5}`).Draw(t, "val"))
				}
				k.Predicates["status"] = s
			case 2:
				s := k.Predicates["total"]
				if s.Min != nil {
					s.Min = fp(*s.Min - rapid.Float64Range(0, 10).Draw(t, "dmin"))
				}
				k.Predicates["total"] = s
			case 3:
				s := k.Predicates["total"]
				s.Max = nil
				k.Predicates["total"] = s
			case 4:
				s := k.Predicates["ref"]
				s.Required = false
				k.Predicates["ref"] = s
			case 5:
				s := k.Predicates["status"]
				s.Enum = nil
				k.Predicates["status"] = s
			}
			next := cur.Overlay(OntologyDoc{Kinds: []KindSpec{k}})
			if c := MaxChangeClass(ClassifyOntologyChange(cur, next)); c != ChangeAdditive {
				t.Fatalf("generated change classified %s: %+v", c, ClassifyOntologyChange(cur, next))
			}
			cur = next
		}
		snap, err := NewOntologySnapshot("default", 2, cur, nil)
		if err != nil {
			t.Fatalf("additive result does not validate: %v", err)
		}
		for _, w := range writes {
			if err := snap.Kinds.ValidateItem(w); err != nil {
				t.Fatalf("a write valid at the start is refused after additive changes: %v", err)
			}
		}
	})
}

func cloneKind(k KindSpec) KindSpec {
	out := k
	out.Predicates = make(map[string]ValueSpec, len(k.Predicates))
	for p, v := range k.Predicates {
		out.Predicates[p] = v
	}
	return out
}

// The snapshot a store holds is immutable: concurrent readers of one snapshot
// see the same answers while another goroutine builds the next one (-race).
func TestOntologySnapshot_ConcurrentReadsWhileBuilding(t *testing.T) {
	t.Parallel()
	snap, err := NewOntologySnapshot("default", 1, OntologyDoc{Kinds: []KindSpec{{Kind: "k", Predicates: map[string]ValueSpec{"p": {Type: "text"}}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	deadline := time.Now().Add(50 * time.Millisecond)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				if err := snap.Kinds.ValidateValues("k", []StatementValue{{Predicate: "p", Type: "text", Text: "x"}}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	for time.Now().Before(deadline) {
		_, _ = NewOntologySnapshot("default", 2, snap.Doc.Overlay(OntologyDoc{Kinds: []KindSpec{{Kind: "k2", Predicates: map[string]ValueSpec{"q": {Type: "text"}}}}}), nil)
	}
	wg.Wait()
}
