package kernel

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/memory"
)

// docVec is a VectorStore that can also store source documents, like
// postgres.PgVectorAdapter. It records what reached the raw SaveDocument.
type docVec struct {
	domain.VectorStore
	saved []domain.SourceDocument
}

func (d *docVec) SaveDocument(_ context.Context, doc domain.SourceDocument) ([]string, error) {
	d.saved = append(d.saved, doc)
	return doc.Tags, nil
}

// denyWrites refuses every classification, so a write that reaches the raw store
// proves the chokepoint was skipped.
type denyWrites struct{ domain.AllowAllAuthorizer }

func (denyWrites) ClassifyWrite(context.Context, domain.PrincipalRef, []string) ([]string, domain.AccessDecision) {
	return nil, domain.AccessDecision{Allowed: false, Reason: domain.ReasonNoPrincipal}
}

// T-20261113-memory-tier-stores-are-composed-outside: the ADR-0093 document-row write
// chokepoint is composed inside NewMemoryStack (AGENTS.md §2.1), wrapping the raw
// adapter with the stack's authorizer — never the raw adapter itself — and is absent
// only when the adapter cannot store documents.
func TestNewMemoryStack_ComposesTheEnforcingDocumentStore(t *testing.T) {
	cfg := config.DefaultConfig()

	raw := &docVec{}
	mem := NewMemoryStack(raw, nil, nil, cfg.Execution, denyWrites{}, cfg.Chunker, nil, FactSubstrate{})
	if mem.DocumentStore == nil {
		t.Fatal("a document-capable adapter must yield a DocumentStore")
	}
	if _, ok := mem.DocumentStore.(*memory.EnforcingDocumentStore); !ok {
		t.Fatalf("DocumentStore is %T, want the enforcing decorator, never the raw adapter", mem.DocumentStore)
	}
	if _, err := mem.DocumentStore.SaveDocument(context.Background(), domain.SourceDocument{ID: "d1", Tags: []string{"x"}}); err == nil {
		t.Fatal("the stack's authorizer was not consulted: a denied write succeeded")
	}
	if len(raw.saved) != 0 {
		t.Fatalf("a denied document write reached the raw adapter: %+v", raw.saved)
	}

	if none := NewMemoryStack(plainVec{}, nil, nil, cfg.Execution, nil, cfg.Chunker, nil, FactSubstrate{}); none.DocumentStore != nil {
		t.Fatalf("an adapter without SaveDocument must leave DocumentStore nil, got %T", none.DocumentStore)
	}
}
