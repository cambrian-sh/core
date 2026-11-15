package app

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// ENG-494 / ENG-449 (owner decision 2026-11-15): a plugin's memory ingests and its
// principal-less knowledge writes run as its own deterministic source principal.

type recordingProcessor struct {
	principal domain.PrincipalRef
	doc       domain.ExternalDocument
}

func (r *recordingProcessor) ProcessSync(ctx context.Context, doc domain.ExternalDocument) (string, error) {
	r.principal, r.doc = domain.PrincipalFromContext(ctx), doc
	return "source_doc:" + doc.SourceURI, nil
}

type recordingKnowledge struct {
	domain.KnowledgeStore
	principal domain.PrincipalRef
}

func (r *recordingKnowledge) PutItem(ctx context.Context, it domain.KnowledgeItem) (domain.KnowledgeItemID, bool, error) {
	r.principal = domain.PrincipalFromContext(ctx)
	return it.ID, true, nil
}

func TestScopePluginServices_IngestRunsAsThePluginSource(t *testing.T) {
	proc := &recordingProcessor{}
	base := &kernelMemoryIngestor{connector: reactiveIngestConnector}
	svc := scopePluginServices("telegram", KernelServices{Ingestor: base})
	// The processor is late-bound (filled after plugins Build): the scoped ingestor
	// must see it through the shared base.
	base.processor = proc

	if _, err := svc.Ingestor.Ingest(context.Background(), domain.ExternalDocument{
		SourceURI: "tg://1", SourceType: "bot1", Body: "hello", Author: "Ayşe",
	}); err != nil {
		t.Fatal(err)
	}
	if proc.principal.String() != "source:telegram:bot1" {
		t.Fatalf("principal %v, want source:telegram:bot1", proc.principal)
	}
	if proc.doc.Author != "source:telegram:bot1" || proc.doc.AuthorLabel != "Ayşe" {
		t.Fatalf("author %q label %q: the writer of record is the source, the plugin's author a label", proc.doc.Author, proc.doc.AuthorLabel)
	}

	// The unscoped ingestor (the reactive signal receiver's) is its own source too —
	// never the shared SystemPrincipal it used to be.
	if _, err := base.Ingest(context.Background(), domain.ExternalDocument{SourceURI: "r://1", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	if proc.principal.String() != "source:reactive:ingest" || proc.principal.Kind == domain.PrincipalSystem {
		t.Fatalf("reactive ingest principal %v, want source:reactive:ingest", proc.principal)
	}
}

func TestScopePluginServices_KnowledgeFallsBackToThePluginSource(t *testing.T) {
	inner := &recordingKnowledge{}
	svc := scopePluginServices("ingress-studio", KernelServices{Knowledge: inner})

	if _, _, err := svc.Knowledge.PutItem(context.Background(), domain.KnowledgeItem{ID: "k1", NamespaceID: "studio:x"}); err != nil {
		t.Fatal(err)
	}
	if inner.principal.String() != "source:ingress-studio:studio:x" {
		t.Fatalf("no principal on ctx: got %v, want source:ingress-studio:studio:x", inner.principal)
	}
	// A principal already on ctx (the outbox consumer's evidence source) is kept.
	ev := domain.SourcePrincipal("slack", "ws")
	if _, _, err := svc.Knowledge.PutItem(domain.WithPrincipal(context.Background(), ev), domain.KnowledgeItem{ID: "k2"}); err != nil {
		t.Fatal(err)
	}
	if inner.principal != ev {
		t.Fatalf("principal on ctx replaced: got %v, want %v", inner.principal, ev)
	}
}

// The composed seam refuses the kernel-reserved kind and classifies every other item.
type denyAuthorizer struct{ domain.AllowAllAuthorizer }

func (denyAuthorizer) ClassifyWrite(_ context.Context, p domain.PrincipalRef, _ []string) ([]string, domain.AccessDecision) {
	return nil, domain.AccessDecision{Principal: p, Reason: domain.ReasonNoPrincipal}
}

func TestNewPluginKnowledgeSeam_ClassifiesWrites(t *testing.T) {
	inner := &recordingKnowledge{}
	seam := NewPluginKnowledgeSeam(inner, nil, denyAuthorizer{})
	_, _, err := seam.PutItem(context.Background(), domain.KnowledgeItem{ID: "k", Kind: "policy_statement"})
	if !errors.Is(err, authz.ErrWriteDenied) || inner.principal != (domain.PrincipalRef{}) {
		t.Fatalf("a write the decision point denies must not reach the store: err=%v", err)
	}
	if _, _, err := seam.PutItem(context.Background(), domain.KnowledgeItem{ID: "f", Kind: domain.KindMemoryFact}); !errors.Is(err, authz.ErrReservedKind) {
		t.Fatalf("memory_fact through the plugin seam must be refused: %v", err)
	}
}
