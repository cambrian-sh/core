package authz

import (
	"context"
	"slices"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0146 acceptance test 10 (kernel half) on the artifact / ledger write path:
// a hint carrying owner:B — an UploadArtifact request's tags, or a tool's declared
// tags riding into a ledger entry — never reaches the row. In a conversation owned
// by A the row carries owner:A (the kernel's D12 stamp); with no owner it carries
// no owner tag at all. Either way it is not "written for B", so B's agents get no
// own-rows exemption for it. The write still succeeds.
func TestClassifyArtifactWrite_CallerOwnerTagDroppedKernelOwnerStamped(t *testing.T) {
	writer := domain.AgentPrincipal("composed_worker")
	hint := []string{"ops", domain.OwnerTag("bob")}

	forA := domain.WithPrivateWrites(context.Background(), "alice")
	tags, err := ClassifyArtifactWrite(forA, domain.AllowAllAuthorizer{}, writer, hint)
	if err != nil {
		t.Fatalf("the write must still succeed: %v", err)
	}
	if slices.Contains(tags, domain.OwnerTag("bob")) || !slices.Contains(tags, domain.OwnerTag("alice")) ||
		!slices.Contains(tags, "ops") {
		t.Fatalf("owned by A: tags = %v, want ops + owner:alice, never owner:bob", tags)
	}

	tags, err = ClassifyArtifactWrite(context.Background(), domain.AllowAllAuthorizer{}, writer, hint)
	if err != nil {
		t.Fatalf("the write must still succeed: %v", err)
	}
	if len(domain.OwnersOf(tags)) != 0 {
		t.Fatalf("no owner: tags = %v, want no owner tag", tags)
	}
	if hint[1] != domain.OwnerTag("bob") {
		t.Fatal("the caller's hint was mutated")
	}
}
