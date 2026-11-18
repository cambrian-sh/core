package authz

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// recordingSubstrate records the item that reached the raw store.
type recordingSubstrate struct {
	domain.FactSubstrateStore
	got []domain.KnowledgeItem
}

func (r *recordingSubstrate) PutItem(_ context.Context, it domain.KnowledgeItem) (domain.KnowledgeItemID, bool, error) {
	r.got = append(r.got, it)
	return it.ID, true, nil
}

type knowledgeStoreStub struct{ domain.KnowledgeStore }

func (knowledgeStoreStub) PutItem(context.Context, domain.KnowledgeItem) (domain.KnowledgeItemID, bool, error) {
	return "x", true, nil
}
func (knowledgeStoreStub) GetItem(context.Context, domain.KnowledgeItemID) (*domain.KnowledgeItem, error) {
	return &domain.KnowledgeItem{Kind: domain.KindMemoryFact}, nil
}
func (knowledgeStoreStub) CurrentResolutions(context.Context, string, string, string) ([]domain.Resolution, error) {
	return nil, nil
}

// classifyAs replaces every hint with fixed tags; deny refuses.
type classifyAs struct {
	domain.AllowAllAuthorizer
	tags []string
	deny bool
}

func (c classifyAs) ClassifyWrite(context.Context, domain.PrincipalRef, []string) ([]string, domain.AccessDecision) {
	if c.deny {
		return nil, domain.AccessDecision{Allowed: false, Reason: domain.ReasonNoPrincipal}
	}
	return c.tags, domain.AccessDecision{Allowed: true}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// ADR-0136 A1.6: the writer classifies each item and re-keys a memory_fact item's
// scope-derived identity on the FINAL classification, never the hint.
func TestEnforcingKnowledgeWriter_ClassifiesAndRekeys(t *testing.T) {
	raw := &recordingSubstrate{}
	w := NewEnforcingKnowledgeWriter(raw, classifyAs{tags: []string{"final"}}, quiet())
	f := domain.Fact{Subject: "user", Attribute: "home_city", Value: "Paris", SubjectKey: "user", AttributeKey: "home_city",
		ValueNorm: "paris", AssertedAt: time.Now(), SourceChunkID: "c"} // no requested tags: an unhonoured request is refused (ENG-2115)
	stored, inserted, err := w.PutClassifiedItem(context.Background(), f.ToItem("default"))
	if err != nil || !inserted {
		t.Fatal(err)
	}
	want := domain.FactEntityID(domain.FactScopeKey([]string{"final"}), "user", "home_city")
	if len(raw.got) != 1 || raw.got[0].EntityID != want || stored.EntityID != want || raw.got[0].Classification[0] != "final" {
		t.Fatalf("stored %+v, want entity %s classified final", raw.got, want)
	}
	if _, _, err := NewEnforcingKnowledgeWriter(raw, classifyAs{deny: true}, quiet()).PutClassifiedItem(context.Background(), f.ToItem("default")); !errors.Is(err, ErrWriteDenied) {
		t.Fatalf("denied write: %v", err)
	}
}

// The plugin seam cannot reach memory_fact through the raw store.
func TestReservedKindGuard_FencesMemoryFact(t *testing.T) {
	g := NewReservedKindGuard(knowledgeStoreStub{}, domain.KindMemoryFact)
	ctx := context.Background()
	if _, _, err := g.PutItem(ctx, domain.KnowledgeItem{Kind: domain.KindMemoryFact}); !errors.Is(err, ErrReservedKind) {
		t.Fatalf("put: %v", err)
	}
	if _, err := g.GetItem(ctx, "x"); !errors.Is(err, ErrReservedKind) {
		t.Fatalf("get: %v", err)
	}
	if _, err := g.CurrentResolutions(ctx, "", domain.KindMemoryFact, ""); !errors.Is(err, ErrReservedKind) {
		t.Fatalf("current: %v", err)
	}
	if _, err := g.EraseItems(ctx, "", domain.KindMemoryFact, domain.KnowledgeErasure{}); !errors.Is(err, ErrReservedKind) {
		t.Fatalf("erase: %v", err)
	}
	if _, _, err := g.PutItem(ctx, domain.KnowledgeItem{Kind: "commitment"}); err != nil {
		t.Fatalf("other kinds pass: %v", err)
	}
}
