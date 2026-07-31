package domain

import "testing"

// The case this returns a bool for: CNF with more than one clause cannot become a
// single OR-set without WIDENING the term.
//
// "(a or b) AND (c or d)" flattened to "a or b or c or d" admits a document
// carrying only `a`, which the predicate refuses. Persisting that as "what this
// caller may see" would hand the caller access the decision point never granted.
func TestTagSetFromPredicate_RefusesMultiClauseCNF(t *testing.T) {
	p := &TagPredicate{AnyOfClauses: [][]string{{"a", "b"}, {"c", "d"}}}
	if _, ok := TagSetFromPredicate(p); ok {
		t.Fatal("multi-clause CNF was flattened; that widens the caller's scope")
	}
}

// A nil predicate means "no read authorized at all". It must not become an empty
// (unrestricted) TagSet — that is the inversion of its meaning.
func TestTagSetFromPredicate_NilIsRefusedNotUnrestricted(t *testing.T) {
	if got, ok := TagSetFromPredicate(nil); ok {
		t.Fatalf("nil predicate converted to %+v; nil means no read is authorized", got)
	}
}

func TestTagSetFromPredicate_RepresentableCases(t *testing.T) {
	t.Run("bypass is no constraint", func(t *testing.T) {
		got, ok := TagSetFromPredicate(&TagPredicate{Bypass: true})
		if !ok || !got.IsZero() {
			t.Fatalf("bypass = %+v ok=%v, want an empty representable set", got, ok)
		}
	})

	t.Run("required and forbidden carry across", func(t *testing.T) {
		got, ok := TagSetFromPredicate(&TagPredicate{
			RequiredTags:  []string{"sales"},
			ForbiddenTags: []string{"confidential"},
		})
		if !ok {
			t.Fatal("a plain predicate was refused")
		}
		if len(got.RequiredTags) != 1 || got.RequiredTags[0] != "sales" {
			t.Errorf("required = %v", got.RequiredTags)
		}
		if len(got.ForbiddenTags) != 1 || got.ForbiddenTags[0] != "confidential" {
			t.Errorf("forbidden = %v", got.ForbiddenTags)
		}
	})

	t.Run("a single OR-set is representable", func(t *testing.T) {
		got, ok := TagSetFromPredicate(&TagPredicate{AnyOfClauses: [][]string{{"a", "b"}}})
		if !ok || len(got.AnyOfTags) != 2 {
			t.Fatalf("single clause = %+v ok=%v", got, ok)
		}
	})
}

// The conversion must COPY, not alias: a caller_scope persisted on a session must
// not change because something later mutated the predicate it came from.
func TestTagSetFromPredicate_DoesNotAliasTheSource(t *testing.T) {
	p := &TagPredicate{RequiredTags: []string{"sales"}}
	got, ok := TagSetFromPredicate(p)
	if !ok {
		t.Fatal("refused")
	}
	p.RequiredTags[0] = "mutated"
	if got.RequiredTags[0] != "sales" {
		t.Fatal("the converted TagSet aliases the predicate's backing array")
	}
}
