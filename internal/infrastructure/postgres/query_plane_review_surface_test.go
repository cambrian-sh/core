package postgres

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ENG-1536 (owner decision 2026-11-14): only operators see candidate links; an
// agent read — even an unrestricted one, as every read is under the OSS
// AllowAllAuthorizer — sees confirmed links only.
func TestSeesReviewCandidates_OperatorSurfaceOnly(t *testing.T) {
	op := domain.WithSurface(context.Background(), domain.SurfaceRef{Kind: domain.SurfaceOperator, ID: "console"})
	agent := domain.WithSurface(context.Background(), domain.SurfaceRef{Kind: domain.SurfaceAgent, ID: "grpc"})
	bypass := &domain.TagPredicate{Bypass: true}
	cases := []struct {
		name  string
		ctx   context.Context
		scope *domain.TagPredicate
		want  bool
	}{
		{"operator plane, unrestricted", op, domain.ScopeSystem, true},
		{"agent under AllowAll (Bypass)", agent, bypass, false},
		{"no surface (in-process)", context.Background(), bypass, false},
		{"operator with a narrowed scope", op, &domain.TagPredicate{RequiredTags: []string{"x"}}, false},
		{"nil scope", op, nil, false},
	}
	for _, c := range cases {
		if got := seesReviewCandidates(c.ctx, c.scope); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
