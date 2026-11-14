package sqlfence

import (
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// Unit tests of the one renderer's invariants. The byte-level SQL and the row
// outcomes of every shape are pinned by the characterization goldens in
// internal/infrastructure/postgres (fence_render_characterization_test.go).

func TestFence_UnfilteredAndNil(t *testing.T) {
	for _, s := range []Shape{TextArray("x.tags"), JSONBTags("c")} {
		if sql, args := s.Where(nil, 0); sql != "" || args != nil {
			t.Errorf("nil predicate must render nothing outside the evidence plane: %q %v", sql, args)
		}
		if got := s.Goqu(domain.ScopeMaintenance); got != nil {
			t.Errorf("maintenance must render nothing, got %d terms", len(got))
		}
	}
	if sql, _ := EvidenceExists(domain.ScopeMaintenance, "o.evidence_id", 0); sql != "" {
		t.Errorf("maintenance on evidence must render nothing: %q", sql)
	}
	defer func() {
		if recover() == nil {
			t.Error("a nil predicate on the evidence plane must panic, as it always did")
		}
	}()
	Evidence("e").Where(nil, 0)
}

func TestFence_OwnerFenceSurvivesBypass(t *testing.T) {
	for _, s := range []Shape{Evidence("e"), TextArray("x.tags")} {
		sql, args := s.Where(domain.ScopeSystem, 4)
		if !strings.Contains(sql, "owner_tag") || !strings.Contains(sql, "$5::text[]") || len(args) != 1 {
			t.Errorf("bypass must still carry the owner fence: %q %v", sql, args)
		}
	}
	if got := JSONBTags("").Goqu(domain.ScopeSystem); len(got) != 1 {
		t.Errorf("jsonb bypass must carry only the owner fence, got %d terms", len(got))
	}
}

func TestFence_PartyScopedRowAwareOnlyWithParties(t *testing.T) {
	p := &domain.TagPredicate{PartyScopedTags: []string{"deal:x"}}
	ev, evArgs := Evidence("e").Where(p, 0)
	if !strings.Contains(ev, "OR e.parties && $3") {
		t.Errorf("evidence party term must be the row-aware implication: %q", ev)
	}
	if ids, ok := evArgs[2].([]string); !ok || ids == nil {
		t.Errorf("no identities must bind an empty (not NULL) array: %#v", evArgs[2])
	}
	ta, _ := TextArray("k.c").Where(p, 0)
	if strings.Contains(ta, "parties") || !strings.Contains(ta, "NOT (k.c && $2::text[])") {
		t.Errorf("a table without parties must deny party-scoped tags outright: %q", ta)
	}
}

func TestFence_OwnRowsDisjunction(t *testing.T) {
	p := &domain.TagPredicate{
		Owners: []string{"a"}, AnyOfClauses: [][]string{{"x"}},
		OwnRows: &domain.TagPredicate{Owners: []string{"a"}},
	}
	sql, args := TextArray("t").Where(p, 0)
	want := " AND ((" // (main) OR (own_row AND ownrows)
	if !strings.HasPrefix(sql, want) || !strings.Contains(sql, ") OR (t @> ARRAY[$3::text] AND ") {
		t.Errorf("own-rows must render (main) OR (own_row AND ownrows): %q", sql)
	}
	if args[2] != "owner:a" || len(args) != 4 {
		t.Errorf("args out of order: %#v", args)
	}
}

func TestOwnerFence_SinglePlaceholder(t *testing.T) {
	got := OwnerFence("d.tags", 7)
	if !strings.HasPrefix(got, "NOT EXISTS (SELECT 1 FROM unnest(d.tags)") || !strings.Contains(got, "ALL($7::text[])") {
		t.Errorf("OwnerFence: %q", got)
	}
}

// An empty any-of clause is FALSE in every shape (owner decision 2026-11-13,
// T-20261113-sql-fence-mirrors-skip-an-empty-any-of-clause): Check denies it, and
// a rendering that skipped it would admit rows Check denies. The differential
// test (TestPg_FenceDifferential) proves the row outcomes; this pins the term.
func TestFence_EmptyAnyOfClauseRendersFalse(t *testing.T) {
	p := &domain.TagPredicate{AnyOfClauses: [][]string{{"a"}, {}}}
	if _, ok := p.Check([]string{"a"}); ok {
		t.Fatal("precondition: Check must deny under an empty clause")
	}
	for _, s := range []Shape{Evidence("e"), TextArray("x.tags"), JSONBTags("c")} {
		sql, _ := s.Where(p, 0)
		if !strings.Contains(sql, " AND FALSE") {
			t.Errorf("an empty any-of clause must render FALSE: %q", sql)
		}
		if n := strings.Count(sql, "FALSE"); n != 1 {
			t.Errorf("one empty clause, one FALSE; got %d in %q", n, sql)
		}
	}
	// A predicate with no empty clause renders no FALSE.
	if sql, _ := TextArray("x.tags").Where(&domain.TagPredicate{AnyOfClauses: [][]string{{"a"}}}, 0); strings.Contains(sql, "FALSE") {
		t.Errorf("a non-empty clause must not render FALSE: %q", sql)
	}
}

// ObjectExtra is ObjectProperty over an extra jsonb entry's own tags/parties
// (T-20261113-listobjects-getobject-do-not-expose-undeclared): an entry without a
// tags array is unclassified and must be denied to a filtered reader, and the
// entry's parties make party scoping row-aware.
func TestFence_ObjectExtraIsPerEntryAndFailsClosed(t *testing.T) {
	p := &domain.TagPredicate{RequiredTags: []string{"sales"}, PartyScopedTags: []string{"deal:x"}}
	sql, args := ObjectExtra("x.value").Expr(p, 2)
	for _, want := range []string{
		"(CASE WHEN jsonb_typeof(x.value->'tags') = 'array' THEN ARRAY(SELECT jsonb_array_elements_text(x.value->'tags')) END) IS NOT NULL",
		"@> $4",
		"x.value->'parties'",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q in %s", want, sql)
		}
	}
	if strings.Contains(sql, "?") {
		t.Errorf("a leftover placeholder: %s", sql)
	}
	if len(args) == 0 || !strings.HasPrefix(sql, "(") && !strings.Contains(sql, "$3") {
		t.Errorf("args must be numbered after startIdx: %s %v", sql, args)
	}
	if body, a := ObjectExtra("x.value").Expr(domain.ScopeMaintenance, 0); body != "TRUE" || a != nil {
		t.Errorf("maintenance sees every entry: %q %v", body, a)
	}
	defer func() {
		if recover() == nil {
			t.Error("a nil predicate must panic, as for ObjectProperty")
		}
	}()
	ObjectExtra("x.value").Expr(nil, 0)
}
