package operator_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
)

// REACT-03 / ADR-0063: a high-risk watch — an llm condition driving a start_plan /
// dispatch_agent action — cannot be registered without approved=true.
func TestRegisterWatch_HighRiskLLM_RequiresApproval(t *testing.T) {
	h := newFakeWatchHandler()
	svc := newWatchService(h)

	highRisk := func(commandID string, approved bool) *pb.RegisterWatchOpRequest {
		return &pb.RegisterWatchOpRequest{
			CommandId: commandID, Reason: "r",
			Config: &pb.WatchConfigOp{
				Name:          "risky",
				Condition:     "the webhook body indicates an emergency",
				ConditionType: "llm",
				Action:        &pb.WatchActionOp{Type: "start_plan"},
				Active:        true,
				Approved:      approved,
			},
		}
	}

	// Without approval → rejected.
	_, err := svc.RegisterWatch(opCtx(), highRisk("r1", false))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument without approval, got %v", err)
	}

	// With approval → accepted.
	if _, err := svc.RegisterWatch(opCtx(), highRisk("r2", true)); err != nil {
		t.Fatalf("approved high-risk watch should register: %v", err)
	}

	// A low-risk llm watch (non-consequential action) needs no approval.
	if _, err := svc.RegisterWatch(opCtx(), &pb.RegisterWatchOpRequest{
		CommandId: "r3", Reason: "r",
		Config: &pb.WatchConfigOp{
			Name: "safe", Condition: "body mentions gold", ConditionType: "llm",
			Action: &pb.WatchActionOp{Type: "emit_event"}, Active: true,
		},
	}); err != nil {
		t.Fatalf("low-risk llm watch should register without approval: %v", err)
	}

	list, _ := svc.ListWatches(context.Background(), &pb.ListWatchesOpRequest{})
	if list.GetTotal() != 2 {
		t.Fatalf("expected 2 registered watches, got %d", list.GetTotal())
	}
}
