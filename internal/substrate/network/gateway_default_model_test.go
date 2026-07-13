package network

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

type fakeStreamer struct{ called bool }

func (f *fakeStreamer) GenerateStream(_ context.Context, _ string) (<-chan domain.StreamChunk, error) {
	f.called = true
	ch := make(chan domain.StreamChunk, 1)
	ch <- domain.StreamChunk{IsFinal: true, Text: "ok"}
	close(ch)
	return ch, nil
}

// When a step has no allocated model (empty StepAllocation winner — the auction
// returned no TraitModel candidate), StreamChunks falls back to the configured
// default model instead of failing with the misleading "all candidates degraded".
func TestStreamChunks_EmptyAllocation_UsesDefaultModel(t *testing.T) {
	gw := newTestGateway(5)
	def := &fakeStreamer{}
	gw.RegisterModelClient("llm:deepseek", def)
	gw.SetDefaultModelID("llm:deepseek")

	gw.AddSession("s1", &domain.SessionState{
		StepAllocation: domain.StepAllocation{}, // Winner.ID == "" (no model allocated)
		TokenLimit:     4096,
		ExpiresAt:      time.Now().Add(time.Hour),
		LastActivityAt: time.Now(),
	})

	out := make(chan domain.StreamChunk, 8)
	if err := gw.StreamChunks(context.TODO(), "s1", "hi", domain.GenerateOptions{}, out); err != nil {
		t.Fatalf("StreamChunks with default fallback: unexpected err: %v", err)
	}
	if !def.called {
		t.Error("expected the default model streamer to be used for an empty allocation")
	}
}

// Without a default configured, an empty allocation fails with a clear
// "no model allocated" error — not a misleading health/degradation message.
func TestStreamChunks_EmptyAllocation_NoDefault_FailsClearly(t *testing.T) {
	gw := newTestGateway(5)
	gw.AddSession("s2", &domain.SessionState{
		StepAllocation: domain.StepAllocation{},
		TokenLimit:     4096,
		ExpiresAt:      time.Now().Add(time.Hour),
		LastActivityAt: time.Now(),
	})

	out := make(chan domain.StreamChunk, 8)
	err := gw.StreamChunks(context.TODO(), "s2", "hi", domain.GenerateOptions{}, out)
	if err == nil {
		t.Fatal("expected an error when no model is allocated and no default is configured")
	}
	if !strings.Contains(err.Error(), "no model allocated") {
		t.Errorf("expected a 'no model allocated' error, got: %v", err)
	}
}
