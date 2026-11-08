package domain

import "testing"

// The owner fence as the authoritative in-memory predicate (owner_scope.go).
// Every SQL mirror is held to this by the Postgres tests in
// internal/infrastructure/postgres/fence_owner_scope_pg_test.go.
func TestOwnerFence_Check(t *testing.T) {
	alicesRow := []string{"mail", "owner:alice"}
	bobsRow := []string{"mail", "owner:bob"}
	corpus := []string{"mail"}
	jointRow := []string{"owner:alice", "owner:bob"} // derived from both people's rows

	cases := []struct {
		name string
		p    *TagPredicate
		tags []string
		want bool
	}{
		{"owner reads own row", &TagPredicate{Owners: []string{"alice"}}, alicesRow, true},
		{"another user is fenced out", &TagPredicate{Owners: []string{"bob"}}, alicesRow, false},
		{"reader acting for nobody is fenced out", &TagPredicate{}, alicesRow, false},
		{"unowned corpus stays readable", &TagPredicate{}, corpus, true},
		{"ScopeSystem (operator plane) is fenced", ScopeSystem, alicesRow, false},
		{"ScopeSystem still reads the corpus", ScopeSystem, corpus, true},
		{"ScopeMaintenance reads every owner", ScopeMaintenance, bobsRow, true},
		{"ScopeSystemFor reads exactly its owners", ScopeSystemFor("alice"), alicesRow, true},
		{"ScopeSystemFor does not read others", ScopeSystemFor("alice"), bobsRow, false},
		{"AllOwners without Bypass is ignored", &TagPredicate{AllOwners: true}, bobsRow, false},
		{"a policy naming the owner tag cannot widen it",
			&TagPredicate{AnyOfClauses: [][]string{{"owner:bob", "mail"}}, Owners: []string{"alice"}}, bobsRow, false},
		{"a row derived from two owners needs both", &TagPredicate{Owners: []string{"alice"}}, jointRow, false},
		{"nil predicate denies (unresolved principal)", nil, alicesRow, false},
	}
	for _, c := range cases {
		dec, got := c.p.Check(c.tags)
		if got != c.want {
			t.Errorf("%s: allowed=%v want %v (%+v)", c.name, got, c.want, dec)
		}
		if !got && c.p != nil && dec.Reason != ReasonNotOwner {
			t.Errorf("%s: reason %q, want %q", c.name, dec.Reason, ReasonNotOwner)
		}
		if !got && dec.Detail != "" && dec.Detail != "owner-scoped row" && c.p != nil {
			t.Errorf("%s: detail %q must not name the owner", c.name, dec.Detail)
		}
		// CheckRow (substrate rows, which carry parties) must agree.
		if _, row := c.p.CheckRow(c.tags, []string{"customer:alice"}); row != c.want {
			t.Errorf("%s: CheckRow allowed=%v want %v", c.name, row, c.want)
		}
	}
}

func TestOwnerFence_Helpers(t *testing.T) {
	if got := OwnersOf([]string{"mail", "owner:alice", "owner:", "owner:bob"}); len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Fatalf("OwnersOf = %v", got)
	}
	if h := (&TagPredicate{}).HeldOwnerTags(); h == nil || len(h) != 0 {
		t.Fatalf("HeldOwnerTags must be a non-nil empty slice (SQL NULL would admit every owner row): %#v", h)
	}
	if h := (&TagPredicate{Owners: []string{"alice"}}).HeldOwnerTags(); len(h) != 1 || h[0] != "owner:alice" {
		t.Fatalf("HeldOwnerTags = %v", h)
	}
	if !ScopeMaintenance.Unfiltered() || ScopeSystem.Unfiltered() || (&TagPredicate{AllOwners: true}).Unfiltered() {
		t.Fatal("only Bypass+AllOwners is unfiltered")
	}
	if OwnerTag("") != "" || OwnerTag("x") != "owner:x" {
		t.Fatal("OwnerTag")
	}
}
