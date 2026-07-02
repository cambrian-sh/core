package network

import (
	"fmt"
	"strings"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/infrastructure/llm"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// sessionStore is the narrow interface GenerateViaModelStream needs to look up
// step metadata (agent_id, plan_id, step_index) stored at Acquire time.
type sessionStore interface {
	GetSessionState(id string) *domain.SessionState
}

// GenerateViaModelStream is a streaming server-side RPC that acts as a metered
// proxy between a cognitive agent and the LLM provider allocated for its step.
func (s *Server) GenerateViaModelStream(req *pb.GenerateStreamRequest, stream pb.Orchestrator_GenerateViaModelStreamServer) error {
	if req.SessionTokenId == "" {
		return status.Error(codes.Unauthenticated, "session_token_id is required")
	}
	if s.LLMGateway == nil {
		return status.Error(codes.Unimplemented, "GenerateViaModelStream: LLMGateway not wired")
	}

	opts := domain.GenerateOptions{}
	if req.Options != nil {
		opts.MaxTokens = req.Options.MaxTokens
		opts.Temperature = req.Options.Temperature
		opts.StopSequences = req.Options.StopSequences
	}

	out := make(chan domain.StreamChunk, 64)
	errCh := make(chan error, 1)

	go func() {
		defer close(out)
		errCh <- s.LLMGateway.StreamChunks(stream.Context(), req.SessionTokenId, req.Prompt, opts, out)
	}()

	var responseBuilder strings.Builder
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
		if err := stream.Send(pbChunk); err != nil {
			return err
		}
		// ADR-0047 0047-23: fork the chunk to the operator feed's live-only token
		// lane (best-effort; never affects the agent stream).
		if s.TokenSink != nil && chunk.Text != "" {
			s.TokenSink(req.SessionTokenId, 0, chunk.Text)
		}
	}

	// After streaming completes, resolve session metadata for logging.
	var agentID, modelID string
	var stepIndex int
	// Calling agent ID from gRPC request metadata (set by Python SDK).
	if md, ok := metadata.FromIncomingContext(stream.Context()); ok {
		if vals := md.Get("x-agent-id"); len(vals) > 0 {
			agentID = vals[0]
		}
	}
	if ss, ok := s.LLMGateway.(sessionStore); ok {
		if state := ss.GetSessionState(req.SessionTokenId); state != nil {
			// Winner is the TraitModel agent allocated for this step.
			modelID = state.StepAllocation.Winner.ID
		}
	}

	completion := responseBuilder.String()

	// Persist full prompt+response as a neural trace in pgvector.
	neuralTrace := fmt.Sprintf("PROMPT:\n%s\n\nRESPONSE:\n%s", req.Prompt, completion)
	if s.VectorStore != nil && neuralTrace != "" {
		storeNeuralTrace(stream.Context(), s.VectorStore, neuralTrace, req.SessionTokenId, "", stepIndex, 0, agentID)
	}

	// OBSERVABILITYREQ REQ1: Log agent LLM call to Langfuse (fire-and-forget).
	if s.AgentCallLogger != nil && completion != "" {
		s.AgentCallLogger.Log(stream.Context(), "agent_llm", req.Prompt, completion, modelID, agentID, stepIndex)
	}

	err := <-errCh
	if err != nil {
		switch err.Error() {
		case "model_unavailable":
			return status.Error(codes.Unavailable, err.Error())
		case "gateway_overflow":
			return status.Error(codes.ResourceExhausted, "gateway overloaded")
		case "session not found":
			return status.Error(codes.NotFound, err.Error())
		default:
			return status.Error(codes.Internal, err.Error())
		}
	}
	return nil
}
