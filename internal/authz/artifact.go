package authz

import (
	"context"

	"github.com/cambrian-sh/core/domain"
)

// ClassifyArtifactWrite derives an artifact's authoritative classification from
// the decision point, given the writer's requested tags. It is the artifact-path
// twin of EnforcingStoreWriter (used by the UploadArtifact RPC and by tool
// artifact promotion): the writer may only narrow, never broaden.
func ClassifyArtifactWrite(ctx context.Context, a domain.Authorizer, principal domain.PrincipalRef, hint []string) ([]string, error) {
	if a == nil {
		a = domain.AllowAllAuthorizer{}
	}
	// ADR-0146 D5: every hint reaching this function is a caller's (the
	// UploadArtifact request) or a tool's declared tags (the ledger recorder) —
	// never an owner the kernel derived — so an owner tag in it is dropped; the
	// conversation owner is stamped below.
	hint = domain.DropCallerOwnerTags(ctx, "artifact/ledger classification hint", hint)
	// ENG-2115: fail closed — every requested tag is carried, or the write is refused.
	final, _, err := ClassifyWriteStrict(ctx, a, principal, hint)
	if err != nil {
		return nil, err
	}
	// ADR-0142 D12: private to the conversation owner the work is for.
	return domain.StampPrivateOwner(ctx, final), nil
}
