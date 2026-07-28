package network

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
	"github.com/cambrian-sh/core/domain"
)

type toolGateway struct {
	turn      domain.ModelTurn
	err       error
	gotTools  []domain.ToolDefinition
	gotMessages []domain.ModelMessage
}

func (g *toolGateway) Acquire(context.Context, domain.StepAllocation, int, time.Duration) (domain.LeaseID, error) {
	return "", nil
}
func (g *toolGateway) Complete(context.Context, domain.LeaseID) (llm.TokenUsage, error) {
	return llm.TokenUsage{}, nil
}
func (g *toolGateway) EvictExpired() {}
func (g *toolGateway) StreamChunks(context.Context, domain.LeaseID, string, domain.GenerateOptions, chan<- domain.StreamChunk) error {
	return nil
}
func (g *toolGateway) GenerateWithTools(_ context.Context, _ domain.LeaseID, msgs []domain.ModelMessage, _ domain.GenerateOptions, tools []domain.ToolDefinition) (domain.ModelTurn, error) {
	g.gotMessages, g.gotTools = msgs, tools
	return g.turn, g.err
}

func TestGenerateWithTools_MapsTurnToProto(t *testing.T) {
	gw := &toolGateway{turn: domain.ModelTurn{
		Text:       "writing it",
		StopReason: domain.StopToolUse,
		ToolCalls: []domain.ModelToolCall{{
			ID: "call_abc", Name: "write_file", Arguments: []byte(`{"path":"x"}`),
		}},
	}}
	s := &Server{LLMGateway: gw}

	resp, err := s.GenerateWithTools(context.Background(), &pb.GenerateWithToolsRequest{
		LeaseId:  "lease-1",
		Messages: []*pb.ModelMessageProto{{Role: "user", Content: "write a file"}},
		Tools: []*pb.ToolDefinitionProto{{
			Name: "write_file", Description: "write", ParametersJson: `{"type":"object"}`,
		}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.StopReason != string(domain.StopToolUse) {
		t.Errorf("stop_reason = %q", resp.StopReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(resp.ToolCalls))
	}
	// The provider id must cross the boundary verbatim — a synthesized one is
	// rejected when the result is correlated back.
	if resp.ToolCalls[0].Id != "call_abc" {
		t.Errorf("tool call id = %q, want it preserved", resp.ToolCalls[0].Id)
	}
	if resp.ToolCalls[0].ArgumentsJson != `{"path":"x"}` {
		t.Errorf("arguments = %q", resp.ToolCalls[0].ArgumentsJson)
	}
	// Tool definitions must reach the gateway, schema intact.
	if len(gw.gotTools) != 1 || string(gw.gotTools[0].Parameters) != `{"type":"object"}` {
		t.Errorf("tools did not reach the gateway intact: %+v", gw.gotTools)
	}
}

// An unsupported model is a CAPABILITY answer. It must be FailedPrecondition so the
// client falls back to the prompt-encoded protocol; Internal would make an ordinary
// deployment look broken and push clients into retry loops.
func TestGenerateWithTools_UnsupportedIsFailedPrecondition(t *testing.T) {
	s := &Server{LLMGateway: &toolGateway{err: ErrToolCallingUnsupported}}

	_, err := s.GenerateWithTools(context.Background(), &pb.GenerateWithToolsRequest{LeaseId: "l",
		Messages: []*pb.ModelMessageProto{{Role: "user", Content: "hi"}}})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v (%v)", status.Code(err), err)
	}
}

// A real generation failure must NOT be mistaken for a capability answer, or an
// outage would silently downgrade every agent to the text path.
func TestGenerateWithTools_RealErrorIsInternal(t *testing.T) {
	s := &Server{LLMGateway: &toolGateway{err: errors.New("model exploded")}}

	_, err := s.GenerateWithTools(context.Background(), &pb.GenerateWithToolsRequest{LeaseId: "l",
		Messages: []*pb.ModelMessageProto{{Role: "user", Content: "hi"}}})
	if status.Code(err) != codes.Internal {
		t.Fatalf("want Internal, got %v (%v)", status.Code(err), err)
	}
}

func TestGenerateWithTools_RequiresLease(t *testing.T) {
	s := &Server{LLMGateway: &toolGateway{}}
	if _, err := s.GenerateWithTools(context.Background(), &pb.GenerateWithToolsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated, got %v", status.Code(err))
	}
}

// ADR-0097 D8: the conversation must cross the boundary intact. An assistant turn and
// its tool result are what let the model see that its own call happened; the first cut
// sent one user message per round and the model re-explored forever.
func TestGenerateWithTools_CarriesTheConversation(t *testing.T) {
	gw := &toolGateway{turn: domain.ModelTurn{Text: "done", StopReason: domain.StopEndTurn}}
	s := &Server{LLMGateway: gw}

	_, err := s.GenerateWithTools(context.Background(), &pb.GenerateWithToolsRequest{
		LeaseId: "l",
		Messages: []*pb.ModelMessageProto{
			{Role: "user", Content: "write it"},
			{Role: "assistant", ToolCalls: []*pb.ModelToolCallProto{{
				Id: "call_1", Name: "write_file", ArgumentsJson: `{"path":"x"}`,
			}}},
			{Role: "tool", ToolCallId: "call_1", Content: `{"ok":true}`},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gw.gotMessages) != 3 {
		t.Fatalf("want 3 messages through the boundary, got %d", len(gw.gotMessages))
	}
	asst := gw.gotMessages[1]
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "call_1" {
		t.Fatalf("assistant turn lost its tool_calls: %+v", asst)
	}
	if string(asst.ToolCalls[0].Arguments) != `{"path":"x"}` {
		t.Errorf("arguments must cross verbatim, got %q", asst.ToolCalls[0].Arguments)
	}
	// The correlation id is the entire point of the tool turn.
	if gw.gotMessages[2].ToolCallID != "call_1" {
		t.Errorf("tool turn lost its tool_call_id: %+v", gw.gotMessages[2])
	}
}

// The deprecated single prompt still works, wrapped as a lone user turn — an
// un-upgraded client must degrade, not send nothing.
func TestGenerateWithTools_DeprecatedPromptStillWorks(t *testing.T) {
	gw := &toolGateway{turn: domain.ModelTurn{StopReason: domain.StopEndTurn}}
	s := &Server{LLMGateway: gw}

	if _, err := s.GenerateWithTools(context.Background(), &pb.GenerateWithToolsRequest{
		LeaseId: "l", Prompt: "just a prompt",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gw.gotMessages) != 1 || gw.gotMessages[0].Role != domain.RoleUser {
		t.Fatalf("prompt must wrap as one user turn, got %+v", gw.gotMessages)
	}
}

func TestGenerateWithTools_EmptyConversationIsRejected(t *testing.T) {
	s := &Server{LLMGateway: &toolGateway{}}
	_, err := s.GenerateWithTools(context.Background(), &pb.GenerateWithToolsRequest{LeaseId: "l"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", status.Code(err))
	}
}
