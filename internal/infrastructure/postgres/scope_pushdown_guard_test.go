package postgres

import (
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// The scope pushdown filters `metadata @> '{"tags":[...]}'`, and the `documents`
// table does not carry tags in metadata — after the schema migration they live in a
// first-class `tags` column (0 of 1281 rows have tags in metadata, measured
// 2026-08-01).
//
// Applied there, ForbiddenTags becomes `NOT (matches nothing)` = TRUE for every
// row, so a scope forbidding `confidential` would filter NOTHING. It fails OPEN,
// which is the direction that matters.
//
// This pins the routing that keeps it unreachable. If someone adds a document-typed
// scoped query, this test tells them why they cannot, before the predicate silently
// stops enforcing.
func TestScopePredicateNeverTargetsDocuments(t *testing.T) {
	for _, docType := range []string{
		domain.DocTypeTool,
		domain.DocTypeSkill,
		domain.DocTypeAgentProfile,
		domain.DocTypeDocSection,
		domain.DocTypeMemory,
		domain.DocTypeMnemonicEntity,
		"anything-else",
		"",
	} {
		if got := tableFor(docType); got == "documents" {
			t.Fatalf("tableFor(%q) routes a SCOPED query at `documents`, where the "+
				"metadata tag predicate binds on nothing and ForbiddenTags fails OPEN. "+
				"Read documents through the enforcing store instead.", docType)
		}
	}
}

// The asymmetry itself, so the reasoning above is not just a comment.
func TestScopeExpressions_ForbiddenIsANegation(t *testing.T) {
	exprs := scopeExpressions(&domain.TagPredicate{ForbiddenTags: []string{"confidential"}})
	if len(exprs) != 1 {
		t.Fatalf("expected one predicate, got %d", len(exprs))
	}
	sql, _, err := dialect.From(TableChunks).Where(exprs...).ToSQL()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(strings.ToUpper(sql), "NOT") {
		t.Fatalf("ForbiddenTags is not a negation: %s", sql)
	}
	if !strings.Contains(sql, "metadata") {
		t.Fatalf("the predicate does not read `metadata`, so it cannot bind on a "+
			"table that keeps tags elsewhere: %s", sql)
	}
	// A negation over a column that never matches is TRUE for every row — which is
	// exactly why the table it runs against must carry tags in `metadata`.
}
