package domain

import (
	"errors"
	"strings"
	"testing"
)

// Object kinds (ADR-0149 D3, owner 2026-11-13): a kind IS an object type,
// IDPrefix / Open / Table on KindSpec.

func objReg(t *testing.T, specs ...KindSpec) *KindRegistry {
	t.Helper()
	r, err := NewKindRegistry(specs, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func orderSpec() KindSpec {
	return KindSpec{Kind: "order", Table: true, Predicates: map[string]ValueSpec{
		"status": {Type: "text", Enum: []string{"open", "shipped"}},
		"total":  {Type: "number", Indexed: true},
	}}
}

func TestObjectKind_DefaultPrefixIsTheKindName(t *testing.T) {
	r := objReg(t, orderSpec(), KindSpec{Kind: "decision_case", IDPrefix: "decision", Predicates: map[string]ValueSpec{"s": {Type: "text"}}})
	if s, ok := r.ObjectKind("order/O-1"); !ok || s.Kind != "order" {
		t.Fatalf("order/O-1 -> %v %v", s.Kind, ok)
	}
	if s, ok := r.ObjectKind("decision/D-1"); !ok || s.Kind != "decision_case" {
		t.Fatalf("decision/D-1 -> %v %v", s.Kind, ok)
	}
	for _, id := range []string{"decision_case/x", "customer/C-1", "order", "order/", "/x", ""} {
		if s, ok := r.ObjectKind(id); ok {
			t.Fatalf("%q must not be an object, got kind %q", id, s.Kind)
		}
	}
	var nilReg *KindRegistry
	if _, ok := nilReg.ObjectKind("order/1"); ok {
		t.Fatal("a nil registry declares nothing")
	}
}

func TestObjectKind_PrefixIsUniqueWithinARegistry(t *testing.T) {
	_, err := NewKindRegistry([]KindSpec{
		orderSpec(),
		{Kind: "sales_order", IDPrefix: "order", Predicates: map[string]ValueSpec{"x": {Type: "text"}}},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), `both declare id prefix "order"`) {
		t.Fatalf("two kinds with one prefix must be refused, got %v", err)
	}
	// A kind named like another's explicit prefix collides too.
	_, err = NewKindRegistry([]KindSpec{
		{Kind: "a", IDPrefix: "b", Predicates: map[string]ValueSpec{"x": {Type: "text"}}},
		{Kind: "b", Predicates: map[string]ValueSpec{"x": {Type: "text"}}},
	}, nil)
	if err == nil {
		t.Fatal("kind b's default prefix collides with kind a's explicit prefix b")
	}
}

func TestObjectKind_ReservedObservationKindHasNoPrefix(t *testing.T) {
	r := objReg(t, KindSpec{Kind: ObservationKind, Predicates: map[string]ValueSpec{"temp": {Type: "number"}}})
	if _, ok := r.ObjectKind("observation/1"); ok {
		t.Fatal("the reserved observation kind is the fallback path, not an object type")
	}
	if _, err := NewKindRegistry([]KindSpec{{Kind: ObservationKind, Table: true, Predicates: map[string]ValueSpec{"t": {Type: "number"}}}}, nil); err == nil {
		t.Fatal("the reserved observation kind takes no table")
	}
}

func TestObjectKind_TableKindsNeedPlainIdentifiers(t *testing.T) {
	bad := []KindSpec{
		{Kind: "discovery.session", Table: true, Predicates: map[string]ValueSpec{"x": {Type: "text"}}},
		{Kind: "Order", Table: true, Predicates: map[string]ValueSpec{"x": {Type: "text"}}},
		{Kind: "order", Table: true, Predicates: map[string]ValueSpec{"a__b": {Type: "text"}}},
		{Kind: "order", Table: true, Predicates: map[string]ValueSpec{"Status": {Type: "text"}}},
		{Kind: "order", Table: true, Predicates: map[string]ValueSpec{"x;drop": {Type: "text"}}},
		{Kind: strings.Repeat("k", 51), Table: true, Predicates: map[string]ValueSpec{"x": {Type: "text"}}},
		{Kind: "order", Table: true, Predicates: map[string]ValueSpec{strings.Repeat("p", 51): {Type: "text"}}},
		{Kind: "order", IDPrefix: "a/b", Predicates: map[string]ValueSpec{"x": {Type: "text"}}},
		{Kind: "order", Predicates: map[string]ValueSpec{"x": {Type: "text", Authority: []string{" "}}}},
	}
	for _, s := range bad {
		if _, err := NewKindRegistry([]KindSpec{s}, nil); err == nil {
			t.Errorf("must refuse %+v", s)
		}
	}
	// Without a table the same names stay legal: existing kinds are unaffected.
	objReg(t, KindSpec{Kind: "discovery.session", Predicates: map[string]ValueSpec{"Odd Name": {Type: "text"}}})
}

func TestObjectKind_StrictRefusesAndOpenKeepsUndeclaredPredicates(t *testing.T) {
	strict := objReg(t, orderSpec())
	err := strict.ValidateValues("order", []StatementValue{{Predicate: "colour", Type: "text", Text: "red"}})
	if !errors.Is(err, ErrKindRefused) {
		t.Fatalf("strict (default) refuses an undeclared predicate, got %v", err)
	}
	o := orderSpec()
	o.Open = true
	open := objReg(t, o)
	if err := open.ValidateValues("order", []StatementValue{{Predicate: "colour", Type: "text", Text: "red"}}); err != nil {
		t.Fatalf("open keeps an undeclared predicate: %v", err)
	}
	// Declared predicates stay constrained in an open kind.
	if err := open.ValidateValues("order", []StatementValue{{Predicate: "total", Type: "text", Text: "x"}}); !errors.Is(err, ErrKindRefused) {
		t.Fatalf("an open kind still types its declared predicates, got %v", err)
	}
}

func TestObjectKind_ItemsAboutAnObjectSpeakItsVocabulary(t *testing.T) {
	r := objReg(t, orderSpec(), KindSpec{Kind: "note", Predicates: map[string]ValueSpec{
		"status": {Type: "text"}, "body": {Type: "text"}}})
	// An item of another kind about an order: its values must fit the order kind.
	ok := KnowledgeItem{Kind: "note", EntityID: "order/O-1", Values: []StatementValue{{Predicate: "status", Type: "text", Text: "open"}}}
	if err := r.ValidateItem(ok); err != nil {
		t.Fatalf("a declared order property: %v", err)
	}
	bad := KnowledgeItem{Kind: "note", EntityID: "order/O-1", Values: []StatementValue{{Predicate: "body", Type: "text", Text: "hi"}}}
	if err := r.ValidateItem(bad); !errors.Is(err, ErrKindRefused) || !strings.Contains(err.Error(), `"order" object`) {
		t.Fatalf("an undeclared order property from another kind must be refused, got %v", err)
	}
	enum := KnowledgeItem{Kind: "note", EntityID: "order/O-1", Values: []StatementValue{{Predicate: "status", Type: "text", Text: "lost"}}}
	if err := r.ValidateItem(enum); !errors.Is(err, ErrKindRefused) {
		t.Fatalf("the order kind's enum binds every item about an order, got %v", err)
	}
	// Not about an object: only its own kind applies.
	if err := r.ValidateItem(KnowledgeItem{Kind: "note", EntityID: "memo/1", Values: bad.Values}); err != nil {
		t.Fatal(err)
	}
}

func TestClassify_ObjectFields(t *testing.T) {
	base := orderSpec()
	cases := []struct {
		name  string
		edit  func(*KindSpec)
		class OntologyChangeClass
	}{
		{"explicit default prefix is no change", func(k *KindSpec) { k.IDPrefix = "order" }, ""},
		{"prefix moved", func(k *KindSpec) { k.IDPrefix = "so" }, ChangeBreaking},
		{"table removed", func(k *KindSpec) { k.Table = false }, ChangeBreaking},
		{"strict -> open", func(k *KindSpec) { k.Open = true }, ChangeAdditive},
		{"index hint removed", func(k *KindSpec) { p := k.Predicates["total"]; p.Indexed = false; k.Predicates["total"] = p }, ChangeAdditive},
		{"authority declared", func(k *KindSpec) {
			p := k.Predicates["status"]
			p.Authority = []string{"erp"}
			k.Predicates["status"] = p
		}, ChangeBreaking},
	}
	for _, c := range cases {
		nw := orderSpec()
		c.edit(&nw)
		got := ClassifyOntologyChange(OntologyDoc{Kinds: []KindSpec{base}}, OntologyDoc{Kinds: []KindSpec{nw}})
		if c.class == "" {
			if len(got) != 0 {
				t.Errorf("%s: want no change, got %+v", c.name, got)
			}
			continue
		}
		if MaxChangeClass(got) != c.class || len(got) != 1 {
			t.Errorf("%s: want one %s change, got %+v", c.name, c.class, got)
		}
	}
	// The other direction of the toggles.
	noTable, open := orderSpec(), orderSpec()
	noTable.Table = false
	open.Open = true
	if c := MaxChangeClass(ClassifyOntologyChange(OntologyDoc{Kinds: []KindSpec{noTable}}, OntologyDoc{Kinds: []KindSpec{base}})); c != ChangeAdditive {
		t.Errorf("adding a table is additive, got %s", c)
	}
	if c := MaxChangeClass(ClassifyOntologyChange(OntologyDoc{Kinds: []KindSpec{open}}, OntologyDoc{Kinds: []KindSpec{base}})); c != ChangeTightening {
		t.Errorf("open -> strict is tightening, got %s", c)
	}
	withAuth := orderSpec()
	p := withAuth.Predicates["status"]
	p.Authority = []string{"erp", "crm"}
	withAuth.Predicates["status"] = p
	reordered := orderSpec()
	p.Authority = []string{"crm", "erp"}
	reordered.Predicates["status"] = p
	if c := MaxChangeClass(ClassifyOntologyChange(OntologyDoc{Kinds: []KindSpec{withAuth}}, OntologyDoc{Kinds: []KindSpec{reordered}})); c != ChangeBreaking {
		t.Errorf("reordering an authority changes who wins: breaking, got %s", c)
	}
}

func TestObjectKind_ZeroValuesKeepExistingDeclarationsUnchanged(t *testing.T) {
	// The JSON of an existing declaration carries none of the new fields.
	old := KindSpec{Kind: "memory_fact", Predicates: map[string]ValueSpec{"v": {Type: "text"}}}
	if old.Open || old.Table || old.ObjectPrefix() != "memory_fact" {
		t.Fatalf("defaults: %+v prefix %q", old, old.ObjectPrefix())
	}
	r := objReg(t, old)
	if err := r.ValidateValues("memory_fact", []StatementValue{{Predicate: "x", Type: "text"}}); !errors.Is(err, ErrKindRefused) {
		t.Fatalf("still strict: %v", err)
	}
}
