package network

import (
	"context"
	"strconv"
	"strings"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// agentIDFromMetadata reads the non-forgeable caller principal from x-agent-id
// gRPC metadata (the same mechanism QueryMemory uses). Empty when absent.
func agentIDFromMetadata(ctx context.Context) string {
	return mdValue(ctx, "x-agent-id")
}

// toolQueryFromMetadata reads the ADR-0044 relevance query (x-tool-query). Empty
// ⇒ the caller wants the full menu, not ranked retrieval.
func toolQueryFromMetadata(ctx context.Context) string {
	return mdValue(ctx, "x-tool-query")
}

// toolKFromMetadata reads the ADR-0044 menu size (x-tool-k); default 3.
func toolKFromMetadata(ctx context.Context) int {
	if v := mdValue(ctx, "x-tool-k"); v != "" {
		if k, err := strconv.Atoi(v); err == nil && k > 0 {
			return k
		}
	}
	return 3
}

// toolFullFromMetadata reads the ADR-0045 two-tier flag (x-tool-full). Absent or
// not "true" ⇒ Tier-1 (the terse menu); "true" ⇒ Tier-2 (full spec, the
// describe_tool path). Same metadata-transport convention as x-tool-query/k.
func toolFullFromMetadata(ctx context.Context) bool {
	return mdValue(ctx, "x-tool-full") == "true"
}

// toolNamesFromMetadata reads the ADR-0045 describe_tool target names
// (x-tool-names, comma-separated). Present ⇒ serve only those named (granted)
// tools; empty entries are dropped.
func toolNamesFromMetadata(ctx context.Context) []string {
	v := mdValue(ctx, "x-tool-names")
	if v == "" {
		return nil
	}
	var names []string
	for _, n := range strings.Split(v, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return names
}

func mdValue(ctx context.Context, key string) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get(key); len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// ExecuteTool is the kernel-owned tool reference monitor RPC (ADR-0039 D4). The
// agent marshalled the args; the kernel authorizes (grant + resource policy +
// scope + approval) and runs the tool in a confined process. The principal is
// taken from x-agent-id metadata, never from the request body.
func (s *Server) ExecuteTool(ctx context.Context, req *pb.ExecuteToolRequest) (*pb.ExecuteToolResponse, error) {
	if s.ToolExecutor == nil {
		return nil, status.Error(codes.Unimplemented, "tool registry not configured")
	}
	resp := s.ToolExecutor.Execute(ctx, domain.ToolCallRequest{
		AgentID:        agentIDFromMetadata(ctx),
		ToolName:       req.GetToolName(),
		ArgsJSON:       []byte(req.GetArgsJson()),
		SessionTokenID: req.GetSessionTokenId(),
		TaskID:         mdValue(ctx, "x-task-id"), // ADR-0049 D3: per-step correlation key
	})
	return &pb.ExecuteToolResponse{
		ResultJson: string(resp.ResultJSON),
		ResultCid:  resp.ResultCID,
		Denied:     resp.Denied,
		DenyReason: resp.DenyReason,
		Error:      resp.Error,
		ArgHash:    resp.ArgHash,
		ResultHash: resp.ResultHash,
	}, nil
}

// ListTools returns the system tools the calling agent may invoke, for building
// the agent's closed ReAct tool menu (ADR-0039). The principal is the
// x-agent-id metadata; the menu is advisory (ExecuteTool still authorizes every
// call). A missing executor yields an empty menu, never an error — the agent
// simply sees no system tools and reasons accordingly.
func (s *Server) ListTools(ctx context.Context, _ *pb.ListToolsRequest) (*pb.ListToolsResponse, error) {
	if s.ToolExecutor == nil {
		return &pb.ListToolsResponse{}, nil
	}
	// ADR-0044: when the caller supplies a relevance query (via x-tool-query
	// metadata), serve the top-k task-relevant tools instead of the full menu.
	// k from x-tool-k (default 3). No query ⇒ the full granted menu (unchanged).
	agentID := agentIDFromMetadata(ctx)
	var tools []domain.SystemTool
	switch {
	case len(toolNamesFromMetadata(ctx)) > 0:
		// ADR-0045 describe_tool: full spec for the named granted tools only
		// (grant-gated, fail-closed — ungranted names are simply absent).
		tools = s.ToolExecutor.AvailableToolsNamed(ctx, agentID, toolNamesFromMetadata(ctx))
	case toolQueryFromMetadata(ctx) != "":
		tools = s.ToolExecutor.AvailableToolsRanked(ctx, agentID, toolQueryFromMetadata(ctx), toolKFromMetadata(ctx))
	default:
		tools = s.ToolExecutor.AvailableTools(ctx, agentID)
	}
	// ADR-0045: serve Tier-1 (terse summary + arg-names-only schema) by default;
	// Tier-2 (full description + full schema) only when x-tool-full is set (the
	// describe_tool path). One render function (domain.ToolDisclosure) keeps the
	// served short form identical to the embedded one.
	full := toolFullFromMetadata(ctx)
	out := make([]*pb.ToolDescriptor, 0, len(tools))
	for _, t := range tools {
		desc, schemaJSON := domain.ToolDisclosure(t, full)
		out = append(out, &pb.ToolDescriptor{
			Name:        t.Name,
			Description: desc,
			SchemaJson:  schemaJSON,
			Dangerous:   t.Dangerous,
		})
	}
	return &pb.ListToolsResponse{Tools: out}, nil
}

// WatchApprovals streams pending dangerous-tool approval requests to a subscribed
// operator (ADR-0039 D10). Operator-plane only: a caller presenting an agent
// principal (x-agent-id) is rejected — an agent can never watch/approve tools.
func (s *Server) WatchApprovals(_ *pb.WatchApprovalsRequest, stream pb.Orchestrator_WatchApprovalsServer) error {
	if s.ApprovalHub == nil {
		return status.Error(codes.Unimplemented, "approval channel not configured")
	}
	if agentIDFromMetadata(stream.Context()) != "" {
		return status.Error(codes.PermissionDenied, "approval plane is operator-only")
	}
	ch, cancel := s.ApprovalHub.Watch()
	defer cancel()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case r, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(&pb.ApprovalRequest{
				Id: r.ID, AgentId: r.AgentID, ToolName: r.ToolName, ArgsPreview: r.ArgsPreview,
			}); err != nil {
				return err
			}
		}
	}
}

// SubmitApprovalDecision resolves a pending approval (ADR-0039 D10). Operator-plane
// only — an agent principal is rejected.
func (s *Server) SubmitApprovalDecision(ctx context.Context, req *pb.ApprovalDecisionRequest) (*pb.ApprovalDecisionResponse, error) {
	if s.ApprovalHub == nil {
		return nil, status.Error(codes.Unimplemented, "approval channel not configured")
	}
	if agentIDFromMetadata(ctx) != "" {
		return nil, status.Error(codes.PermissionDenied, "approval plane is operator-only")
	}
	ok := s.ApprovalHub.Submit(req.GetId(), req.GetApprove(), req.GetApproverId())
	return &pb.ApprovalDecisionResponse{Ok: ok}, nil
}
