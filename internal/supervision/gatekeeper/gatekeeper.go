package gatekeeper

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

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
	// DefaultProvisionalScore (a flat 0.1 for a provisional agent) was RETIRED
	// 2026-11-05 with the LLM interview (T-20261105-delete-the-llm-agent-interview-now-owner):
	// a provisional agent — registered, profile not yet seeded — is scored on the
	// neutral prior like any agent with no evidence, so it is routable from boot.
	DefaultSimilarityThreshold = 0.2 // ADR-0023: lowered from 0.5 so agent descriptions that are
	// semantically related (but not identical) to the task still pass the Gatekeeper.
	// The Dispatcher's merit ranking (over cognitive and tool agents alike) refines the match.
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

// Gatekeeper is the three-layer interrupt controller (Declaration → Semantic → Merit).
type Gatekeeper struct {
	Registry domain.AgentDeclarationSource
	Profiles GatekeeperProfileReader
	Embedder domain.Embedder
	Searcher domain.ProfileSearcher
	ExecCfg  config.ExecutionConfig
	// RouteScorer is the ROUTE-07 learned gatekeeper scorer (ADR-0076). When set AND
	// execution.learned_scorer is on, it replaces the hand-weighted GatekeeperScore with
	// a model learned from orchestration artifacts. nil (or arm off) ⇒ hand weights
	// (byte-identical). Structural interface — satisfied by *routescorer.Model.
	RouteScorer RouteScorer

	// Rand drives the ADR-0139 D6 Thompson sampler (merit_mode = thompson).
	// Injectable so tests are deterministic (WithRand); nil ⇒ a time-seeded source
	// created on first use. Guarded by randMu.
	Rand   *rand.Rand
	randMu sync.Mutex
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

func WithSearcher(s domain.ProfileSearcher) GatekeeperOption {
	return func(g *Gatekeeper) { g.Searcher = s }
}

// WithRand seeds the Thompson sampler (ADR-0139 D6) — tests and reproducible runs.
func WithRand(r *rand.Rand) GatekeeperOption {
	return func(g *Gatekeeper) { g.Rand = r }
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

func (g *Gatekeeper) FindCandidates(ctx context.Context, task *domain.DispatchTask) ([]domain.ScoredCandidate, error) {
	agents, err := g.Registry.GetAllAgents(ctx)
	if err != nil {
		return nil, err
	}

	// Agent pinning. Resolved once so the hard short-circuit and the soft
	// exemptions below cannot disagree about which agent is pinned.
	pinnedID := ""
	if g.ExecCfg.Routing.AgentPinning && task != nil {
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
			if agent.Trait == domain.TraitDaemon || domain.IsSystemDefinition(agent) {
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
	if g.ExecCfg.Capability.CapabilityResolution && task != nil && len(task.RequiredCapabilities) > 0 {
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
			g.ExecCfg.Capability.CapabilityAliases, agentSets)

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

	// ROUTE-02 routing trace: record the Declaration→Semantic→Merit funnel so a
	// mis-routed step is explainable from the persisted auction event alone. The
	// funnel only captures values the layers already compute; it is nil (zero
	// cost beyond the flag check) when tracing is off.
	trace := g.ExecCfg.Routing.RoutingTraceEnabled && task != nil
	var funnel *domain.GatekeeperFunnel
	if trace {
		funnel = &domain.GatekeeperFunnel{MaxCandidates: g.ExecCfg.Gatekeeper.GatekeeperMaxCandidates}
	}
	// Every candidate's merit breakdown, keyed by agent id: the ranking below reads
	// the tie-break fields from it (posterior modes), and the funnel records it.
	meritByAgent := make(map[string]MeritBreakdown)

	var candidates []domain.ScoredCandidate
	var slate []SlateEntry
	for _, agent := range agents {
		// Daemon agents are signal producers, not task executors; they never
		// serve AgentService and cannot bid or execute steps.
		if agent.Trait == domain.TraitDaemon {
			continue
		}
		// Privileged system organs (ADR-0051 class: kg_extractor, reranker) are kernel-invoked directly, never
		// auctioned/EFE-selected for a user task — exclude them from the candidate pool.
		if domain.IsSystemDefinition(agent) {
			continue
		}

		manifest := getManifest(agent.ID)

		if !PassesDeclaration(manifest, gateTask, g.ExecCfg.Capability.CanonicalVocab) {
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

		// Every agent — provisional or not — is ranked on merit. A provisional agent
		// (registered, profile not yet seeded) usually has no profile for its source
		// hash, so ScoreMerit gives it the NEUTRAL prior (SR = Trust = 0.5, or the
		// Beta prior in the posterior modes): the same footing as a seeded agent with
		// no verified outcomes yet (ADR-0139 D6). It used to carry a flat
		// DefaultProvisionalScore of 0.1 until an LLM interview promoted it; the
		// interview was deleted 2026-11-05.
		//
		// Rank on the SAME tags the gate enforced: that is where the agent's
		// per-capability history lives (ROUTE-06). An unmatched requirement has
		// no history anyway and falls back to the global profile.
		candidates = append(candidates, domain.ScoredCandidate{Agent: agent})
		slate = append(slate, SlateEntry{Profile: g.fetchProfile(ctx, agent), Trait: agent.Trait})
	}

	// Scored as ONE slate, because two parts of the score are slate-relative
	// (ADR-0139 D6): a missing latency (and, in the posterior modes, a missing cost)
	// takes the slate's mean known value — neutral, rather than the full w3 bonus
	// `normLatency == 0 → 1.0` once handed every agent without data — and the
	// Thompson mode draws one posterior sample per candidate per dispatch.
	breakdowns := ScoreSlate(slate, gateTask.RequiredCapabilities, g.ExecCfg, g.RouteScorer, g.sampler())
	for i := range candidates {
		mb := breakdowns[i]
		score := mb.Score
		// A soft pin is a thumb on the scale, not a verdict: the named agent is
		// boosted but still ranked, so a markedly better candidate can win. Note
		// the pin is applied AFTER L1 — a soft pin never buys past the capability
		// contract, it only competes harder inside it.
		if candidates[i].Agent.ID == pinnedID {
			score = softPinned(score)
		}
		candidates[i].Score = score
		meritByAgent[candidates[i].Agent.ID] = mb
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
				slog.Warn("Gatekeeper: ProfileSearcher failed, skipping Layer 2", "err", searchErr)
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
				// A provisional agent bypasses L2 unconditionally: its profile vector
				// has not been seeded yet (the embedder may still be coming up at
				// boot), so the searcher cannot return it, and gating it would keep a
				// just-registered agent out of every slate.
				//
				// ROUTE-06 / ADR-0069 once bounded this bypass with a per-capability
				// exploration budget, withdrawn after N provisional wins per window.
				// That budget was DELETED 2026-08-08: the Auctioneer was its only
				// recorder of wins, ADR-0100 P3 deleted the Auctioneer, and from then
				// on `Allowed` always returned true — so the bound had already been
				// inert for months and only looked like a guarantee. It was removed
				// rather than re-wired here, because a bound that cannot bind is worse
				// than no bound. If exploration needs bounding again it belongs on the
				// Dispatcher, which is the component that now knows a candidate won.
				var filtered []domain.ScoredCandidate
				for _, c := range candidates {
					// A soft-pinned agent also bypasses the semantic gate. L2 measures
					// whether an agent's DESCRIPTION reads as similar to the step,
					// which is the wrong question once a user has named the executor:
					// "use terminal_agent to summarise this" is a legitimate request
					// that description similarity would veto.
					bypass := c.Agent.ID == pinnedID || c.Agent.Provisional
					if bypass {
						filtered = append(filtered, c)
						if trace {
							funnel.L2 = append(funnel.L2, domain.SemanticGateResult{
								AgentID: c.Agent.ID, Survived: true, ProvisionalBypass: true,
							})
						}
					} else if _, ok := qualifyingAgents[c.Agent.ID]; ok {
						filtered = append(filtered, c)
						if trace {
							funnel.L2 = append(funnel.L2, domain.SemanticGateResult{
								AgentID: c.Agent.ID, Similarity: simByAgent[c.Agent.ID], Survived: true,
							})
						}
					} else {
						slog.Info("Gatekeeper: Layer 2 semantic gate eliminated agent", "agent_id", c.Agent.ID)
						if trace {
							// Below-threshold agents are not returned by the searcher, so
							// similarity is unknown (recorded as 0) — Survived=false is the
							// load-bearing signal.
							funnel.L2 = append(funnel.L2, domain.SemanticGateResult{
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
				// (its profile vector was missing), producing `no_candidate`.
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

	// legacy: score only (byte-identical). Posterior modes: score, then the cost
	// and latency tie-breaks (MeritRanksBefore).
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		return MeritRanksBefore(g.ExecCfg, a.Score, meritByAgent[a.Agent.ID], b.Score, meritByAgent[b.Agent.ID])
	})

	// Per-dispatch scores at debug (ADR-0139 D6 observability): enough to explain
	// a ranking under any mode without the routing trace.
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		for rank, c := range candidates {
			mb := meritByAgent[c.Agent.ID]
			slog.Debug("Gatekeeper: merit",
				"task_id", taskID(task), "mode", mb.Mode, "rank", rank, "agent_id", c.Agent.ID,
				"score", c.Score, "success_rate", mb.SuccessRate, "trust", mb.TrustScore,
				"alpha", mb.Alpha, "beta", mb.Beta, "cost", mb.Cost, "latency_ms", mb.LatencyMs,
				"latency_known", mb.LatencyKnown)
		}
	}

	maxK := g.ExecCfg.Gatekeeper.GatekeeperMaxCandidates
	if maxK > 0 && len(candidates) > maxK {
		candidates = candidates[:maxK]
	}

	// Record the final Merit slate (post-sort, post-cap) in presentation order.
	if trace {
		for _, c := range candidates {
			mb, ok := meritByAgent[c.Agent.ID]
			if !ok {
				// Defensive: every candidate is merit-scored above, so this is only
				// reached if a future path appends one without a breakdown.
				mb = MeritBreakdown{Score: c.Score}
			}
			funnel.L3 = append(funnel.L3, domain.MeritResult{
				AgentID:     c.Agent.ID,
				Score:       mb.Score,
				SuccessRate: mb.SuccessRate,
				TrustScore:  mb.TrustScore,
				LatencyTerm: mb.LatencyTerm,
				CostTerm:    mb.CostTerm,
				Provisional: mb.Provisional,
				Mode:        mb.Mode,
				Alpha:       mb.Alpha,
				Beta:        mb.Beta,
			})
		}
		task.Funnel = funnel
	}

	return candidates, nil
}

// FindModelCandidates returns all TraitModel agents, filtered by required capabilities
// and ranked by merit score. Used by the Dispatcher for ADR-0018 TraitModel sub-selection.
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
//
// Under the ADR-0139 D6 posterior modes the additive terms do not exist: Score is
// the ranking statistic (the LCB quantile, or the Thompson sample / posterior mean),
// SuccessRate is the posterior mean, TrustScore / LatencyTerm / CostTerm are 0, and
// Cost / LatencyMs carry the tie-break values.
type MeritBreakdown struct {
	Score       float64
	SuccessRate float64
	TrustScore  float64
	LatencyTerm float64 // w3 * (1/normLatency) contribution
	// LatencyKnown is false when the profile has no latency yet; LatencyTerm (or,
	// in the posterior modes, LatencyMs) is then the neutral fill, not a measurement.
	LatencyKnown bool
	CostTerm     float64 // w4 * normalizedCost contribution (subtracted from Score)
	Provisional  bool

	// Mode is the merit mode that produced Score (config.MeritMode*).
	Mode string
	// Alpha / Beta are the Beta posterior (posterior modes only; 0 under legacy).
	Alpha, Beta float64
	// Cost is the agent's average cost per task and LatencyMs its median latency,
	// the posterior modes' tie-breaks. Missing values are filled with the slate's
	// mean known value by ScoreSlate (CostKnown / LatencyKnown say which).
	Cost      float64
	CostKnown bool
	LatencyMs float64
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
	return ScoreMerit(g.fetchProfile(ctx, agent), agent.Trait, requiredCaps, g.ExecCfg, g.RouteScorer)
}

// fetchProfile reads the profile routing scores on: (agent.ID, agent.SourceHash).
// A fetch error reads as nil ⇒ neutral priors.
func (g *Gatekeeper) fetchProfile(ctx context.Context, agent domain.AgentDefinition) *domain.AgentProfile {
	if g.Profiles == nil {
		return nil
	}
	p, err := g.Profiles.GetProfile(ctx, agent.ID, agent.SourceHash)
	if err != nil {
		slog.Warn("Gatekeeper: profile fetch error, using neutral score",
			"agent_id", agent.ID, "err", err)
	}
	return p
}

// meanOf is the arithmetic mean; 0 for an empty slice.
func meanOf(vs []float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	var sum float64
	for _, v := range vs {
		sum += v
	}
	return sum / float64(len(vs))
}

// ScoreMerit is the PURE merit-scoring core (ADR-0077): given a resolved profile (nil ⇒
// neutral cold-start priors), the agent trait, the step's required capabilities, the
// execution config (hand weights + arm flags + merit mode) and an optional learned
// scorer, it produces the GatekeeperScore + its component terms. Decoupled from
// profile FETCHING so both the live Gatekeeper path and the PreviewRoute RPC (the
// gatekeeper benchmark, ADR-0077) score candidates identically — the benchmark
// supplies synthetic profiles inline, no live fleet.
//
// It scores ONE candidate in isolation. A slate is scored with ScoreSlate, which
// adds the slate-relative parts: the neutral fill of a missing latency (legacy:
// the slate's mean known latency term; this function alone gives it 0 — no
// evidence, no bonus) and, under thompson, the per-dispatch posterior sample (this
// function alone reports the posterior mean).
func ScoreMerit(profile *domain.AgentProfile, trait domain.AgentTrait, requiredCaps []string, cfg config.ExecutionConfig, scorer RouteScorer) MeritBreakdown {
	return scoreMerit(profile, trait, requiredCaps, cfg, scorer, 0)
}

// scoreMerit is ScoreMerit with the latency term to use when the profile has no
// latency data (neutralLatencyTerm, already weighted by w3; legacy mode only).
//
// TraitModel candidates (ADR-0018 sub-selection) always take the legacy formula:
// D6 is a posterior per (agent, capability) over step outcomes, and model
// providers are not dispatched to steps, so they have no such outcomes.
func scoreMerit(profile *domain.AgentProfile, trait domain.AgentTrait, requiredCaps []string, cfg config.ExecutionConfig, scorer RouteScorer, neutralLatencyTerm float64) MeritBreakdown {
	if mode := cfg.Gatekeeper.ActiveMeritMode(); mode != config.MeritModeLegacy && trait != domain.TraitModel {
		return scorePosterior(profile, requiredCaps, cfg, mode)
	}
	mb := scoreLegacy(profile, trait, requiredCaps, cfg, scorer, neutralLatencyTerm)
	mb.Mode = config.MeritModeLegacy
	return mb
}

// scoreLegacy is the hand-weighted GatekeeperScore, unchanged by ADR-0139 D6
// (golden-tested: TestScoreMerit_LegacyGolden).
func scoreLegacy(profile *domain.AgentProfile, trait domain.AgentTrait, requiredCaps []string, cfg config.ExecutionConfig, scorer RouteScorer, neutralLatencyTerm float64) MeritBreakdown {
	w1, w2, w3, w4 := cfg.Gatekeeper.GatekeeperW1, cfg.Gatekeeper.GatekeeperW2, cfg.Gatekeeper.GatekeeperW3, cfg.Gatekeeper.GatekeeperW4

	const (
		neutralSuccessRate = 0.5
		neutralTrustScore  = 0.5
	)
	successRate, trustScore := neutralSuccessRate, neutralTrustScore
	var (
		normLatency        float64
		latencyKnown       bool
		profileProvisional bool
		normalizedCost     float64
	)

	if profile != nil {
		successRate = profile.SuccessRate
		trustScore = profile.TrustScore
		// ROUTE-06: prefer capability-scoped success/trust for the step's required
		// capability when that tag has history; otherwise keep the global values.
		if cfg.Routing.PerCapabilityMerit && len(requiredCaps) > 0 && len(profile.CapabilityStats) > 0 {
			for _, rc := range requiredCaps {
				if st, ok := profile.CapabilityStats[rc]; ok && st.SampleCount > 0 {
					successRate = st.SuccessRate
					trustScore = st.TrustScore
					break
				}
			}
		}
		latencyKnown = profile.NetworkLatencyMedianMs+profile.ComputationLatencyMedianMs > 0
		normLatency = float64(profile.NetworkLatencyMedianMs+profile.ComputationLatencyMedianMs) +
			domain.ContextGrowthPenalty(profile.ContextGrowthBytesMedian, cfg.Plan.ContextGrowthK)
		profileProvisional = profile.Provisional
		if profile.ModelMetrics != nil && profile.ModelMetrics.AvgCostPerTask > 0 {
			normalizedCost = profile.ModelMetrics.AvgCostPerTask / 0.01
			if normalizedCost > 1.0 {
				normalizedCost = 1.0
			}
		}
	}

	// Missing latency is neutral, not the maximum bonus: this used to set
	// normLatency = 1.0 (i.e. 1 ms), so an agent with NO latency data earned the
	// full w3 while every measured agent (hundreds of ms) earned ~0 — a free merit
	// bonus for having no evidence (T-20261104-merit-math-distorts-routing-interview-grade-is).
	latencyTerm := neutralLatencyTerm
	if latencyKnown && normLatency > 0 {
		latencyTerm = w3 * (1.0 / normLatency)
	}
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
	// model's score REPLACES the hand-weighted score (the provisional flag is one of the
	// model's features; only legacy profile rows still carry it). The merit
	// terms are still returned for the ROUTE-02 funnel. Byte-identical when the arm is off.
	if cfg.Routing.LearnedScorer && scorer != nil {
		provFloat := 0.0
		if profileProvisional {
			provFloat = 1.0
		}
		score = scorer.Score([5]float64{successRate, trustScore, latencyTerm, costTerm, provFloat})
		return MeritBreakdown{Score: score, SuccessRate: successRate, TrustScore: trustScore, LatencyTerm: latencyTerm, LatencyKnown: latencyKnown, CostTerm: costTerm, Provisional: profileProvisional}
	}

	// The ×ColdStartPenaltyMultiplier (0.6) penalty on a provisional PROFILE was
	// RETIRED 2026-11-05 (T-20261105-delete-the-llm-agent-interview-now-owner). Its
	// only writer was the interview's A2A path, which saved remote agents'
	// profiles as provisional; an agent without evidence now carries the neutral
	// prior, not a discount on top of it.

	return MeritBreakdown{Score: score, SuccessRate: successRate, TrustScore: trustScore, LatencyTerm: latencyTerm, LatencyKnown: latencyKnown, CostTerm: costTerm, Provisional: profileProvisional}
}

// Defaults the posterior modes fall back to when a config carries a zero value
// (a hand-built ExecutionConfig in a test or plugin; a loaded config is validated
// and defaulted by internal/config).
const (
	defaultMeritPrior       = 1.0
	defaultMeritLCBQuantile = 0.1
)

// posteriorParams resolves the posterior settings, zero values read as defaults.
func posteriorParams(cfg config.ExecutionConfig) (priorA, priorB, quantile, unverifiedW float64) {
	g := cfg.Gatekeeper
	priorA, priorB, quantile, unverifiedW = g.MeritPriorAlpha, g.MeritPriorBeta, g.MeritLCBQuantile, g.MeritUnverifiedWeight
	if priorA <= 0 {
		priorA = defaultMeritPrior
	}
	if priorB <= 0 {
		priorB = defaultMeritPrior
	}
	if quantile <= 0 || quantile >= 1 {
		quantile = defaultMeritLCBQuantile
	}
	if unverifiedW < 0 {
		unverifiedW = 0
	}
	return priorA, priorB, quantile, unverifiedW
}

// outcomeEvidence picks the evidence a step is scored on: for a step that declares
// required capabilities, the first of them the agent has evidence for (the
// capability-keyed posterior, ADR-0139 D6); none ⇒ NO evidence (the prior), not the
// global record — an agent's PDF-parsing record must not vouch for it on a browser
// step (the ROUTE-06 failure). A step that declares none scores on the global
// evidence over every outcome.
func outcomeEvidence(profile *domain.AgentProfile, requiredCaps []string) domain.OutcomeEvidence {
	if profile == nil {
		return domain.OutcomeEvidence{}
	}
	if len(requiredCaps) == 0 {
		return profile.Outcomes
	}
	for _, rc := range requiredCaps {
		if st, ok := profile.CapabilityStats[rc]; ok && !st.Outcomes.IsZero() {
			return st.Outcomes
		}
	}
	return domain.OutcomeEvidence{}
}

// scorePosterior scores one candidate under a posterior mode (ADR-0139 D6):
//
//	α = prior_α + verified successes + unverified_weight·unverified
//	β = prior_β + verified failures
//
// posterior_lcb ⇒ Score = the merit_lcb_quantile quantile of Beta(α, β);
// thompson ⇒ Score = the posterior mean (ScoreSlate replaces it with a sample when
// it is given a sampler; PreviewRoute is not, so it reports the mean).
// Cost and latency are NOT in the score: they are tie-breaks (MeritRanksBefore).
// No SR binarisation, no calibration term, no hand weights, no learned scorer.
func scorePosterior(profile *domain.AgentProfile, requiredCaps []string, cfg config.ExecutionConfig, mode string) MeritBreakdown {
	priorA, priorB, quantile, unverifiedW := posteriorParams(cfg)
	ev := outcomeEvidence(profile, requiredCaps)
	alpha := priorA + ev.VerifiedSuccess + unverifiedW*float64(ev.Unverified)
	beta := priorB + ev.VerifiedFailure
	mean := alpha / (alpha + beta)

	mb := MeritBreakdown{Mode: mode, Alpha: alpha, Beta: beta, SuccessRate: mean}
	if mode == config.MeritModePosteriorLCB {
		mb.Score = betaQuantile(quantile, alpha, beta)
	} else {
		mb.Score = mean
	}
	if profile != nil {
		mb.Provisional = profile.Provisional
		if ms := profile.NetworkLatencyMedianMs + profile.ComputationLatencyMedianMs; ms > 0 {
			mb.LatencyMs, mb.LatencyKnown = float64(ms), true
		}
		if profile.ModelMetrics != nil {
			mb.Cost, mb.CostKnown = profile.ModelMetrics.AvgCostPerTask, true
		}
	}
	return mb
}

// SlateEntry is one candidate of a slate: its resolved profile (nil ⇒ no evidence)
// and trait.
type SlateEntry struct {
	Profile *domain.AgentProfile
	Trait   domain.AgentTrait
}

// BetaSampler draws one value from Beta(alpha, beta). nil ⇒ no sampling.
type BetaSampler func(alpha, beta float64) float64

// ScoreSlate scores a slate under the active merit mode and returns the
// breakdowns in input order. Both the live Gatekeeper (FindCandidates) and the
// PreviewRoute RPC call it, so they apply the same slate-relative neutral fills
// (T-20261104-previewroute-scores-missing-latency-as-0-while):
//
//   - legacy: a candidate with no latency takes the mean latency TERM of the
//     non-model candidates that have one; none has one ⇒ 0 for all (rank-neutral).
//   - posterior modes: a missing latency or cost tie-break takes the mean of the
//     known values among the posterior-scored candidates; and, when sample is
//     non-nil under thompson, Score is one posterior draw per candidate.
//
// Model-trait entries keep the legacy formula in every mode (see scoreMerit).
func ScoreSlate(entries []SlateEntry, requiredCaps []string, cfg config.ExecutionConfig, scorer RouteScorer, sample BetaSampler) []MeritBreakdown {
	out := make([]MeritBreakdown, len(entries))
	mode := cfg.Gatekeeper.ActiveMeritMode()
	if mode == config.MeritModeLegacy {
		var known []float64
		var pending []int
		for i, e := range entries {
			out[i] = scoreMerit(e.Profile, e.Trait, requiredCaps, cfg, scorer, 0)
			if e.Trait == domain.TraitModel {
				continue
			}
			if out[i].LatencyKnown {
				known = append(known, out[i].LatencyTerm)
			} else {
				pending = append(pending, i)
			}
		}
		if len(pending) > 0 {
			neutral := meanOf(known)
			for _, i := range pending {
				out[i] = scoreMerit(entries[i].Profile, entries[i].Trait, requiredCaps, cfg, scorer, neutral)
			}
		}
		return out
	}

	var knownLat, knownCost []float64
	for i, e := range entries {
		out[i] = scoreMerit(e.Profile, e.Trait, requiredCaps, cfg, scorer, 0)
		if e.Trait == domain.TraitModel {
			continue
		}
		if out[i].LatencyKnown {
			knownLat = append(knownLat, out[i].LatencyMs)
		}
		if out[i].CostKnown {
			knownCost = append(knownCost, out[i].Cost)
		}
	}
	neutralLat, neutralCost := meanOf(knownLat), meanOf(knownCost)
	for i, e := range entries {
		if e.Trait == domain.TraitModel {
			continue
		}
		if !out[i].LatencyKnown {
			out[i].LatencyMs = neutralLat
		}
		if !out[i].CostKnown {
			out[i].Cost = neutralCost
		}
		if mode == config.MeritModeThompson && sample != nil {
			out[i].Score = sample(out[i].Alpha, out[i].Beta)
		}
	}
	return out
}

// MeritRanksBefore reports whether a candidate with final score sa (soft pin
// included) and breakdown a ranks ahead of one with sb / b. Legacy compares the
// score alone (the pre-D6 order, byte-identical under a stable sort). The
// posterior modes break an exact score tie — the common case at cold start, when
// every candidate sits on the same prior — by lower cost, then lower latency;
// both are slate-mean filled when missing, so the order is a strict weak order.
func MeritRanksBefore(cfg config.ExecutionConfig, sa float64, a MeritBreakdown, sb float64, b MeritBreakdown) bool {
	if sa != sb {
		return sa > sb
	}
	if cfg.Gatekeeper.ActiveMeritMode() == config.MeritModeLegacy {
		return false
	}
	if a.Cost != b.Cost {
		return a.Cost < b.Cost
	}
	return a.LatencyMs < b.LatencyMs
}

// sampler returns the Thompson sampler over the Gatekeeper's RNG, or nil outside
// thompson mode. FindCandidates runs concurrently for parallel plan steps and
// *rand.Rand is not safe for concurrent use, so every draw holds randMu.
func (g *Gatekeeper) sampler() BetaSampler {
	if g.ExecCfg.Gatekeeper.ActiveMeritMode() != config.MeritModeThompson {
		return nil
	}
	return func(alpha, beta float64) float64 {
		g.randMu.Lock()
		defer g.randMu.Unlock()
		if g.Rand == nil {
			g.Rand = rand.New(rand.NewSource(time.Now().UnixNano()))
		}
		return sampleBeta(g.Rand, alpha, beta)
	}
}

// taskID is the task's id for logging; "" for a nil task.
func taskID(task *domain.DispatchTask) string {
	if task == nil {
		return ""
	}
	return task.ID
}
