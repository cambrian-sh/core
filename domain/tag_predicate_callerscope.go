package domain

// Turning a resolved read predicate back into a caller_scope term (BRAIN-01).
//
// Session.CallerScope is a TagSet — one required set, one OR-set, one forbidden
// set. A resolved TagPredicate is richer: its AnyOfClauses are in CONJUNCTIVE
// NORMAL FORM, so it can express "(a or b) AND (c or d)", which a single OR-set
// cannot.
//
// That difference is the whole reason this function returns a bool. Flattening
// two AND-ed OR-sets into one OR-set WIDENS the term — it would admit a document
// carrying only `a`, which the predicate refuses — and widening a caller's scope
// while persisting it as "what this caller may see" is the failure direction that
// matters. So an unrepresentable predicate is REFUSED rather than approximated.

// TagSetFromPredicate converts a resolved read predicate into a caller_scope
// term. ok is false when the predicate cannot be represented without widening it.
//
// A nil predicate is "no read authorized at all", which is not the same as "no
// constraint" — it is refused rather than flattened to an empty, unrestricted set.
func TagSetFromPredicate(p *TagPredicate) (TagSet, bool) {
	if p == nil {
		return TagSet{}, false
	}
	// Bypass is the unscoped/kernel case: no constraint, faithfully representable.
	if p.Bypass {
		return TagSet{}, true
	}
	if len(p.AnyOfClauses) > 1 {
		// CNF with more than one clause. Representable only by widening, so no.
		return TagSet{}, false
	}
	out := TagSet{
		RequiredTags:  append([]string(nil), p.RequiredTags...),
		ForbiddenTags: append([]string(nil), p.ForbiddenTags...),
	}
	if len(p.AnyOfClauses) == 1 {
		out.AnyOfTags = append([]string(nil), p.AnyOfClauses[0]...)
	}
	return out, true
}
