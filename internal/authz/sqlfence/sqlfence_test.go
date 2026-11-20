package sqlfence

import (
	"strconv"
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

// ADR-0158 D2: a row-tags shape is the base fence, unchanged and first, AND-ed
// with the NULL/empty short-circuit over the same terms on the concatenation.
func TestFence_WithRowTagsIsBaseAndNarrowed(t *testing.T) {
	preds := []*domain.TagPredicate{
		domain.ScopeSystem,
		{Owners: []string{"alice"}, RequiredTags: []string{"sales"}, ForbiddenTags: []string{"hr"},
			AnyOfClauses: [][]string{{"a"}}, PartyScopedTags: []string{"deal:x"}, PartyIdentities: []string{"p"}},
		{Owners: []string{"alice"}, AnyOfClauses: [][]string{{"sales"}},
			OwnRows: &domain.TagPredicate{Owners: []string{"alice"}}},
	}
	for _, p := range preds {
		base, baseArgs := Evidence("e").Expr(p, 3)
		typed, typedArgs := Evidence("e").WithRowTags("o.classification").Expr(p, 3)
		// ENG-2264: IS NULL leads (a row with no classification stores NULL, and
		// cardinality(NULL) = 0 is NULL), then the empty-array test.
		if !strings.HasPrefix(typed, base[:len(base)-1]+" AND (o.classification IS NULL OR cardinality(o.classification) = 0 OR (") {
			t.Errorf("%+v: typed fence must start with the base fence and the short-circuit:\nbase:  %s\ntyped: %s", *p, base, typed)
		}
		if !strings.Contains(typed, "(e.classification || o.classification)") {
			t.Errorf("%+v: second fence must read the concatenation: %s", *p, typed)
		}
		if strings.Contains(typed, "(e.parties || ") {
			t.Errorf("%+v: a row classification adds no parties: %s", *p, typed)
		}
		if len(typedArgs) != 2*len(baseArgs) {
			t.Errorf("%+v: the second fence binds the same args again: %d vs %d", *p, len(typedArgs), len(baseArgs))
		}
	}
	if body, args := Evidence("e").WithRowTags("o.classification").Expr(domain.ScopeMaintenance, 0); body != "TRUE" || args != nil {
		t.Errorf("maintenance must render no fence: %q %v", body, args)
	}
}

// TypedRowExists is EvidenceExists with the row-tags shape: same EXISTS frame,
// "" for maintenance, a nil predicate panics as on the evidence plane.
func TestFence_TypedRowExists(t *testing.T) {
	p := &domain.TagPredicate{ForbiddenTags: []string{"hr"}}
	ev, _ := EvidenceExists(p, "o.evidence_id", 1)
	tr, args := TypedRowExists(p, "o.evidence_id", "o.classification", 1)
	if !strings.HasPrefix(tr, ev[:len(ev)-1]) || !strings.HasSuffix(tr, ")") {
		t.Errorf("TypedRowExists must extend EvidenceExists' terms in place:\nev: %s\ntr: %s", ev, tr)
	}
	if !strings.Contains(tr, "cardinality(o.classification) = 0") || len(args) != 4 {
		t.Errorf("unexpected rendering: %s %v", tr, args)
	}
	if sql, _ := TypedRowExists(domain.ScopeMaintenance, "o.evidence_id", "o.classification", 0); sql != "" {
		t.Errorf("maintenance must render nothing: %q", sql)
	}
	defer func() {
		if recover() == nil {
			t.Error("a nil predicate must panic, as for EvidenceExists")
		}
	}()
	TypedRowExists(nil, "o.evidence_id", "o.classification", 0)
}

func TestFence_WithRowTagsRefusesJSONB(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("WithRowTags on a jsonb shape must panic")
		}
	}()
	JSONBTags("c").WithRowTags("c.row_tags")
}

func TestFence_ObjectRowTagVariants(t *testing.T) {
	p := &domain.TagPredicate{ForbiddenTags: []string{"hr"}}
	fc, _ := FenceClassRowTags("c").Expr(p, 0)
	if !strings.Contains(fc, "cardinality(c.row_tags) = 0") || !strings.Contains(fc, "(c.tags || c.row_tags)") ||
		!strings.Contains(fc, "c.tags IS NOT NULL") {
		t.Errorf("FenceClassRowTags: %s", fc)
	}
	ex, _ := ObjectExtraRowTags("x.value").Expr(p, 0)
	if !strings.Contains(ex, "x.value->'row_tags'") {
		t.Errorf("ObjectExtraRowTags must read the entry's row_tags: %s", ex)
	}
	base, _ := ObjectExtra("x.value").Expr(p, 0)
	if !strings.HasPrefix(ex, base[:len(base)-1]+" AND ") {
		t.Errorf("ObjectExtraRowTags must keep ObjectExtra's fence first:\nbase: %s\nrow:  %s", base, ex)
	}
}

// LinkExists is TypedRowExists over the link's cited evidence, unchanged and
// first, AND every supporting evidence readable through the evidence fence
// (ENG-2049): placeholders continue after the cited fence's, "" for maintenance,
// a nil predicate panics as on the evidence plane.
func TestFence_LinkExists(t *testing.T) {
	p := &domain.TagPredicate{ForbiddenTags: []string{"hr"}, PartyScopedTags: []string{"deal:x"}, PartyIdentities: []string{"p1"}}
	typed, targs := TypedRowExists(p, "l.evidence_id", "l.classification", 2)
	link, largs := LinkExists(p, "l", 2)
	if !strings.HasPrefix(link, typed+" AND NOT EXISTS (SELECT 1 FROM unnest(l.supporting_evidence) AS link_support(id)") {
		t.Errorf("LinkExists must be TypedRowExists first, unchanged, then the supporting-evidence test:\ntyped: %s\nlink:  %s", typed, link)
	}
	ev, eargs := Evidence("e").Where(p, 2+len(targs))
	if !strings.HasSuffix(link, "WHERE e.id = link_support.id"+ev+"))") {
		t.Errorf("each supporting id must pass the evidence fence (parties included):\n%s", link)
	}
	if !strings.Contains(ev, "e.parties &&") {
		t.Errorf("the supporting fence must be row-aware (parties): %s", ev)
	}
	if len(largs) != len(targs)+len(eargs) {
		t.Errorf("args: %d, want %d + %d", len(largs), len(targs), len(eargs))
	}
	if !strings.Contains(link, "$"+strconv.Itoa(2+len(largs))) || strings.Contains(link, "$"+strconv.Itoa(3+len(largs))) {
		t.Errorf("placeholders must run from $3 to $%d: %s", 2+len(largs), link)
	}
	if sql, args := LinkExists(domain.ScopeMaintenance, "l", 0); sql != "" || args != nil {
		t.Errorf("maintenance must render nothing: %q %v", sql, args)
	}
	defer func() {
		if recover() == nil {
			t.Error("a nil predicate must panic, as for TypedRowExists")
		}
	}()
	LinkExists(nil, "l", 0)
}
