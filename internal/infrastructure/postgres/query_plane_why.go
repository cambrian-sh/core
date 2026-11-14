package postgres

// The why shape of PgQueryPlane: stored lineage and its fenced hint (moved from
// query_plane.go, ADR-0149 P0; no logic change).

import (
	"context"
	"fmt"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// whyRef promotes a bare entity id to a typed ref and leaves an already-typed one
// alone. Both forms are accepted because the two callers differ: a console walks
// back from an event it is looking at, an agent from the entity it was asked
// about, and making either prefix by hand is a footgun with no upside.
func whyRef(id string) string {
	for _, prefix := range []string{
		domain.RefPrefixEntity, domain.RefPrefixEvent,
		domain.RefPrefixDecision, domain.RefPrefixEvidence,
	} {
		if strings.HasPrefix(id, prefix) {
			return id
		}
	}
	return domain.EntityRef(id)
}

// why walks lineage BACKWARD from a typed ref, hop by hop, labelling every hop
// with the mechanism that produced it.
//
// Two sources feed the walk and they are deliberately distinguishable on the
// wire. STORED lineage links (stored=true) were asserted by somebody, so they
// carry state, producer and evidence. LIVE shared-object hops (stored=false) are
// computed at read time from event_roles — two occurrences sharing a participant
// — and nobody asserted them; emitting those with a state would claim a review
// that never happened.
//
// ENFORCED per row: a link is filtered on its own evidence, an event on its
// event's evidence, and the walk DOES NOT CONTINUE through a row the caller
// cannot read (the traverse precedent — a forbidden edge is not a bridge, or
// reachability leaks what the rows themselves would not).
//
// Candidate lineage is withheld from non-bypass callers for entityOp's reason: an
// unreviewed proposal must not become a step in somebody's causal story.
func (p *PgQueryPlane) why(ctx context.Context, q domain.KnowledgeQuery,
	scope *domain.TagPredicate, limit int) ([]domain.QueryRow, error) {
	depth := q.Hops
	if depth <= 0 {
		depth = domain.ClosureDefaultDepth
	}
	if depth > domain.ClosureMaxDepth {
		return nil, domain.ClosureDepthRefusal(depth)
	}
	root := whyRef(q.EntityID)
	seen := map[string]bool{root: true}
	// Related rows anchored on an ENTITY are deduplicated by the PAIR rather
	// than by the target: one entity binds many events to each other, and a
	// target-keyed check would emit the first pair and swallow every other
	// relationship the same entity holds together.
	seenPair := map[string]bool{}
	seenHub := map[string]bool{}
	frontier := []string{root}
	var out []domain.QueryRow
	cp := correlationParamsFor(q)
	rootExpanded := false

	state := domain.LinkStateConfirmed
	if seesReviewCandidates(ctx, scope) { // ENG-1536: operators only
		state = ""
	}
	for hop := 1; hop <= depth && len(frontier) > 0; hop++ {
		// The walk STANDS on every frontier ref before it expands it, and
		// standing on an event is reading it: its lineage and its related rows
		// (which carry its own role) are claims about that event. So each event
		// ref is decided on its OWN evidence first — the root as much as one
		// reached through a readable link at hop >= 2 — and one the caller may
		// not read (or that ADR-0147 retired) is not expanded at all. A hidden
		// event therefore answers exactly as an absent id does (ADR-0128 D12).
		readable, err := p.readableEventRefs(ctx, scope, frontier)
		if err != nil {
			return nil, err
		}
		var expand, eventRefs, entityRefs []string
		for _, ref := range frontier {
			switch {
			case strings.HasPrefix(ref, domain.RefPrefixEvent):
				if readable != nil && !readable[ref] {
					continue
				}
				eventRefs = append(eventRefs, ref)
			case strings.HasPrefix(ref, domain.RefPrefixEntity):
				entityRefs = append(entityRefs, ref)
			}
			expand = append(expand, ref)
		}
		if hop == 1 && len(expand) == 1 {
			rootExpanded = true
		}
		// ONE read per lane per hop for the whole frontier, each node guarded
		// (guardNodes) and paged until every ref has `limit` rows the caller may
		// read (filter before limit). Rows are emitted in frontier order, stored
		// links before related rows.
		var linksBy map[string][]domain.Link
		if p.links != nil {
			linksBy, err = pageVisible(ctx, p, scope, expand, limit,
				func(keys []string, offset, n int) (map[string][]domain.Link, error) {
					return p.links.LinksForRefs(ctx, defaultNamespace, keys, domain.LinkQuery{
						Family: domain.LinkFamilyLineage, State: state,
						IncludeIncoming: true, Limit: n, Offset: offset,
					})
				}, linkEvidence)
			if err != nil {
				return nil, err
			}
		}
		// ADR-0148 D1/D2: related rows only when the caller asked for them.
		var relBy, pairBy map[string][]relatedRow
		var hubsBy map[string][]hubFlag
		if q.IncludeCorrelation {
			if relBy, hubsBy, err = p.relatedFromEvents(ctx, scope, eventRefs, cp); err != nil {
				return nil, err
			}
			pairHubs := map[string][]hubFlag{}
			if pairBy, pairHubs, err = p.relatedFromEntities(ctx, scope, entityRefs, cp, limit); err != nil {
				return nil, err
			}
			for k, v := range pairHubs {
				if hubsBy == nil {
					hubsBy = map[string][]hubFlag{}
				}
				hubsBy[k] = append(hubsBy[k], v...)
			}
		}

		var next []string
		for _, ref := range expand {
			for _, l := range linksBy[ref] {
				other := l.ToRef
				if other == ref {
					other = l.FromRef
				}
				if other == ref || seen[other] {
					continue
				}
				seen[other] = true
				if len(out) >= limit {
					return out, nil
				}
				row := linkRow(l, "hop")
				row["hop"] = hop
				row["stored"] = true
				out = append(out, row)
				next = append(next, other)
			}
			switch {
			case strings.HasPrefix(ref, domain.RefPrefixEvent):
				for _, h := range relBy[ref] {
					if seen[h.toRef] {
						continue
					}
					seen[h.toRef] = true
					if len(out) >= limit {
						return out, nil
					}
					out = append(out, h.row(hop))
					next = append(next, h.toRef)
				}
			case strings.HasPrefix(ref, domain.RefPrefixEntity):
				// why(entity:X) with include_correlation: the pairs of events X
				// took part in (D-W5-6). The rows run between the EVENTS; X is
				// what they share and rides along as shared_entity_id.
				for _, h := range pairBy[ref] {
					key := h.fromRef + "\x00" + h.toRef
					if seenPair[key] {
						continue
					}
					seenPair[key] = true
					if len(out) >= limit {
						return out, nil
					}
					out = append(out, h.row(hop))
					// BOTH ends enter the walk: neither event has had its own
					// lineage or related rows looked at yet.
					for _, end := range []string{h.fromRef, h.toRef} {
						if !seen[end] {
							seen[end] = true
							next = append(next, end)
						}
					}
				}
			}
			// D5: a participant over the hub threshold was not followed; say
			// so once, where a reader can see why an obvious relation is absent.
			for _, hb := range hubsBy[ref] {
				if seenHub[hb.entity] {
					continue
				}
				seenHub[hb.entity] = true
				if len(out) >= limit {
					return out, nil
				}
				out = append(out, domain.QueryRow{
					"row": "flagged", "ref": domain.EntityRef(hb.entity), "via_ref": ref,
					"guard": "why_hub_participant", "limit": domain.CorrelationHubThreshold,
					"events": hb.events,
				})
			}
		}
		frontier = next
	}
	if len(out) == 0 && !q.IncludeCorrelation && rootExpanded {
		return p.whyHint(ctx, scope, root, cp, limit)
	}
	return out, nil
}

// whyHint is ADR-0148's answer to an empty flag-off walk: if the root has
// related events the caller could see with include_correlation, say how many
// rather than answer with silence (owner decision 2026-11-12). It is computed by
// the very lane the flag would run, at hop 1, so N is exactly the number of
// related rows (event root) or distinct related events (entity root) the caller
// would get — every one of them caller-visible, fenced on its own evidence. The
// root has already passed readableEventRefs, so a hidden root never reaches
// here and still answers as an absent id.
//
// Wire shape: one row {row:"hint", hint:"related_available", related_count:N,
// flag:"include_correlation", text:"no stored lineage; N related events
// available with include_correlation"}. A row rather than a new response field:
// the rows are the contract's extensible part (ADR-0148 §4), and a client that
// does not know the shape still renders it as a row.
func (p *PgQueryPlane) whyHint(ctx context.Context, scope *domain.TagPredicate,
	root string, cp correlationParams, limit int) ([]domain.QueryRow, error) {
	n := 0
	switch {
	case strings.HasPrefix(root, domain.RefPrefixEvent):
		rel, _, err := p.relatedFromEvents(ctx, scope, []string{root}, cp)
		if err != nil {
			return nil, err
		}
		n = len(rel[root])
	case strings.HasPrefix(root, domain.RefPrefixEntity):
		pairs, _, err := p.relatedFromEntities(ctx, scope, []string{root}, cp, limit)
		if err != nil {
			return nil, err
		}
		distinct := map[string]bool{}
		for _, h := range pairs[root] {
			distinct[h.fromRef], distinct[h.toRef] = true, true
		}
		n = len(distinct)
	}
	if n == 0 {
		return nil, nil
	}
	return []domain.QueryRow{{
		"row": "hint", "hint": "related_available", "related_count": n,
		"flag": "include_correlation",
		"text": fmt.Sprintf("no stored lineage; %d related events available with include_correlation", n),
	}}, nil
}
