package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/scope"
)

// capturingSaveStore records Save calls and implements domain.VectorStore.
type capturingSaveStore struct {
	saved []*domain.Document
}

func (c *capturingSaveStore) Save(_ context.Context, d *domain.Document) error {
	c.saved = append(c.saved, d)
	return nil
}
func (c *capturingSaveStore) SaveBatch(_ context.Context, ds []*domain.Document) error {
	c.saved = append(c.saved, ds...)
	return nil
}
func (c *capturingSaveStore) Search(context.Context, []float32, domain.SearchOptions) ([]domain.SearchResult, error) {
	return nil, nil
}
func (c *capturingSaveStore) GetByID(context.Context, string) (*domain.Document, error) {
	return nil, nil
}
func (c *capturingSaveStore) GetBatch(context.Context, []string) ([]domain.Document, error) {
	return nil, nil
}
func (c *capturingSaveStore) Delete(context.Context, string) error        { return nil }
func (c *capturingSaveStore) DeleteBatch(context.Context, []string) error { return nil }
func (c *capturingSaveStore) IncrementAccess(context.Context, string) error {
	return nil
}
func (c *capturingSaveStore) GetStaleMemories(context.Context, int) ([]domain.Document, error) {
	return nil, nil
}
func (c *capturingSaveStore) QueryByMetadata(context.Context, map[string]string, int) ([]domain.Document, error) {
	return nil, nil
}

type fakeWriteResolver struct {
	known     map[string]bool
	writeTags map[string][]string
}

func (f fakeWriteResolver) EffectiveForAgent(_ context.Context, id string) (*domain.EffectiveScope, bool) {
	if !f.known[id] {
		return nil, false
	}
	eff := domain.NewEffectiveScope(domain.ScopeConfig{}, domain.ScopeConfig{})
	return &eff, true
}
func (f fakeWriteResolver) DefaultWriteTags(_ context.Context, id string) []string {
	return f.writeTags[id]
}

func tagsOfDoc(d *domain.Document) []string {
	if v, ok := d.Metadata["tags"].([]string); ok {
		return v
	}
	return nil
}

func has(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestRemember_UnknownPrincipalRejected(t *testing.T) {
	store := scope.NewScopedStoreWriter(&capturingSaveStore{}, scope.NewVocabulary(nil), nil)
	svc := NewRememberService(store, &fakeEmbedder{}, fakeWriteResolver{known: map[string]bool{}})

	if _, err := svc.Remember(context.Background(), "ghost", "x", nil, "src", "sess", 0); !errors.Is(err, ErrUnknownPrincipal) {
		t.Fatalf("expected ErrUnknownPrincipal, got %v", err)
	}
}

// C2: the remembered doc is classified by the agent's DefaultWriteTags + provenance.
func TestRemember_DerivesClassification(t *testing.T) {
	cap := &capturingSaveStore{}
	store := scope.NewScopedStoreWriter(cap, scope.NewVocabulary(nil), nil)
	svc := NewRememberService(store, &fakeEmbedder{}, fakeWriteResolver{
		known:     map[string]bool{"analyst": true},
		writeTags: map[string][]string{"analyst": {"company_wide"}},
	})

	id, err := svc.Remember(context.Background(), "analyst", "an insight", nil, "analyst", "sess-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if id == "" || len(cap.saved) != 1 {
		t.Fatalf("expected one saved doc with an id, got id=%q n=%d", id, len(cap.saved))
	}
	got := tagsOfDoc(cap.saved[0])
	if !has(got, "company_wide") || !has(got, "provenance:source=analyst") {
		t.Errorf("expected derived classification + provenance, got %v", got)
	}
}

// A remembered fact is stamped with a recallable ActivationStrength — never 0. A
// 0-activation fact's floor-multiplier recall score (cosine·α) can never clear
// RecallSimilarityFloor, so it would be permanently unrecallable. This guards that bug.
func TestRemember_StampsRecallableActivation(t *testing.T) {
	cap := &capturingSaveStore{}
	store := scope.NewScopedStoreWriter(cap, scope.NewVocabulary(nil), nil)
	svc := NewRememberService(store, &fakeEmbedder{}, fakeWriteResolver{known: map[string]bool{"a": true}})
	svc.SetDefaultActivation(0.5)

	// No importance hint → the configured default activation.
	if _, err := svc.Remember(context.Background(), "a", "x", nil, "a", "s", 0); err != nil {
		t.Fatal(err)
	}
	if got := cap.saved[0].ActivationStrength; got != 0.5 {
		t.Errorf("hint-less remember must use default activation 0.5, got %v", got)
	}
	// An explicit importance hint sets the activation directly (clamped to [0,1]).
	if _, err := svc.Remember(context.Background(), "a", "y", nil, "a", "s", 0.8); err != nil {
		t.Fatal(err)
	}
	if got := cap.saved[1].ActivationStrength; got != 0.8 {
		t.Errorf("importance hint must set activation, got %v", got)
	}
}

// The agent cannot broaden via the hint.
func TestRemember_HintCannotBroaden(t *testing.T) {
	cap := &capturingSaveStore{}
	store := scope.NewScopedStoreWriter(cap, scope.NewVocabulary([]string{"company_wide", "secrets"}), nil)
	svc := NewRememberService(store, &fakeEmbedder{}, fakeWriteResolver{
		known:     map[string]bool{"a": true},
		writeTags: map[string][]string{"a": {"company_wide"}},
	})

	if _, err := svc.Remember(context.Background(), "a", "x", []string{"secrets"}, "a", "s", 0); err != nil {
		t.Fatal(err)
	}
	if has(tagsOfDoc(cap.saved[0]), "secrets") {
		t.Errorf("agent must not broaden classification to secrets, got %v", tagsOfDoc(cap.saved[0]))
	}
}
