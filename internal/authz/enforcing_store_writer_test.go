package authz_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
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

func (r *recordingStore) SaveBatch(_ context.Context, docs []*domain.Document) error {
	r.saved = append(r.saved, docs...)
	return nil
}

// stubAuthorizer lets a test stand in for a policy plugin without depending on
// one — the kernel must behave correctly against ANY decision point.
type stubAuthorizer struct {
	domain.AllowAllAuthorizer
	classify func(hint []string) ([]string, domain.AccessDecision)
	seen     []domain.PrincipalRef
}

func (s *stubAuthorizer) ClassifyWrite(_ context.Context, p domain.PrincipalRef, hint []string) ([]string, domain.AccessDecision) {
	s.seen = append(s.seen, p)
	return s.classify(hint)
}

func tagsOf(doc *domain.Document) []string { return authz.DocTags(doc) }

func hasTag(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// The chokepoint must REPLACE the authored tags with the decision point's answer.
// A writer's tags are a request, never the answer.
func TestEnforcingWriter_ReplacesAuthoredTagsWithClassification(t *testing.T) {
	store := &recordingStore{}
	a := &stubAuthorizer{classify: func([]string) ([]string, domain.AccessDecision) {
		return []string{"company_wide", "provenance:source=analyst"}, domain.AccessDecision{Allowed: true, Reason: domain.ReasonAllowed}
	}}
	w := authz.NewEnforcingStoreWriter(store, a, nil)

	ctx := domain.WithPrincipal(context.Background(), domain.AgentPrincipal("analyst"))
	doc := &domain.Document{Metadata: map[string]interface{}{"tags": []string{"secrets"}}}
	if err := w.Save(ctx, doc); err != nil {
		t.Fatal(err)
	}
	got := tagsOf(store.saved[0])
	if hasTag(got, "secrets") {
		t.Errorf("authored tags must not survive classification, got %v", got)
	}
	if !hasTag(got, "company_wide") || !hasTag(got, "provenance:source=analyst") {
		t.Errorf("classification must be written verbatim, got %v", got)
	}
}

// INV-5: the principal handed to the decision point comes from the kernel's
// context, never from the document.
func TestEnforcingWriter_PassesTheContextPrincipal(t *testing.T) {
	store := &recordingStore{}
	a := &stubAuthorizer{classify: func(h []string) ([]string, domain.AccessDecision) {
		return h, domain.AccessDecision{Allowed: true}
	}}
	w := authz.NewEnforcingStoreWriter(store, a, nil)

	ctx := domain.WithPrincipal(context.Background(), domain.AgentPrincipal("real_writer"))
	doc := &domain.Document{Metadata: map[string]interface{}{
		"tags": []string{"x"}, "source_agent_id": "forged_writer",
	}}
	if err := w.Save(ctx, doc); err != nil {
		t.Fatal(err)
	}
	if len(a.seen) != 1 || a.seen[0].ID != "real_writer" {
		t.Fatalf("decision point must see the context principal, saw %+v", a.seen)
	}
}

// A denied classification stops the write; nothing reaches the store.
func TestEnforcingWriter_DeniedWriteNeverReachesTheStore(t *testing.T) {
	store := &recordingStore{}
	a := &stubAuthorizer{classify: func([]string) ([]string, domain.AccessDecision) {
		return nil, domain.AccessDecision{Allowed: false, Reason: domain.ReasonForbiddenTag, Detail: "coinage"}
	}}
	w := authz.NewEnforcingStoreWriter(store, a, nil)

	err := w.Save(context.Background(), &domain.Document{})
	if !errors.Is(err, authz.ErrWriteDenied) {
		t.Fatalf("expected ErrWriteDenied, got %v", err)
	}
	if len(store.saved) != 0 {
		t.Errorf("a denied write must not reach the store")
	}
}

// Fail-closed on batches: one rejection fails the whole batch, so a partially
// classified batch can never be persisted.
func TestEnforcingWriter_BatchIsAllOrNothing(t *testing.T) {
	store := &recordingStore{}
	calls := 0
	a := &stubAuthorizer{classify: func(h []string) ([]string, domain.AccessDecision) {
		calls++
		if calls == 2 {
			return nil, domain.AccessDecision{Allowed: false, Reason: domain.ReasonForbiddenTag}
		}
		return h, domain.AccessDecision{Allowed: true}
	}}
	w := authz.NewEnforcingStoreWriter(store, a, nil)

	err := w.SaveBatch(context.Background(), []*domain.Document{{}, {}, {}})
	if !errors.Is(err, authz.ErrWriteDenied) {
		t.Fatalf("expected the batch to fail closed, got %v", err)
	}
	if len(store.saved) != 0 {
		t.Errorf("no document from a rejected batch may be persisted, got %d", len(store.saved))
	}
}

// A nil authorizer degrades to the OSS default rather than to nil-panic or to a
// silent bypass of the chokepoint.
func TestEnforcingWriter_NilAuthorizerIsAllowAll(t *testing.T) {
	store := &recordingStore{}
	w := authz.NewEnforcingStoreWriter(store, nil, nil)

	doc := &domain.Document{Metadata: map[string]interface{}{"tags": []string{"public_kb"}}}
	if err := w.Save(context.Background(), doc); err != nil {
		t.Fatalf("OSS write must succeed, got %v", err)
	}
	if got := tagsOf(store.saved[0]); !hasTag(got, "public_kb") {
		t.Errorf("OSS writes keep their authored tags, got %v", got)
	}
}

func TestDocTags_HandlesBothEncodings(t *testing.T) {
	// A JSON round-trip yields []interface{}, which is the encoding that silently
	// broke tag reads before it was handled explicitly.
	fromJSON := &domain.Document{Metadata: map[string]interface{}{"tags": []interface{}{"a", "b", 7}}}
	got := authz.DocTags(fromJSON)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("expected the string members only, got %v", got)
	}
	if authz.DocTags(nil) != nil {
		t.Errorf("a nil document has no tags")
	}
	if authz.DocTags(&domain.Document{}) != nil {
		t.Errorf("a document with no metadata has no tags")
	}
}
