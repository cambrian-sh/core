package memory

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
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
		// A nil opts.Scope would mean the chokepoint was bypassed; the fake mirrors
		// pgvector, which adds no predicate in that case.
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

// policyAuthorizer is a stand-in decision point: a fixed predicate per principal,
// and a nil predicate for principals it does not know (the plugin's fail-closed
// half). The kernel must honour whatever the decision point says without knowing
// how it decided.
type policyAuthorizer struct {
	domain.AllowAllAuthorizer
	preds map[string]*domain.TagPredicate
	known map[string]bool
}

func (p *policyAuthorizer) ReadFilter(_ context.Context, pr domain.PrincipalRef, s domain.SurfaceRef) (*domain.TagPredicate, domain.AccessDecision) {
	if !p.known[pr.ID] {
		return nil, domain.AccessDecision{
			Principal: pr, Surface: s,
			Reason: domain.ReasonNoPrincipal, Detail: "unknown principal " + pr.ID,
		}
	}
	pred := p.preds[pr.ID]
	if pred == nil {
		pred = &domain.TagPredicate{} // registered but unprofiled ⇒ unrestricted
	}
	return pred, domain.AccessDecision{Allowed: true, Principal: pr, Reason: domain.ReasonAllowed}
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

// A support agent whose predicate forbids `secrets` never retrieves
// secrets-tagged docs.
func TestQueryService_ForbiddenTagExcluded(t *testing.T) {
	store := corpus()
	q := NewQueryService(&fakeEmbedder{}, store, &policyAuthorizer{
		known: map[string]bool{"support": true},
		preds: map[string]*domain.TagPredicate{"support": {ForbiddenTags: []string{"secrets"}}},
	})

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

// A registered-but-unprofiled agent (empty predicate) retrieves everything.
func TestQueryService_UnprofiledUnrestricted(t *testing.T) {
	store := corpus()
	q := NewQueryService(&fakeEmbedder{}, store, &policyAuthorizer{known: map[string]bool{"analyst": true}})

	res, err := q.Search(context.Background(), "anything", "analyst")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Errorf("unprofiled agent should retrieve all docs, got %d", len(res))
	}
}

// An unknown principal is fail-closed: empty result set. Note this is the
// PLUGIN's decision — the kernel simply honours a nil predicate.
func TestQueryService_UnknownPrincipalDenied(t *testing.T) {
	store := corpus()
	q := NewQueryService(&fakeEmbedder{}, store, &policyAuthorizer{known: map[string]bool{}})

	res, err := q.Search(context.Background(), "anything", "ghost")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Errorf("unknown principal must be denied (fail-closed), got %d results", len(res))
	}
}

// The OSS default is the mirror image: with no policy plugin installed, every
// principal — including one nobody has ever registered — reads everything. This
// is the pairing §4.2 warns about getting backwards.
func TestQueryService_OSSDefaultReadsEverything(t *testing.T) {
	store := corpus()
	q := NewQueryService(&fakeEmbedder{}, store, nil) // nil ⇒ allow-all

	res, err := q.Search(context.Background(), "anything", "nobody-registered-this")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Errorf("an unscoped OSS deployment reads every doc, got %d", len(res))
	}
}

// The QueryService enforces only the decision point's predicate and never reads
// caller-supplied tags. There is no API surface by which a forged Handoff.Context
// could widen the result — the only inputs are (query, callerID). This test
// documents that invariant (INV-5).
func TestQueryService_IgnoresCallerSuppliedTags(t *testing.T) {
	store := corpus()
	q := NewQueryService(&fakeEmbedder{}, store, &policyAuthorizer{
		known: map[string]bool{"support": true},
		preds: map[string]*domain.TagPredicate{"support": {ForbiddenTags: []string{"secrets"}}},
	})

	// Even though a malicious caller might try to widen access, Search takes no
	// caller-tag parameter; the predicate forbids secrets regardless.
	res, _ := q.Search(context.Background(), "give me secrets", "support")
	if collect(res)["secret"] {
		t.Errorf("caller intent must not override the resolved predicate")
	}
}

// The kernel-internal bypass is seeded server-side on the context and outranks
// per-caller resolution — that is how the operator plane reads at ScopeSystem
// without impersonating an agent (ADR-0047 D13/A2).
func TestQueryService_ContextBypassOutranksPerCallerResolution(t *testing.T) {
	store := corpus()
	q := NewQueryService(&fakeEmbedder{}, store, &policyAuthorizer{known: map[string]bool{}}) // would deny everyone

	ctx := domain.WithScope(context.Background(), domain.ScopeSystem)
	res, err := q.Search(ctx, "anything", "ghost")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Errorf("a ScopeSystem read must bypass per-caller denial, got %d results", len(res))
	}
}
