package harness

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cambrian-sh/core/domain"
)

// FakeModelStreamer is the model behind the harness's REAL LLM gateway: it
// implements domain.LLMStreamer (what OllamaClient/OpenAIClient implement) and
// streams the next scripted FakeGenerator response as chunks of ChunkChars
// characters. Driving the real SubstrateLLMGateway through a fake MODEL, rather
// than faking the gateway, is what lets the integration suite observe the
// gateway's own CONWIP semaphore and per-step budget cut-off instead of a copy.
//
// Gate, when non-nil, holds every stream open until the channel is closed, so a
// test can pin N steps inside the gateway's critical section and see what the
// semaphore does to the N+1th. Entered counts streams that have been opened.
type FakeModelStreamer struct {
	Generator  *FakeGenerator
	ChunkChars int // characters per streamed chunk; 0 means 16
	Gate       chan struct{}

	entered atomic.Int32
	mu      sync.Mutex
	prompts []string
}

func NewFakeModelStreamer(gen *FakeGenerator) *FakeModelStreamer {
	return &FakeModelStreamer{Generator: gen}
}

// Entered is the number of streams opened so far (gated ones included).
func (s *FakeModelStreamer) Entered() int { return int(s.entered.Load()) }

// Prompts returns every prompt the gateway handed this model, in order.
func (s *FakeModelStreamer) Prompts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.prompts...)
}

func (s *FakeModelStreamer) GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error) {
	s.entered.Add(1)
	s.mu.Lock()
	s.prompts = append(s.prompts, prompt)
	s.mu.Unlock()

	if s.Gate != nil {
		select {
		case <-s.Gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	text, err := s.Generator.Generate(ctx, prompt)
	if err != nil {
		return nil, err
	}
	size := s.ChunkChars
	if size <= 0 {
		size = 16
	}
	var parts []string
	for len(text) > size {
		parts = append(parts, text[:size])
		text = text[size:]
	}
	parts = append(parts, text)

	// Pre-filled and closed: no producer goroutine, so nothing can leak.
	out := make(chan domain.StreamChunk, len(parts))
	for i, p := range parts {
		out <- domain.StreamChunk{Text: p, IsFinal: i == len(parts)-1}
	}
	close(out)
	return out, nil
}

var _ domain.LLMStreamer = (*FakeModelStreamer)(nil)

// chunksText joins the chunks a gateway stream delivered to the agent side.
func chunksText(chunks []domain.StreamChunk) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString(c.Text)
	}
	return b.String()
}
