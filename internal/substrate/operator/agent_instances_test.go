package operator

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

type fakeInstances []domain.AgentInstanceInfo

func (f fakeInstances) ListAgentInstances() []domain.AgentInstanceInfo { return f }

// ENG-1634 (contract 0117): the live agent processes with their pids, filtered by
// agent; operator-only; Unimplemented without an agent manager.
func TestListAgentInstances_ListsFiltersAndIsOperatorOnly(t *testing.T) {
	svc := NewService(NewSpool(SpoolConfig{}))
	if _, err := svc.ListAgentInstances(context.Background(), &pb.ListAgentInstancesOpRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("no agent manager: %v", err)
	}
	svc.SetAgentInstances(fakeInstances{
		{AgentID: "chat_agent", InstanceID: "i1", StreamID: "chat-0", Mode: domain.ModeDaemon, PID: 4242},
		{AgentID: "planner", InstanceID: "i2", Mode: domain.ModeJIT, PID: 77},
	})
	all, err := svc.ListAgentInstances(context.Background(), &pb.ListAgentInstancesOpRequest{})
	if err != nil || len(all.GetInstances()) != 2 {
		t.Fatalf("all = %+v, %v", all, err)
	}
	one, _ := svc.ListAgentInstances(context.Background(), &pb.ListAgentInstancesOpRequest{AgentId: "chat_agent"})
	if len(one.GetInstances()) != 1 {
		t.Fatalf("filtered = %+v", one)
	}
	got := one.GetInstances()[0]
	if got.GetPid() != 4242 || got.GetStreamId() != "chat-0" || got.GetMode() != "daemon" || got.GetInstanceId() != "i1" {
		t.Fatalf("instance = %+v", got)
	}
	if !isOperatorOnly(operatorMethodPrefix+"ListAgentInstances", nil) {
		t.Fatal("ListAgentInstances discloses the process layout: it must be operator-only, not a member read")
	}
}
