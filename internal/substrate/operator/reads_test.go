package operator_test

import (
	"context"
	"strings"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

func seedTwoGrants(t *testing.T, svc *operator.Service) {
	t.Helper()
	if _, err := svc.SetToolGrant(opCtx(), &pb.SetToolGrantRequest{
		CommandId: "c1", Reason: "r1", AgentId: "agent-1", ToolName: "web_search", Granted: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetToolGrant(opCtx(), &pb.SetToolGrantRequest{
		CommandId: "c2", Reason: "r2", AgentId: "agent-2", ToolName: "file_read", Granted: true,
	}); err != nil {
		t.Fatal(err)
	}
}

// QueryAudit returns all entries, and filters by target id.
func TestQueryAudit_FilterByTarget(t *testing.T) {
	svc, _, _, _ := newCommandService()
	seedTwoGrants(t, svc)

	all, err := svc.QueryAudit(context.Background(), &pb.QueryAuditRequest{})
	if err != nil || len(all.GetEntries()) != 2 {
		t.Fatalf("expected 2 audit entries, got %d err=%v", len(all.GetEntries()), err)
	}

	one, err := svc.QueryAudit(context.Background(), &pb.QueryAuditRequest{TargetId: "agent-2"})
	if err != nil || len(one.GetEntries()) != 1 || one.GetEntries()[0].GetTargetId() != "agent-2" {
		t.Fatalf("expected 1 entry for agent-2, got %+v err=%v", one.GetEntries(), err)
	}
}

// Export renders entries as CSV (header + rows) and JSON.
func TestAuditExport_CSVAndJSON(t *testing.T) {
	entries := []domain.AuditEntry{
		{ID: "1", CommandID: "c1", Actor: "alice", ActionType: "set_tool_grant", TargetID: "agent-1", Reason: "r1"},
	}

	csvOut, err := operator.AuditToCSV(entries)
	if err != nil {
		t.Fatalf("csv: %v", err)
	}
	if !strings.Contains(csvOut, "command_id") || !strings.Contains(csvOut, "alice") || !strings.Contains(csvOut, "agent-1") {
		t.Fatalf("CSV export missing header/data: %q", csvOut)
	}

	jsonOut, err := operator.AuditToJSON(entries)
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	if !strings.Contains(string(jsonOut), "\"Actor\": \"alice\"") {
		t.Fatalf("JSON export missing data: %s", jsonOut)
	}
}
