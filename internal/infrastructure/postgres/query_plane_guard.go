package postgres

// The node guard every graph walk of PgQueryPlane goes through (moved from
// query_plane.go, ADR-0149 P0; no logic change).

import (
	"context"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz/rowfence"
)

// ─── The node guard: one scope decision for every graph walk ─────────────────
//
// why, traverse and the identity closure each used to fence their nodes with a
// hand-rolled copy of the same evidenceTags/AllowsRow loop, and the copies
// drifted: one lane checked only the far end of a hop, so why(event:<hidden>)
// was an existence oracle (T-20261112-query-plane-why-never-fences-the-root-
// event-or). guardNodes is now the only loop. Every node a walk emits or stands
// on — a link, an event, a co-occurrence hop, an entity's minting evidence —
// passes through it, directly or through filterLinks/filterEvents, which are
// one-line wrappers over it.

// guardNodes keeps the nodes the scope may read, resolving every node's
// evidence in one batch (evidenceTags) and judging each with rowfence.FilterRows
// (TagPredicate.AllowsRow on every classification a node stands on) — the same
// test point/history/events apply in SQL (sqlfence.Evidence is CheckRow's
// mirror), owner fence and party scoping included (ADR-0121), never a second
// rendering of the policy (AGENTS §2.5).
//
// A node names the evidence it stands on, and is admitted only if it names at
// least one and the scope may read EVERY one (a hop between two events is a
// claim about both). Fail closed: an empty or unresolvable evidence id hides
// the node, for a plain Bypass too — only ScopeMaintenance (Unfiltered) admits
// everything, as everywhere else on this plane. Order is preserved.
func guardNodes[T any](ctx context.Context, p *PgQueryPlane, scope *domain.TagPredicate,
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
	// The row decision itself is the authz-owned rowfence (AGENTS §2.5, ENG-1492);
	// this walk only says what each node stands on.
	return rowfence.FilterRows(scope, nodes, func(n T) ([]rowfence.Classification, bool) {
		ev := evidenceOf(n)
		cs := make([]rowfence.Classification, 0, len(ev))
		for _, id := range ev {
			t, resolvable := tags[id]
			if !resolvable {
				return nil, false
			}
			cs = append(cs, rowfence.Classification{Tags: t.tags, Parties: t.parties})
		}
		return cs, true
	}), nil
}

// walkMaxPages bounds filter-before-limit paging in the walk shapes: a lane
// reads at most this many pages of `want` rows per key before it settles for
// what it has. The first page is exactly the read the walk always made, so a
// caller whose scope hides nothing never reads a second one; the bound only
// caps how far a narrow caller's lane digs past rows it may not see.
const walkMaxPages = 8

// keyedNode is one node of a frontier read, remembered with the key it was read
// for so a whole page can be guarded in one batch.
type keyedNode[T any] struct {
	key  string
	node T
}

// pageVisible is filter-before-limit for one lane of a walk: for every key it
// reads pages of `want` rows (fetch: one statement for all keys still short),
// guards them (guardNodes), and keeps going for a key until it holds `want`
// nodes the caller may read, its rows run out (a short page), or walkMaxPages
// pages were read. Without it a page the store cut BEFORE the scope was known
// gave a narrow caller a short or empty answer while visible rows existed.
func pageVisible[T any](ctx context.Context, p *PgQueryPlane, scope *domain.TagPredicate,
	keys []string, want int,
	fetch func(keys []string, offset, n int) (map[string][]T, error),
	evidenceOf func(T) []string) (map[string][]T, error) {
	out := map[string][]T{}
	if want <= 0 {
		return out, nil
	}
	seen := map[string]bool{}
	var pending []string
	for _, k := range keys {
		if !seen[k] {
			seen[k] = true
			pending = append(pending, k)
		}
	}
	for page := 0; page < walkMaxPages && len(pending) > 0; page++ {
		raw, err := fetch(pending, page*want, want)
		if err != nil {
			return nil, err
		}
		var batch []keyedNode[T]
		for _, k := range pending {
			for _, n := range raw[k] {
				batch = append(batch, keyedNode[T]{key: k, node: n})
			}
		}
		kept, err := guardNodes(ctx, p, scope, batch,
			func(x keyedNode[T]) []string { return evidenceOf(x.node) })
		if err != nil {
			return nil, err
		}
		for _, x := range kept {
			if len(out[x.key]) < want {
				out[x.key] = append(out[x.key], x.node)
			}
		}
		var still []string
		for _, k := range pending {
			if len(raw[k]) >= want && len(out[k]) < want {
				still = append(still, k)
			}
		}
		pending = still
	}
	return out, nil
}
