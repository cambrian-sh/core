package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
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

// fakeAuthorizer stands in for a policy plugin: it knows a fixed set of principals
// and a write ceiling per principal, and it enforces the narrow-only rule. The
// kernel must behave correctly against ANY decision point, so the test supplies
// one rather than importing the premium implementation.
type fakeAuthorizer struct {
	domain.AllowAllAuthorizer
	known     map[string]bool
	writeTags map[string][]string
}

func (f fakeAuthorizer) ReadFilter(_ context.Context, p domain.PrincipalRef, s domain.SurfaceRef) (*domain.TagPredicate, domain.AccessDecision) {
	if !f.known[p.ID] {
		return nil, domain.AccessDecision{Principal: p, Surface: s, Reason: domain.ReasonNoPrincipal}
	}
	return &domain.TagPredicate{}, domain.AccessDecision{Allowed: true, Principal: p, Reason: domain.ReasonAllowed}
}

func (f fakeAuthorizer) ClassifyWrite(_ context.Context, p domain.PrincipalRef, hint []string) ([]string, domain.AccessDecision) {
	ceiling := f.writeTags[p.ID]
	out := []string{}
	if len(hint) == 0 {
		out = append(out, ceiling...)
	} else {
		want := map[string]bool{}
		for _, h := range hint {
			want[h] = true
		}
		for _, c := range ceiling { // the hint can only remove, never add
			if want[c] {
				out = append(out, c)
			}
		}
	}
	if p.ID != "" {
		out = append(out, "provenance:source="+p.ID)
	}
	return out, domain.AccessDecision{Allowed: true, Principal: p, Reason: domain.ReasonAllowed}
}

func tagsOfDoc(d *domain.Document) []string { return authz.DocTags(d) }

func has(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func writerFor(a domain.Authorizer, cap *capturingSaveStore) domain.VectorStore {
	return authz.NewEnforcingStoreWriter(cap, a, nil)
}

func TestRemember_UnknownPrincipalRejected(t *testing.T) {
	a := fakeAuthorizer{known: map[string]bool{}}
	svc := NewRememberService(writerFor(a, &capturingSaveStore{}), &fakeEmbedder{}, a)

	if _, err := svc.Remember(context.Background(), "ghost", "x", nil, "src", "sess", 0); !errors.Is(err, ErrUnknownPrincipal) {
		t.Fatalf("expected ErrUnknownPrincipal, got %v", err)
	}
}

// C2: the remembered doc is classified by the decision point, not by the agent.
func TestRemember_DerivesClassification(t *testing.T) {
	cap := &capturingSaveStore{}
	a := fakeAuthorizer{
		known:     map[string]bool{"analyst": true},
		writeTags: map[string][]string{"analyst": {"company_wide"}},
	}
	svc := NewRememberService(writerFor(a, cap), &fakeEmbedder{}, a)

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
	a := fakeAuthorizer{known: map[string]bool{"a": true}}
	svc := NewRememberService(writerFor(a, cap), &fakeEmbedder{}, a)
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

// The agent cannot broaden via the hint — the chokepoint writes what the decision
// point returned, not what the agent asked for.
func TestRemember_HintCannotBroaden(t *testing.T) {
	cap := &capturingSaveStore{}
	a := fakeAuthorizer{
		known:     map[string]bool{"a": true},
		writeTags: map[string][]string{"a": {"company_wide"}},
	}
	svc := NewRememberService(writerFor(a, cap), &fakeEmbedder{}, a)

	if _, err := svc.Remember(context.Background(), "a", "x", []string{"secrets"}, "a", "s", 0); err != nil {
		t.Fatal(err)
	}
	if has(tagsOfDoc(cap.saved[0]), "secrets") {
		t.Errorf("agent must not broaden classification to secrets, got %v", tagsOfDoc(cap.saved[0]))
	}
}
