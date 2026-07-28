package postgres

import (
	"strings"
	"testing"

	"github.com/doug-martin/goqu/v9"

	"github.com/cambrian-sh/core/domain"
)

// render builds the WHERE clause the expressions produce, so the assertions read
// against real generated SQL rather than against goqu's AST.
func render(t *testing.T, exprs []goqu.Expression) string {
	t.Helper()
	ds := dialect.From(goqu.T("chunk_triplets").As("ct")).
		Join(goqu.T("chunks").As("c"), goqu.On(goqu.Ex{"c.id": goqu.I("ct.chunk_id")})).
		Select("ct.chunk_id")
	for _, e := range exprs {
		ds = ds.Where(e)
	}
	sql, _, err := ds.ToSQL()
	if err != nil {
		t.Fatalf("ToSQL: %v", err)
	}
	return sql
}

// TestScopeExpressionsOn_QualifiesMetadataColumn covers the ADR-0095 D9 addition.
// ChunksMentioningEntity applies the predicate across a JOIN, where an unqualified
// `metadata` is at best ambiguous and at worst silently resolves to the wrong table.
func TestScopeExpressionsOn_QualifiesMetadataColumn(t *testing.T) {
	eff := &domain.TagPredicate{
		RequiredTags:  []string{"airline"},
		ForbiddenTags: []string{"internal"},
		AnyOfClauses:  [][]string{{"a", "b"}},
	}

	got := render(t, scopeExpressionsOn(eff, "c"))
	if strings.Contains(got, `"metadata"`) || strings.Contains(got, " metadata ") {
		t.Errorf("found an UNQUALIFIED metadata reference in a joined query:\n%s", got)
	}
	for _, want := range []string{"c.metadata @>", "NOT (c.metadata @>"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// One required + two any-of + one forbidden = 4 containment terms.
	if n := strings.Count(got, "c.metadata @>"); n != 4 {
		t.Errorf("expected 4 containment terms, got %d:\n%s", n, got)
	}
}

// TestScopeExpressionsOn_EmptyAliasMatchesLegacy: the existing single-table callers
// (fetchCandidates and its lexical twin) go through scopeExpressions, which delegates
// with an empty alias. That path must be byte-identical to what it produced before the
// qualifier was introduced — unqualified `metadata`, so the GIN index still applies.
func TestScopeExpressionsOn_EmptyAliasMatchesLegacy(t *testing.T) {
	eff := &domain.TagPredicate{RequiredTags: []string{"crm"}}

	viaWrapper := render(t, scopeExpressions(eff))
	viaEmpty := render(t, scopeExpressionsOn(eff, ""))
	if viaWrapper != viaEmpty {
		t.Errorf("scopeExpressions must equal scopeExpressionsOn(_, \"\"):\n%s\n%s", viaWrapper, viaEmpty)
	}
	if !strings.Contains(viaWrapper, "metadata @>") || strings.Contains(viaWrapper, "c.metadata @>") {
		t.Errorf("empty alias must stay unqualified:\n%s", viaWrapper)
	}
}

// TestScopeExpressionsOn_NilAndBypass: both yield no terms. For the single-table callers
// that is correct (the chokepoint refuses the read first). ChunksMentioningEntity does NOT
// rely on it — it guards nil itself, because nothing sits in front of that lookup.
func TestScopeExpressionsOn_NilAndBypass(t *testing.T) {
	if got := scopeExpressionsOn(nil, "c"); got != nil {
		t.Errorf("nil predicate: expected no expressions, got %d", len(got))
	}
	if got := scopeExpressionsOn(&domain.TagPredicate{Bypass: true}, "c"); got != nil {
		t.Errorf("bypass predicate: expected no expressions, got %d", len(got))
	}
}
