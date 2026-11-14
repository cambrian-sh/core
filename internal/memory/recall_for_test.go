package memory

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// kindRecordingAuthorizer remembers the full principal (id AND kind) each read
// was resolved for.
type kindRecordingAuthorizer struct {
	domain.AllowAllAuthorizer
	got []domain.PrincipalRef
}

func (a *kindRecordingAuthorizer) ReadFilter(_ context.Context, p domain.PrincipalRef, _ domain.SurfaceRef) (*domain.TagPredicate, domain.AccessDecision) {
	a.got = append(a.got, p)
	return &domain.TagPredicate{}, domain.AccessDecision{Allowed: true, Principal: p, Reason: domain.ReasonAllowed}
}

// queryPrefixEmbedder records whether the query side (EmbedQuery) was used.
type queryPrefixEmbedder struct {
	fakeEmbedder
	queries, docs int
}

func (e *queryPrefixEmbedder) Embed(ctx context.Context, s string) ([]float32, error) {
	e.docs++
	return e.fakeEmbedder.Embed(ctx, s)
}

func (e *queryPrefixEmbedder) EmbedQuery(ctx context.Context, s string) ([]float32, error) {
	e.queries++
	return e.fakeEmbedder.Embed(ctx, s)
}

// ENG-1339 / ENG-465 (owner decision 2026-11-14): RecallFor reads AS the given
// principal — kind included, so a person is not resolved as an agent of the same
// id — embeds the query through the shared query-side helper, and is not widened
// by a ScopeSystem an upstream caller left on ctx.
func TestRecallFor_ReadsAsThePrincipalWithTheQueryEmbedding(t *testing.T) {
	t.Parallel()
	az := &kindRecordingAuthorizer{}
	emb := &queryPrefixEmbedder{}
	q := NewQueryService(emb, corpusOfOne(), az)

	ctx := domain.WithScope(context.Background(), domain.ScopeSystem) // must not widen the read
	person := domain.PrincipalRef{ID: "ayse", Kind: domain.PrincipalUser}
	if _, err := q.RecallFor(ctx, person, "the answer", domain.DocTypeMnemonicFact); err != nil {
		t.Fatal(err)
	}
	if len(az.got) == 0 || az.got[0] != person {
		t.Fatalf("authorizer asked about %v, want %v (ScopeSystem on ctx must not bypass it)", az.got, person)
	}
	if emb.queries == 0 || emb.docs != 0 {
		t.Fatalf("EmbedQuery calls %d, Embed calls %d: the query must use the query-side embedding", emb.queries, emb.docs)
	}
}

// No principal ⇒ nothing is read.
func TestRecallFor_NoPrincipalReadsNothing(t *testing.T) {
	t.Parallel()
	az := &kindRecordingAuthorizer{}
	q := NewQueryService(&fakeEmbedder{}, corpusOfOne(), az)
	got, err := q.RecallFor(context.Background(), domain.PrincipalRef{}, "the answer", domain.DocTypeMnemonicFact)
	if err != nil || got != nil || len(az.got) != 0 {
		t.Fatalf("RecallFor(no principal) = %v, %v; authorizer asked %v", got, err, az.got)
	}
}
