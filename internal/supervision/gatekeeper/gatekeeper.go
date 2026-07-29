package gatekeeper

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// SoftPinBoost is added to a soft-pinned agent's merit score, clamped to 1.0.
// Sized to dominate ordinary merit spread (success-rate/trust differences are
// typically well under 0.2) without being absolute — a soft pin should win among
// comparable agents and lose to one that is drastically better. PinHard is the
// mechanism for "this agent, regardless".
const SoftPinBoost = 0.25

// softPinned applies SoftPinBoost with clamping, so a pin can never push a score
// outside the [0,1] range the rest of selection assumes.
func softPinned(score float64) float64 {
	if boosted := score + SoftPinBoost; boosted < 1.0 {
		return boosted
	}
	return 1.0
}

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
	// ExplorationBudget bounds the provisional L2 bypass per capability (ROUTE-06 /
	// ADR-0069). nil (or arm off) ⇒ unbounded bypass, the pre-ROUTE-06 behavior. Shared
	// with the Auctioneer, which records provisional wins into it.
	ExplorationBudget *domain.ExplorationBudget
	// RouteScorer is the ROUTE-07 learned gatekeeper scorer (ADR-0076). When set AND
	// execution.learned_scorer is on, it replaces the hand-weighted GatekeeperScore with
	// a model learned from orchestration artifacts. nil (or arm off) ⇒ hand weights
	// (byte-identical). Structural interface — satisfied by *routescorer.Model.
	RouteScorer RouteScorer
}

// RouteScorer scores a candidate from its merit feature vector (ROUTE-07 / ADR-0076). The
// feature order matches routescorer.FeatureNames: success_rate, trust_score, inv_latency,
// normalized_cost, provisional. Satisfied by *routescorer.Model.
type RouteScorer interface {
	Score(features [5]float64) float64
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

	// Agent pinning. Resolved once so the hard short-circuit and the soft
	// exemptions below cannot disagree about which agent is pinned.
	pinnedID := ""
	if g.ExecCfg.AgentPinning && task != nil {
		pinnedID = task.PreferredAgent
	}
	// EqualFold, not ==: the pin strength comes from an LLM, and "Hard" losing its
	// meaning on capitalisation would be a silent downgrade. Anything that is not
	// recognisably "hard" still degrades to soft, which is the safe direction.
	if pinnedID != "" && strings.EqualFold(task.AgentPin, domain.PinHard) {
		// A hard pin skips discovery entirely: the user named the executor, so
		// there is nothing to select. Daemons and privileged system organs stay
		// undispatchable — they do not serve task steps at all, and letting a pin
		// reach them would route work into machinery that cannot run it.
		for _, agent := range agents {
			if agent.ID != pinnedID {
				continue
			}
			if agent.Trait == domain.TraitDaemon || domain.IsSystemAgent(agent.ID) {
				return nil, fmt.Errorf("%w: %q is not dispatchable", domain.ErrPinnedAgentUnavailable, pinnedID)
			}
			slog.Info("Gatekeeper: hard agent pin bound", "agent_id", pinnedID, "task_id", task.ID)
			return []domain.ScoredCandidate{{Agent: agent, Score: 1.0}}, nil
		}
		return nil, fmt.Errorf("%w: %q is not registered", domain.ErrPinnedAgentUnavailable, pinnedID)
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
	// Memoized so the ADR-0100 D5 vocabulary pre-pass below costs nothing extra
	// when the registry cannot batch: each manifest is fetched at most once.
	manifestMemo := make(map[string]*domain.AgentManifest, len(agents))
	getManifest := func(agentID string) *domain.AgentManifest {
		if manifestCache != nil {
			return manifestCache[agentID]
		}
		if m, ok := manifestMemo[agentID]; ok {
			return m
		}
		m, _ := g.Registry.GetManifest(ctx, agentID)
		manifestMemo[agentID] = m
		return m
	}

	// ADR-0100 D5: resolve the step's declared requirements against the LIVE
	// capability vocabulary before gating on them. Gating on a capability no
	// agent declares admits nobody, which is how a plan step dies with a generic
	// "no candidates" (measured: ADR-0096, 4 dead steps). The ladder is
	//   normalize → authored alias → generalist tier → fail loudly.
	// Only runs when the step actually declares requirements, so the pre-ROUTE-03
	// path and the control arm stay byte-identical.
	gateTask := task
	if g.ExecCfg.CapabilityResolution && task != nil && len(task.RequiredCapabilities) > 0 {
		live := make([]*domain.AgentManifest, 0, len(agents))
		for _, a := range agents {
			if a.Trait == domain.TraitDaemon || domain.IsSystemAgent(a.ID) {
				continue // never dispatchable; not part of the routable vocabulary
			}
			live = append(live, getManifest(a.ID))
		}
		vocabulary := domain.BuildCapabilityVocabulary(live)
		// Per-agent sets, not just the union: L1 needs ONE agent to declare every
		// required capability, so satisfiability must be checked agent-wise.
		agentSets := make([][]string, 0, len(live))
		for _, m := range live {
			if m != nil {
				agentSets = append(agentSets, m.Capabilities)
			}
		}
		resolution := domain.ResolveCapabilities(task.RequiredCapabilities, vocabulary,
			g.ExecCfg.CapabilityAliases, agentSets)

		if !resolution.Satisfiable() {
			// Loud, diagnosable failure naming the gap and the live vocabulary,
			// rather than an empty slate the caller has to guess about.
			return nil, &domain.NoCapabilityMatchError{
				TaskID:     task.ID,
				Unmatched:  resolution.Unmatched,
				Vocabulary: resolution.Vocabulary,
			}
		}
		// ALWAYS gate on the resolved set, including on the exact tier. Resolved
		// carries the fleet's DECLARED spelling, and L1 compares verbatim unless
		// canonical_vocab is on — so leaving the planner's own spelling in place
		// means `code-search` "resolves" against a declared `code_search` and then
		// L1 rejects every agent on spelling alone. Measured 2026-07-29: that is
		// what produced `no_candidate` on the orchestration probe.
		// The COPY keeps the caller's task — the record of what the planner
		// actually asked for — untouched.
		if len(resolution.Resolved) > 0 {
			tCopy := *task
			tCopy.RequiredCapabilities = resolution.Resolved
			gateTask = &tCopy
		}
		// Logged on EVERY tier, not just fallbacks: the ADR-0100 P2 A/B needs to be
		// able to explain any routing outcome from the run's own logs, and an
		// "exact" resolution that still admits nobody is precisely the case that is
		// impossible to diagnose without it.
		slog.Info("Gatekeeper: capability requirements resolved",
			"task_id", task.ID, "tier", resolution.Tier,
			"required", task.RequiredCapabilities, "resolved", resolution.Resolved,
			"unmatched", resolution.Unmatched, "vocabulary", resolution.Vocabulary)
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
	var meritByAgent map[string]MeritBreakdown
	if trace {
		meritByAgent = make(map[string]MeritBreakdown)
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

		if !PassesDeclaration(manifest, gateTask, g.ExecCfg.CanonicalVocab) {
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
			// Rank on the SAME tags the gate enforced: that is where the agent's
			// per-capability history lives (ROUTE-06). An unmatched requirement has
			// no history anyway and falls back to the global profile.
			mb := g.computeMeritBreakdown(ctx, agent, gateTask.RequiredCapabilities)
			score = mb.Score
			if trace {
				meritByAgent[agent.ID] = mb
			}
		}
		// A soft pin is a thumb on the scale, not a verdict: the named agent is
		// boosted but still ranked, so a markedly better candidate can win. Note
		// the pin is applied AFTER L1 — a soft pin never buys past the capability
		// contract, it only competes harder inside it.
		if agent.ID == pinnedID {
			score = softPinned(score)
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
				// ROUTE-06 / ADR-0069: the provisional L2 bypass is bounded by the
				// per-capability exploration budget. A provisional agent bypasses only
				// while budget remains for the step's capability; once exhausted it must
				// pass the semantic gate like everyone else (exploration granted, not
				// unbounded). Arm off / nil budget ⇒ always allowed (unchanged).
				budgetCap := ""
				if len(task.RequiredCapabilities) > 0 {
					budgetCap = task.RequiredCapabilities[0]
				}
				var filtered []domain.ScoredCandidate
				for _, c := range candidates {
					// A soft-pinned agent bypasses the semantic gate. L2 measures
					// whether an agent's DESCRIPTION reads as similar to the step,
					// which is the wrong question once a user has named the executor:
					// "use terminal_agent to summarise this" is a legitimate request
					// that description similarity would veto.
					bypass := c.Agent.ID == pinnedID ||
						(c.Agent.Provisional &&
							(!g.ExecCfg.PerCapabilityMerit || g.ExplorationBudget.Allowed(budgetCap)))
					if bypass {
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
				// ADR-0100 D1: L1 decides ELIGIBILITY, L2 only expresses PREFERENCE.
				// A semantic gate that empties a slate L1 approved is doing L1's job
				// with a fuzzy tool (routing diagnosis D3) and kills the step outright.
				// Measured 2026-07-29 on the live probe: L1 admitted exactly one
				// capability-eligible agent and L2 eliminated it at similarity 0.0
				// (its interview vector was missing), producing `no_candidate`.
				// Keep the eligible set and let merit rank it.
				// Only when L1 ACTUALLY gated: eligibility must have been established
				// by the capability contract for L2 to be overridable. With no declared
				// requirements L1 is a free pass, so L2 is the only filter there is and
				// emptying the slate is legitimate (ADR-0023: tool agents must qualify
				// semantically rather than winning everything).
				l1Enforced := gateTask != nil && len(gateTask.RequiredCapabilities) > 0
				if l1Enforced && len(filtered) == 0 && len(candidates) > 0 {
					slog.Warn("Gatekeeper: L2 eliminated every capability-eligible candidate — keeping the L1 set (eligibility is L1's decision, not L2's)",
						"l1_survivors", len(candidates), "l2_threshold", DefaultSimilarityThreshold)
					if trace {
						for i := range funnel.L2 {
							funnel.L2[i].Survived = true
						}
					}
				} else {
					candidates = filtered
				}
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
				mb = MeritBreakdown{Score: c.Score, Provisional: true}
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
		score := g.computeMeritScore(ctx, a, nil) // TraitModel path: no capability scoping
		candidates = append(candidates, domain.ScoredCandidate{Agent: a, Score: score})
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})

	return candidates, nil
}

// MeritBreakdown is the GatekeeperScore and the individual terms that produced
// it, so the ROUTE-02 funnel can show which component drove a candidate's rank.
type MeritBreakdown struct {
	Score       float64
	SuccessRate float64
	TrustScore  float64
	LatencyTerm float64 // w3 * (1/normLatency) contribution
	CostTerm    float64 // w4 * normalizedCost contribution (subtracted from Score)
	Provisional bool
}

func (g *Gatekeeper) computeMeritScore(ctx context.Context, agent domain.AgentDefinition, requiredCaps []string) float64 {
	return g.computeMeritBreakdown(ctx, agent, requiredCaps).Score
}

// computeMeritBreakdown scores an agent. requiredCaps carries the step's required
// capabilities (ROUTE-06 / ADR-0069): when execution.per_capability_merit is on and the
// agent has capability-scoped history for one of them, that tag-scoped success/trust is
// used instead of the global profile — an agent's PDF-parsing merit no longer inflates
// its browser-auction score. Empty caps or no tag history ⇒ global (byte-identical).
func (g *Gatekeeper) computeMeritBreakdown(ctx context.Context, agent domain.AgentDefinition, requiredCaps []string) MeritBreakdown {
	var profile *domain.AgentProfile
	if g.Profiles != nil {
		p, err := g.Profiles.GetProfile(ctx, agent.ID, agent.SourceHash)
		if err != nil {
			slog.Warn("Gatekeeper: profile fetch error, using neutral score",
				"agent_id", agent.ID, "err", err)
		}
		profile = p
	}
	return ScoreMerit(profile, agent.Trait, requiredCaps, g.ExecCfg, g.RouteScorer)
}

// ScoreMerit is the PURE merit-scoring core (ADR-0077): given a resolved profile (nil ⇒
// neutral cold-start priors), the agent trait, the step's required capabilities, the
// execution config (hand weights + arm flags) and an optional learned scorer, it produces
// the GatekeeperScore + its component terms. Decoupled from profile FETCHING so both the
// live Gatekeeper path and the PreviewRoute RPC (the gatekeeper benchmark, ADR-0077) score
// candidates identically — the benchmark supplies synthetic profiles inline, no live fleet.
func ScoreMerit(profile *domain.AgentProfile, trait domain.AgentTrait, requiredCaps []string, cfg config.ExecutionConfig, scorer RouteScorer) MeritBreakdown {
	w1, w2, w3, w4 := cfg.GatekeeperW1, cfg.GatekeeperW2, cfg.GatekeeperW3, cfg.GatekeeperW4

	const (
		neutralSuccessRate = 0.5
		neutralTrustScore  = 0.5
	)
	successRate, trustScore := neutralSuccessRate, neutralTrustScore
	var (
		normLatency        float64
		profileProvisional bool
		normalizedCost     float64
	)

	if profile != nil {
		successRate = profile.SuccessRate
		trustScore = profile.TrustScore
		// ROUTE-06: prefer capability-scoped success/trust for the step's required
		// capability when that tag has history; otherwise keep the global values.
		if cfg.PerCapabilityMerit && len(requiredCaps) > 0 && len(profile.CapabilityStats) > 0 {
			for _, rc := range requiredCaps {
				if st, ok := profile.CapabilityStats[rc]; ok && st.SampleCount > 0 {
					successRate = st.SuccessRate
					trustScore = st.TrustScore
					break
				}
			}
		}
		normLatency = float64(profile.NetworkLatencyMedianMs+profile.ComputationLatencyMedianMs) +
			domain.ContextGrowthPenalty(profile.ContextGrowthBytesMedian, cfg.ContextGrowthK)
		profileProvisional = profile.Provisional
		if profile.ModelMetrics != nil && profile.ModelMetrics.AvgCostPerTask > 0 {
			normalizedCost = profile.ModelMetrics.AvgCostPerTask / 0.01
			if normalizedCost > 1.0 {
				normalizedCost = 1.0
			}
		}
	}

	if normLatency == 0 {
		normLatency = 1.0
	}

	latencyTerm := w3 * (1.0 / normLatency)
	costTerm := w4 * normalizedCost

	var score float64
	if trait == domain.TraitModel {
		// TraitModel scoring omits the latency term (ADR-0018 sub-selection).
		score = successRate + trustScore - costTerm
		latencyTerm = 0
	} else {
		score = w1*successRate + w2*trustScore + latencyTerm - costTerm
	}

	// ROUTE-07 / ADR-0076: when the learned-scorer arm is on and a model is loaded, the
	// model's score REPLACES the hand-weighted score (and the cold-start penalty — the
	// provisional flag is a model feature, so it must not be double-applied). The merit
	// terms are still returned for the ROUTE-02 funnel. Byte-identical when the arm is off.
	if cfg.LearnedScorer && scorer != nil {
		provFloat := 0.0
		if profileProvisional {
			provFloat = 1.0
		}
		score = scorer.Score([5]float64{successRate, trustScore, latencyTerm, costTerm, provFloat})
		return MeritBreakdown{Score: score, SuccessRate: successRate, TrustScore: trustScore, LatencyTerm: latencyTerm, CostTerm: costTerm, Provisional: profileProvisional}
	}

	if profileProvisional {
		penalty := cfg.ColdStartPenaltyMultiplier
		if penalty == 0 {
			penalty = 0.6
		}
		score *= penalty
	}

	return MeritBreakdown{Score: score, SuccessRate: successRate, TrustScore: trustScore, LatencyTerm: latencyTerm, CostTerm: costTerm, Provisional: profileProvisional}
}
