package network

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
)

// GenerateWithTools serves one managed generation turn with native tool-calling
// (ADR-0097 Phase B), as a C1 request (ADR-0139 D1): system blocks, the message
// conversation, tools, a response schema and options all reach the adapter.
//
// Unary rather than streaming: a tool call is only actionable once complete, so
// streaming partial calls would force every client to reassemble them for no gain.
// Text-only generation keeps using GenerateViaModelStream. The OPERATOR still sees
// the turn live: since the ADR-0139 amendment of 2026-11-06 the kernel streams from
// the provider internally and forwards text and the final-answer argument to the
// step-output stream (step_output.go).
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
	// ADR-0139 D8, same rule as GenerateViaModelStream: an agent may spend only under a
	// lease bound to itself. This lane had no ownership check at all, so a stolen or
	// unbound lease generated freely here
	// (T-20261104-leases-minted-with-no-agentid-are-usable-by-any).
	if s.leaseForeignToCaller(ctx, leaseID) {
		return nil, status.Error(codes.PermissionDenied, "lease is not valid for this caller")
	}

	greq, err := rpcRequest{
		system:   req.GetSystem(),
		messages: req.GetMessages(),
		prompt:   req.GetPrompt(), //nolint:staticcheck // deprecated field, deliberate fallback
		tools:    req.GetTools(),
		schema:   req.GetResponseSchema(),
		options:  req.GetOptions(),
		cacheTTL: req.GetCacheTtl(),
	}.toGenerateRequest()
	if err != nil {
		return nil, err
	}

	// ADR-0139 amendment 2026-11-06: the turn's live output goes to the step-output
	// stream while it is generated (streamed from the provider internally); the
	// RPC's answer is unchanged. nil forwarder ⇒ served unary, as before.
	resp, err := generateWithTools(ctx, s.LLMGateway, leaseID, greq, s.stepOutputForwarder(ctx, leaseID))
	if err != nil {
		// A model that cannot do tool-calling is a CAPABILITY answer, not a failure.
		// FailedPrecondition is the documented signal for "fall back to the
		// prompt-encoded action protocol" — mapping it to Internal would make an
		// ordinary deployment look broken and push clients into retry loops.
		return nil, gatewayStatus(err)
	}

	// ADR-0079: the exchange lane captures tool turns too — with a native-tools
	// generator the SDK's ReAct loop runs here, so without this a benchmark told
	// "every agent turn is captured" saw none of them
	// (T-20261104-llm-exchange-capture-adr-0079-misses-every). Gated and truncated by
	// the sink; fire-and-forget.
	if s.LLMExchangeSink != nil {
		if completion := renderExchangeCompletion(resp); completion != "" {
			s.LLMExchangeSink(string(leaseID), callerAgentHeader(ctx), s.exchangeModelID(leaseID, &resp), 0,
				renderExchangePrompt(greq), completion)
		}
	}

	out := &pb.GenerateWithToolsResponse{
		Text: resp.Text,
		// The stop reason is already normalized at the adapter, so no provider
		// vocabulary crosses this boundary.
		StopReason:  string(resp.StopReason),
		UsageTokens: int32(resp.Usage.Input + resp.Usage.Output),
		Ignored:     resp.Ignored,
		Usage:       usageProto(resp.Usage),
		Model:       resp.Model,
	}
	for _, tc := range resp.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, &pb.ModelToolCallProto{
			// Verbatim: providers correlate the tool result on their own id and
			// reject a synthesized one.
			Id:            tc.ID,
			Name:          tc.Name,
			ArgumentsJson: string(tc.Arguments),
		})
	}
	return out, nil
}
