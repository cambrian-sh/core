package memory

import (
	"context"
	"slices"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// A fact derived from a person's personal ingest inherits the owner:<id> stamp
// from its source chunk, so the owner fence (domain/owner_scope.go) governs the
// fact exactly as it governs the chunk — and, because the stamp is part of the
// classification, the fact's resolution key is owner-local: alice's facts never
// supersede, or are superseded by, anyone else's
// (T-20261106-owner-scope-tag-owner-principal-from-personal).
func TestBuildFacts_InheritsTheOwnerStamp(t *testing.T) {
	e := &FactExtractor{}
	chunk := &domain.Document{ID: "c1", Metadata: map[string]any{"tags": []any{"mail", "owner:alice"}}}
	pub := &domain.Document{ID: "c2", Metadata: map[string]any{"tags": []any{"mail"}}}
	j := factJob{ctx: context.Background(), documentID: "d1", evidenceID: "ev1", actor: "alice", chunks: []*domain.Document{chunk, pub}}
	window := []factPassage{{Chunk: 0, Text: "I have the flu"}, {Chunk: 1, Text: "I have the flu"}}
	facts := e.buildFacts(j, window, []extractedFact{
		{Subject: "user", Attribute: "diagnosis", Value: "flu", Passage: 0, Quote: "flu"},
		{Subject: "user", Attribute: "diagnosis", Value: "flu", Passage: 1, Quote: "flu"},
	})
	if len(facts) != 2 {
		t.Fatalf("facts = %+v", facts)
	}
	if !slices.Contains(facts[0].Tags, "owner:alice") {
		t.Fatalf("the derived fact dropped the owner stamp: %v", facts[0].Tags)
	}
	if facts[0].ScopeKey == facts[1].ScopeKey {
		t.Fatal("an owner-stamped fact shares a resolution scope with the unowned corpus")
	}
	if (&domain.TagPredicate{Owners: []string{"bob"}}).Allows(facts[0].Tags) {
		t.Fatal("bob can read a fact derived from alice's personal ingest")
	}
	if !(&domain.TagPredicate{Owners: []string{"alice"}}).Allows(facts[0].Tags) {
		t.Fatal("alice cannot read her own derived fact")
	}
}
