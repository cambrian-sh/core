package verify

import (
	"context"
	"errors"
	"log/slog"
	"sort"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/supervision/gatekeeper"
)

// ErrNoVerifierAvailable is returned by VerifierPool.Select when no eligible
// verifier passes all exclusion and threshold filters.
var ErrNoVerifierAvailable = errors.New("verifier pool: no eligible verifier available")

// VerifierRegistry is the subset of the agent catalogue needed by VerifierPool.
type VerifierRegistry interface {
	GetAllAgents(ctx context.Context) ([]domain.AgentDefinition, error)
	GetManifest(ctx context.Context, agentID string) (*domain.AgentManifest, error)
	GetAgentByName(ctx context.Context, name string) (*domain.AgentDefinition, error)
}

// ProfileReader is the narrow read-only profile interface used by VerifierPool.
type ProfileReader interface {
	GetProfile(ctx context.Context, agentID, sourceHash string) (*domain.AgentProfile, error)
}

// VerifierPool is a filtered, read-only view of the agent registry.
type VerifierPool struct {
	Registry      VerifierRegistry
	Profiles      ProfileReader
	Threshold     float64
	RecencyWindow int

	MinSize        int
	ThresholdStep  float64
	ThresholdFloor float64

	// CanonicalVocab is execution.capability.canonical_vocab (ROUTE-04 / ADR-0067),
	// handed to the gatekeeper's L1 gate so a verifier is admitted by exactly the
	// declaration check an executor is. Set by the kernel next to the other
	// Verification settings; false keeps the verbatim comparison.
	CanonicalVocab bool
}

// NewVerifierPool creates a VerifierPool with the health guard disabled.
func NewVerifierPool(registry VerifierRegistry, profiles ProfileReader, threshold float64, recencyWindow int) *VerifierPool {
	return &VerifierPool{
		Registry:      registry,
		Profiles:      profiles,
		Threshold:     threshold,
		RecencyWindow: recencyWindow,
	}
}

// WithHealthGuard enables threshold relaxation when the pool is undersized.
// If step is zero, a default of 0.05 is used and a warning is logged.
func (vp *VerifierPool) WithHealthGuard(minSize int, step, floor float64) *VerifierPool {
	if minSize > 0 && step == 0 {
		slog.Warn("VerifierPool: MinSize set but ThresholdStep is zero; defaulting ThresholdStep to 0.05")
		step = 0.05
	}
	vp.MinSize = minSize
	vp.ThresholdStep = step
	vp.ThresholdFloor = floor
	return vp
}

// Select returns the highest-TrustScore Verifier Pool member that passes all filters.
func (vp *VerifierPool) Select(
	ctx context.Context,
	task *domain.DispatchTask,
	excludeAgentID string,
	subjectProfile *domain.AgentProfile,
) (*domain.AgentDefinition, error) {
	agents, err := vp.Registry.GetAllAgents(ctx)
	if err != nil {
		return nil, err
	}

	excluded := buildExclusionSet(excludeAgentID, subjectProfile)

	type agentWithMetrics struct {
		agent       domain.AgentDefinition
		trustScore  float64
		successRate float64
	}

	var candidates []agentWithMetrics
	for _, agent := range agents {
		if excluded[agent.ID] || agent.Provisional || agent.Trait == domain.TraitTool || agent.Trait == domain.TraitDaemon {
			continue
		}
		// Privileged system organs (ADR-0051 class) are kernel-invoked directly and never
		// serve task work, so they cannot judge it either. The trait check above never
		// covered them: the organs ship no AGENT_MANIFEST, so discovery registers them with
		// the cognitive zero-value trait whatever SDK base class they use, and they were
		// verifier-eligible until T-20261031-core-system-agents-docling-kg-extractor. System
		// status is checked by the same IsSystemDefinition the Gatekeeper and the interview
		// worker use, not a parallel notion.
		if domain.IsSystemDefinition(agent) {
			continue
		}
		profile, err := vp.Profiles.GetProfile(ctx, agent.ID, agent.SourceHash)
		if err != nil || profile == nil {
			continue
		}
		manifest, _ := vp.Registry.GetManifest(ctx, agent.ID)
		// ONE declaration policy (AGENTS.md §4.3): the gatekeeper's L1 gate, not a
		// local copy. A format-only copy lived here until
		// T-20261031-verifierpool-carries-a-drifted-copy-of-the and had already
		// drifted (no ROUTE-03 capability gate, no canonical normalisation). A
		// verifier is therefore held to whatever the task declares — today both
		// callers (VerificationWorker.processOne, kernel poolJudge) build a task
		// with no formats or capabilities, so every manifest passes, as before.
		if !gatekeeper.PassesDeclaration(manifest, task, vp.CanonicalVocab) {
			continue
		}
		candidates = append(candidates, agentWithMetrics{
			agent:       agent,
			trustScore:  profile.TrustScore,
			successRate: profile.SuccessRate,
		})
	}

	effectiveThreshold := vp.Threshold

	for {
		var pool []agentWithMetrics
		for _, c := range candidates {
			if c.trustScore >= effectiveThreshold && c.successRate >= effectiveThreshold {
				pool = append(pool, c)
			}
		}

		guardDisabled := vp.MinSize == 0
		poolMeetsMin := guardDisabled || len(pool) >= vp.MinSize
		canRelax := !guardDisabled &&
			vp.ThresholdStep > 0 &&
			effectiveThreshold > 0 &&
			(vp.ThresholdFloor <= 0 || effectiveThreshold > vp.ThresholdFloor)

		if poolMeetsMin || !canRelax {
			if len(pool) == 0 {
				return nil, ErrNoVerifierAvailable
			}
			sort.Slice(pool, func(i, j int) bool {
				return pool[i].trustScore > pool[j].trustScore
			})
			result := pool[0].agent
			return &result, nil
		}

		nextThreshold := effectiveThreshold - vp.ThresholdStep
		if vp.ThresholdFloor > 0 && nextThreshold < vp.ThresholdFloor {
			nextThreshold = vp.ThresholdFloor
		}
		slog.Warn("verifier pool health degraded; relaxing threshold",
			"current_threshold", nextThreshold,
			"target_size", vp.MinSize,
			"pool_size", len(pool),
		)
		effectiveThreshold = nextThreshold
	}
}

func buildExclusionSet(excludeAgentID string, subjectProfile *domain.AgentProfile) map[string]bool {
	excluded := map[string]bool{excludeAgentID: true}
	if subjectProfile != nil {
		for _, id := range subjectProfile.RecentVerifierIDs {
			excluded[id] = true
		}
	}
	return excluded
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
