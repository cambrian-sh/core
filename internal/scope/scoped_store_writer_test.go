package scope_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/scope"
)

// recordingStore captures the documents passed to Save.
type recordingStore struct {
	fakeStore
	saved []*domain.Document
}

func (r *recordingStore) Save(_ context.Context, doc *domain.Document) error {
	r.saved = append(r.saved, doc)
	return nil
}

func tagsOf(doc *domain.Document) []string {
	if v, ok := doc.Metadata["tags"].([]string); ok {
		return v
	}
	return nil
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// writerCtx seeds a WriterScope with the agent's operator-configured DefaultWriteTags.
func writerCtx(id string, defaultWriteTags ...string) context.Context {
	return scope.WithWriterScope(context.Background(),
		scope.WriterScope{WriterID: id, DefaultWriteTags: defaultWriteTags})
}

func docWithTags(tags ...string) *domain.Document {
	return &domain.Document{Metadata: map[string]interface{}{"tags": tags}}
}

// C2: classification is derived from DefaultWriteTags; agent-supplied tags are a
// narrow-only hint. With no hint, the write carries exactly DefaultWriteTags.
func TestWriter_DerivesFromDefaultWriteTags(t *testing.T) {
	store := &recordingStore{}
	w := scope.NewScopedStoreWriter(store, scope.NewVocabulary(nil), nil)

	ctx := writerCtx("analyst", "company_wide", "analytics")
	if err := w.Save(ctx, &domain.Document{}); err != nil {
		t.Fatal(err)
	}
	got := tagsOf(store.saved[0])
	if !contains(got, "company_wide") || !contains(got, "analytics") {
		t.Errorf("write must carry DefaultWriteTags, got %v", got)
	}
	if !contains(got, "provenance:source=analyst") {
		t.Errorf("provenance must be kernel-stamped, got %v", got)
	}
}

// The agent hint can only NARROW — keep a subset, never add.
func TestWriter_HintNarrowsOnly(t *testing.T) {
	store := &recordingStore{}
	w := scope.NewScopedStoreWriter(store, scope.NewVocabulary(nil), nil)

	// Hint = subset → narrows to that subset.
	_ = w.Save(writerCtx("a", "company_wide", "analytics"), docWithTags("analytics"))
	if got := tagsOf(store.saved[0]); contains(got, "company_wide") || !contains(got, "analytics") {
		t.Errorf("hint should narrow to {analytics}, got %v", got)
	}

	// Hint contains a tag NOT in DefaultWriteTags → it cannot add; result excludes it.
	_ = w.Save(writerCtx("a", "company_wide"), docWithTags("secrets"))
	if got := tagsOf(store.saved[1]); contains(got, "secrets") {
		t.Errorf("agent must not be able to ADD a classification, got %v", got)
	}
}

// R3 (now stronger under C2): a cognitive system agent can NEVER emit a tag outside
// its DefaultWriteTags — even by trying to hint it. The Consolidator cannot write secrets.
func TestWriter_ConsolidatorCannotEmitSecrets(t *testing.T) {
	store := &recordingStore{}
	w := scope.NewScopedStoreWriter(store, scope.NewVocabulary([]string{"secrets", "company_wide"}), nil)

	ctx := writerCtx("ConsolidatorAgent", "company_wide", "analytics", "derived")
	_ = w.Save(ctx, docWithTags("secrets")) // tries to hint secrets
	if got := tagsOf(store.saved[0]); contains(got, "secrets") {
		t.Errorf("Consolidator must never emit secrets, got %v", got)
	}
}

// A coined hint (outside the controlled vocabulary) is rejected.
func TestWriter_RejectsCoinageHint(t *testing.T) {
	store := &recordingStore{}
	w := scope.NewScopedStoreWriter(store, scope.NewVocabulary([]string{"public_kb"}), nil)

	err := w.Save(writerCtx("attacker", "public_kb"), docWithTags("superuser_bypass"))
	if !errors.Is(err, scope.ErrUnknownClassification) {
		t.Fatalf("expected ErrUnknownClassification for coined hint, got %v", err)
	}
}

// Provenance is kernel-stamped and forged provenance is stripped.
func TestWriter_StampsProvenanceItself(t *testing.T) {
	store := &recordingStore{}
	w := scope.NewScopedStoreWriter(store, scope.NewVocabulary(nil), nil)

	ctx := writerCtx("agent_007", "public_kb")
	doc := &domain.Document{Metadata: map[string]interface{}{"tags": []string{"public_kb", "provenance:source=victim"}}}
	if err := w.Save(ctx, doc); err != nil {
		t.Fatal(err)
	}
	got := tagsOf(store.saved[0])
	if !contains(got, "provenance:source=agent_007") || contains(got, "provenance:source=victim") {
		t.Errorf("expected kernel provenance, forged stripped; got %v", got)
	}
}

// A write with no WriterScope (kernel curation) passes through unchanged.
func TestWriter_NoWriterScopePassesThrough(t *testing.T) {
	store := &recordingStore{}
	w := scope.NewScopedStoreWriter(store, scope.NewVocabulary([]string{"public_kb"}), nil)

	doc := docWithTags("anything_uncontrolled")
	if err := w.Save(context.Background(), doc); err != nil {
		t.Fatalf("kernel curation write must pass through, got %v", err)
	}
	if len(store.saved) != 1 {
		t.Errorf("unscoped write should reach the store")
	}
}
