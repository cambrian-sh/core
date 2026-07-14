package gatekeeper

import (
	"context"
	"log/slog"
	"sort"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

const (
	DefaultProvisionalScore    = 0.1
	DefaultSimilarityThreshold = 0.2 // ADR-0023: lowered from 0.5 so agent descriptions that are
	// semantically related (but not identical) to the task still pass the Gatekeeper.
	// The Auctioneer's proposal phase (now including tool agents) refines the match.
)

// GatekeeperProfileReader is the narrow read-only interface used to fetch
// AgentProfiles during Merit ranking. Defined consumer-side for testability.
type GatekeeperProfileReader interface {
	GetProfile(ctx context.Context, agentID, sourceHash string) (*domain.AgentProfile, error)
}

// batchManifestReader is an optional upgrade over AgentDeclarationSource.
// If the registry implements this, FindCandidates uses a single bbolt Tx
// for all manifests instead of N individual reads.
type batchManifestReader interface {
	GetManifestBatch(ids []string) (map[string]*domain.AgentManifest, error)
}

// Gatekeeper is the three-layer interrupt controller (Declaration → Interview → Merit).
type Gatekeeper struct {
	Registry domain.AgentDeclarationSource
	Profiles GatekeeperProfileReader
	Embedder domain.Embedder
	Searcher domain.InterviewSearcher
	ExecCfg  config.ExecutionConfig
}

// GatekeeperOption configures a Gatekeeper via functional options.
type GatekeeperOption func(*Gatekeeper)

func WithProfiles(r GatekeeperProfileReader) GatekeeperOption {
	return func(g *Gatekeeper) { g.Profiles = r }
}

func WithEmbedder(e domain.Embedder) GatekeeperOption {
	return func(g *Gatekeeper) { g.Embedder = e }
}

func WithSearcher(s domain.InterviewSearcher) GatekeeperOption {
	return func(g *Gatekeeper) { g.Searcher = s }
}

func NewGatekeeper(registry domain.AgentDeclarationSource, cfg config.ExecutionConfig, opts ...GatekeeperOption) *Gatekeeper {
	g := &Gatekeeper{
		Registry: registry,
		ExecCfg:  cfg,
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

func (g *Gatekeeper) FindCandidates(ctx context.Context, task *domain.AuctionTask) ([]domain.ScoredCandidate, error) {
	agents, err := g.Registry.GetAllAgents(ctx)
	if err != nil {
		return nil, err
	}

	// Pre-load all manifests in one Tx if the registry supports batch reads.
	var manifestCache map[string]*domain.AgentManifest
	if batcher, ok := g.Registry.(batchManifestReader); ok {
		ids := make([]string, len(agents))
		for i, a := range agents {
			ids[i] = a.ID
		}
		manifestCache, _ = batcher.GetManifestBatch(ids)
	}
	getManifest := func(agentID string) *domain.AgentManifest {
		if manifestCache != nil {
			return manifestCache[agentID]
		}
		m, _ := g.Registry.GetManifest(ctx, agentID)
		return m
	}

	// ROUTE-02 routing trace: record the Declaration→Interview→Merit funnel so a
	// mis-routed step is explainable from the persisted auction event alone. The
	// funnel only captures values the layers already compute; it is nil (zero
	// cost beyond the flag check) when tracing is off.
	trace := g.ExecCfg.RoutingTraceEnabled && task != nil
	var funnel *domain.GatekeeperFunnel
	if trace {
		funnel = &domain.GatekeeperFunnel{MaxCandidates: g.ExecCfg.GatekeeperMaxCandidates}
	}
	var meritByAgent map[string]meritBreakdown
	if trace {
		meritByAgent = make(map[string]meritBreakdown)
	}

	var candidates []domain.ScoredCandidate
	for _, agent := range agents {
		// Daemon agents are signal producers, not task executors; they never
		// serve AgentService and cannot bid or execute steps.
		if agent.Trait == domain.TraitDaemon {
			continue
		}
		// Privileged system organs (ADR-0051 Scout) are kernel-invoked directly, never
		// auctioned/EFE-selected for a user task — exclude them from the candidate pool.
		if domain.IsSystemAgent(agent.ID) {
			continue
		}

		manifest := getManifest(agent.ID)

		if !PassesDeclaration(manifest, task) {
			slog.Info("Gatekeeper: agent filtered by declaration", "agent_id", agent.ID)
			if trace {
				funnel.L1 = append(funnel.L1, domain.DeclarationResult{
					AgentID: agent.ID,
					Passed:  false,
					Reason:  "required-format/declaration mismatch",
				})
			}
			continue
		}
		if trace {
			funnel.L1 = append(funnel.L1, domain.DeclarationResult{AgentID: agent.ID, Passed: true})
		}

		score := DefaultProvisionalScore
		if !agent.Provisional {
			mb := g.computeMeritBreakdown(ctx, agent)
			score = mb.Score
			if trace {
				meritByAgent[agent.ID] = mb
			}
		}
		candidates = append(candidates, domain.ScoredCandidate{Agent: agent, Score: score})
	}

	// ADR-0023 Routing Fix: Layer 2 semantic search now applies to ALL
	// non-provisional agents (cognitive + tool). Previously it only ran
	// when cognitive agents were present, and tool agents were exempt.
	needsLayer2 := false
	for _, c := range candidates {
		if !c.Agent.Provisional {
			needsLayer2 = true
			break
		}
	}
	if g.Embedder != nil && g.Searcher != nil && task.Description != "" && needsLayer2 {
		embedding, embedErr := g.Embedder.Embed(ctx, task.Description)
		if embedErr != nil {
			slog.Warn("Gatekeeper: embed task description failed, skipping Layer 2", "err", embedErr)
		} else {
			topK := len(candidates) + 10
			results, searchErr := g.Searcher.SearchByEmbedding(ctx, embedding, DefaultSimilarityThreshold, topK)
			if searchErr != nil {
				slog.Warn("Gatekeeper: InterviewSearcher failed, skipping Layer 2", "err", searchErr)
			} else {
				qualifyingAgents := make(map[string]struct{}, len(results))
				simByAgent := make(map[string]float64, len(results))
				for _, r := range results {
					qualifyingAgents[r.AgentID] = struct{}{}
					// The searcher returns only above-threshold matches; keep the
					// best similarity seen per agent for the funnel.
					if s, ok := simByAgent[r.AgentID]; !ok || r.Similarity > s {
						simByAgent[r.AgentID] = r.Similarity
					}
				}
				if trace {
					funnel.L2Threshold = DefaultSimilarityThreshold
				}
				var filtered []domain.ScoredCandidate
				for _, c := range candidates {
					if c.Agent.Provisional {
						filtered = append(filtered, c)
						if trace {
							funnel.L2 = append(funnel.L2, domain.InterviewResult{
								AgentID: c.Agent.ID, Survived: true, ProvisionalBypass: true,
							})
						}
					} else if _, ok := qualifyingAgents[c.Agent.ID]; ok {
						filtered = append(filtered, c)
						if trace {
							funnel.L2 = append(funnel.L2, domain.InterviewResult{
								AgentID: c.Agent.ID, Similarity: simByAgent[c.Agent.ID], Survived: true,
							})
						}
					} else {
						slog.Info("Gatekeeper: Layer 2 semantic gate eliminated agent", "agent_id", c.Agent.ID)
						if trace {
							// Below-threshold agents are not returned by the searcher, so
							// similarity is unknown (recorded as 0) — Survived=false is the
							// load-bearing signal.
							funnel.L2 = append(funnel.L2, domain.InterviewResult{
								AgentID: c.Agent.ID, Similarity: simByAgent[c.Agent.ID], Survived: false,
							})
						}
					}
				}
				candidates = filtered
			}
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})

	maxK := g.ExecCfg.GatekeeperMaxCandidates
	if maxK > 0 && len(candidates) > maxK {
		candidates = candidates[:maxK]
	}

	// Record the final Merit slate (post-sort, post-cap) in presentation order.
	if trace {
		for _, c := range candidates {
			mb, ok := meritByAgent[c.Agent.ID]
			if !ok {
				// Provisional agent: no merit breakdown, carries the flat score.
				mb = meritBreakdown{Score: c.Score, Provisional: true}
			}
			funnel.L3 = append(funnel.L3, domain.MeritResult{
				AgentID:     c.Agent.ID,
				Score:       mb.Score,
				SuccessRate: mb.SuccessRate,
				TrustScore:  mb.TrustScore,
				LatencyTerm: mb.LatencyTerm,
				CostTerm:    mb.CostTerm,
				Provisional: mb.Provisional,
			})
		}
		task.Funnel = funnel
	}

	return candidates, nil
}

// FindModelCandidates returns all TraitModel agents, filtered by required capabilities
// and ranked by merit score. Used by the Auctioneer for ADR-0018 TraitModel sub-selection.
func (g *Gatekeeper) FindModelCandidates(ctx context.Context, requiredCapabilities []string) ([]domain.ScoredCandidate, error) {
	agents, err := g.Registry.GetAllAgents(ctx)
	if err != nil {
		return nil, err
	}

	var matches []string
	for _, a := range agents {
		if a.Trait != domain.TraitModel {
			continue
		}
		matches = append(matches, a.ID)
	}

	// Pre-load manifests in batch if available.
	var manifestCache map[string]*domain.AgentManifest
	if batcher, ok := g.Registry.(batchManifestReader); ok {
		manifestCache, _ = batcher.GetManifestBatch(matches)
	}
	getManifest := func(agentID string) *domain.AgentManifest {
		if manifestCache != nil {
			return manifestCache[agentID]
		}
		m, _ := g.Registry.GetManifest(ctx, agentID)
		return m
	}

	// Filter by required capabilities: the TraitModel's Capabilities list must
	// contain all strings in requiredCapabilities (from the cognitive agent's
	// RequiredModelCapabilities).
	capabilityFilter := func(manifest *domain.AgentManifest) bool {
		if len(requiredCapabilities) == 0 {
			return true
		}
		if manifest == nil {
			return false
		}
		hasCap := make(map[string]bool, len(manifest.Capabilities))
		for _, c := range manifest.Capabilities {
			hasCap[c] = true
		}
		for _, req := range requiredCapabilities {
			if !hasCap[req] {
				return false
			}
		}
		return true
	}

	var candidates []domain.ScoredCandidate
	for _, a := range agents {
		if a.Trait != domain.TraitModel {
			continue
		}
		if !capabilityFilter(getManifest(a.ID)) {
			continue
		}
		score := g.computeMeritScore(ctx, a)
		candidates = append(candidates, domain.ScoredCandidate{Agent: a, Score: score})
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})

	return candidates, nil
}

// meritBreakdown is the GatekeeperScore and the individual terms that produced
// it, so the ROUTE-02 funnel can show which component drove a candidate's rank.
type meritBreakdown struct {
	Score       float64
	SuccessRate float64
	TrustScore  float64
	LatencyTerm float64 // w3 * (1/normLatency) contribution
	CostTerm    float64 // w4 * normalizedCost contribution (subtracted from Score)
	Provisional bool
}

func (g *Gatekeeper) computeMeritScore(ctx context.Context, agent domain.AgentDefinition) float64 {
	return g.computeMeritBreakdown(ctx, agent).Score
}

func (g *Gatekeeper) computeMeritBreakdown(ctx context.Context, agent domain.AgentDefinition) meritBreakdown {
	w1 := g.ExecCfg.GatekeeperW1
	w2 := g.ExecCfg.GatekeeperW2
	w3 := g.ExecCfg.GatekeeperW3
	w4 := g.ExecCfg.GatekeeperW4

	const (
		neutralSuccessRate = 0.5
		neutralTrustScore  = 0.5
	)

	var (
		successRate        float64 = neutralSuccessRate
		trustScore         float64 = neutralTrustScore
		normLatency        float64
		profileProvisional bool
		normalizedCost     float64
	)

	if g.Profiles != nil {
		profile, err := g.Profiles.GetProfile(ctx, agent.ID, agent.SourceHash)
		if err != nil {
			slog.Warn("Gatekeeper: profile fetch error, using neutral score",
				"agent_id", agent.ID, "err", err)
		}
		if profile != nil {
			successRate = profile.SuccessRate
			trustScore = profile.TrustScore
			normLatency = float64(profile.NetworkLatencyMedianMs+profile.ComputationLatencyMedianMs) +
				domain.ContextGrowthPenalty(profile.ContextGrowthBytesMedian, g.ExecCfg.ContextGrowthK)
			profileProvisional = profile.Provisional
			if profile.ModelMetrics != nil && profile.ModelMetrics.AvgCostPerTask > 0 {
				normalizedCost = profile.ModelMetrics.AvgCostPerTask / 0.01
				if normalizedCost > 1.0 {
					normalizedCost = 1.0
				}
			}
		}
	}

	if normLatency == 0 {
		normLatency = 1.0
	}

	latencyTerm := w3 * (1.0 / normLatency)
	costTerm := w4 * normalizedCost

	var score float64
	if agent.Trait == domain.TraitModel {
		// TraitModel scoring omits the latency term (ADR-0018 sub-selection).
		score = successRate + trustScore - costTerm
		latencyTerm = 0
	} else {
		score = w1*successRate + w2*trustScore + latencyTerm - costTerm
	}

	if profileProvisional {
		penalty := g.ExecCfg.ColdStartPenaltyMultiplier
		if penalty == 0 {
			penalty = 0.6
		}
		score *= penalty
	}

	return meritBreakdown{
		Score:       score,
		SuccessRate: successRate,
		TrustScore:  trustScore,
		LatencyTerm: latencyTerm,
		CostTerm:    costTerm,
		Provisional: profileProvisional,
	}
}
