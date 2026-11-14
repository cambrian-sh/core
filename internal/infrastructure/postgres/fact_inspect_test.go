package postgres

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// The operator plane's ListFacts (T-20261103-operator-plane-cannot-inspect-facts-no-rpc):
// filters are compared on the normalised entity-id keys, the keyset cursor round-trips,
// a forged cursor is refused (never silently restarting at page one), and the scope
// fence is rendered IN SQL after the filter arguments.
func TestFactListWhere_FiltersCursorAndFence(t *testing.T) {
	at := time.Date(2026, 11, 14, 10, 0, 0, 123, time.UTC)
	where, args, err := factListWhere(domain.FactListFilter{
		Subject: "  The User ", Attribute: "Home City", Actor: "user:alice",
		After: encodeFactCursor(at, "ki_fact:x"),
	}, &domain.TagPredicate{ForbiddenTags: []string{"secret"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"split_part(ki.entity_id, '/', 2) = $3",
		"split_part(ki.entity_id, '/', 3) = $4",
		"ki.asserted_by = $5",
		"(ki.asserted_at, ki.id) < ($6, $7)",
		"ki.classification",
	} {
		if !strings.Contains(where, want) {
			t.Errorf("WHERE lacks %q:\n%s", want, where)
		}
	}
	if args[0] != domain.NormalizeFactText("  The User ") || args[1] != domain.NormalizeFactAttribute("Home City") ||
		args[2] != "user:alice" || !args[3].(time.Time).Equal(at) || args[4] != "ki_fact:x" {
		t.Fatalf("args = %v", args)
	}
	if len(args) < 6 {
		t.Fatalf("the fence must add its own arguments after the filters: %v", args)
	}
	// Placeholders are contiguous: $1/$2 are the fixed kind/namespace terms, so the
	// highest placeholder is exactly len(args)+2 and the first fence one follows $7.
	if !strings.Contains(where, "$8") || strings.Contains(where, fmt.Sprintf("$%d", len(args)+3)) {
		t.Errorf("fence placeholders not contiguous with %d args:\n%s", len(args), where)
	}

	// No filter, no fence (nil predicate adds nothing — the decorator decides).
	if w, a, err := factListWhere(domain.FactListFilter{}, nil); err != nil || w != "" || len(a) != 0 {
		t.Fatalf("empty filter: %q %v %v", w, a, err)
	}
	// A cursor this reader did not issue is refused.
	for _, bad := range []string{"not-base64!", encodeRaw("no-separator"), encodeRaw("notatime|id"), encodeRaw(at.Format(time.RFC3339Nano) + "|")} {
		if _, _, err := factListWhere(domain.FactListFilter{After: bad}, nil); !errors.Is(err, domain.ErrFactCursor) {
			t.Errorf("cursor %q: err = %v, want ErrFactCursor", bad, err)
		}
	}
}

func encodeRaw(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestFactListLimit_Clamps(t *testing.T) {
	for in, want := range map[int]int{0: domain.FactListDefaultLimit, -3: domain.FactListDefaultLimit, 10: 10, 10000: domain.FactListMaxLimit} {
		if got := factListLimit(in); got != want {
			t.Errorf("factListLimit(%d) = %d, want %d", in, got, want)
		}
	}
}

// Against a real, migrated Postgres: ListFacts pages newest-first with a working
// cursor and applies the scope fence in SQL; FactCoverage counts markers by version.
func TestPg_FactInspect_ListPagesAndCoverage(t *testing.T) {
	ks, r, ctx := newFactSubstrate(t)
	pub := a1Fact("pet", "cat", "a1test-c1", a1Day("2023-01-01"), "public")
	sec := a1Fact("pet", "dragon", "a1test-c2", a1Day("2023-01-01"), "secret")
	home := a1Fact("home_city", "Paris", "a1test-c3", a1Day("2023-01-01"), "public")
	putAll(t, ks, ctx, pub, sec, home)

	all, next, err := r.ListFacts(ctx, domain.FactListFilter{Limit: 2}, domain.ScopeSystem)
	if err != nil || len(all) != 2 || next == "" {
		t.Fatalf("page 1: %d facts, next %q, err %v", len(all), next, err)
	}
	rest, next2, err := r.ListFacts(ctx, domain.FactListFilter{Limit: 2, After: next}, domain.ScopeSystem)
	if err != nil || len(rest) != 1 || next2 != "" {
		t.Fatalf("page 2: %d facts, next %q, err %v", len(rest), next2, err)
	}
	seen := map[string]bool{}
	for _, f := range append(all, rest...) {
		if seen[f.ID] {
			t.Fatalf("fact %s on two pages", f.ID)
		}
		seen[f.ID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("paged %d distinct facts, want 3", len(seen))
	}
	scoped, _, err := r.ListFacts(ctx, domain.FactListFilter{}, &domain.TagPredicate{ForbiddenTags: []string{"secret"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range scoped {
		if f.ID == string(sec.ID) {
			t.Fatal("the forbidden fact must be filtered in SQL")
		}
	}
	byAttr, _, err := r.ListFacts(ctx, domain.FactListFilter{Attribute: "Home City"}, domain.ScopeSystem)
	if err != nil || len(byAttr) != 1 || byAttr[0].ID != string(home.ID) {
		t.Fatalf("attribute filter: %+v err %v", byAttr, err)
	}

	if _, err := ks.pool.Exec(ctx, `INSERT INTO knowledge_extractions
			(namespace_id, extractor, document_id, content_hash, items, extractor_version, passages_covered, passages_total)
		VALUES ($1, $2, 'a1test-d1', 'h1', 2, 'p/v1', 3, 3), ($1, $2, 'a1test-d2', 'h2', 1, 'p/v1', 1, 3)`,
		a1NS, FactBackfillExtractor); err != nil {
		t.Fatal(err)
	}
	cov, err := r.FactCoverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cov.DocumentsExtracted != 2 || cov.FactItems != 3 || cov.LastExtractedAt.IsZero() {
		t.Fatalf("coverage = %+v", cov)
	}
	if len(cov.Versions) != 1 || cov.Versions[0].Version != "p/v1" || cov.Versions[0].DocumentsComplete != 1 || cov.Versions[0].DocumentsPartial != 1 {
		t.Fatalf("versions = %+v", cov.Versions)
	}
}
