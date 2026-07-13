package verify

import (
	"context"
	"errors"
	"log/slog"
	"sort"

	"github.com/cambrian-sh/core/domain"
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
	task *domain.AuctionTask,
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
		profile, err := vp.Profiles.GetProfile(ctx, agent.ID, agent.SourceHash)
		if err != nil || profile == nil {
			continue
		}
		manifest, _ := vp.Registry.GetManifest(ctx, agent.ID)
		if !PassesDeclaration(manifest, task) {
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

// PassesDeclaration performs the Layer 1 (Declaration) hard compatibility check.
func PassesDeclaration(manifest *domain.AgentManifest, task *domain.AuctionTask) bool {
	if manifest == nil {
		return true
	}
	if len(manifest.Tools) == 0 && len(manifest.SupportedFormats) == 0 {
		return true
	}
	formatSet := make(map[string]struct{}, len(manifest.SupportedFormats))
	for _, f := range manifest.SupportedFormats {
		formatSet[f] = struct{}{}
	}
	for _, required := range task.RequiredFormats {
		if _, ok := formatSet[required]; !ok {
			return false
		}
	}
	return true
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
