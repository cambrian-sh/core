package harness

import (
	"context"
	"fmt"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/infrastructure/llm"
)

// FakeLLMGateway simulates the LLM Gateway for testing.
type FakeLLMGateway struct {
	sessions          map[string]*domain.SessionState
	maxConcurrent     int
	activeCount       int
	chunkCount        int
	chunkContent      string
}

func NewFakeLLMGateway(maxConcurrent int) *FakeLLMGateway {
	return &FakeLLMGateway{
		sessions:      make(map[string]*domain.SessionState),
		maxConcurrent: maxConcurrent,
		chunkCount:    3,
		chunkContent:  "response chunk",
	}
}

func (g *FakeLLMGateway) SetChunkCount(n int)          { g.chunkCount = n }
func (g *FakeLLMGateway) SetChunkContent(s string)     { g.chunkContent = s }
func (g *FakeLLMGateway) SetTokenLimit(sessionID string, limit int) {
	if ss, ok := g.sessions[sessionID]; ok {
		ss.TokenLimit = limit
	}
}

func (g *FakeLLMGateway) Acquire(_ context.Context, _ domain.StepAllocation, tokenLimit int, _ interface{}) (string, error) {
	if g.maxConcurrent > 0 && g.activeCount >= g.maxConcurrent {
		return "", fmt.Errorf("ErrGatewayOverloaded")
	}
	g.activeCount++
	sessionID := fmt.Sprintf("sess-%d", len(g.sessions))
	g.sessions[sessionID] = &domain.SessionState{TokenLimit: tokenLimit}
	return sessionID, nil
}

func (g *FakeLLMGateway) Complete(_ context.Context, sessionID string) (llm.TokenUsage, error) {
	g.activeCount--
	return llm.TokenUsage{TotalTokens: 100}, nil
}

func (g *FakeLLMGateway) EvictExpired() {}

func (g *FakeLLMGateway) StreamChunks(ctx context.Context, sessionID string, prompt string, opts domain.GenerateOptions, out chan<- domain.StreamChunk) error {
	for i := 0; i < g.chunkCount; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- domain.StreamChunk{Text: g.chunkContent, IsFinal: i == g.chunkCount-1}:
		}
	}
	return nil
}
