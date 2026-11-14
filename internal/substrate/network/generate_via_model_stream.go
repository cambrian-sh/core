package network

import (
	"context"
	"strings"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// sessionStore is the narrow interface GenerateViaModelStream needs to look up
// step metadata (agent_id, plan_id, step_index) stored at Acquire time.
type sessionStore interface {
	GetBudgetLeaseState(id domain.LeaseID) *domain.BudgetLeaseState
}

// GenerateViaModelStream is a streaming server-side RPC that acts as a metered
// proxy between a cognitive agent and the LLM provider allocated for its step.
func (s *Server) GenerateViaModelStream(req *pb.GenerateStreamRequest, stream pb.Orchestrator_GenerateViaModelStreamServer) error {
	// Phase 1: prefer the correctly-named lease_id, accept the deprecated
	// session_token_id so an un-upgraded agent keeps working. Both carry the same thing —
	// a per-step BudgetLease — which is exactly why the old name had to go.
	leaseID := leaseIDOf(req.GetLeaseId(), req.GetSessionTokenId())
	if leaseID == "" {
		return status.Error(codes.Unauthenticated, "lease_id is required")
	}
	if s.LLMGateway == nil {
		return status.Error(codes.Unimplemented, "GenerateViaModelStream: LLMGateway not wired")
	}
	// ADR-0139 D8: a lease is bound to the agent it was issued to. Spending under
	// another agent's lease — or, for an agent caller, under a lease bound to no agent —
	// is refused before a token is generated; the answer is the same as for a lease that
	// does not exist.
	if s.leaseForeignToCaller(stream.Context(), leaseID) {
		return status.Error(codes.PermissionDenied, "lease is not valid for this caller")
	}

	// ADR-0139 C1: one structured request, built the same way for both lanes.
	greq, err := rpcRequest{
		system:   req.GetSystem(),
		messages: req.GetMessages(),
		prompt:   req.GetPrompt(), //nolint:staticcheck // deprecated field, deliberate fallback
		schema:   req.GetResponseSchema(),
		options:  req.GetOptions(),
		cacheTTL: req.GetCacheTtl(),
	}.toGenerateRequest()
	if err != nil {
		return err
	}

	// The step-output stream's gateway producer (ADR-0139 amendment 2026-11-06);
	// nil when no delta may flow for this step. When a D11 post_model hook matches
	// the step, the forwarder is nil (the suppression rule) AND the Provider serves
	// the call buffered (llm.Provider.StreamOn), so every chunk below is already the
	// complete post-hook reply and a refusal arrives as the error with no chunk
	// (T-20261106-text-protocol-agent-replies-never-reach-post).
	fwd := s.stepOutputForwarder(stream.Context(), leaseID)

	out := make(chan domain.StreamChunk, 64)
	errCh := make(chan error, 1)

	go func() {
		defer close(out)
		errCh <- s.LLMGateway.StreamChunks(stream.Context(), leaseID, greq, out)
	}()

	var responseBuilder strings.Builder
	var final *domain.GenerateResponse
	for chunk := range out {
		responseBuilder.WriteString(chunk.Text)
		usageTokens := int32(0)
		if chunk.IsFinal {
			usageTokens = int32(chunk.UsageTotalTokens)
		}
		pbChunk := &pb.GenerateChunk{
			Text:        chunk.Text,
			TokenCount:  int32(llm.EstimateTokens(chunk.Text)),
			IsFinal:     chunk.IsFinal,
			UsageTokens: usageTokens,
		}
		// ADR-0139 C1: the final chunk of a normally-ended call carries the whole
		// response — what the generator ignored, the usage breakdown, the stop
		// reason and the serving model — so a streaming agent loses nothing.
		if r := chunk.Response; chunk.IsFinal && r != nil {
			final = r
			pbChunk.Ignored = r.Ignored
			pbChunk.Usage = usageProto(r.Usage)
			pbChunk.StopReason = string(r.StopReason)
			pbChunk.Model = r.Model
		}
		if err := stream.Send(pbChunk); err != nil {
			return err
		}
		// Fork the chunk to the live step-output stream (the operator feed's
		// live-only lane and MCP progress). Best-effort; never affects the agent
		// stream. Formerly ADR-0047 0047-23's TokenSink.
		fwd.text(chunk.Text)
	}

	// After streaming completes, resolve session metadata for logging.
	agentID := callerAgentHeader(stream.Context())
	var modelID string
	var stepIndex int
	modelID = s.exchangeModelID(leaseID, final)

	completion := responseBuilder.String()
	prompt := renderExchangePrompt(greq)

	// OBSERVABILITYREQ REQ1: Log agent LLM call to Langfuse (fire-and-forget).
	if s.AgentCallLogger != nil && completion != "" {
		s.AgentCallLogger.Log(stream.Context(), "agent_llm", prompt, completion, modelID, agentID, stepIndex)
	}

	// ADR-0079: fork the same prompt+completion to the operator feed's live-only
	// exchange lane so a benchmark can review every agent output and reconstruct the
	// internal ReAct loop. Gated (nil unless execution.capture_llm_exchanges); the sink
	// truncates. Fire-and-forget — never affects the agent stream.
	if s.LLMExchangeSink != nil && completion != "" {
		s.LLMExchangeSink(string(leaseID), agentID, modelID, stepIndex, prompt, completion)
	}

	return gatewayStatus(<-errCh)
}

// callerAgentHeader is the calling agent's id from request metadata (set by the
// SDK), for exchange and trace attribution only — never for authorization.
func callerAgentHeader(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("x-agent-id"); len(vals) > 0 {
			return vals[0]
		}
	}
	return ""
}

// exchangeModelID names the model for an exchange record: the one the provider says
// served the call, else the step's allocated winner.
func (s *Server) exchangeModelID(leaseID domain.LeaseID, resp *domain.GenerateResponse) string {
	if resp != nil && resp.Model != "" {
		return resp.Model
	}
	if ss, ok := s.LLMGateway.(sessionStore); ok {
		if state := ss.GetBudgetLeaseState(leaseID); state != nil {
			return state.StepAllocation.Winner.ID
		}
	}
	return ""
}
