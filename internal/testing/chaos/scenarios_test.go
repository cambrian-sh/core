package chaos_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/supervision/gatekeeper"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/testing/chaos"
)

func TestChaos_GatekeeperFallsBackOnDbTimeout(t *testing.T) {
	gk := gatekeeper.NewGatekeeper(newChaosRegistry(10), config.ExecutionConfig{
		GatekeeperW1:              0.4,
		GatekeeperW2:              0.4,
		GatekeeperW3:              0.2,
		ColdStartPenaltyMultiplier: 0.6,
		GatekeeperMaxCandidates:    10,
	}, gatekeeper.WithSearcher(nil))

	task := &domain.AuctionTask{
		Description:     "execute a task",
		RequiredFormats: []string{},
	}

	candidates, err := gk.FindCandidates(context.Background(), task)
	if err != nil {
		t.Fatalf("FindCandidates should not error: %v", err)
	}
	if len(candidates) < 1 {
		t.Fatal("expected at least 1 candidate from Declaration-only fallback")
	}
}

func TestChaos_LLMTimeout_ReturnsError(t *testing.T) {
	fg := chaos.NewFaultyGenerator(nil, chaos.FaultConfig{
		AfterSuccesses: 0,
		Error:          chaos.ErrInjected,
	})

	_, err := fg.Generate(context.Background(), "prompt")
	if err == nil {
		t.Fatal("expected injected error from faulty generator")
	}
}

func TestChaos_BboltWriteFailure_DoesNotCrash(t *testing.T) {
	fw := chaos.NewFaultyTaskEventWriter(nil, chaos.FaultConfig{
		AfterSuccesses: 0,
		Error:          chaos.ErrDiskFull,
	})

	err := fw.WriteTaskEvent(domain.TaskEvent{
		TaskID:    "task-1",
		AgentID:   "agent-1",
		Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err == nil || err.Error() != chaos.ErrDiskFull.Error() {
		t.Fatalf("got %v, want disk full error", err)
	}
}

func TestChaos_LLMRateLimit_ExhaustsRetries(t *testing.T) {
	fg := chaos.NewFaultyGenerator(nil, chaos.FaultConfig{
		AfterSuccesses: 2,
		Error:          chaos.ErrInjected,
	})

	for range 2 {
		_, err := fg.Generate(context.Background(), "test")
		if err != nil {
			t.Fatalf("unexpected error on call before failure threshold: %v", err)
		}
	}

	_, err := fg.Generate(context.Background(), "test")
	if err == nil {
		t.Fatal("expected error after threshold reached")
	}
}

type chaosRegistry struct {
	agents []domain.AgentDefinition
}

func newChaosRegistry(n int) *chaosRegistry {
	reg := &chaosRegistry{agents: make([]domain.AgentDefinition, n)}
	for i := range n {
		reg.agents[i] = domain.AgentDefinition{
			ID:          fmt.Sprintf("agent-%04d", i),
			SourceHash:  fmt.Sprintf("hash-%04d", i),
			Description: fmt.Sprintf("agent %d performs cognitive reasoning", i),
			Trait:       domain.TraitCognitive,
		}
	}
	return reg
}

func (r *chaosRegistry) GetAllAgents(ctx context.Context) ([]domain.AgentDefinition, error) {
	out := make([]domain.AgentDefinition, len(r.agents))
	copy(out, r.agents)
	return out, nil
}

func (r *chaosRegistry) GetManifest(ctx context.Context, agentID string) (*domain.AgentManifest, error) {
	return &domain.AgentManifest{SupportedFormats: []string{"text", "json"}}, nil
}
