package operator

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
)

// WatchToolApprovals streams pending dangerous-tool approval requests to an
// operator (ADR-0047 Amendment A2.5). It mirrors the ADR-0039 ApprovalRequest
// shape (tool_name / agent_id / args_preview) that the CLI's Approvals pane
// renders — deliberately NOT HITLRaisedOp, which lacks those fields. Operator-only
// (the interceptor gates it; the agent plane can never watch/approve tools).
//
// Resolution reuses ResolveHITL: it Submits over the SAME ApprovalHub id-space
// (the approval request ID == the ResolveHITL intervention_id). See
// TestWatchToolApprovals_SharesApprovalHubIDSpace.
func (s *Service) WatchToolApprovals(_ *pb.SubscribeRequest, stream pb.OperatorConsole_WatchToolApprovalsServer) error {
	if s.hitl == nil {
		return status.Error(codes.Unimplemented, "operator approval hub not configured")
	}
	ch, cancel := s.hitl.Watch()
	defer cancel()
	if ch == nil {
		// A hub that yields no channel has nothing to stream; block until the client
		// disconnects rather than busy-returning.
		<-stream.Context().Done()
		return nil
	}
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case r, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(&pb.ApprovalOp{
				Id:          r.ID,
				AgentId:     r.AgentID,
				ToolName:    r.ToolName,
				ArgsPreview: r.ArgsPreview,
				// An approval request is raised ONLY for a dangerous tool (ADR-0039),
				// so every streamed approval is destructive by construction.
				IsDestructive: true,
			}); err != nil {
				return err
			}
		}
	}
}
