package backfill

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// AgentLister returns all registered agents.
type AgentLister interface {
	GetAllAgents(ctx context.Context) ([]domain.AgentDefinition, error)
}

// ProfileChecker checks whether an AgentProfile exists for (agentID, sourceHash).
type ProfileChecker interface {
	GetProfile(ctx context.Context, agentID, sourceHash string) (*domain.AgentProfile, error)
}

// BackfillEnqueuer enqueues an agent for Interview processing.
type BackfillEnqueuer interface {
	Enqueue(agent domain.AgentDefinition)
}

// EmbedderHealth verifies the embedder is reachable.
type EmbedderHealth interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// BackfillConfig holds tuning knobs for the startup migration pass.
type BackfillConfig struct {
	TimeoutMs        int // total retry window; default 60000
	InitialBackoffMs int // first retry delay; default 1000
}

// RunInterviewBackfill ensures every registered agent has an Interview Vector.
// It health-checks the embedder with exponential backoff before enqueuing.
// Returns after all Enqueue calls complete; Interview processing is async.
func RunInterviewBackfill(
	ctx context.Context,
	agents AgentLister,
	profiles ProfileChecker,
	enqueuer BackfillEnqueuer,
	embedder EmbedderHealth,
	cfg BackfillConfig,
) error {
	timeout := time.Duration(cfg.TimeoutMs) * time.Millisecond
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	backoff := time.Duration(cfg.InitialBackoffMs) * time.Millisecond
	if backoff == 0 {
		backoff = time.Second
	}

	deadline := time.Now().Add(timeout)

	// Health-check embedder with exponential backoff.
	for {
		_, err := embedder.Embed(ctx, "health")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("interview backfill: embedder unavailable after %v: %w", timeout, err)
		}
		slog.Warn("interview backfill: embedder unavailable, retrying", "err", err, "backoff", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}

	// Enqueue agents that have no Interview Vector.
	allAgents, err := agents.GetAllAgents(ctx)
	if err != nil {
		return fmt.Errorf("interview backfill: list agents: %w", err)
	}

	var enqueued, skipped int
	for _, agent := range allAgents {
		// The registry (bbolt) is the source of truth for whether an agent
		// needs interviewing. A profile in pgvector from a previous run does
		// not mean the agent is ready if the registry says it is still
		// provisional (e.g. after a data-dir wipe). ADR-0023.
		if !agent.Provisional {
			skipped++
			continue
		}
		enqueuer.Enqueue(agent)
		enqueued++
	}

	slog.Info("interview backfill complete", "enqueued", enqueued, "skipped", skipped)
	return nil
}
