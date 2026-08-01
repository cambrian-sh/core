package domain

import (
	"testing"
	"time"
)

// The order-independence gate (memo §17): evidence ingested in different
// orders must yield the same final resolution. ResolveLatestAssertion is the
// one function every store derives resolutions through, so the gate is proved
// here, over every permutation, rather than sampled.

func item(entity, actor, ref string, at time.Time, neg bool) KnowledgeItem {
	return KnowledgeItem{
		Kind: "commitment", EntityID: entity, AssertedBy: actor,
		SourceRef: ref, AssertedAt: at, Negation: neg,
	}
}

func permutations(items []KnowledgeItem) [][]KnowledgeItem {
	if len(items) <= 1 {
		return [][]KnowledgeItem{append([]KnowledgeItem(nil), items...)}
	}
	var out [][]KnowledgeItem
	for i := range items {
		rest := make([]KnowledgeItem, 0, len(items)-1)
		rest = append(rest, items[:i]...)
		rest = append(rest, items[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]KnowledgeItem{items[i]}, p...))
		}
	}
	return out
}

func TestResolveLatestAssertion_OrderIndependent(t *testing.T) {
	t0 := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	items := []KnowledgeItem{
		item("po/1", "sofia", "m1", t0, false),
		item("po/1", "sofia", "m2", t0.Add(time.Hour), false),
		item("po/1", "sofia", "m3", t0.Add(time.Hour), false), // tie on time with m2
		item("po/1", "sofia", "m4", t0.Add(30*time.Minute), true),
	}
	wantWinner, wantReason := ResolveLatestAssertion(items)
	if wantWinner == nil || wantWinner.SourceRef != "m3" || wantReason != ReasonLatestAssertion {
		t.Fatalf("reference resolution unexpected: %+v %q", wantWinner, wantReason)
	}
	for _, perm := range permutations(items) {
		got, reason := ResolveLatestAssertion(perm)
		if got == nil || got.SourceRef != wantWinner.SourceRef || reason != wantReason {
			t.Fatalf("resolution depends on arrival order: perm produced %+v %q", got, reason)
		}
	}
}

func TestResolveLatestAssertion_LatestNegationMeansNoBelief(t *testing.T) {
	t0 := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	items := []KnowledgeItem{
		item("po/1", "sofia", "m1", t0, false),
		item("po/1", "sofia", "", t0.Add(time.Hour), true), // dateless retraction, no ref
	}
	for _, perm := range permutations(items) {
		got, reason := ResolveLatestAssertion(perm)
		if got != nil || reason != ReasonNegated {
			t.Fatalf("latest negation must yield no belief; got %+v %q", got, reason)
		}
	}
}

func TestResolveLatestAssertion_SimultaneousNegationLosesToAssertion(t *testing.T) {
	t0 := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	items := []KnowledgeItem{
		item("po/1", "sofia", "", t0, true),
		item("po/1", "sofia", "", t0, false),
	}
	for _, perm := range permutations(items) {
		got, _ := ResolveLatestAssertion(perm)
		if got == nil {
			t.Fatal("the simultaneous assertion must win over the negation, deterministically")
		}
	}
}

func TestResolveLatestAssertion_EmptySet(t *testing.T) {
	if got, reason := ResolveLatestAssertion(nil); got != nil || reason != "" {
		t.Fatalf("empty set must resolve to nothing, got %+v %q", got, reason)
	}
}

// Contradiction coexistence (memo §8): two actors asserting different values
// about the same entity are two separate resolution keys — resolving one must
// never involve, exclude or reject the other. The item layer itself imposes no
// uniqueness at all; this pins the per-actor keying assumption the projection
// relies on.
func TestResolveLatestAssertion_PerActorKeysKeepContradictionAlive(t *testing.T) {
	t0 := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	slack := item("po/1", "slack", "s1", t0, false)
	erp := item("po/1", "erp", "e1", t0.Add(time.Minute), false)

	gotSlack, _ := ResolveLatestAssertion([]KnowledgeItem{slack})
	gotERP, _ := ResolveLatestAssertion([]KnowledgeItem{erp})
	if gotSlack == nil || gotERP == nil {
		t.Fatal("both sides of a contradiction must resolve independently")
	}
	if gotSlack.SourceRef == gotERP.SourceRef {
		t.Fatal("the two actors' beliefs collapsed onto one item")
	}
}
