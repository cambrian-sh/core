package network

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0140 D4: the run principal a per_user integration selects a credential
// by comes from the kernel's lease binding (the D14 resolver), under the
// ADR-0139 D8 ownership rule — never from anything the caller can set.
func TestWithRunPrincipal_ComesFromTheLeaseOnly(t *testing.T) {
	t.Parallel()
	s, gw := newLeaseServer(t)
	s.RunPrincipal = func(_ context.Context, b domain.LeaseBinding) string {
		return strings.TrimPrefix(b.ConversationID, "conv-of-")
	}
	alices := bindNotesLease(t, gw, domain.LeaseBinding{RunID: "r1", AgentID: "planner", ConversationID: "conv-of-alice"})
	bobs := bindNotesLease(t, gw, domain.LeaseBinding{RunID: "r2", AgentID: "other-agent", ConversationID: "conv-of-bob"})

	ctx := agentPlaneCtx("planner")
	if got := domain.RunPrincipalFromContext(s.withRunPrincipal(ctx, alices)); got != "alice" {
		t.Fatalf("own lease: run principal %q, want alice", got)
	}
	// A lease issued to ANOTHER agent confers nothing: presenting bob's lease
	// does not let this agent act with bob's credentials.
	if got := domain.RunPrincipalFromContext(s.withRunPrincipal(ctx, bobs)); got != "" {
		t.Fatalf("foreign lease resolved to %q", got)
	}
	// No lease, or an unknown one: no principal (per_user tools fail closed).
	if got := domain.RunPrincipalFromContext(s.withRunPrincipal(ctx, "")); got != "" {
		t.Fatalf("no lease resolved to %q", got)
	}
	if got := domain.RunPrincipalFromContext(s.withRunPrincipal(ctx, "lease-never-issued")); got != "" {
		t.Fatalf("unknown lease resolved to %q", got)
	}
	// Caller-set metadata is not an input.
	md := metadata.Pairs("x-run-principal", "bob", "x-principal-id", "bob")
	forged := metadata.NewIncomingContext(ctx, metadata.Join(md, mdOf(ctx)))
	if got := domain.RunPrincipalFromContext(s.withRunPrincipal(forged, alices)); got != "alice" {
		t.Fatalf("metadata influenced the run principal: %q", got)
	}
	// No resolver wired: none.
	s.RunPrincipal = nil
	if got := domain.RunPrincipalFromContext(s.withRunPrincipal(ctx, alices)); got != "" {
		t.Fatalf("no resolver resolved to %q", got)
	}
}

func mdOf(ctx context.Context) metadata.MD {
	md, _ := metadata.FromIncomingContext(ctx)
	return md
}
