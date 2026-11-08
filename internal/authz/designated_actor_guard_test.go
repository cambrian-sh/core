package authz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

type recordingKnowledge struct{ puts []domain.KnowledgeItem }

func (r *recordingKnowledge) PutItem(_ context.Context, it domain.KnowledgeItem) (domain.KnowledgeItemID, bool, error) {
	r.puts = append(r.puts, it)
	return "ki", true, nil
}
func (r *recordingKnowledge) GetItem(context.Context, domain.KnowledgeItemID) (*domain.KnowledgeItem, error) {
	return &domain.KnowledgeItem{}, nil
}
func (r *recordingKnowledge) CurrentResolutions(context.Context, string, string, string) ([]domain.Resolution, error) {
	return nil, nil
}
func (r *recordingKnowledge) EraseItems(context.Context, string, string, domain.KnowledgeErasure) (int, error) {
	return 0, nil
}

// The plugin seam cannot forge a designated assertion: neither a designated
// principal's word nor the kernel's commit marker. Candidates and sources pass.
func TestDesignatedActorGuard_RefusesForgedAssertionsOnly(t *testing.T) {
	reg, err := domain.NewKindRegistry([]domain.KindSpec{{
		Kind: "case", Policy: domain.ResolutionPolicyDesignatedActor,
		Designated: &domain.DesignatedActors{Principals: []string{"user:boss"}, Sources: []string{"erp"}, ApprovedViaCommit: true},
		Predicates: map[string]domain.ValueSpec{"v": {Type: "text"}},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	inner := &recordingKnowledge{}
	g := NewReservedKindGuard(NewDesignatedActorGuard(inner, reg), domain.KindMemoryFact)
	item := func(by, ref string) domain.KnowledgeItem {
		return domain.KnowledgeItem{Kind: "case", EntityID: "c1", AssertedBy: by, SourceRef: ref, AssertedAt: time.Now(),
			Values: []domain.StatementValue{{Predicate: "v", Type: "text", Text: "x"}}}
	}
	ctx := context.Background()
	for name, it := range map[string]domain.KnowledgeItem{
		"designated principal": item("user:boss", "m1"),
		"commit marker":        item("detector", domain.KnowledgeCommitSourceRefPrefix+"ap1"),
		"commit marker, other kind": {Kind: "anything", EntityID: "e", AssertedBy: "p",
			SourceRef: domain.KnowledgeCommitSourceRefPrefix + "x", AssertedAt: time.Now()},
	} {
		if _, _, err := g.PutItem(ctx, it); !errors.Is(err, ErrForgedAssertion) {
			t.Errorf("%s: want ErrForgedAssertion, got %v", name, err)
		}
	}
	for name, it := range map[string]domain.KnowledgeItem{
		"candidate by a detector": item("detector", "d1"),
		"source of record":        item("erp", "po-1"),
	} {
		if _, _, err := g.PutItem(ctx, it); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
	if len(inner.puts) != 2 {
		t.Fatalf("inner saw %d writes, want 2", len(inner.puts))
	}
	// memory_fact stays fenced by the outer guard.
	if _, _, err := g.PutItem(ctx, domain.KnowledgeItem{Kind: domain.KindMemoryFact}); !errors.Is(err, ErrReservedKind) {
		t.Fatalf("memory_fact: %v", err)
	}
}
