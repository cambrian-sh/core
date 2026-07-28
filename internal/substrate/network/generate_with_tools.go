package network

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// GenerateWithTools serves one managed generation turn with native tool-calling
// (ADR-0097 Phase B).
//
// Unary rather than streaming: a tool call is only actionable once complete, so
// streaming partial calls would force every client to reassemble them for no gain.
// Text-only generation keeps using GenerateViaModelStream.
func (s *Server) GenerateWithTools(
	ctx context.Context, req *pb.GenerateWithToolsRequest,
) (*pb.GenerateWithToolsResponse, error) {
	leaseID := leaseIDOf(req.GetLeaseId(), "")
	if leaseID == "" {
		return nil, status.Error(codes.Unauthenticated, "lease_id is required")
	}
	if s.LLMGateway == nil {
		return nil, status.Error(codes.Unimplemented, "GenerateWithTools: LLMGateway not wired")
	}

	opts := domain.GenerateOptions{}
	if req.Options != nil {
		opts.MaxTokens = req.Options.MaxTokens
		opts.Temperature = req.Options.Temperature
		opts.StopSequences = req.Options.StopSequences
	}

	tools := make([]domain.ToolDefinition, 0, len(req.GetTools()))
	for _, t := range req.GetTools() {
		tools = append(tools, domain.ToolDefinition{
			Name:        t.GetName(),
			Description: t.GetDescription(),
			Parameters:  []byte(t.GetParametersJson()),
		})
	}

	// Prefer the message list; fall back to wrapping the deprecated prompt as a lone
	// user turn so an un-upgraded client still works rather than sending nothing.
	messages := make([]domain.ModelMessage, 0, len(req.GetMessages()))
	for _, m := range req.GetMessages() {
		msg := domain.ModelMessage{
			Role:       m.GetRole(),
			Content:    m.GetContent(),
			ToolCallID: m.GetToolCallId(),
		}
		for _, tc := range m.GetToolCalls() {
			msg.ToolCalls = append(msg.ToolCalls, domain.ModelToolCall{
				ID:   tc.GetId(),
				Name: tc.GetName(),
				// Forwarded as the provider's own bytes: re-encoding would reorder
				// keys and change what the provider matches its record of the call
				// against.
				Arguments: []byte(tc.GetArgumentsJson()),
			})
		}
		messages = append(messages, msg)
	}
	if len(messages) == 0 {
		if p := req.GetPrompt(); p != "" { //nolint:staticcheck // deprecated field, deliberate fallback
			messages = append(messages, domain.UserMessage(p))
		}
	}
	if len(messages) == 0 {
		return nil, status.Error(codes.InvalidArgument, "messages (or the deprecated prompt) is required")
	}

	turn, err := s.LLMGateway.GenerateWithTools(ctx, leaseID, messages, opts, tools)
	if err != nil {
		// A model that cannot do tool-calling is a CAPABILITY answer, not a failure.
		// FailedPrecondition is the documented signal for "fall back to the
		// prompt-encoded action protocol" — mapping it to Internal would make an
		// ordinary deployment look broken and push clients into retry loops.
		if errors.Is(err, ErrToolCallingUnsupported) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}

	resp := &pb.GenerateWithToolsResponse{
		Text: turn.Text,
		// The stop reason is already normalized at the adapter, so no provider
		// vocabulary crosses this boundary.
		StopReason: string(turn.StopReason),
	}
	for _, tc := range turn.ToolCalls {
		resp.ToolCalls = append(resp.ToolCalls, &pb.ModelToolCallProto{
			// Verbatim: providers correlate the tool result on their own id and
			// reject a synthesized one.
			Id:            tc.ID,
			Name:          tc.Name,
			ArgumentsJson: string(tc.Arguments),
		})
	}
	return resp, nil
}
