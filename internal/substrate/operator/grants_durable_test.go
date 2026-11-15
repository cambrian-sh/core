package operator_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// flakyGrants fails its writes while failWrites is set (a durable store that is
// down, ENG-1116).
type flakyGrants struct {
	*domain.InMemoryGrantsStore
	failWrites bool
}

func (f *flakyGrants) ReplaceGrants(ctx context.Context, id string, gs []domain.ToolGrant) error {
	if f.failWrites {
		return errors.New("db down")
	}
	return f.InMemoryGrantsStore.ReplaceGrants(ctx, id, gs)
}

// A failed grant write is Internal, leaves no audit row, and a retry of the same
// command_id applies once the store is back (not a silent dedupe).
func TestSetToolGrant_FailedWriteIsNotAuditedAndRetryApplies(t *testing.T) {
	t.Parallel()
	audit := operator.NewInMemoryAuditStore()
	grants := &flakyGrants{InMemoryGrantsStore: domain.NewInMemoryGrantsStore(), failWrites: true}
	svc := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	svc.SetCommandSources(audit, grants)
	req := &pb.SetToolGrantRequest{CommandId: "cmd-durable", Reason: "r", AgentId: "agent-1", ToolName: "propose_knowledge", Granted: true}

	if _, err := svc.SetToolGrant(opCtx(), req); status.Code(err) != codes.Internal {
		t.Fatalf("failed write: want Internal, got %v", err)
	}
	if rows, _ := audit.Query(context.Background(), operator.AuditFilter{CommandID: "cmd-durable"}); len(rows) != 0 {
		t.Fatalf("a failed write was audited: %+v", rows)
	}
	grants.failWrites = false
	ack, err := svc.SetToolGrant(opCtx(), req)
	if err != nil || ack.GetDeduped() {
		t.Fatalf("retry: ack=%+v err=%v", ack, err)
	}
	got, _ := grants.GrantsFor(context.Background(), "agent-1")
	if len(got) != 1 || got[0].Tool != "propose_knowledge" {
		t.Fatalf("grants after retry = %+v", got)
	}
	if ack, err := svc.SetToolGrant(opCtx(), req); err != nil || !ack.GetDeduped() {
		t.Fatalf("replay after success must dedupe: ack=%+v err=%v", ack, err)
	}
}

// SetToolPolicy: a failed write is Internal and not audited.
func TestSetToolPolicy_FailedWriteIsNotAudited(t *testing.T) {
	t.Parallel()
	audit := operator.NewInMemoryAuditStore()
	grants := &flakyGrants{InMemoryGrantsStore: domain.NewInMemoryGrantsStore()}
	grants.Set("agent-1", []domain.ToolGrant{{Tool: "fetch"}})
	grants.failWrites = true
	svc := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	svc.SetCommandSources(audit, grants)
	_, err := svc.SetToolPolicy(opCtx(), &pb.SetToolPolicyRequest{CommandId: "cmd-pol", Reason: "r", AgentId: "agent-1",
		ToolName: "fetch", Policy: &pb.ToolPolicyOp{AllowedUrls: []string{"example.com"}}})
	if status.Code(err) != codes.Internal {
		t.Fatalf("want Internal, got %v", err)
	}
	if rows, _ := audit.Query(context.Background(), operator.AuditFilter{CommandID: "cmd-pol"}); len(rows) != 0 {
		t.Fatalf("a failed policy write was audited: %+v", rows)
	}
}
