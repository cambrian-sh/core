package postgres

import (
	"context"
	"fmt"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// DB-less unit tests of the walk machinery (T-20261112-graph-walks-fence-nodes-
// by-hand-per-lane-one, T-20261112-why-traverse-issue-one-query-per-frontier-
// node). Unfiltered (ScopeMaintenance) keeps guardNodes off the database, so
// these pin the paging and batching logic itself; the scope decisions are pinned
// against a real engine in the fence_*_pg_test.go files.

// An unfiltered caller reads exactly the page the walk always read: one fetch,
// `want` rows, never a second page.
func TestPageVisible_UnfilteredReadsOnePage(t *testing.T) {
	var fetches []int
	got, err := pageVisible(context.Background(), nil, domain.ScopeMaintenance,
		[]string{"a", "b", "a"}, 3,
		func(keys []string, offset, n int) (map[string][]int, error) {
			fetches = append(fetches, offset)
			if len(keys) != 2 {
				t.Fatalf("duplicate keys reached the store: %v", keys)
			}
			return map[string][]int{"a": {1, 2, 3}, "b": {4}}, nil
		}, func(int) []guardRef { return []guardRef{{evidence: "ev"}} })
	if err != nil {
		t.Fatal(err)
	}
	if len(fetches) != 1 {
		t.Fatalf("unfiltered caller fetched %d pages, want 1", len(fetches))
	}
	if fmt.Sprint(got["a"], got["b"]) != "[1 2 3] [4]" {
		t.Fatalf("got %v", got)
	}
}

// ScopeMaintenance is the one predicate the guard does not consult the
// database for; it admits every node and keeps the walk's order.
func TestGuardNodes_UnfilteredAdmitsAndPreservesOrder(t *testing.T) {
	got, err := guardNodes(context.Background(), nil, domain.ScopeMaintenance,
		[]string{"c", "a", "b"}, func(s string) []guardRef { return []guardRef{{evidence: s}} })
	if err != nil || fmt.Sprint(got) != "[c a b]" {
		t.Fatalf("got %v %v", got, err)
	}
}

// The closure reads its whole frontier in ONE LinksForRefs call per hop: a
// star of five aliases two hops deep is two reads, not six.
func TestIdentityClosure_OneFrontierReadPerHop(t *testing.T) {
	f := &countingLinkStore{fakeLinkStore: fakeLinkStore{byRef: map[string][]domain.Link{}}}
	hub := "entity:k/hub"
	for i := 0; i < 5; i++ {
		spoke := fmt.Sprintf("entity:k/s%d", i)
		l := aliasLink(fmt.Sprintf("star%d", i), hub, spoke, domain.RelationSameAs)
		f.byRef[hub] = append(f.byRef[hub], l)
		f.byRef[spoke] = append(f.byRef[spoke], l)
	}
	p := closurePlane(t, f, nil)
	got, err := p.identityClosure(context.Background(), "k/hub", 2, domain.ScopeMaintenance)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.members) != 5 {
		t.Fatalf("closure = %d members, want 5", len(got.members))
	}
	if f.frontierReads != 2 {
		t.Fatalf("closure made %d frontier reads, want 2 (one per hop)", f.frontierReads)
	}
}

type countingLinkStore struct {
	fakeLinkStore
	frontierReads int
}

func (c *countingLinkStore) LinksForRefs(ctx context.Context, ns string, refs []string, opts domain.LinkQuery) (map[string][]domain.Link, error) {
	c.frontierReads++
	return c.fakeLinkStore.LinksForRefs(ctx, ns, refs, opts)
}
