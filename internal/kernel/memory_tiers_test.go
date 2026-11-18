package kernel

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// ENG-1476: the tiers are reached only through typed accessors, and each accessor
// hands out the narrowest port. These are compile-time facts; the assertions below
// keep them from being widened silently (an accessor returning domain.VectorStore
// again would make the reader writable).
var (
	_ func(*MemoryStack) domain.VectorReader               = (*MemoryStack).Reader
	_ func(*MemoryStack) domain.VectorWriter               = (*MemoryStack).Writer
	_ func(*MemoryStack, domain.ADRRef) domain.VectorStore = (*MemoryStack).KernelArtifactStore
)

func TestMemoryStack_AccessorsHandOutTheTiers(t *testing.T) {
	cfg := config.DefaultConfig()
	raw := &docVec{}
	mem := NewMemoryStack(MemoryBackend{Vector: raw}, nil, nil, cfg.Execution, nil, cfg.Chunker, nil, FactSubstrate{}, RetrievalDeps{})

	if _, ok := mem.Reader().(*authz.EnforcingVectorStore); !ok {
		t.Errorf("Reader() is %T, want the tier-1 *authz.EnforcingVectorStore", mem.Reader())
	}
	if _, ok := mem.Writer().(*authz.EnforcingStoreWriter); !ok {
		t.Errorf("Writer() is %T, want the tier-2 *authz.EnforcingStoreWriter", mem.Writer())
	}
	// The reader cannot be turned into a writer by assertion: the type has no write side.
	if _, ok := mem.Reader().(domain.VectorWriter); ok {
		t.Error("Reader() satisfies domain.VectorWriter: the read tier must not write")
	}
	if _, ok := mem.Writer().(domain.VectorReader); ok {
		t.Error("Writer() satisfies domain.VectorReader: the write tier must not be a read path")
	}
	if got := mem.KernelArtifactStore("ADR-0044 D3"); got != domain.VectorStore(raw) {
		t.Errorf("KernelArtifactStore = %T, want the raw adapter", got)
	}
}

func TestMemoryStack_KernelArtifactStoreRequiresAnADR(t *testing.T) {
	cfg := config.DefaultConfig()
	mem := NewMemoryStack(MemoryBackend{Vector: &docVec{}}, nil, nil, cfg.Execution, nil, cfg.Chunker, nil, FactSubstrate{}, RetrievalDeps{})
	for _, reason := range []domain.ADRRef{"", "because", "adr-0044"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("KernelArtifactStore(%q) did not refuse a reason that names no ADR", reason)
				}
			}()
			mem.KernelArtifactStore(reason)
		}()
	}
}

// classifyAs replaces every write's tags with a fixed classification, so a stored
// row that carries it proves the write passed the tier-2 chokepoint.
type classifyAs struct {
	domain.AllowAllAuthorizer
	tags []string
}

func (c classifyAs) ClassifyWrite(context.Context, domain.PrincipalRef, []string) ([]string, domain.AccessDecision) {
	return c.tags, domain.AccessDecision{Allowed: true}
}

// ENG-1476 end to end on real Postgres, through the composition the boot path
// performs (NewMemoryBackend over a real PgVectorAdapter → NewMemoryStack): writes
// through Writer() and through the memory manager are classified; reads through
// Reader() fail closed without a predicate and hide what the predicate forbids.
//
// Requires PG_TEST_DSN pointing at a scratch database (the adapter auto-migrates it).
func TestPg_MemoryTiers_WritesClassifiedReadsEnforced(t *testing.T) {
	cfg := pgTestConfig(t)
	ctx := context.Background()
	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter: %v", err)
	}
	t.Cleanup(vec.Close)

	const classified = "eng1476-classified"
	mem := NewMemoryStack(NewMemoryBackend(vec), nil, nil, cfg.Execution, classifyAs{tags: []string{classified}},
		cfg.Chunker, nil, FactSubstrate{}, RetrievalDeps{})

	stamp := time.Now().UTC().Format("150405.000000")
	idWriter, idManager := "eng1476-writer-"+stamp, "eng1476-manager-"+stamp
	maint := domain.WithScope(ctx, domain.ScopeMaintenance)
	t.Cleanup(func() {
		_ = vec.Delete(maint, idWriter)
		_ = vec.Delete(maint, idManager)
	})
	vector := make([]float32, cfg.Embedder.Dimensions)
	vector[0] = 1
	doc := func(id string) *domain.Document {
		return &domain.Document{ID: id, DocumentType: domain.DocTypeMnemonicFact, Text: "eng1476 " + id,
			Embedding: domain.Embedding{Vector: vector}, ActivationStrength: 0.5,
			// A forged provenance stamp, not a classification request: a requested tag the
			// decision point's answer lacks would refuse the write (ENG-2115).
			Metadata: map[string]any{"tags": []string{"provenance:source=forged"}}}
	}
	wctx := domain.WithPrincipal(ctx, domain.AgentPrincipal("eng1476-agent"))

	// Writes: the authored tag is replaced by the decision point's classification on
	// both principal-serving write paths.
	if err := mem.Writer().Save(wctx, doc(idWriter)); err != nil {
		t.Fatalf("Writer().Save: %v", err)
	}
	if err := mem.Agent.Manager.Store.Save(wctx, doc(idManager)); err != nil {
		t.Fatalf("Manager.Store.Save: %v", err)
	}
	for _, id := range []string{idWriter, idManager} {
		got, err := vec.GetByID(maint, id) // raw adapter: what was persisted
		if err != nil || got == nil {
			t.Fatalf("raw GetByID %s: %v, %v", id, got, err)
		}
		if tags := authz.DocTags(got); !slices.Equal(tags, []string{classified}) {
			t.Errorf("%s persisted with tags %v, want the classification [%s] (tier 2 skipped?)", id, tags, classified)
		}
	}

	// Reads through the tier-1 reader.
	if _, err := mem.Reader().GetByID(ctx, idWriter); !errors.Is(err, authz.ErrScopeMissing) {
		t.Errorf("Reader().GetByID with no predicate: got %v, want ErrScopeMissing", err)
	}
	if _, err := mem.Reader().Search(ctx, vector, domain.SearchOptions{TopK: 5}); !errors.Is(err, authz.ErrScopeMissing) {
		t.Errorf("Reader().Search with no predicate: got %v, want ErrScopeMissing", err)
	}
	forbid := &domain.TagPredicate{ForbiddenTags: []string{classified}}
	if got, err := mem.Reader().GetByID(domain.WithScope(ctx, forbid), idWriter); err != nil || got != nil {
		t.Errorf("Reader().GetByID under a forbidding predicate = %v, %v; want absent", got, err)
	}
	allow := &domain.TagPredicate{RequiredTags: []string{classified}}
	if got, err := mem.Reader().GetByID(domain.WithScope(ctx, allow), idWriter); err != nil || got == nil {
		t.Errorf("Reader().GetByID under a permitting predicate = %v, %v; want the row", got, err)
	}
	// The memory manager's reads go through the same tier-1 reader.
	if got, err := mem.Agent.Manager.GetByID(domain.WithScope(ctx, forbid), idManager); err != nil || got != nil {
		t.Errorf("Manager.GetByID under a forbidding predicate = %v, %v; want absent", got, err)
	}
	if _, err := mem.Agent.Manager.GetByID(ctx, idManager); !errors.Is(err, authz.ErrScopeMissing) {
		t.Errorf("Manager.GetByID with no predicate: got %v, want ErrScopeMissing", err)
	}
}
