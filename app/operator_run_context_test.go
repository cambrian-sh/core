package app

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"google.golang.org/grpc/metadata"
)

// ENG-1378 (owner decision 2026-11-14): an operator-started run carries the
// operator's principal and surface, survives the request's cancellation, and sees
// only the session header — never the operator's own request metadata.
func TestOperatorRunContext_CarriesTheOperatorDetached(t *testing.T) {
	op := domain.PrincipalRef{Kind: domain.PrincipalUser, ID: "alice"}
	req, cancel := context.WithCancel(context.Background())
	req = domain.WithPrincipal(req, op)
	req = domain.WithSurface(req, domain.SurfaceRef{Kind: domain.SurfaceOperator})
	req = metadata.NewIncomingContext(req, metadata.Pairs("authorization", "Bearer operator-token"))

	run := operatorRunContext(req, "ses-1")
	cancel()

	if run.Err() != nil {
		t.Fatalf("the request's cancellation reached the run: %v", run.Err())
	}
	if got := domain.PrincipalFromContext(run); got != op {
		t.Fatalf("run principal = %+v, want the operator %+v", got, op)
	}
	if domain.SurfaceFromContext(run).Kind != domain.SurfaceOperator {
		t.Fatalf("run surface = %+v, want operator", domain.SurfaceFromContext(run))
	}
	md, _ := metadata.FromIncomingContext(run)
	if got := md.Get("x-session-id"); len(got) != 1 || got[0] != "ses-1" {
		t.Fatalf("x-session-id = %v", got)
	}
	if len(md.Get("authorization")) != 0 {
		t.Fatal("the operator's request metadata leaked into the run")
	}
}

// SubmitPlan hands its request ctx to the execute path (planSubmitter).
func TestPlanSubmitter_PassesTheRequestContext(t *testing.T) {
	op := domain.PrincipalRef{Kind: domain.PrincipalUser, ID: "alice"}
	var got context.Context
	p := planSubmitter{execute: func(ctx context.Context, _, _ string) { got = ctx }}
	if _, _, err := p.SubmitPlan(domain.WithPrincipal(context.Background(), op), "ses-1", "s",
		[]domain.Step{{Query: "q"}}); err != nil {
		t.Fatal(err)
	}
	if got == nil || domain.PrincipalFromContext(got) != op {
		t.Fatal("SubmitPlan did not pass the operator's ctx to execute")
	}
}
