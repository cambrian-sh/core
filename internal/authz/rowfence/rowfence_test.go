package rowfence

import (
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

type row struct {
	name string
	cs   []Classification
	ok   bool // resolved
}

func classOf(r row) ([]Classification, bool) { return r.cs, r.ok }

func names(rs []row) []string {
	out := []string{}
	for _, r := range rs {
		out = append(out, r.name)
	}
	return out
}

func one(tags, parties []string) []Classification {
	return []Classification{{Tags: tags, Parties: parties}}
}

// Readable is exactly TagPredicate.AllowsRow: the helper restates no policy.
func TestReadable_IsAllowsRow(t *testing.T) {
	preds := []*domain.TagPredicate{
		nil,
		domain.ScopeSystem,
		domain.ScopeMaintenance,
		{Bypass: true, Owners: []string{"alice"}},
		{RequiredTags: []string{"sales"}},
		{ForbiddenTags: []string{"hr"}},
		{AnyOfClauses: [][]string{{"a", "b"}}},
		{AnyOfClauses: [][]string{{}}},
		{PartyScopedTags: []string{"deal:x"}, PartyIdentities: []string{"p1"}},
	}
	rows := []Classification{
		{},
		{Tags: []string{"sales"}},
		{Tags: []string{"hr", "sales"}},
		{Tags: []string{"a"}},
		{Tags: []string{"owner:alice"}},
		{Tags: []string{"owner:bob", "sales"}},
		{Tags: []string{"deal:x"}, Parties: []string{"p1"}},
		{Tags: []string{"deal:x"}, Parties: []string{"p2"}},
		{Tags: []string{"deal:x"}},
	}
	for i, p := range preds {
		for j, c := range rows {
			if got, want := Readable(p, c), p.AllowsRow(c.Tags, c.Parties); got != want {
				t.Errorf("pred %d row %d: Readable=%v AllowsRow=%v", i, j, got, want)
			}
		}
	}
}

func TestFilterRows(t *testing.T) {
	rows := []row{
		{"plain", one([]string{"sales"}, nil), true},
		{"hr", one([]string{"sales", "hr"}, nil), true},
		{"alice", one([]string{"sales", "owner:alice"}, nil), true},
		{"bob", one([]string{"sales", "owner:bob"}, nil), true},
		{"deal-p1", one([]string{"deal:x"}, []string{"p1"}), true},
		{"deal-p2", one([]string{"deal:x"}, []string{"p2"}), true},
		{"none", nil, true},
		{"unresolved", one([]string{"sales"}, nil), false},
		{"pair-ok", []Classification{{Tags: []string{"sales"}}, {Tags: []string{"a"}}}, true},
		{"pair-hr", []Classification{{Tags: []string{"sales"}}, {Tags: []string{"hr"}}}, true},
	}
	cases := []struct {
		name  string
		scope *domain.TagPredicate
		want  []string
	}{
		{"nil scope reads nothing", nil, []string{}},
		{"maintenance admits every row, unresolved and empty included", domain.ScopeMaintenance,
			[]string{"plain", "hr", "alice", "bob", "deal-p1", "deal-p2", "none", "unresolved", "pair-ok", "pair-hr"}},
		{"bypass keeps the owner fence and fails closed on empty/unresolved", domain.ScopeSystem,
			[]string{"plain", "hr", "deal-p1", "deal-p2", "pair-ok", "pair-hr"}},
		{"bypass for alice reads alice's rows", domain.ScopeSystemFor("alice"),
			[]string{"plain", "hr", "alice", "deal-p1", "deal-p2", "pair-ok", "pair-hr"}},
		{"required tag", &domain.TagPredicate{RequiredTags: []string{"sales"}},
			[]string{"plain", "hr"}},
		{"forbidden tag hides a row and every row standing on it", &domain.TagPredicate{ForbiddenTags: []string{"hr"}},
			[]string{"plain", "deal-p1", "deal-p2", "pair-ok"}},
		{"any-of", &domain.TagPredicate{AnyOfClauses: [][]string{{"deal:x", "a"}}},
			[]string{"deal-p1", "deal-p2"}},
		{"empty any-of clause denies", &domain.TagPredicate{AnyOfClauses: [][]string{{}}}, []string{}},
		{"party-scoped: only the reader's parties", &domain.TagPredicate{
			PartyScopedTags: []string{"deal:x"}, PartyIdentities: []string{"p1"}},
			[]string{"plain", "hr", "deal-p1", "pair-ok", "pair-hr"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := names(FilterRows(tc.scope, rows, classOf))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// The filtered result never writes through the caller's backing array, and an
// unfiltered scope returns the input untouched without calling the accessor.
func TestFilterRows_NoAliasingAndNoAccessorWhenUnfiltered(t *testing.T) {
	rows := []row{
		{"hr", one([]string{"hr"}, nil), true},
		{"plain", one([]string{"sales"}, nil), true},
	}
	_ = FilterRows(&domain.TagPredicate{ForbiddenTags: []string{"hr"}}, rows, classOf)
	if rows[0].name != "hr" || rows[1].name != "plain" {
		t.Fatalf("input mutated: %v", names(rows))
	}
	called := false
	got := FilterRows(domain.ScopeMaintenance, rows, func(r row) ([]Classification, bool) {
		called = true
		return nil, false
	})
	if called || len(got) != 2 {
		t.Fatalf("unfiltered: called=%v got=%v", called, names(got))
	}
	if out := FilterRows[row](domain.ScopeSystem, nil, classOf); out != nil {
		t.Fatalf("empty input: %v", out)
	}
}

// TypedRow (ADR-0158 D2): one classification for an empty row classification,
// otherwise the evidence's and the union, parties unchanged, input not aliased.
func TestTypedRow_Shape(t *testing.T) {
	e := Classification{Tags: []string{"internal"}, Parties: []string{"p1"}}
	for _, row := range [][]string{nil, {}} {
		if got := TypedRow(e, row); !reflect.DeepEqual(got, []Classification{e}) {
			t.Errorf("empty row classification %#v: got %#v, want only e", row, got)
		}
	}
	tags := make([]string, 1, 4) // spare capacity: an append on e.Tags would write into it
	tags[0] = "internal"
	e.Tags = tags
	got := TypedRow(e, []string{"hr"})
	want := []Classification{e, {Tags: []string{"internal", "hr"}, Parties: []string{"p1"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	if got := TypedRow(e, []string{"pay"}); got[1].Tags[1] != "pay" || want[1].Tags[1] != "hr" {
		t.Fatalf("TypedRow must not alias e.Tags: %#v / %#v", got, want)
	}
}

// Through FilterRows, TypedRow is CheckRow(E,P) && CheckRow(E∪R,P): a closed tag
// in R hides the row from a reader it is forbidden to; a party-scoped tag in R
// admits only the evidence's parties; an open tag that satisfies an any-of
// clause never widens (the row stays hidden when E alone fails).
func TestTypedRow_NarrowOnlyThroughFilterRows(t *testing.T) {
	type typed struct {
		name string
		e    Classification
		r    []string
	}
	of := func(x typed) ([]Classification, bool) { return TypedRow(x.e, x.r), true }
	rows := []typed{
		{"plain", Classification{Tags: []string{"internal"}}, nil},
		{"closed", Classification{Tags: []string{"internal"}}, []string{"hr"}},
		{"party-mine", Classification{Tags: []string{"internal"}, Parties: []string{"p1"}}, []string{"deal:x"}},
		{"party-other", Classification{Tags: []string{"internal"}, Parties: []string{"p2"}}, []string{"deal:x"}},
		{"would-widen", Classification{Tags: []string{"internal"}}, []string{"finance"}},
	}
	cases := []struct {
		name string
		p    *domain.TagPredicate
		want []string
	}{
		{"forbids hr", &domain.TagPredicate{ForbiddenTags: []string{"hr"}, PartyScopedTags: []string{"deal:x"}, PartyIdentities: []string{"p1"}},
			[]string{"plain", "party-mine", "would-widen"}},
		{"any-of finance cannot be satisfied by R", &domain.TagPredicate{AnyOfClauses: [][]string{{"finance"}}}, []string{}},
		{"maintenance", domain.ScopeMaintenance, []string{"plain", "closed", "party-mine", "party-other", "would-widen"}},
	}
	for _, c := range cases {
		got := []string{}
		for _, x := range FilterRows(c.p, rows, of) {
			got = append(got, x.name)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
		// Never wider than the evidence alone.
		for _, x := range FilterRows(c.p, rows, of) {
			if !c.p.Unfiltered() && !Readable(c.p, x.e) {
				t.Errorf("%s: %s admitted though its evidence is not readable", c.name, x.name)
			}
		}
	}
}
