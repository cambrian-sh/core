package postgres

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"pgregory.net/rapid"

	"github.com/cambrian-sh/core/domain"
)

// Differential guard for ENG-1492 (AGENTS.md §2.5): the query plane's two
// in-memory row decisions — guardNodes (every graph walk) and evidenceRow (the
// evidence inspection shape) — moved from inline TagPredicate.AllowsRow loops to
// the authz-owned internal/authz/rowfence. The move must change nothing: on the
// same real-Postgres fixture, for random predicates (drawPredicate, shared with
// TestPg_FenceDifferential) and random nodes, the new code admits exactly the
// rows the pre-move loop admitted, in the same order. legacyGuardNodes below is
// that loop verbatim (only renamed); do not "fix" it.
//
// The file name sorts before migration_guard_test.go on purpose (that test drops
// and recreates the public schema).

// legacyGuardNodes is guardNodes' body as it was before ENG-1492.
func legacyGuardNodes[T any](ctx context.Context, p *PgQueryPlane, scope *domain.TagPredicate,
	nodes []T, evidenceOf func(T) []string) ([]T, error) {
	if scope.Unfiltered() || len(nodes) == 0 {
		return nodes, nil
	}
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, evidenceOf(n)...)
	}
	tags, err := p.evidenceTags(ctx, ids)
	if err != nil {
		return nil, err
	}
	kept := nodes[:0:0]
	for _, n := range nodes {
		ev := evidenceOf(n)
		ok := len(ev) > 0
		for _, id := range ev {
			t, resolvable := tags[id]
			if !resolvable || !scope.AllowsRow(t.tags, t.parties) {
				ok = false
				break
			}
		}
		if ok {
			kept = append(kept, n)
		}
	}
	return kept, nil
}

// rfNode is a walk node standing on the evidence ids it names.
type rfNode struct {
	n   int
	evs []string
}

func TestPg_RowfenceDifferential(t *testing.T) {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	clean := func() {
		bg := context.Background()
		for _, q := range []string{
			`DELETE FROM evidence_outbox WHERE evidence_id IN (SELECT id FROM evidence WHERE source_id = 'rowfence-src')`,
			`DELETE FROM evidence WHERE source_id = 'rowfence-src'`,
		} {
			if _, err := pool.Exec(bg, q); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)

	// The fixture: one evidence row per (tags, parties) combination, covering the
	// owner fence, plain/required/forbidden/any-of material, party-scoped tags
	// with and without parties, and an unclassified row.
	tagSets := [][]string{
		{}, {"a"}, {"b"}, {"sales"}, {"hr"}, {"a", "sales"}, {"sales", "hr"},
		{"deal:x"}, {"deal:y", "b"}, {"owner:alice"}, {"owner:bob", "sales"},
		{"owner:alice", "deal:x"}, {domain.OwnerTagPrefix},
	}
	partySets := [][]string{nil, {"p1"}, {"p2"}, {"p1", "p2"}}
	evs := NewPgEvidenceStore(pool)
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	var ids []string
	for i, tags := range tagSets {
		for j, parties := range partySets {
			key := fmt.Sprintf("t%d-p%d", i, j)
			id, _, err := evs.Insert(ctx, domain.Evidence{
				NamespaceID: "default", SourceID: "rowfence-src", SourceKey: key,
				SourceTime: base, IngestedAt: base,
				ContentHash: domain.CID("sha256:rowfence-" + key), Classification: tags, Parties: parties,
			})
			if err != nil {
				t.Fatalf("evidence %s: %v", key, err)
			}
			ids = append(ids, string(id))
		}
	}
	// An id no row has: unresolvable evidence must hide the node in both forms.
	pool2 := append(append([]string{}, ids...), "ev-rowfence-missing", "")
	plane := NewPgQueryPlane(pool, NewPgEventStore(pool, nil), NewPgKnowledgeStore(pool, nil), evs, nil,
		NewPgEntityStore(pool, nil), nil)

	var admitted, denied int
	check := func(t *rapid.T, scope *domain.TagPredicate) {
		nNodes := rapid.IntRange(0, 12).Draw(t, "nodes")
		nodes := make([]rfNode, nNodes)
		for i := range nodes {
			nodes[i].n = i
			k := rapid.IntRange(0, 3).Draw(t, fmt.Sprintf("node%d:evs", i))
			for e := 0; e < k; e++ {
				nodes[i].evs = append(nodes[i].evs, rapid.SampledFrom(pool2).Draw(t, fmt.Sprintf("node%d:ev%d", i, e)))
			}
		}
		of := func(n rfNode) []string { return n.evs }
		want, err := legacyGuardNodes(ctx, plane, scope, nodes, of)
		if err != nil {
			t.Fatalf("legacy: %v", err)
		}
		got, err := guardNodes(ctx, plane, scope, nodes, of)
		if err != nil {
			t.Fatalf("guardNodes: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("guardNodes diverged for %+v:\n got  %v\n want %v", scope, got, want)
		}
		admitted += len(got)
		denied += len(nodes) - len(got)

		// evidenceRow: one inspection, judged before by AllowsRow on the stored row.
		id := rapid.SampledFrom(ids).Draw(t, "inspect")
		rows, err := plane.evidenceRow(ctx, domain.KnowledgeQuery{EvidenceID: id}, scope)
		if err != nil {
			t.Fatalf("evidenceRow: %v", err)
		}
		ev, err := evs.Get(ctx, domain.EvidenceID(id))
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if legacy := scope.AllowsRow(ev.Classification, ev.Parties); legacy != (len(rows) == 1) {
			t.Fatalf("evidenceRow diverged for %+v on %v/%v: rows=%d legacy=%v",
				scope, ev.Classification, ev.Parties, len(rows), legacy)
		}
	}

	rapid.Check(t, func(t *rapid.T) {
		check(t, drawPredicate(t, "p", 0))
	})
	// nil is not drawn by drawPredicate; both forms must deny every row for it.
	rapid.Check(t, func(t *rapid.T) { check(t, nil) })

	if admitted == 0 || denied == 0 {
		t.Fatalf("vacuous differential: admitted=%d denied=%d", admitted, denied)
	}
	t.Logf("rowfence differential: %d nodes admitted, %d denied, identical in both forms", admitted, denied)
}
