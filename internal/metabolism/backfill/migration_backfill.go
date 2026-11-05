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

// ProfileChecker checks an agent's stored profile state for (agentID, sourceHash).
type ProfileChecker interface {
	GetProfile(ctx context.Context, agentID, sourceHash string) (*domain.AgentProfile, error)
	// HasProfileVector reports whether a NON-EMPTY profile embedding is stored. A
	// profile row can exist with a null/empty vector (e.g. written while the embedder was
	// down), which makes the agent invisible to the Gatekeeper's Layer-2 semantic gate.
	HasProfileVector(ctx context.Context, agentID, sourceHash string) (bool, error)
}

// BackfillEnqueuer enqueues an agent for profile seeding (registration.ProfileSeeder).
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

// RunProfileBackfill ensures every registered agent has a profile vector (the
// "brain integrity" pass at boot). It health-checks the embedder with exponential
// backoff before enqueuing. Returns after all Enqueue calls complete; seeding is
// async. Seeding is deterministic — one embedding of the agent's declared
// description, no LLM (the ADR-0037 interview this once drove was deleted
// 2026-11-05, T-20261105-delete-the-llm-agent-interview-now-owner).
func RunProfileBackfill(
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
			return fmt.Errorf("profile backfill: embedder unavailable after %v: %w", timeout, err)
		}
		slog.Warn("profile backfill: embedder unavailable, retrying", "err", err, "backoff", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}

	// Enqueue agents that have no profile vector.
	allAgents, err := agents.GetAllAgents(ctx)
	if err != nil {
		return fmt.Errorf("profile backfill: list agents: %w", err)
	}

	var enqueued, skipped, healed int
	for _, agent := range allAgents {
		// TraitModel agents (`llm:<id>`) have no profile vector BY DESIGN (a model is
		// sub-selected, never semantically gated), so "ready with a missing vector" is
		// their normal state, not damage to heal
		// (T-20261102-interview-backfill-tries-to-boot-llm-model).
		if agent.Trait == domain.TraitModel {
			skipped++
			continue
		}
		// The registry (bbolt) is the source of truth for whether an agent
		// needs seeding. A profile in pgvector from a previous run does
		// not mean the agent is ready if the registry says it is still
		// provisional (e.g. after a data-dir wipe). ADR-0023.
		if agent.Provisional {
			enqueuer.Enqueue(agent)
			enqueued++
			continue
		}
		// Self-heal: a "ready" (non-provisional) agent whose stored profile vector is
		// missing/empty is invisible to the Layer-2 semantic gate (this is what stranded
		// every worker seeded while the embedder was down). The embedder is confirmed
		// healthy above, so re-seed it now; the seeder keeps its earned outcomes.
		hasVec, vErr := profiles.HasProfileVector(ctx, agent.ID, agent.SourceHash)
		if vErr != nil {
			slog.Warn("profile backfill: profile-vector check failed; leaving agent as-is",
				"agent_id", agent.ID, "err", vErr)
			skipped++
			continue
		}
		if !hasVec {
			slog.Warn("profile backfill: re-seeding ready agent with missing profile vector",
				"agent_id", agent.ID)
			enqueuer.Enqueue(agent)
			healed++
			continue
		}
		skipped++
	}
	slog.Info("profile backfill: enqueue pass complete",
		"enqueued", enqueued, "healed_missing_vector", healed, "skipped", skipped)

	slog.Info("profile backfill complete", "enqueued", enqueued, "skipped", skipped)
	return nil
}
