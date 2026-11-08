package operator

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/cambrian-sh/core/domain"
)

// T-20261107-approver-is-not-told-that-an-identical-side: the operator plane
// carries the hub item's duplicate notice to the console, and the op marshals
// (the notice is non-ASCII: an em dash).
func TestApprovalOp_CarriesDuplicateNotice(t *testing.T) {
	t.Parallel()
	notice := "Possible duplicate of create_issue (approval apr-1) — done at 2026-11-07 17:02:11 UTC, ids: key=OPS-11; same project_key, subject."
	op := approvalOp(domain.ApprovalItem{ID: "apr-2", ToolName: "create_issue", State: domain.ApprovalPending, DuplicateNotice: notice}, true)
	if op.GetDuplicateNotice() != notice {
		t.Fatalf("duplicate_notice = %q", op.GetDuplicateNotice())
	}
	if _, err := proto.Marshal(op); err != nil {
		t.Fatalf("marshal: %v", err)
	}
}
