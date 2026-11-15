// Package rowfence is the ONE in-memory application of the access predicate
// (domain.TagPredicate) to rows a store has already read (AGENTS.md §2.5: row
// filtering belongs in internal/authz and nowhere else; ENG-1492).
//
// It is the in-memory twin of sqlfence. sqlfence renders the predicate into the
// SQL a read sends; rowfence judges rows after the read, for the shapes that
// cannot push the test down — a graph walk whose nodes stand on evidence read in
// a second batch, a by-id inspection of one evidence row. Both defer the decision
// itself to TagPredicate.CheckRow (owner fence, Bypass, required / forbidden /
// any-of terms, party-scoped tags, own-rows), so neither restates the policy.
//
// A leaf package, importing only domain, on purpose: the Postgres adapter calls
// it, and internal/authz holds the enforcing decorators that wrap the adapter's
// ports — the adapter importing its own decorator package would invert that
// edge. sqlfence is a leaf for the same reason.
package rowfence

import "github.com/cambrian-sh/core/domain"

// Classification is one classification a row stands on: its tags and, for a
// substrate row, its parties (ADR-0121). A row with no parties passes nil, and a
// party-scoped tag it carries then denies (TagPredicate.CheckRow).
type Classification struct {
	Tags    []string
	Parties []string
}

// Readable reports whether scope may read a row carrying c. It is
// TagPredicate.AllowsRow, named here so every row decision outside the
// decorators goes through this package. A nil scope reads nothing (fail closed).
func Readable(scope *domain.TagPredicate, c Classification) bool {
	return scope.AllowsRow(c.Tags, c.Parties)
}

// FilterRows keeps the rows scope may read, preserving order.
//
// classOf is the classification accessor: it returns every classification a row
// stands on, and resolved=false when any of them could not be found. A row is
// admitted only if it stands on at least one classification, all of them
// resolved, and scope reads EVERY one (a row derived from several sources is
// never easier to read than the most restrictive of them). Fail closed: a row
// with no classification, or an unresolvable one, is hidden — for a plain
// Bypass too. Only ScopeMaintenance (TagPredicate.Unfiltered) admits every row,
// and classOf is then not called.
//
// A filtered result is a new slice; rows itself is returned unchanged for an
// unfiltered scope or an empty input.
func FilterRows[T any](scope *domain.TagPredicate, rows []T, classOf func(T) ([]Classification, bool)) []T {
	if scope.Unfiltered() || len(rows) == 0 {
		return rows
	}
	kept := rows[:0:0]
	for _, r := range rows {
		cs, resolved := classOf(r)
		if !resolved || len(cs) == 0 {
			continue
		}
		ok := true
		for _, c := range cs {
			if !Readable(scope, c) {
				ok = false
				break
			}
		}
		if ok {
			kept = append(kept, r)
		}
	}
	return kept
}
