package operator_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

func newCommandService() (*operator.Service, *operator.Spool, *operator.InMemoryAuditStore, *domain.InMemoryGrantsStore) {
	feed := operator.NewSpool(operator.SpoolConfig{})
	audit := operator.NewInMemoryAuditStore()
	grants := domain.NewInMemoryGrantsStore()
	svc := operator.NewService(feed)
	svc.SetCommandSources(audit, grants)
	return svc, feed, audit, grants
}

func opCtx() context.Context {
	return operator.ContextWithPrincipal(context.Background(), "alice", operator.RoleOperator)
}

// A grant command applies the mutation, records the audit row, and emits an
// AuditEvent on the feed (write-then-emit).
func TestSetToolGrant_AppliesAuditsAndEmits(t *testing.T) {
	svc, feed, audit, grants := newCommandService()

	ack, err := svc.SetToolGrant(opCtx(), &pb.SetToolGrantRequest{
		CommandId: "cmd-1", Reason: "grant web_search for research",
		AgentId: "agent-1", ToolName: "web_search", Granted: true,
	})
	if err != nil || ack.GetDeduped() {
		t.Fatalf("expected applied grant, got ack=%+v err=%v", ack, err)
	}

	// Mutation applied.
	gs, _ := grants.GrantsFor(context.Background(), "agent-1")
	if len(gs) != 1 || gs[0].Tool != "web_search" {
		t.Fatalf("expected agent-1 granted web_search, got %+v", gs)
	}
	// Audit row recorded with actor/before/after/reason.
	rows, _ := audit.Query(context.Background(), operator.AuditFilter{})
	if len(rows) != 1 || rows[0].Actor != "alice" || rows[0].Reason == "" || rows[0].After == "" {
		t.Fatalf("expected one audit row with actor+reason+after, got %+v", rows)
	}
	// AuditEvent on the feed (write-then-emit: the row exists and the event is there).
	evs, _ := feed.ReadFrom(0)
	if len(evs) != 1 {
		t.Fatalf("expected one AuditEvent on the feed, got %d", len(evs))
	}
	ae, ok := evs[0].Event.(domain.AuditEvent)
	if !ok || ae.Entry.CommandID != "cmd-1" {
		t.Fatalf("expected AuditEvent for cmd-1 on the feed, got %+v", evs[0].Event)
	}
}

// A retried command_id is applied exactly once (dedup); no second mutation,
// audit row, or feed event.
func TestSetToolGrant_IdempotentOnCommandID(t *testing.T) {
	svc, feed, audit, grants := newCommandService()
	req := &pb.SetToolGrantRequest{
		CommandId: "cmd-dup", Reason: "grant", AgentId: "agent-1", ToolName: "web_search", Granted: true,
	}

	if _, err := svc.SetToolGrant(opCtx(), req); err != nil {
		t.Fatalf("first call: %v", err)
	}
	ack, err := svc.SetToolGrant(opCtx(), req) // retry
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !ack.GetDeduped() {
		t.Fatal("retry of the same command_id must report deduped")
	}

	rows, _ := audit.Query(context.Background(), operator.AuditFilter{})
	if len(rows) != 1 {
		t.Fatalf("expected exactly one audit row after a retry, got %d", len(rows))
	}
	evs, _ := feed.ReadFrom(0)
	if len(evs) != 1 {
		t.Fatalf("expected exactly one feed event after a retry, got %d", len(evs))
	}
	gs, _ := grants.GrantsFor(context.Background(), "agent-1")
	if len(gs) != 1 {
		t.Fatalf("retry must not duplicate the grant, got %+v", gs)
	}
}

// A mutating command without a reason is rejected.
func TestSetToolGrant_ReasonRequired(t *testing.T) {
	svc, _, _, _ := newCommandService()
	_, err := svc.SetToolGrant(opCtx(), &pb.SetToolGrantRequest{
		CommandId: "cmd-x", AgentId: "agent-1", ToolName: "web_search", Granted: true,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for missing reason, got %v", err)
	}
}

// Revoke removes the tool from the grant set.
func TestSetToolGrant_Revoke(t *testing.T) {
	svc, _, _, grants := newCommandService()
	grants.Set("agent-1", []domain.ToolGrant{{Tool: "web_search"}, {Tool: "file_read"}})

	if _, err := svc.SetToolGrant(opCtx(), &pb.SetToolGrantRequest{
		CommandId: "cmd-rev", Reason: "revoke web_search", AgentId: "agent-1", ToolName: "web_search", Granted: false,
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	gs, _ := grants.GrantsFor(context.Background(), "agent-1")
	if len(gs) != 1 || gs[0].Tool != "file_read" {
		t.Fatalf("expected only file_read after revoke, got %+v", gs)
	}
}
