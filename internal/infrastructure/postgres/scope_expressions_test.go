package postgres

import (
	"strings"
	"testing"

	"github.com/doug-martin/goqu/v9"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz/sqlfence"
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
	t.Parallel()
	eff := &domain.TagPredicate{
		RequiredTags:  []string{"airline"},
		ForbiddenTags: []string{"internal"},
		AnyOfClauses:  [][]string{{"a", "b"}},
	}

	got := render(t, sqlfence.JSONBTags("c").Goqu(eff))
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

// TestScopeExpressionsOn_EmptyAliasMatchesLegacy: the single-table callers
// (fetchCandidates and its lexical twin) render with an empty alias, which must stay
// the unqualified `metadata` it was before the qualifier was introduced, so the GIN
// index still applies. (The exact text is pinned by testdata/fence_render_sql.golden.)
func TestScopeExpressionsOn_EmptyAliasMatchesLegacy(t *testing.T) {
	t.Parallel()
	eff := &domain.TagPredicate{RequiredTags: []string{"crm"}}

	got := render(t, sqlfence.JSONBTags("").Goqu(eff))
	if !strings.Contains(got, "metadata @>") || strings.Contains(got, "c.metadata @>") {
		t.Errorf("empty alias must stay unqualified:\n%s", got)
	}
}

// TestScopeExpressionsOn_NilAndBypass: nil and ScopeMaintenance yield no terms. For the
// single-table callers nil is correct (the chokepoint refuses the read first).
// ChunksMentioningEntity does NOT rely on it — it guards nil itself, because nothing sits
// in front of that lookup. A plain Bypass yields exactly ONE term: the owner fence
// (domain/owner_scope.go), which Bypass does not waive.
func TestScopeExpressionsOn_NilAndBypass(t *testing.T) {
	t.Parallel()
	if got := sqlfence.JSONBTags("c").Goqu(nil); got != nil {
		t.Errorf("nil predicate: expected no expressions, got %d", len(got))
	}
	if got := sqlfence.JSONBTags("c").Goqu(domain.ScopeMaintenance); got != nil {
		t.Errorf("maintenance predicate: expected no expressions, got %d", len(got))
	}
	got := sqlfence.JSONBTags("c").Goqu(&domain.TagPredicate{Bypass: true})
	if len(got) != 1 {
		t.Fatalf("bypass predicate: expected only the owner fence, got %d", len(got))
	}
	if sql := render(t, got); !strings.Contains(sql, "jsonb_path_exists(c.metadata") || !strings.Contains(sql, "owner:") {
		t.Errorf("bypass term is not the owner fence: %s", sql)
	}
}

// TestScopeExpressionsOn_PartyScopedDeniedLikeForbidden pins the ADR-0121 term on
// the chunk plane. The authoritative in-memory form (TagPredicate.Check — the
// party-LESS form; chunks have no parties column) denies any row carrying a
// party-scoped tag. The SQL mirror must therefore render each party-scoped tag as
// a NOT-containment term, exactly like a forbidden tag — before this term existed,
// dense/lexical/entity chunk reads returned rows Check refuses (the unguarded
// mirror-divergence the RAG Hardening Review flagged as Q2).
func TestScopeExpressionsOn_PartyScopedDeniedLikeForbidden(t *testing.T) {
	t.Parallel()
	eff := &domain.TagPredicate{
		PartyScopedTags: []string{"deal:acme", "deal:globex"},
		PartyIdentities: []string{"customer:acme"}, // identities are irrelevant on this plane
	}

	got := render(t, sqlfence.JSONBTags("c").Goqu(eff))
	for _, tag := range eff.PartyScopedTags {
		want := `NOT (c.metadata @> '{"tags":["` + tag + `"]}'`
		if !strings.Contains(got, want) {
			t.Errorf("missing party-scoped NOT-containment for %q in:\n%s", tag, got)
		}
	}
	// Mirror agreement: the in-memory authoritative form denies the same rows.
	if eff.Allows([]string{"deal:acme"}) {
		t.Fatal("authoritative Check must deny a party-less row carrying a party-scoped tag")
	}
	if !eff.Allows([]string{"unrelated"}) {
		t.Fatal("a row without party-scoped tags must remain admitted")
	}
}
