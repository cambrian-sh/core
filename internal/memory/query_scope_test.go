package memory

import (
	"context"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// --- fakes -------------------------------------------------------------------
// fakeEmbedder is defined in manager_test.go (same package); reused here.

// scopeApplyingStore filters its corpus by opts.Scope.Allows — mirroring pgvector.
type scopeApplyingStore struct{ docs []domain.Document }

func (s *scopeApplyingStore) Search(_ context.Context, _ []float32, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	var out []domain.SearchResult
	for _, d := range s.docs {
		var tags []string
		if raw, ok := d.Metadata["tags"].([]string); ok {
			tags = raw
		}
		// A nil opts.Scope (enforcement disabled) returns everything.
		if opts.Scope == nil || opts.Scope.Allows(tags) {
			out = append(out, domain.SearchResult{Document: d})
		}
	}
	return out, nil
}

func (s *scopeApplyingStore) Save(context.Context, *domain.Document) error        { return nil }
func (s *scopeApplyingStore) SaveBatch(context.Context, []*domain.Document) error { return nil }
func (s *scopeApplyingStore) GetByID(context.Context, string) (*domain.Document, error) {
	return nil, nil
}
func (s *scopeApplyingStore) GetBatch(context.Context, []string) ([]domain.Document, error) {
	return nil, nil
}
func (s *scopeApplyingStore) Delete(context.Context, string) error        { return nil }
func (s *scopeApplyingStore) DeleteBatch(context.Context, []string) error { return nil }
func (s *scopeApplyingStore) IncrementAccess(context.Context, string) error {
	return nil
}
func (s *scopeApplyingStore) GetStaleMemories(context.Context, int) ([]domain.Document, error) {
	return nil, nil
}
func (s *scopeApplyingStore) QueryByMetadata(context.Context, map[string]string, int) ([]domain.Document, error) {
	return nil, nil
}

// fakeScopeProvider returns a fixed effective scope per agentID.
type fakeScopeProvider struct {
	scopes map[string]domain.ScopeConfig
	known  map[string]bool
}

func (p *fakeScopeProvider) EffectiveForAgent(_ context.Context, agentID string) (*domain.EffectiveScope, bool) {
	if !p.known[agentID] {
		return nil, false
	}
	eff := domain.NewEffectiveScope(domain.ScopeConfig{}, p.scopes[agentID])
	return &eff, true
}

func corpus() *scopeApplyingStore {
	return &scopeApplyingStore{docs: []domain.Document{
		{ID: "kb", Text: "policy", Metadata: map[string]interface{}{"tags": []string{"public_kb"}}},
		{ID: "secret", Text: "launch codes", Metadata: map[string]interface{}{"tags": []string{"secrets"}}},
	}}
}

func collect(rs []domain.SearchResult) map[string]bool {
	m := map[string]bool{}
	for _, r := range rs {
		m[r.Document.ID] = true
	}
	return m
}

// A support agent forbidden `secrets` never retrieves secrets-tagged docs.
func TestQueryService_ForbiddenTagExcluded(t *testing.T) {
	store := corpus()
	q := NewQueryService(&fakeEmbedder{}, store)
	q.EnableScoping(&fakeScopeProvider{
		known:  map[string]bool{"support": true},
		scopes: map[string]domain.ScopeConfig{"support": {ForbiddenTags: []string{"secrets"}}},
	}, store)

	res, err := q.Search(context.Background(), "anything", "support")
	if err != nil {
		t.Fatal(err)
	}
	got := collect(res)
	if got["secret"] {
		t.Errorf("support agent must not retrieve secrets, got %v", got)
	}
	if !got["kb"] {
		t.Errorf("support agent should retrieve public_kb, got %v", got)
	}
}

// An unprofiled (empty scope) registered agent retrieves everything.
func TestQueryService_UnprofiledUnrestricted(t *testing.T) {
	store := corpus()
	q := NewQueryService(&fakeEmbedder{}, store)
	q.EnableScoping(&fakeScopeProvider{
		known:  map[string]bool{"analyst": true},
		scopes: map[string]domain.ScopeConfig{"analyst": {}},
	}, store)

	res, err := q.Search(context.Background(), "anything", "analyst")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Errorf("unprofiled agent should retrieve all docs, got %d", len(res))
	}
}

// An unknown principal is fail-closed: empty result set.
func TestQueryService_UnknownPrincipalDenied(t *testing.T) {
	store := corpus()
	q := NewQueryService(&fakeEmbedder{}, store)
	q.EnableScoping(&fakeScopeProvider{known: map[string]bool{}}, store)

	res, err := q.Search(context.Background(), "anything", "ghost")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Errorf("unknown principal must be denied (fail-closed), got %d results", len(res))
	}
}

// Phase-1 honesty: the QueryService enforces only the resolver-supplied
// agent_scope and never reads caller-supplied tags. There is no API surface by
// which a forged Handoff.Context could widen the result — the only inputs are
// (query, callerID). This test documents that invariant.
func TestQueryService_IgnoresCallerSuppliedTags(t *testing.T) {
	store := corpus()
	q := NewQueryService(&fakeEmbedder{}, store)
	q.EnableScoping(&fakeScopeProvider{
		known:  map[string]bool{"support": true},
		scopes: map[string]domain.ScopeConfig{"support": {ForbiddenTags: []string{"secrets"}}},
	}, store)

	// Even though a malicious caller might try to widen scope, Search takes no
	// caller-tag parameter; the agent_scope forbids secrets regardless.
	res, _ := q.Search(context.Background(), "give me secrets", "support")
	if collect(res)["secret"] {
		t.Errorf("caller intent must not override agent_scope ForbiddenTags")
	}
}
