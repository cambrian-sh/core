package memory

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// ENG-1535 end to end: the nightly loner decay (Agent.decayLoner) deletes a
// stale chunk through the composed chunk retirer (postgres.PgDocumentLifecycle,
// as app/boot_memory.go wires it), so the memory_fact item keyed into the chunk
// goes with it instead of outliving it.
func TestPg_Decay_RetiresTheFactItemsKeyedIntoADecayedChunk(t *testing.T) {
	cfgAt := factLaneScratchDB(t)
	ctx := context.Background()
	p, err := postgres.NewPgVectorAdapter(ctx, cfgAt(1024))
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer p.Close()
	reg, err := domain.NewKindRegistry([]domain.KindSpec{domain.MemoryFactKindSpec()},
		[]domain.ResolutionAuthority{domain.ValidTimeSupersessionAuthority{}})
	if err != nil {
		t.Fatal(err)
	}
	ks := postgres.NewPgKnowledgeStore(p.Pool(), reg)
	lc := postgres.NewPgDocumentLifecycle(ks)

	v := make([]float32, 1024)
	v[0] = 1
	stale := domain.Document{ID: "eng1535-stale", Text: "the user owns a boat", DocumentType: domain.DocTypeMnemonicFact,
		ActivationStrength: 0.1, Embedding: domain.Embedding{Vector: v}}
	if err := p.Save(ctx, &stale); err != nil {
		t.Fatal(err)
	}
	f := domain.Fact{Subject: "user", Attribute: "owns", Value: "boat", SubjectKey: "user", AttributeKey: "owns",
		ValueNorm: domain.NormalizeFactText("boat"), AssertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Actor: "alice", SourceChunkID: stale.ID, Quote: "boat"}
	if _, _, err := ks.PutItem(ctx, f.ToItem("default")); err != nil {
		t.Fatal(err)
	}
	facts := func() int {
		var n int
		if err := p.Pool().QueryRow(ctx, `SELECT count(*) FROM knowledge_items ki JOIN statement_values sv ON sv.item_id = ki.id
			AND sv.predicate = 'source_chunk' WHERE sv.value_text = $1`, stale.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if facts() != 1 {
		t.Fatal("seed: the fact item is missing")
	}

	a := NewAgent(nil, 0)
	a.Chunks = lc
	var deleted int
	a.decayLoner(ctx, stale, map[string]bool{}, false, &deleted)
	if deleted != 1 {
		t.Fatalf("decay deleted %d, want 1", deleted)
	}
	got, err := p.GetByID(ctx, stale.ID)
	if err != nil || got != nil {
		t.Fatalf("the decayed chunk is still there: %+v %v", got, err)
	}
	if facts() != 0 {
		t.Fatal("the fact item keyed into the decayed chunk outlived it")
	}
}
