package operator

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// AgentInstanceSource lists the live agent processes (agentmgr.AgentManager).
type AgentInstanceSource interface {
	ListAgentInstances() []domain.AgentInstanceInfo
}

// ListAgentInstances reports the live agent processes with their OS pids
// (ENG-1634, contract 0117): operator-only (operatorOnlyReads).
func (s *Service) ListAgentInstances(_ context.Context, req *pb.ListAgentInstancesOpRequest) (*pb.ListAgentInstancesOpResponse, error) {
	if s.agentInstances == nil {
		return nil, status.Error(codes.Unimplemented, "this kernel has no agent manager to list instances from")
	}
	resp := &pb.ListAgentInstancesOpResponse{}
	for _, in := range s.agentInstances.ListAgentInstances() {
		if req.GetAgentId() != "" && in.AgentID != req.GetAgentId() {
			continue
		}
		resp.Instances = append(resp.Instances, &pb.AgentInstanceOp{
			AgentId: in.AgentID, InstanceId: in.InstanceID, StreamId: in.StreamID,
			Mode: string(in.Mode), Pid: int32(in.PID),
		})
	}
	return resp, nil
}
