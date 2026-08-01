package config

import (
	stdjson "encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cambrian-sh/core/domain"

	"github.com/go-viper/mapstructure/v2"
	kjson "github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/rawbytes"
	koanf "github.com/knadh/koanf/v2"
)

// ExecutionConfig holds DAG execution timing parameters.
// Zero values are replaced with safe defaults by LoadConfig.
// PlanConfig — Plan-level execution limits: timeouts, recursion, replanning, fan-out and the energy/cost budget.
//
// Split out of the former 198-field flat ExecutionConfig (config schema v2).
// Field names, types and json tags are unchanged; only the nesting is new, so
// each field means exactly what it meant before.
type PlanConfig struct {
	// StepTimeoutMultiplier scales the winning bid's latency estimate.
	StepTimeoutMultiplier float64 `json:"step_timeout_multiplier"`

	// StepTimeoutBaseBufferMs is the additive floor (ms) added after scaling.
	StepTimeoutBaseBufferMs int `json:"step_timeout_base_buffer_ms"`

	// PlanTimeoutMs is the hard ceiling (ms) for the entire plan execution.
	PlanTimeoutMs int `json:"plan_timeout_ms"`

	// ContextGrowthK is the scalar used in ContextGrowthPenalty = k * growthBytes.
	// Penalises agents that inflate the DAG context with large payloads.
	ContextGrowthK float64 `json:"context_growth_k"`

	// PlanPreviewOnly makes Execute return the generated plan as JSON and skip
	// DAG execution (ROUTE-03 offline routing eval). It lets an eval score the
	// planner's required_capabilities emission + deterministic L1 gating without
	// the cost/noise of full agentic execution. Benchmark/eval-only; never
	// production. Default false.
	PlanPreviewOnly bool `json:"plan_preview_only"`

	// MaxRecursionDepth is the global limit for nested sub-auctions (Recursive Bidding).
	// Default: 3.
	MaxRecursionDepth int `json:"max_recursion_depth"`

	// FallbackConfidenceThreshold is the minimum confidence a runner-up must have
	// to be eligible as a fallback agent when the winner fails. Default: 0.4.
	FallbackConfidenceThreshold float64 `json:"fallback_confidence_threshold"`

	// FallbackEnabled enables inter-step fallback to runner-up candidates when
	// the auction winner fails. Default: true.
	FallbackEnabled bool `json:"fallback_enabled"`

	// MaxReplanAttempts is the maximum number of replan cycles allowed when
	// all intra-step retries and inter-step fallbacks are exhausted. Default: 2.
	MaxReplanAttempts int `json:"max_replan_attempts"`

	// MaxFanOutWidth caps how many concrete children a parametric fan-out step
	// (ADR-0079 R2) may expand into from a discovered set. Exceeding it is a hard,
	// structured error routed to replan — never a silent truncation. Default: 64;
	// 0 disables the cap.
	MaxFanOutWidth int `json:"max_fanout_width"`

	// MaxPartialContextBytes caps the size of partial context returned in
	// PartialPlanError to bound memory usage. Default: 51200 (50 KB).
	MaxPartialContextBytes int `json:"max_partial_context_bytes"`

	// MaxPlanCost is the hard ceiling ($) for the total cost of a plan execution.
	// Set to 0 to disable budget enforcement. Default: 0.0.
	MaxPlanCost            float64 `json:"max_plan_cost"`
	PlanDriftDays          int     `json:"plan_drift_days"`
	MaxContextSlots        int     `json:"max_context_slots"`         // default 20
	ContextRefSnippetChars int     `json:"context_ref_snippet_chars"` // default 500

	// BudgetExhaustionAlarmRate is the threshold above which PLAN_BUDGET_INSUFFICIENT
	// signal is emitted. Default: 0.05 (5%). ADR-0018.
	BudgetExhaustionAlarmRate float64 `json:"budget_exhaustion_alarm_rate"`

	// MinStepEnergy is the adaptive histogram adjustment floor (tokens).
	// Default: 256. ADR-0018.
	MinStepEnergy int `json:"min_step_energy"`

	// MaxStepEnergy is the adaptive histogram adjustment ceiling (tokens).
	// Default: 32768. ADR-0018.
	MaxStepEnergy int `json:"max_step_energy"`

	// StepCachePolicies maps policy name to TTL in hours (ADR-0026).
	// Overrides the heuristic fallback in resolveCacheTTL when a step's query
	// matches a registered policy. Zero or absent = use heuristic.
	// Example: {"thought": 2, "tool": 168}
	StepCachePolicies map[string]int `json:"step_cache_policies,omitempty"`
}

// GatekeeperConfig — Gatekeeper admission scoring: candidate cap and the w1..w4 signal weights.
//
// Split out of the former 198-field flat ExecutionConfig (config schema v2).
// Field names, types and json tags are unchanged; only the nesting is new, so
// each field means exactly what it meant before.
type GatekeeperConfig struct {
	// Gatekeeper Merit ranking settings.
	// GatekeeperMaxCandidates is the maximum number of candidates returned after Merit ranking.
	GatekeeperMaxCandidates int `json:"gatekeeper_max_candidates"`

	// GatekeeperW1 is the weight for SuccessRate in the GatekeeperScore formula.
	GatekeeperW1 float64 `json:"gatekeeper_w1"`

	// GatekeeperW2 is the weight for TrustScore in the GatekeeperScore formula.
	GatekeeperW2 float64 `json:"gatekeeper_w2"`

	// GatekeeperW3 is the weight for 1/NormalisedLatency in the GatekeeperScore formula.
	GatekeeperW3 float64 `json:"gatekeeper_w3"`

	// ColdStartPenaltyMultiplier is applied to GatekeeperScore when AgentProfile.Provisional==true
	// (post-Interview but pre-verified). Default: 0.6.
	ColdStartPenaltyMultiplier float64 `json:"cold_start_penalty_multiplier"`

	// MinAuctionConfidence is the minimum bid Confidence an agent must submit to
	// be considered for auction winner selection. Bids strictly below this threshold
	// are discarded — the agent has signalled it cannot reliably handle the task.
	// If all bids are discarded, the step fails with NO_WINNER and DAG cancel-on-first-error fires.
	// Default: 0.3.
	MinAuctionConfidence float64 `json:"min_auction_confidence"`

	// GatekeeperW4 is the weight for the cost-penalty term in GatekeeperScore.
	// Set to 0 to disable cost-awareness entirely. Default: 0.15.
	GatekeeperW4 float64 `json:"gatekeeper_w4"`
}

// VerificationConfig — Verifier pool sizing, trust-score weighting and cross-verification sampling.
//
// Split out of the former 198-field flat ExecutionConfig (config schema v2).
// Field names, types and json tags are unchanged; only the nesting is new, so
// each field means exactly what it meant before.
type VerificationConfig struct {
	// Verifier Pool settings.
	// VerifierPoolThreshold is the minimum TrustScore and SuccessRate to enter the Verifier Pool.
	VerifierPoolThreshold float64 `json:"verifier_pool_threshold"`

	// TrustBoostThreshold is the TrustScore below which an agent enters Surveillance mode (100% sampling).
	TrustBoostThreshold float64 `json:"trust_boost_threshold"`

	// VerificationQueueCapacity is the buffer size of the async verification channel.
	VerificationQueueCapacity int `json:"verification_queue_capacity"`

	// MinVerifiedEvents is the minimum verified TaskEvents before ProfileAggregator
	// writes a non-neutral TrustScore and clears AgentProfile.Provisional.
	MinVerifiedEvents int `json:"min_verified_events"`

	// VerifierRecencyWindow is the number of recent verifier IDs stored in
	// AgentProfile.RecentVerifierIDs and excluded from VerifierPool.Select.
	VerifierRecencyWindow int `json:"verifier_recency_window"`

	// TrustScoreCalWeight (w_cal) weights the calibration signal in the TrustScore formula:
	// signal = w_cal*(clamp(vs/bc,0,2)/2) + w_abs*vs.
	TrustScoreCalWeight float64 `json:"trust_score_cal_weight"`

	// TrustScoreAbsWeight (w_abs) weights the absolute quality signal in the TrustScore formula.
	TrustScoreAbsWeight float64 `json:"trust_score_abs_weight"`

	// Pool health guard settings (D15).
	// VerifierPoolMinSize is the minimum Pool size before threshold relaxation kicks in.
	// Zero disables the health guard.
	VerifierPoolMinSize int `json:"verifier_pool_min_size"`

	// VerifierPoolThresholdStep is the relaxation increment per guard iteration.
	VerifierPoolThresholdStep float64 `json:"verifier_pool_threshold_step"`

	// VerifierPoolThresholdFloor is the hard floor for threshold relaxation.
	// Explicit (not derived) so operators can control it independently of the base threshold.
	VerifierPoolThresholdFloor float64 `json:"verifier_pool_threshold_floor"`

	// CrossVerifyRate is the fraction of primary verifier outputs that are
	// re-scored by a second Pool member (D16). Cross-verification events are
	// terminal — they are never themselves eligible for further passes (depth=1).
	CrossVerifyRate float64 `json:"cross_verify_rate"`
}

// RoutingConfig — How a step reaches an agent: auction vs deterministic route, bidding, merit, calibration and the EFE selector.
//
// Split out of the former 198-field flat ExecutionConfig (config schema v2).
// Field names, types and json tags are unchanged; only the nesting is new, so
// each field means exactly what it meant before.
type RoutingConfig struct {
	// CapabilityContract turns on the ROUTE-03 capability contract: the planner
	// emits per-step required_capabilities from the live capability-cluster
	// vocabulary, those flow into the AuctionTask, and L1 Declaration hard-gates
	// candidates on required ⊆ manifest.Capabilities. Default false (arm toggle):
	// OFF is byte-identical to the pre-ROUTE-03 kernel — the planner uses its
	// original prompt/hash and no capability requirements are threaded.
	CapabilityContract bool `json:"capability_contract"`

	// AgentPinning honours a step's PreferredAgent/AgentPin: a soft pin exempts the
	// named agent from the L2 semantic gate and boosts its merit, a hard pin binds
	// it directly. Default TRUE — unlike the learning arms this is directive
	// following, not a model, and a user who says "use X" expects X. It stays a
	// flag so the routing change can be A/B'd and switched off in one place; OFF
	// ignores both fields and restores unpinned selection exactly.
	AgentPinning bool `json:"agent_pinning"`

	// RoutingTraceEnabled turns on the ROUTE-02 gatekeeper candidate-funnel trace
	// (Declaration→Interview→Merit per-agent verdicts + winner margin) carried on
	// the auction event. Default true: the funnel only records values the
	// Gatekeeper already computes, so the cost is a per-auction slice allocation.
	// Toggle it off to measure that overhead as an A/B arm (acceptance: zero
	// auction-latency regression).
	RoutingTraceEnabled bool `json:"routing_trace_enabled"`

	// ExplorationRate is the probability of selecting a random TraitModel candidate
	// instead of the top-scored one (ε-greedy exploration). Default: 0.05.
	ExplorationRate float64 `json:"exploration_rate"`

	// BidRound (ADR-0100 P0) controls WHICH selection mechanism runs.
	//
	// false (default) ⇒ capability-typed DISPATCH: eligibility → merit ranking →
	// argmax (or cheapest-competent, see DispatchCheapEnergyMax). Zero RPCs, zero
	// LLM calls, and only the winner's process is booted.
	//
	// true ⇒ the legacy AUCTION: solicit a bid from every candidate, booting each
	// one to ask. Retained ONLY to capture the A/B evidence; both this flag and the
	// auction are deleted in ADR-0100 P3. Do not build on it.
	BidRound bool `json:"bid_round"`

	// DispatchCheapEnergyMax is the Step.MaxEnergy at or below which a VERIFIED step
	// (CheckpointAfter) takes the cheapest competent agent instead of merit-argmax
	// (ADR-0100 D4). 0 disables the branch entirely — every step takes argmax.
	// A step with no declared budget is never treated as cheap.
	DispatchCheapEnergyMax float64 `json:"dispatch_cheap_energy_max"`

	// DispatchMeritFloor is the minimum merit a candidate needs before the
	// cheapest-competent branch will consider it. Keeps "cheapest" from meaning
	// "worst". Ignored on the argmax path.
	DispatchMeritFloor float64 `json:"dispatch_merit_floor"`

	// CalibratedBids (ROUTE-05 / ADR-0068) selects the auction winner by a CALIBRATED
	// bid confidence (expected verified quality, learned per-agent from the event log)
	// instead of the raw LLM self-report. OFF ⇒ raw confidence (byte-identical).
	// Offline-first: enable only after an offline replay shows lift.
	CalibratedBids bool `json:"calibrated_bids"`

	// BidCalibrationMinSamples is the shrinkage threshold — an agent with fewer verified
	// observations is blended toward the fleet-global calibration curve. Default 10.
	BidCalibrationMinSamples int `json:"bid_calibration_min_samples"`

	// PerCapabilityMerit (ROUTE-06 / ADR-0069) makes L3 merit read the agent's
	// capability-scoped success/trust for the step's required capability (fallback to
	// the global profile when the tag has no history), and bounds the provisional L2
	// bypass with a per-capability exploration budget. OFF ⇒ global merit + unconditional
	// provisional bypass (byte-identical to pre-ROUTE-06).
	PerCapabilityMerit bool `json:"per_capability_merit"`

	// LearnedScorer (ROUTE-07 / ADR-0076) replaces the hand-weighted GatekeeperScore with
	// a model learned offline from orchestration artifacts (routescorer.Model loaded from
	// LearnedScorerModelPath). OFF ⇒ hand weights (byte-identical). Adopt online only on a
	// published offline win over the calibrated hand-weights arm (offline-before-online).
	LearnedScorer bool `json:"learned_scorer"`

	// LearnedScorerModelPath points to the JSON model the learned scorer loads at boot.
	// Empty ⇒ no model wired (the arm stays inert even if the flag is on).
	LearnedScorerModelPath string `json:"learned_scorer_model_path,omitempty"`

	// ProvisionalExplorationBudget is the max provisional WINS allowed per capability per
	// window before the free L2 bypass is withdrawn. Default 3.
	ProvisionalExplorationBudget int `json:"provisional_exploration_budget"`

	// ProvisionalExplorationWindowSeconds is the sliding window for that budget. Default 3600.
	ProvisionalExplorationWindowSeconds int `json:"provisional_exploration_window_seconds"`

	// AuctionBidTimeoutMs is the total duration (ms) for all agents to submit bids in an auction.
	// Default: 2000.
	AuctionBidTimeoutMs int `json:"auction_bid_timeout_ms"`

	// ProposalTimeoutMs is the per-agent RPC timeout (ms) when calling RequestProposal.
	// Default: 2000.
	ProposalTimeoutMs int `json:"proposal_timeout_ms"`

	// BypassAuction (ADR-0050 D1) short-circuits Server.Execute past the
	// planner/auction/DAG path and dispatches the user's input verbatim to
	// SingleAgentID — the within-substrate no-orchestration control arm the
	// benchmark harness uses for routing A/Bs (ROUTE-01 baselines). Default
	// OFF; production behavior is unchanged when false.
	BypassAuction bool `json:"bypass_auction,omitempty"`

	// SingleAgentID is the agent the bypass path dispatches to. Required when
	// BypassAuction is true; Execute fails loud when it is empty or unknown.
	SingleAgentID string `json:"single_agent_id,omitempty"`

	// ADR-0057 (three-tier config): ReactiveEngine tuning is PREMIUM config and lives
	// in the premium repo's own config, fed in via the app.Options reactive hook.
	// It is intentionally absent from the OSS ExecutionConfig schema.

	// ADR-0037: Central-Executive Planner resource selection.
	// ResourceSelector chooses the selection mechanism: "auction" (status quo
	// control), "efe" (inference treatment), or "auto" (session-scoped A/B split
	// governed by EFETrafficPercent). Default: "auction".
	ResourceSelector string `json:"resource_selector,omitempty"`

	// EFETrafficPercent is the fraction [0,100] of sessions assigned to the EFE
	// arm when ResourceSelector="auto". Session-scoped (not step-scoped) so every
	// step in a plan uses the same mechanism, keeping A/B attribution clean.
	// Default: 0 (safe rollout — 100% auction even in "auto").
	EFETrafficPercent int `json:"efe_traffic_percent,omitempty"`

	// EFEExplorationBonus scales the epistemic (uncertainty-driven) term in the
	// EFE pick (ADR-0037 D9). A deferred estimator tuned within the spike.
	EFEExplorationBonus float64 `json:"efe_exploration_bonus,omitempty"`
}

// CapabilityConfig — Capability vocabulary, resolution and the retired clusterer knobs.
//
// Split out of the former 198-field flat ExecutionConfig (config schema v2).
// Field names, types and json tags are unchanged; only the nesting is new, so
// each field means exactly what it meant before.
type CapabilityConfig struct {
	// CapabilityClusterer settings (ADR-0014).
	// CapabilityClusterThreshold is the minimum cosine similarity for cluster membership.
	CapabilityClusterThreshold float64 `json:"capability_cluster_threshold"`

	// CapabilityClusterEpsilon is the hysteresis cushion for Sticky Representative stability.
	CapabilityClusterEpsilon float64 `json:"capability_cluster_epsilon"`

	// CapabilityClusterMinAgents is the minimum registry size before a sweep runs.
	CapabilityClusterMinAgents int `json:"capability_cluster_min_agents"`

	// CapabilityClusterIntervalSeconds is the defensive reconciliation ticker interval
	// (reused by the ROUTE-04 canonicalizer sweep).
	CapabilityClusterIntervalSeconds int `json:"capability_cluster_interval_seconds"`

	// CanonicalVocab (ROUTE-04 / ADR-0067) applies DETERMINISTIC normalization to
	// capability tags (lowercase, trim, collapse `-`/`_`/space to a single form) on both
	// sides of ROUTE-03's hard L1 match and in the planner vocabulary, so format/typo
	// variance (`Web-Navigation` ≡ `web_navigation`) matches. It deliberately does NOT do
	// fuzzy/embedding synonym merges — those risk wrong merges (e.g. `file-read` ↔
	// `file-write`) and worse misroutes. OFF ⇒ declared strings verbatim (pre-ROUTE-04).
	// The LLM CapabilityClusterer is retired regardless of this flag.
	CanonicalVocab bool `json:"canonical_vocab"`

	// CapabilityResolution (ADR-0100 D5) turns on the capability resolution
	// ladder: normalize → authored alias → generalist tier → fail loudly. Without
	// it, a step requiring a capability nothing declares gates every agent out and
	// dies as a generic "no candidates". Only runs for steps that declare
	// requirements, so the pre-ROUTE-03 path is unaffected. Default true.
	CapabilityResolution bool `json:"capability_resolution"`

	// CapabilityAliases is the AUTHORED synonym map — the second rung of that
	// ladder. Keys and values are normalized before lookup, so casing and
	// separators do not matter: {"websearch": "research"} maps a planner-emitted
	// `web-search` onto the declared `research`.
	//
	// This is DATA, reviewed by a human, deliberately NOT an embedding match:
	// ADR-0067 rejected fuzzy capability merging because `file-read`/`file-write`
	// and `read`/`delete` are embedding-close and semantically opposite, and a
	// wrong merge silently misroutes. Empty by default — every entry should be
	// added because a real step failed to route, not speculatively.
	CapabilityAliases map[string]string `json:"capability_aliases"`
}

// SessionConfig — Session lifecycle: idle timeout, retention sweeps and token TTL.
//
// Split out of the former 198-field flat ExecutionConfig (config schema v2).
// Field names, types and json tags are unchanged; only the nesting is new, so
// each field means exactly what it meant before.
type SessionConfig struct {
	SessionTTLDays int `json:"session_ttl_days"`

	// SessionIdleTimeoutMinutes is how long an ACTIVE session may sit untouched before
	// the kernel transitions it to DORMANT (Phase 2). It is the driver the lifecycle
	// never had: without it every session ever created stayed "active" forever, so the
	// dormant→completed half of ADR-0012/0030 was unreachable and the session store grew
	// without bound.
	//
	// Dormancy is non-destructive and reversible — a dormant session is still resumed by
	// a later Execute — which is why this defaults ON. 0 disables the sweep.
	SessionIdleTimeoutMinutes int `json:"session_idle_timeout_minutes"`

	// SessionIdleSweepIntervalSeconds is how often the idle sweep runs. 0 ⇒ 300s.
	SessionIdleSweepIntervalSeconds int `json:"session_idle_sweep_interval_seconds"`

	// SessionRetentionDays is how long a COMPLETED session is kept before it (and its runs
	// and checkpoints, by cascade) is reclaimed. This is what actually bounds growth: a
	// session store with a lifecycle but no retention still only ever grows. 0 disables it.
	SessionRetentionDays int `json:"session_retention_days"`

	// SessionRetentionSweepIntervalSeconds is how often retention runs. 0 ⇒ disabled.
	SessionRetentionSweepIntervalSeconds int `json:"session_retention_sweep_interval_seconds"`

	// RequireExplicitSession makes Execute REJECT a request that names no known session,
	// instead of silently opening one (Phase 2).
	//
	// Implicit creation cannot distinguish "new work" from "a continuation" from "a
	// replay", so it is wrong for at least one of them — and it is why every Execute
	// minted a session that nothing ever closed. Strict mode forces the caller to say
	// which it means via CreateSession.
	//
	// Default OFF: turning it on is a breaking change for any client that has never
	// opened a session (benchmarks, ad-hoc dispatch), so it is an opt-in arm until those
	// callers are migrated.
	RequireExplicitSession bool `json:"require_explicit_session"`

	// SessionTokenSweepIntervalSeconds is how often the CircadianRhythm scavenger
	// runs to evict expired session tokens. Default: 30. ADR-0018.
	SessionTokenSweepIntervalSeconds int `json:"session_token_sweep_interval_seconds"`

	// SessionTokenTTLMultiplier multiplies estimatedStepDuration to determine the
	// keepalive TTL for a session token. Default: 5.0. ADR-0018.
	SessionTokenTTLMultiplier float64 `json:"session_token_ttl_multiplier"`

	// ADR-0030: Scavenger thresholds.
	MaxSessionTokens int `json:"max_session_tokens,omitempty"`
}

// MemoryConfig — Long-term memory: experiential capture, the Tier-1/Tier-2 promotion pipeline, consolidation and GC.
//
// Split out of the former 198-field flat ExecutionConfig (config schema v2).
// Field names, types and json tags are unchanged; only the nesting is new, so
// each field means exactly what it meant before.
type MemoryConfig struct {
	// ExperientialMemoryEnabled turns on the passive "experiential" write path: every
	// completed planner step result (RecordExecution) and every tool output
	// (RecordToolOutput) is embedded and stored to long-term memory as a fact. Default
	// FALSE — the design stores whole, unfocused tool outputs (a directory listing, a
	// file's contents) as single embeddings, which both overflows the embedder's context
	// window and pollutes recall with low-signal auto-captured junk. Knowledge that is
	// genuinely a memory source belongs in the explicit chunked ingest pipeline, not this
	// auto-capture. Off by default; benchmark rigs that measure experiential recall can
	// re-enable it via tuning.local.json.
	ExperientialMemoryEnabled bool `json:"experiential_memory_enabled"`

	// ExperienceRecordsEnabled turns on the ADR-0049 §A2.2 OUTCOME RECORD: one
	// structured record per completed plan (the abstracted D7 projection + engaged
	// entity refs + action path + outcome), parented to an `experiences` row.
	//
	// This is NOT the flag above. `experiential_memory_enabled` guards the RAW path
	// removed on 2026-07-18 — whole tool payloads embedded as single vectors — and
	// stays false permanently. This one guards the abstraction-only replacement:
	// nothing raw is ever embedded, and a plan that engaged no entity writes nothing
	// at all.
	//
	// DEFAULT TRUE as of 2026-07-28 (owner decision). The reasoning, recorded because
	// "we turned the removed thing back on" is the wrong summary:
	//
	//   - What broke in July was the RAW path. That stays false permanently, and this
	//     path cannot embed a tool payload by construction (§A2.2 hard constraint).
	//   - Defect 4 — experience crowding corpus recall — now has a structural guard
	//     (§A2.4 lane-aware truncation), measured to cost nothing.
	//   - The volume is bounded: ONE record per plan, and none at all for a plan that
	//     engaged no entity.
	//   - It is the only way to progress. `mean_foreign_share` cannot detect
	//     displacement until experience has volume, and volume only accrues if this is
	//     on. Enabling is how the measurement STARTS, not a substitute for it.
	//
	// The pre-enable baseline is runs/lg_1 and runs/lg_2 (hit 1.0, MRR ~0.60,
	// foreign_share ~0.43). Drift against those is the thing to watch.
	ExperienceRecordsEnabled bool `json:"experience_records_enabled"`

	// ExperienceSurpriseFloor is the ADR-0049 A2.3 gate: an episode whose prediction
	// error reaches this writes an additional FAILURE PRECEDENT (a negative edge)
	// beyond its ordinary outcome record. Surprise is |merit expectation - actual|, so
	// 0.5 means "merit was wrong by half" — a confident agent that failed, or a
	// distrusted one that succeeded. Both are worth remembering; a routine failure by
	// an agent already known to fail is not.
	//
	// Default 0.5 is editorial, not measured. It should be tuned from the surprise
	// distribution the outcome records accumulate, which is exactly why they are
	// written unconditionally with the score stamped on them.
	ExperienceSurpriseFloor float64 `json:"experience_surprise_floor"`

	// (execution.scope_enforcement_enabled is GONE — ADR-0085. Access control is no
	// longer a flag: the enforcement points always run, and whether they restrict
	// anything depends on whether a policy plugin is installed. A flag that turned
	// the CHOKEPOINT off, rather than the POLICY, meant an unscoped deployment had
	// no gate at all instead of a permissive one.)
	// MemoryRelevanceThreshold is the minimum similarity score to include a memory result
	// in FetchContext. Default: 0.70.
	MemoryRelevanceThreshold float64 `json:"memory_relevance_threshold"`

	// MaxMemoryResults is the maximum number of relevant memories returned by FetchContext.
	// Default: 5.
	MaxMemoryResults int `json:"max_memory_results"`

	// MaxNeighborExpansion is the maximum number of neighbor documents fetched per memory
	// result via graph traversal. Default: 3.
	MaxNeighborExpansion int `json:"max_neighbor_expansion"`

	// MinGCAgeDays is the minimum age (in days) before a zero-access memory is eligible
	// for garbage collection by the nightly Ebbinghaus decay stored procedure.
	// Default: 30. ADR-0015.
	MinGCAgeDays int `json:"min_gc_age_days"`

	// Tier2MaxIdleSeconds is the idle-time drain trigger for the Tier-2 background goroutine.
	// Default: 300. ADR-0015.
	Tier2MaxIdleSeconds int `json:"tier2_max_idle_seconds"`

	// Tier2LLMTimeout is the timeout (seconds) for the Tier-2 Generator scoring call.
	// On timeout, the entire batch falls through to heuristic FACT-only commit.
	// Default: 30. ADR-0015.
	Tier2LLMTimeout int `json:"tier2_llm_timeout"`

	// Tier2BatchSize is the minimum channel length that triggers a load-driven drain.
	// Default: 32. ADR-0015.
	Tier2BatchSize int `json:"tier2_batch_size"`

	// CircadianStaleDocWarnThreshold is the count of low-activation-strength documents above
	// which CircadianRhythm logs a WARN on startup (indicating pg_cron may be disabled).
	// Default: 50. ADR-0015.
	CircadianStaleDocWarnThreshold int `json:"circadian_stale_doc_warn_threshold"`

	// Tier1ChannelCapacity is the bounded size of the pending channel in MemoryAgent.
	// Default: 256. ADR-0015.
	Tier1ChannelCapacity int `json:"tier1_channel_capacity"`

	// RememberDefaultActivation is the initial ActivationStrength (ADR-0015 lifecycle
	// metric) stamped on a fact written via memory.remember()/IngestMemory when the
	// caller supplies no importance hint. Without it a remembered fact defaults to 0,
	// so its floor-multiplier recall score (cosine × (α + (1-α)·activation)) maxes at
	// cosine·α ≈ 0.2 and can never clear RecallSimilarityFloor (0.25) — the fact is
	// permanently unrecallable. A LoCoMo-tunable hyperparameter (sweep it alongside
	// RecallSimilarityFloor / RetrievalFloor). Range [0,1]; 0 reproduces legacy behavior.
	RememberDefaultActivation float64 `json:"remember_default_activation"`

	// ADR-0022: Global Workspace capacity model.
	// ActivationThreshold is the post-BFS selection floor for PrimeForStep.
	ActivationThreshold float64 `json:"activation_threshold"` // default 0.1

	// ADR-0030: Lazy consolidation thresholds.
	// ConsolidationThresholdDocCount triggers global consolidation when the document
	// store exceeds this count. 0 disables count-based triggering.
	ConsolidationThresholdDocCount int `json:"consolidation_threshold_doc_count,omitempty"`

	// ConsolidationThresholdBytes triggers global consolidation when the pgvector
	// index exceeds this byte size. 0 disables size-based triggering.
	ConsolidationThresholdBytes int64 `json:"consolidation_threshold_bytes,omitempty"`
	MaxOrphanedDocuments        int   `json:"max_orphaned_documents,omitempty"`
}

// ProcedureConfig — ADR-0094 procedure induction.
//
// Split out of the former 198-field flat ExecutionConfig (config schema v2).
// Field names, types and json tags are unchanged; only the nesting is new, so
// each field means exactly what it meant before.
type ProcedureConfig struct {
	// ProcedureInductionIntervalHours schedules the ADR-0094 D3 batch pass that
	// distils recurring successful episodes into reusable routines. ZERO DISABLES IT,
	// and zero is the default.
	//
	// This is the batch consolidation ADR-0049 originally rejected and §A2.5 later
	// permitted, under constraints the scheduler is built around: nothing load-bearing
	// may depend on it, it produces only abstractions, and it is idempotent. Off by
	// default because it cannot produce anything useful until outcome records have
	// accumulated under the capability_contract arm — a scheduler with nothing to
	// induce from is pure cost.
	ProcedureInductionIntervalHours int `json:"procedure_induction_interval_hours"`

	// ProcedureMinSamples is the D3 promotion threshold: how many times a routine must
	// recur before it is a routine rather than a coincidence. Floors at 2 regardless —
	// a single occurrence is never inducible.
	ProcedureMinSamples int `json:"procedure_min_samples"`

	// ProcedureMaxEpisodesPerPass bounds one pass's read of the store. Default 500.
	ProcedureMaxEpisodesPerPass int `json:"procedure_max_episodes_per_pass"`

	// ProcedureDeprecateBelow is the confidence floor under which a routine that keeps
	// failing is retired (ADR-0094 D4). 0 disables procedure feedback. Default 0.3 —
	// low, because deprecation is a strong action and ApplyOutcome's slow consolidation
	// already means reaching this floor takes sustained failure, not a bad afternoon.
	ProcedureDeprecateBelow float64 `json:"procedure_deprecate_below"`
}

// RetrievalConfig — The recall path: seed sizes, hybrid fusion, spreading, KG expansion, blending and reranking.
//
// Split out of the former 198-field flat ExecutionConfig (config schema v2).
// Field names, types and json tags are unchanged; only the nesting is new, so
// each field means exactly what it meant before.
type RetrievalConfig struct {
	// RetrievalFloor (α) is the minimum contribution of cosine similarity in the
	// floor-multiplier re-ranking formula: cosine × (α + (1-α) × activation_strength).
	// Default: 0.2. ADR-0015.
	RetrievalFloor float64 `json:"retrieval_floor"`

	// RecallSpreadingEnabled turns on ADR-0048 D2: the agent's recall (QueryService)
	// expands its seed associatively over the memory graph (SpreadingEngine) and
	// re-ranks by activation. Default false — flag-gated so its per-recall latency
	// and cost are opt-in. Requires a GraphStore-backed vector store.
	RecallSpreadingEnabled bool `json:"recall_spreading_enabled"`

	// RecallSimilarityFloor is the minimum cosine similarity a recalled
	// mnemonic_fact must clear to be returned to the agent (ADR-0048 #1). Below
	// this, a result is dropped as irrelevant rather than padded into the top-k —
	// so an unrelated promoted tool output (a prior task's web search, a stray
	// shell error) does not masquerade as grounding, and an empty result set lets
	// the agent KNOW recall found nothing and answer from its own knowledge.
	// A deterministic quality pre-filter (same precedent as the D6 byte-floor), not
	// routing logic. 0 disables the floor (legacy flat top-k).
	RecallSimilarityFloor float64 `json:"recall_similarity_floor"`

	// Hebbian co-activation (ADR-0049 D10): when recall co-retrieves strongly-activated
	// memories, reinforce the `co_activated` edge between them so the graph self-organizes
	// from usage. Default OFF (opt-in) — the constants below are HITL-tuned against real
	// traces, not unit-asserted to a single "correct" value.
	HebbianEnabled           bool    `json:"hebbian_enabled"`            // default false
	HebbianLearningRate      float64 `json:"hebbian_learning_rate"`      // weight added per co-activation (default 0.05)
	HebbianMaxWeight         float64 `json:"hebbian_max_weight"`         // Matthew-effect cap (default 0.9)
	HebbianCoActivationFloor float64 `json:"hebbian_coactivation_floor"` // both results must exceed to "co-fire" (default 0.5)
	HebbianTopN              int     `json:"hebbian_top_n"`              // cap on co-activated docs paired per recall (default 5)
	HebbianDecayPerDay       float64 `json:"hebbian_decay_per_day"`      // weight decay base per day of edge age (default 0.95)
	HebbianBaseWeight        float64 `json:"hebbian_base_weight"`        // initial weight for a new co_activated edge (default 0.2)

	// ADR-0053 Phase 0: KG²RAG one-hop chunk expansion. When true, the recall
	// path walks the per-chunk (h, r, t) triplets extracted at write time
	// (or via the offline chunk-fill CLI) and pulls in chunks that share
	// entities with the seed chunks. Default OFF (opt-in) — the recall
	// surface is unchanged when the chunk_triplets table is empty, but the
	// SQL query still runs; flip to true to enable the expansion. (ADR-0053
	// D3: KG²RAG retrieval pipeline.)
	KG2RAGEnabled bool `json:"kg2rag_enabled"`

	// ADR-0053 Phase 0: KG²RAG expansion depth. Default 1 (one-hop, KG²RAG
	// paper's setting). 0 or negative → use default.
	KG2RAGMaxHops int `json:"kg2rag_max_hops"`

	// ADR-0053 Phase 0: max additional chunks the KG expansion can add on
	// top of the vector-search seeds. Default 20. Total recall sent to the
	// LLM = top_k (request) + this. Tune for your task: higher = more
	// context for the LLM but more noise; lower = tighter recall but
	// multi-hop may suffer. 0 or negative → use default.
	KG2RAGMaxExpanded int `json:"kg2rag_max_expanded"`

	// ADR-0053 Phase 0: max entities considered per hop. The frontier chunks
	// have triplets whose (h, t) values are entities; the most-frequent
	// ones (by mention count in the frontier) get expanded. Default 30.
	// 0 or negative → use default.
	KG2RAGMaxEntities int `json:"kg2rag_max_entities"`

	// ADR-0053 Phase 0: max chunks pulled per entity via ChunksMentioningEntity.
	// Default 5. Higher = deeper recall per shared entity (more missed chunks
	// rescued) at the cost of pool noise. 0 or negative → use default.
	KG2RAGPerEntity int `json:"kg2rag_per_entity"`

	// QueryEntitySeedingEnabled turns on LLM-free, structure-aware recall:
	// entities are extracted from the QUERY text (token/n-gram match against the
	// live chunk_triplets vocabulary, no LLM) and the chunks mentioning them are
	// injected as seeds before kgExpand — rescuing the gold on a vector miss.
	// Needs KG2RAGEnabled. ADR-0053.
	QueryEntitySeedingEnabled bool `json:"query_entity_seeding_enabled,omitempty"`

	// AnchorConstraintEnabled turns on document-local ANCHOR promotion: the
	// query is parsed for structural anchors (Chapter 1, scene 1 / decimal
	// sections / explicit ids) normalized to the same tokens the deterministic
	// anchor tier stored at ingest, and the chunks carrying them are lifted above
	// the relevance floor so the reranker cannot bury them among template-identical
	// siblings. LLM-free; needs KG2RAGEnabled (the chunk_triplets store). ADR-0053.
	AnchorConstraintEnabled bool `json:"anchor_constraint_enabled,omitempty"`

	// StructureGraphEnabled turns on ADR-0060 structure-aware ingestion: the
	// docling_agent parses each document into its real hierarchy and the kernel
	// persists a structure graph (section nodes + PART_OF/NEXT edges) with every
	// chunk stamped with its inherited section path. Opt-in (default false).
	StructureGraphEnabled bool `json:"structure_graph_enabled,omitempty"`

	// AgenticRetrievalEnabled turns on the agentic retrieval loop
	// (AGENTIC_RETRIEVAL_SPEC): an LLM query-planner rewrites the query before
	// the single pass (Phase 2a, hops=1), growing into the full plan/iterate/
	// synthesize loop. Default OFF — the query path is unchanged when disabled or
	// when the retrieval_agent is unreachable (fail-open to searchByType), so the
	// benchmark baseline arm measures the true single pass. A/B via config.
	AgenticRetrievalEnabled bool `json:"agentic_retrieval_enabled,omitempty"`

	// AgenticMaxHops bounds the retrieval loop's iterations (per-query LLM cost
	// = O(max_hops)). Phase 2a runs hops=1 (plan once, retrieve once).
	AgenticMaxHops int `json:"agentic_max_hops,omitempty"`

	// AgenticPlannerModel is the model id the retrieval planner reasons with
	// (e.g. "llm:gemini-flash"). "" ⇒ the gateway default. The planner is on the
	// hot recall path, so a FAST model matters — a slow default (e.g. a remote
	// deepseek) blows the query deadline and every plan fails open to the raw
	// query. Pick a fast/local model here.
	AgenticPlannerModel string `json:"agentic_planner_model,omitempty"`

	// NeighborWindowEnabled expands each returned chunk with its document
	// neighbors (preceding/following) for adjacent context (ADR-0060). Cheap id
	// lookups, no model. Default off.
	NeighborWindowEnabled bool `json:"neighbor_window_enabled,omitempty"`

	// Recall sizing (ADR-0054 retrieval tuning). The seed vector/ANN search and
	// the returned window were hardcoded (over-fetch 25 → return 10), so the
	// request top_k had no effect and the candidate pool was too small for the
	// gold chunk to surface. These make them tunable. 0 ⇒ built-in defaults
	// (RecallTopK 10, RecallOverFetch 25).
	//   RecallOverFetch — how many candidates the seed search fetches (the pool
	//     the gold chunk must be in). HnswEfSearch must be >= this to take effect.
	//   RecallTopK      — how many results recall returns to the caller.
	//   HnswEfSearch    — HNSW ef_search GUC (candidate breadth); >= RecallOverFetch.
	RecallTopK      int `json:"recall_top_k,omitempty"`
	RecallOverFetch int `json:"recall_over_fetch,omitempty"`
	HnswEfSearch    int `json:"hnsw_ef_search,omitempty"`

	// Hybrid retrieval (ADR-0054): fuse dense (pgvector cosine) with sparse/lexical
	// (Postgres full-text) via Reciprocal Rank Fusion, so exact-token chunks the
	// embedder misses (names, titles, places) still enter the candidate pool.
	// false ⇒ vector-only (legacy). HybridRRFK is the RRF constant (default 60).
	HybridSearchEnabled     bool    `json:"hybrid_search_enabled,omitempty"`
	HybridRRFK              int     `json:"hybrid_rrf_k,omitempty"`
	HybridLexicalWeight     float64 `json:"hybrid_lexical_weight,omitempty"`     // >1 leans RRF toward exact-term/entity matches; ≤0 ⇒ 1.0
	HydeEnabled             bool    `json:"hyde_enabled,omitempty"`              // HyDE: embed a hypothetical answer passage for hop-1 dense retrieval
	AgenticIrcotEnabled     bool    `json:"agentic_ircot_enabled,omitempty"`     // IRCoT: reason-then-retrieve loop (CoT step drives next retrieval)
	AgenticDecomposeEnabled bool    `json:"agentic_decompose_enabled,omitempty"` // up-front grounded decomposition: decompose whole question, retrieve+answer each sub-question

	// BlendEnabled (ADR-0054 Stage A) re-ranks recall candidates by the
	// multi-signal blend (cosine + recency + confidence + pagerank + activation)
	// instead of bare cosine. false (default) ⇒ unchanged ranking. The bge
	// cross-encoder (Stage B) is a separate, later flag. Reads chunk_pagerank
	// (populated by the pagerank-recompute worker) — absent scores count as 0.
	BlendEnabled bool `json:"blend_enabled,omitempty"`

	// Stage-A signal weights. Zero/unset for ALL five ⇒ DefaultBlendWeights.
	// They need not sum to 1 (the blend normalizes by their sum).
	BlendWeightCosine     float64 `json:"blend_weight_cosine,omitempty"`
	BlendWeightRecency    float64 `json:"blend_weight_recency,omitempty"`
	BlendWeightConfidence float64 `json:"blend_weight_confidence,omitempty"`
	BlendWeightPageRank   float64 `json:"blend_weight_pagerank,omitempty"`
	BlendWeightActivation float64 `json:"blend_weight_activation,omitempty"`
	BlendWeightLexical    float64 `json:"blend_weight_lexical,omitempty"` // hybrid full-text/RRF signal (ADR-0054)

	// BlendWeightCoherence weights the graph-coherence signal: a candidate's
	// connectivity to the rest of the pool over chunk_triplets (shared entities +
	// `dated at`/`spoke at` timestamp hubs). The graph-native conversation
	// disambiguator (ADR-0054 / ADR-0053). Zero = off.
	BlendWeightCoherence float64 `json:"blend_weight_coherence,omitempty"`

	// RerankerEnabled (ADR-0054 Stage B) turns on cross-encoder reranking of the
	// top-K Stage-A candidates via the warm reranker_agent system organ. false
	// (default) ⇒ Stage-A order kept. A down/erroring reranker fail-softs to the
	// Stage-A order — retrieval never fails because the oracle is unreachable. The
	// model id lives in the agent's RERANK_MODEL env (large for the ceiling, base/
	// v2-m3 for a CPU edge), not here — the kernel only dispatches.
	RerankerEnabled bool `json:"reranker_enabled,omitempty"`

	// RerankerTopK is how many top Stage-A candidates the cross-encoder rescores.
	// Must be ≥ the depth at which the gold lands (LoCoMo: recall@50≈0.91), else the
	// reranker cannot promote gold it never sees. 0 ⇒ defaultRerankTopK (50). The
	// dominant CPU-latency driver (K forward passes/query).
	RerankerTopK int `json:"reranker_top_k,omitempty"`

	// RerankerWeight is w_bge in FinalScore = w_bge·bge + (1-w_bge)·stageA. 0 ⇒
	// the ADR default 0.5 (an enabled reranker with zero weight would be a no-op).
	RerankerWeight float64 `json:"reranker_weight,omitempty"`
}

// WorkspaceConfig — ADR-0016 cross-session workspace enrichment.
//
// Split out of the former 198-field flat ExecutionConfig (config schema v2).
// Field names, types and json tags are unchanged; only the nesting is new, so
// each field means exactly what it meant before.
type WorkspaceConfig struct {
	// WorkspaceMinFactCosine is the minimum raw cosine similarity (pre-floor-multiplier)
	// a FACT document must have against the planning query to be injected into the Planner.
	// Default: 0.60. PLANNERREQ REQ1. Documents below this threshold are discarded even if
	// they pass the broad RetrievalFloor.
	WorkspaceMinFactCosine float64 `json:"workspace_min_fact_cosine"`

	// WorkspacePlanningSlots is the number of LTM FACT documents injected into the
	// Planner's system prompt after MinFactCosine filtering. Default: 5. PLANNERREQ REQ2.
	WorkspacePlanningSlots int `json:"workspace_planning_slots"`

	// WorkspaceExecutionSlots is the number of LTM FACT documents merged into the
	// DAGExecutor's initial context. Default: 5. ADR-0016.
	WorkspaceExecutionSlots int `json:"workspace_execution_slots"`

	// WorkspaceEnableDriftGuard enables session-ID cross-contamination filtering
	// during scene-primed FACT pull. Default: false. ADR-0016.
	WorkspaceEnableDriftGuard bool `json:"workspace_enable_drift_guard"`

	// WorkspaceDriftThreshold is the cosine similarity threshold for contradiction
	// detection between top-K FACT results. Default: 0.7. ADR-0016.
	WorkspaceDriftThreshold float64 `json:"workspace_drift_threshold"`

	// WorkspaceLRUCacheCapacity is the number of distinct query strings held in
	// PrimeForStep's embedding and ContextRef LRU caches. Default: 256. ADR-0022.
	WorkspaceLRUCacheCapacity int `json:"workspace_lru_cache_capacity"`

	// ADR-0022 Phase 3: circuit-breaker flag.
	// When true, DAGExecutor populates Handoff.WorkingMemory from PrimeForStep.
	// When false, falls back to Phase 0 filterSnapshotForStep into Handoff.Context.
	UseGlobalWorkspace bool `json:"use_global_workspace"`
}

// IngestionConfig — Document ingest: the HTTP inbox, queue/worker sizing and per-chunk enrichment.
//
// Split out of the former 198-field flat ExecutionConfig (config schema v2).
// Field names, types and json tags are unchanged; only the nesting is new, so
// each field means exactly what it meant before.
type IngestionConfig struct {
	// EdgeExtractionBatchSize is the load-driven drain trigger for the
	// LLM-based entity+relation extractor (ADR-0052). One LLM call extracts
	// entities+relations for the whole batch; the per-fact sync path was
	// ~Nx slower. Default: 16. With streaming (the EdgeExtractor prefers
	// streaming when the Generator supports it), the prompt can be larger
	// without the http.Client.Timeout killing the body mid-stream.
	EdgeExtractionBatchSize int `json:"edge_extraction_batch_size"`

	// EdgeExtractionMaxIdleMs is the idle-time drain trigger: when the queue
	// is non-empty but hasn't hit batchSize, this is the max time the
	// batcher waits before flushing whatever it has. Default: 2000ms —
	// short enough that the graph warms up quickly under a steady ingest.
	EdgeExtractionMaxIdleMs int `json:"edge_extraction_max_idle_ms"`

	// EdgeExtractionQueueSize is the bounded channel size for the batcher.
	// When full, new ingests log a warning and skip enrichment (the doc
	// is still saved; only the graph population is dropped). Default: 4096.
	// Generous because the LLM call is the slow leg; a small queue drops
	// under steady ingest.
	EdgeExtractionQueueSize int `json:"edge_extraction_queue_size"`

	// EdgeExtractionLLMTimeoutMs is the per-batch LLM call timeout. On
	// timeout the batch falls through (no edges written). Default: 300000.
	// With streaming, the http.Client.Timeout doesn't apply; this context
	// timeout is the only cap. 5 minutes leaves headroom for the hosted
	// reasoning model to stream a 16-fact batch (typical: 60-180s).
	EdgeExtractionLLMTimeoutMs int `json:"edge_extraction_llm_timeout_ms"`

	// SceneGenOnIngestEnabled turns the per-item episodic scene-generation LLM
	// call back ON for the document-ingest path. Default OFF (ADR-0060): it is a
	// synchronous per-item LLM call that stalls ingest when no LLM is reachable
	// and is not needed for document/structure retrieval.
	SceneGenOnIngestEnabled bool `json:"scene_gen_on_ingest_enabled,omitempty"`

	// EvidenceCaptureEnabled preserves every ingested document's original bytes
	// as immutable evidence (content-addressed blob + evidence row + outbox)
	// BEFORE the semantic pipeline touches it — the knowledge substrate's
	// evidence foundation (ADR-0105). Default OFF until Phase 2 gives evidence a
	// consumer; when on, a capture failure fails the ingest, because a lane that
	// accepted content and cannot preserve it must not pretend it did. No LLM
	// anywhere in the lane.
	EvidenceCaptureEnabled bool `json:"evidence_capture_enabled,omitempty"`

	// IngestionHTTPPort is the port for the standalone plain-HTTP ingestion server (ADR-0028).
	// 0 disables the server (opt-in default). Use 8080 or similar for production.
	IngestionHTTPPort int `json:"ingestion_http_port,omitempty"`

	// IngestToken is the X-Ingest-Token secret for the webhook receiver (ADR-0028).
	// Empty string disables token validation (for local dev).
	IngestToken string `json:"ingest_token,omitempty"`

	// InboxDir is the directory watched by DirectoryWatcher (ADR-0028).
	InboxDir string `json:"inbox_dir,omitempty"`

	// IngestionQueueSize is the bounded document queue depth (ADR-0028). Default 1000.
	IngestionQueueSize int `json:"ingestion_queue_size,omitempty"`

	// IngestionBatchSize is the documents-per-scene-generation call (ADR-0028). Default 5.
	IngestionBatchSize int `json:"ingestion_batch_size,omitempty"`

	// IngestionWorkers is the number of concurrent pipeline workers (ADR-0028). Default 5.
	IngestionWorkers int `json:"ingestion_workers,omitempty"`

	// IngestionBatchWaitMs is the max batcher wait before flushing (ADR-0028). Default 1000.
	IngestionBatchWaitMs int `json:"ingestion_batch_wait_ms,omitempty"`

	// KgExtractorEnabled (ADR-0053 D2 revised) routes write-time chunk-triplet
	// extraction through the deterministic, NO-LLM kg_extractor system agent
	// (metadata + spacy_patterns) instead of the LLM. false ⇒ the LLM extractor
	// (legacy default); true ⇒ the rule-stack organ. The kg_extractor_agent
	// process must be registered + serving for true to take effect.
	KgExtractorEnabled bool `json:"kg_extractor_enabled,omitempty"`
}

// HippocampusConfig — Hippocampus retrieval policies.
//
// Split out of the former 198-field flat ExecutionConfig (config schema v2).
// Field names, types and json tags are unchanged; only the nesting is new, so
// each field means exactly what it meant before.
type HippocampusConfig struct {
	// HippocampusPolicies maps policy name to retrieval thresholds (ADR-0027).
	// The Hippocampus looks up thresholds by the CachePolicy emitted by the Planner.
	HippocampusPolicies map[string]domain.HippocampusPolicy `json:"hippocampus_policies,omitempty"`

	// HippocampusDefaultPolicy is the fallback policy name when CachePolicy is empty
	// or unknown. Must be a key in HippocampusPolicies.
	HippocampusDefaultPolicy string `json:"hippocampus_default_policy,omitempty"`
}

// ScoutConfig — Scout discovery.
//
// Split out of the former 198-field flat ExecutionConfig (config schema v2).
// Field names, types and json tags are unchanged; only the nesting is new, so
// each field means exactly what it meant before.
type ScoutConfig struct {
	// ScoutEnabled (ADR-0051 issue-008) gates the pre-plan Scout. It is the A/B falsification
	// switch: false ⇒ one-shot planning (the baseline arm, the default — ADR-0051 is Proposed
	// and gated on the spike), true ⇒ Scout-grounded planning (the treatment arm). Flip it to
	// run the comparison; the promote/don't-promote decision stays a human call.
	ScoutEnabled bool `json:"scout_enabled,omitempty"`

	// ScoutModel (ADR-0051) is the model id the pre-plan Scout reasons with. Under the
	// ADR-0079 deterministic-first design this is only the FALLBACK model for the opt-in
	// LLM tier (ScoutLLMTierEnabled) — a cheap/fast variant (e.g. llm:mimo) is ideal.
	// "" ⇒ the gateway's default model (still via a properly-allocated managed session).
	ScoutModel string `json:"scout_model,omitempty"`

	// ScoutLLMTierEnabled (ADR-0079 D1) turns on the opt-in LLM discovery tier (the
	// ADR-0051 run_think scout) layered ON TOP of the deterministic probe registry. Off
	// (default) ⇒ deterministic probes only, zero LLM on the discovery hot path.
	ScoutLLMTierEnabled bool `json:"scout_llm_tier_enabled,omitempty"`

	// ScoutHTTPProbeEnabled (ADR-0079 D2) registers the http/openapi discovery source.
	// Off (default) because probing a URL from an untrusted request is an SSRF surface;
	// when on, the source's guard refuses loopback/private/link-local hosts unless
	// ScoutHTTPAllowPrivate is also set.
	ScoutHTTPProbeEnabled bool `json:"scout_http_probe_enabled,omitempty"`

	// ScoutHTTPAllowPrivate (ADR-0079 D2) permits the http source to reach loopback/private
	// hosts (dev only — e.g. a localhost API). Default false (fail-closed).
	ScoutHTTPAllowPrivate bool `json:"scout_http_allow_private,omitempty"`

	// ScoutDiscoveryRoots (ADR-0079 D2/D6) are the directories the deterministic filesystem
	// source may read. Empty ⇒ the kernel's working directory only (fail-closed-ish).
	ScoutDiscoveryRoots []string `json:"scout_discovery_roots,omitempty"`

	// ScoutScanCap (ADR-0051 D5) is the max number of LIVE world observations the Scout
	// makes per request before planning. Bounds plan-time latency; over-budget referents
	// become a discovery step. 0 ⇒ DefaultDiscoveryCap (3).
	ScoutScanCap int `json:"scout_scan_cap,omitempty"`
}

// ToolsConfig — Tool execution policy and retrieval.
//
// Split out of the former 198-field flat ExecutionConfig (config schema v2).
// Field names, types and json tags are unchanged; only the nesting is new, so
// each field means exactly what it meant before.
type ToolsConfig struct {
	// ToolEffectsStrict (ADR-0086) makes an undeclared effect set a REGISTRATION
	// ERROR instead of inferring effects from the tool's other manifest fields. A
	// tool that declares no effects is a registration error, not an unrestricted
	// tool — but flipping that on before the manifests carry `effects` would refuse
	// every tool in an existing install, so the default is false and inference
	// covers the migration. Turn it on once `cambrian tools` reports no inferred
	// tools; after that, a new tool cannot ship unclassified.
	//
	// An effect OUTSIDE the closed set is fatal in both modes — this flag governs
	// absence, never validity.
	ToolEffectsStrict bool `json:"tool_effects_strict,omitempty"`

	// ToolsUnrestricted (ADR-0039) is the operator-chosen bypass of the tool-grant
	// system: when true, every named agent may call every registered tool with an
	// allow-all resource policy. Approval for dangerous tools STILL applies. For
	// trusted/dev deployments only — default false (fail-closed: no grants ⇒ no tools).
	ToolsUnrestricted bool `json:"tools_unrestricted,omitempty"`

	// ToolsAutoApprove (ADR-0039) installs an auto-approving ApprovalController so
	// dangerous tools (execute_command/execute_python) run WITHOUT an operator
	// decision. Distinct from ToolsUnrestricted (which bypasses grants but NOT
	// approval): this bypasses ONLY the human-in-the-loop approval gate, which is
	// unanswerable in an unattended local/dev run. For trusted/dev deployments
	// only — default false (fail-closed: dangerous tools require an operator).
	// Note: interview/evaluation sessions auto-approve independently of this flag
	// (the sandbox is their containment boundary) — see EvaluationSessionSet.
	ToolsAutoApprove bool `json:"tools_auto_approve,omitempty"`

	// ToolRetrievalFloor (ADR-0044) is the minimum cosine similarity for a tool to
	// be served by semantic retrieval. Below it, "no tool fits" ⇒ an empty menu
	// (grounding safeguard). 0 ⇒ no floor (any top-k is returned). Tunable.
	ToolRetrievalFloor float64 `json:"tool_retrieval_floor,omitempty"`

	// DiscoverySafeTools (ADR-0051 D6) is the operator-curated allowlist of tool names the
	// Scout principal may use for read-only pre-plan discovery — a HARD ceiling that holds
	// even under ToolsUnrestricted (the Scout fires unattended at plan time, so it earns a
	// higher bar than "not dangerous"). Empty ⇒ no ceiling configured: the Scout falls back
	// to normal grant resolution (so dev/unrestricted still works), and in a default prod
	// run finds no tools ⇒ degrades to one-shot.
	DiscoverySafeTools []string `json:"discovery_safe_tools,omitempty"`
}

// ChatConfig — Chat session worker pool.
//
// Split out of the former 198-field flat ExecutionConfig (config schema v2).
// Field names, types and json tags are unchanged; only the nesting is new, so
// each field means exactly what it meant before.
type ChatConfig struct {
	// ADR-0084 D4: the OSS chat lane runs conversational turns on a BOUNDED POOL of
	// stateless session workers rather than one process per conversation. Chat is an OSS
	// capability (a premium manager adds auth/tenanting in front of it), so these keys name
	// no premium feature.
	//
	// ChatPoolSize is the worker count and therefore the maximum number of turns executing
	// at once. 0 (default) disables the chat lane — nothing is spawned.
	ChatPoolSize int `json:"chat_pool_size,omitempty"`

	// ChatPoolAgentID is the agent each worker runs. Defaults to "chat_agent" (the OSS
	// conversational worker in agents/chat_agent.py). Premium ships a customer-service
	// specialisation for the airline domain, "airline_chat_agent"; select it by setting this key.
	ChatPoolAgentID string `json:"chat_pool_agent_id,omitempty"`

	// ChatPoolQueueSize bounds how many turns may WAIT for a free worker before new turns
	// are shed. An unbounded queue in front of a bounded pool is just a slower crash, so 0
	// (shed as soon as every worker is busy) is a safe setting.
	ChatPoolQueueSize int `json:"chat_pool_queue_size,omitempty"`

	// ChatPoolAcquireTimeoutSeconds bounds how long an admitted turn waits for a worker
	// before being shed. 0 means wait for the caller's context.
	ChatPoolAcquireTimeoutSeconds int `json:"chat_pool_acquire_timeout_seconds,omitempty"`
}

// AgentsConfig — Agent process runtime: resource caps, env passthrough, daemon restart policy and the interview/scout disable flags.
//
// Split out of the former 198-field flat ExecutionConfig (config schema v2).
// Field names, types and json tags are unchanged; only the nesting is new, so
// each field means exactly what it meant before.
type AgentsConfig struct {
	// AgentMemoryLimitMB caps the resident memory of each spawned agent process
	// (SEC-01 resource caps): Windows Job Object ProcessMemoryLimit; Unix
	// RLIMIT_AS. A runaway agent (e.g. a torch/docling OOM) is killed at its cap
	// instead of taking down the kernel host. 0 = disabled (default) — opt-in so
	// legitimately memory-heavy agents are not capped by surprise.
	AgentMemoryLimitMB int `json:"agent_memory_limit_mb,omitempty"`

	// AgentEnvPassthrough lists extra NON-SECRET environment variable names that
	// spawned agents may inherit beyond the OS base allowlist (SEC-01). Secret-
	// looking names (*_API_KEY, CAMBRIAN_*, provider prefixes, …) are stripped
	// regardless. Empty by default: agents get only OS essentials + the substrate
	// socket/addr (passed as flags), never the kernel's credentials.
	AgentEnvPassthrough []string `json:"agent_env_passthrough,omitempty"`

	// DisableScout skips the ADR-0051 pre-plan discovery Scout. Benchmark/eval-
	// only: the Scout spends an LLM discovery pass per request to shape the plan
	// to the observed world, which the offline routing eval does not need and
	// which serialises ahead of every planner call. Default false ⇒ Scout runs.
	DisableScout bool `json:"disable_scout"`

	// DisableInterviews skips the ADR-0037 graded LLM interview (Examiner) at
	// registration. Benchmark/eval-only: the graded interview spends an LLM Q&A
	// per agent to seed cold-start trust priors, which the offline routing eval
	// does not need (it reads manifest capabilities directly) and which otherwise
	// starves the planner for LLM throughput. Default false. Merit falls back to
	// the neutral cold-start prior.
	DisableInterviews bool `json:"disable_interviews"`

	// ── REACT-04 / ADR-0070: daemon supervision ──
	// DaemonRestartMaxAttempts is the max automatic restarts of a crashed daemon per
	// window before it is quarantined (crash-loop guard). 0 disables auto-restart
	// (pre-REACT-04 behavior: a crashed daemon stays down). Default 5.
	DaemonRestartMaxAttempts int `json:"daemon_restart_max_attempts"`

	// DaemonRestartWindowSeconds is the flap window for the attempt count. Default 300.
	DaemonRestartWindowSeconds int `json:"daemon_restart_window_seconds"`

	// DaemonRestartBaseBackoffMs / DaemonRestartMaxBackoffMs bound the exponential
	// (full-jitter) restart backoff. Defaults 1000 / 30000.
	DaemonRestartBaseBackoffMs int `json:"daemon_restart_base_backoff_ms"`
	DaemonRestartMaxBackoffMs  int `json:"daemon_restart_max_backoff_ms"`
}

// SupervisionConfig — Supervision statistics: aggregation cadence, EWMA/latency windows and histogram sizing.
//
// Split out of the former 198-field flat ExecutionConfig (config schema v2).
// Field names, types and json tags are unchanged; only the nesting is new, so
// each field means exactly what it meant before.
type SupervisionConfig struct {
	// ProfileAggregator settings.
	// ProfileAggregatorIntervalSeconds is how often the background aggregator runs.
	ProfileAggregatorIntervalSeconds int `json:"profile_aggregator_interval_seconds"`

	// EWMAAlpha is the smoothing factor for exponentially weighted moving averages.
	EWMAAlpha float64 `json:"ewma_alpha"`

	// LatencyWindowSize is the number of recent events used for latency median computation.
	LatencyWindowSize int `json:"latency_window_size"`

	// SignalNoiseThreshold is the max number of invalid signals within SignalNoiseWindowSecs
	// before a Daemon Observer instance is inhibited (circuit breaker). Default: 3.
	SignalNoiseThreshold int `json:"signal_noise_threshold"`

	// SignalNoiseWindowSecs is the sliding window in seconds for neural noise detection.
	// Default: 10.
	SignalNoiseWindowSecs int `json:"signal_noise_window_secs"`

	// HistogramMinSamples is the minimum observations before adaptive token sizing
	// activates for a step type. Default: 20. ADR-0018.
	HistogramMinSamples int `json:"histogram_min_samples"`

	// HistogramAlpha is the damped update gain for adaptive MaxEnergy tuning.
	// Default: 0.2. ADR-0018.
	HistogramAlpha float64 `json:"histogram_alpha"`
}

// RouterConfig — Ingress classification routing.
//
// Split out of the former 198-field flat ExecutionConfig (config schema v2).
// Field names, types and json tags are unchanged; only the nesting is new, so
// each field means exactly what it meant before.
type RouterConfig struct {
	// ADR-0031: Universal Input Router — Layer 3 classification tuning.
	// RouterMinClassificationConfidence is the minimum LLM confidence score
	// required to act on a Layer 3 decision. Below this threshold the Router
	// returns DecisionClarification with structured options instead of acting.
	RouterMinClassificationConfidence float64 `json:"router_min_classification_confidence,omitempty"`

	// RouterClassificationBodyChars is the maximum number of characters of
	// RouterInput.Body passed to the Layer 3 classification prompt. Intent
	// signals are almost always in the first sentence; truncation limits token cost.
	RouterClassificationBodyChars int `json:"router_classification_body_chars,omitempty"`

	// ADR-0034: controlled classification-tag vocabulary. The deployment-time set
	// of tags an agent may write via memory.remember()/artifacts.save(). A write
	// requesting any tag outside this set is rejected (InvalidArgument) before
	// scope checks — preventing tag coinage. Empty disables the coinage check
	// (ForbiddenTags enforcement still applies). ADR-0034 (D8).
	ClassificationVocabulary []string `json:"classification_vocabulary,omitempty"`

	// KAnonymityFloor is the minimum number of distinct source sessions a theme
	// cluster must aggregate before a derived insight may be promoted to a broader
	// scope (ADR-0034 D11). Per-record promotion is forbidden — a single customer's
	// data can never become a Tier-2 doc. Default 5 when unset (<=1 means K=5).
	KAnonymityFloor int `json:"k_anonymity_floor,omitempty"`
}

// LLMConfig — LLM gateway concurrency, retry and exchange capture.
//
// Split out of the former 198-field flat ExecutionConfig (config schema v2).
// Field names, types and json tags are unchanged; only the nesting is new, so
// each field means exactly what it meant before.
type LLMConfig struct {
	// CaptureLLMExchanges forks the full prompt+completion of every managed-proxy
	// agent generation to the operator feed as a live-only AgentLLMExchangeOp, so a
	// benchmark can review every agent output and reconstruct its internal ReAct loop
	// without SDK instrumentation (ADR-0079). Default OFF: prompts are large and may be
	// sensitive, so this is a benchmark/diagnostic lane, not a production default. Zero
	// behavior change — emission is fire-and-forget and never affects the agent stream.
	CaptureLLMExchanges bool `json:"capture_llm_exchanges,omitempty"`

	// LLMGatewayMaxConcurrency is the CONWIP semaphore size bounding in-flight
	// GenerateViaModelStream stream initializations. Default: 20. ADR-0018.
	LLMGatewayMaxConcurrency int `json:"llm_gateway_max_concurrency"`

	// LLMGatewayRetryBackoffMs is the base backoff for jittered retry on
	// ErrGatewayOverloaded. Default: 100. ADR-0018.
	LLMGatewayRetryBackoffMs int `json:"llm_gateway_retry_backoff_ms"`
}

type ExecutionConfig struct {
	// Plan-level execution limits: timeouts, recursion, replanning, fan-out and the energy/cost budget.
	Plan PlanConfig `json:"plan"`

	// Gatekeeper admission scoring: candidate cap and the w1..w4 signal weights.
	Gatekeeper GatekeeperConfig `json:"gatekeeper"`

	// Verifier pool sizing, trust-score weighting and cross-verification sampling.
	Verification VerificationConfig `json:"verification"`

	// How a step reaches an agent: auction vs deterministic route, bidding, merit, calibration and the EFE selector.
	Routing RoutingConfig `json:"routing"`

	// Capability vocabulary, resolution and the retired clusterer knobs.
	Capability CapabilityConfig `json:"capability"`

	// Session lifecycle: idle timeout, retention sweeps and token TTL.
	Session SessionConfig `json:"session"`

	// Long-term memory: experiential capture, the Tier-1/Tier-2 promotion pipeline, consolidation and GC.
	Memory MemoryConfig `json:"memory"`

	// ADR-0094 procedure induction.
	Procedure ProcedureConfig `json:"procedure"`

	// The recall path: seed sizes, hybrid fusion, spreading, KG expansion, blending and reranking.
	Retrieval RetrievalConfig `json:"retrieval"`

	// ADR-0016 cross-session workspace enrichment.
	Workspace WorkspaceConfig `json:"workspace"`

	// Document ingest: the HTTP inbox, queue/worker sizing and per-chunk enrichment.
	Ingestion IngestionConfig `json:"ingestion"`

	// Hippocampus retrieval policies.
	Hippocampus HippocampusConfig `json:"hippocampus"`

	// Scout discovery.
	Scout ScoutConfig `json:"scout"`

	// Tool execution policy and retrieval.
	Tools ToolsConfig `json:"tools"`

	// Chat session worker pool.
	Chat ChatConfig `json:"chat"`

	// Agent process runtime: resource caps, env passthrough, daemon restart policy and the interview/scout disable flags.
	Agents AgentsConfig `json:"agents"`

	// Supervision statistics: aggregation cadence, EWMA/latency windows and histogram sizing.
	Supervision SupervisionConfig `json:"supervision"`

	// Ingress classification routing.
	Router RouterConfig `json:"router"`

	// LLM gateway concurrency, retry and exchange capture.
	LLM LLMConfig `json:"llm"`

	// Graph holds spreading activation configuration (ADR-0017).
	Graph GraphConfig `json:"graph"`
}

// GraphConfig defines spreading activation parameters (ADR-0017).
type GraphConfig struct {
	DecayFactor            float64 `json:"decay_factor"`
	MaxDepth               int     `json:"max_depth"`
	EnergyFloor            float64 `json:"energy_floor"`
	WeightContradicts      float64 `json:"weight_contradicts"`
	WeightSpecifies        float64 `json:"weight_specifies"`
	WeightCloses           float64 `json:"weight_closes"`
	WeightDiscussedIn      float64 `json:"weight_discussed_in"`
	ConsolidatorLLMTimeout int     `json:"consolidator_llm_timeout"`
}

// TelemetryConfig controls runtime observability (OTel, Prometheus, OTLP).
// Zero values disable all exporters — safe default for OSS deployments.
type TelemetryConfig struct {
	OTLPEndpoint                 string  `json:"otlp_endpoint"`
	TraceSamplingRate            float64 `json:"trace_sampling_rate"`             // default 1.0 when set
	MetricsExportIntervalSeconds int     `json:"metrics_export_interval_seconds"` // default 10 when set
	EnableStdoutExporter         bool    `json:"enable_stdout_exporter"`
	PrometheusPort               int     `json:"prometheus_port"` // 0 = disabled
}

// ADR-0057: Langfuse is a PREMIUM feature. Its config lives in the premium module,
// not in the OSS config schema. (Was LangfuseRawConfig — removed from OSS.)

type Config struct {
	// SchemaVersion identifies the config layout this document is written in.
	//
	//	1 (or absent) — the original flat `execution` block, 198 keys at one level.
	//	2             — `execution` grouped into named blocks (retrieval, routing, …).
	//
	// Absent means 1, because every config written before v2 existed has no such
	// field and treating "unstamped" as "current" would skip the migration for
	// exactly the files that need it. A v1 document is converted in memory on load
	// (see execution_migrate.go); the file on disk is left alone.
	SchemaVersion int `json:"schema_version,omitempty"`

	Metabolism struct {
		PythonExecutable string `json:"python_executable"`
		AgentsDir        string `json:"agents_dir"`
	} `json:"metabolism"`
	Storage struct {
		DataDir string `json:"data_dir"`
		DBName  string `json:"db_name"`
		// AutoMigrate runs the DB migration runner at boot (PLAT-02 / ADR-0064:
		// stamp baseline + apply pending forward-delta migrations + refuse if the DB
		// is ahead of the binary). Default true — set false to manage migrations only
		// via the `migrate` CLI / external tooling.
		AutoMigrate bool `json:"auto_migrate"`
	} `json:"storage"`
	Database struct {
		Host     string `json:"host"`
		Port     string `json:"port"`
		User     string `json:"user"`
		Password string `json:"password"`
		DBName   string `json:"dbname"`
	} `json:"database"`
	Server struct {
		Port string `json:"port"`
		// BindAddress is the interface the gRPC plane listens on. Default
		// "127.0.0.1" — LOOPBACK, not every interface.
		//
		// It used to bind ":port", which is 0.0.0.0: the operator plane was reachable
		// from any host on the network, in plaintext, with no way to notice from the
		// config. The zero-config edge profile wants localhost anyway; a deployment
		// that genuinely serves other machines sets this deliberately, and then the
		// TLS guard below applies.
		BindAddress string `json:"bind_address"`
		// TLSCertFile / TLSKeyFile enable TLS on the operator listener (SEC-03).
		// Both required together; either alone is a config error rather than a
		// silent downgrade to plaintext.
		TLSCertFile string `json:"tls_cert_file"`
		TLSKeyFile  string `json:"tls_key_file"`
		// InsecureLocalhost permits PLAINTEXT on a non-loopback bind. Off by default:
		// without it, binding a routable address with no TLS FAILS at boot rather
		// than quietly serving an unencrypted operator plane to the network.
		//
		// Named for the case it is meant to cover — a trusted single host — and
		// deliberately awkward to reach for on a server.
		InsecureLocalhost bool `json:"insecure_localhost"`
		// HealthzPort, when > 0, starts an HTTP /healthz shim on that port for dumb
		// probes (PLAT-03 / ADR-0065). Off by default; the gRPC grpc.health.v1 service
		// is always on the main listener.
		HealthzPort int `json:"healthz_port"`
		// HealthCheckIntervalSeconds is how often the readiness probe (DB ping) runs.
		// Default 10.
		HealthCheckIntervalSeconds int `json:"health_check_interval_seconds"`
	} `json:"server"`
	// ADR-0042: new model-provisioning spine. Additive for now — the legacy LLM
	// block and Models array above remain until the cutover (slice 0042-07).
	Embedder    EmbedderConfig    `json:"embedder"`
	LLMProvider LLMProviderConfig `json:"llm_provider"`
	Execution   ExecutionConfig   `json:"execution"`
	Telemetry   TelemetryConfig   `json:"telemetry"`
	AgentPool   AgentPoolConfig   `json:"agent_pool"`
	// ADR-0043: external MCP servers the kernel consumes tools from. Opt-in —
	// absent/empty ⇒ no MCP behaviour.
	MCP     MCPConfig     `json:"mcp,omitempty"`
	Chunker ChunkerConfig `json:"chunker"`
}

type ChunkerConfig struct {
	Default   string            `json:"default"`
	Routes    map[string]string `json:"routes,omitempty"`
	ExtRoutes map[string]string `json:"ext_routes,omitempty"`
	Late      LateChunkerConfig `json:"late"`
}

type LateChunkerConfig struct {
	Enabled      bool `json:"enabled"`
	MaxDocTokens int  `json:"max_doc_tokens"`
}

var KnownChunkerNames = map[string]struct{}{
	"option_c":            {},
	"recursive_character": {},
	"ast_go":              {},
	"markdown_header":     {},
	"late":                {},
}

func (c ChunkerConfig) Validate() *ConfigError {
	var errs []string
	if c.Default == "" {
		errs = append(errs, "chunker.default is required")
	} else if _, ok := KnownChunkerNames[c.Default]; !ok {
		errs = append(errs, fmt.Sprintf("chunker.default %q is not a known chunker (known: %s)", c.Default, knownChunkerNameList()))
	}
	if c.Late.Enabled && c.Late.MaxDocTokens <= 0 {
		errs = append(errs, "chunker.late.max_doc_tokens must be > 0 when chunker.late.enabled is true")
	}
	for k, v := range c.Routes {
		if _, ok := KnownChunkerNames[v]; !ok {
			errs = append(errs, fmt.Sprintf("chunker.routes[%q] -> %q: %q is not a known chunker (known: %s)", k, v, v, knownChunkerNameList()))
		}
	}
	for k, v := range c.ExtRoutes {
		if _, ok := KnownChunkerNames[v]; !ok {
			errs = append(errs, fmt.Sprintf("chunker.ext_routes[%q] -> %q: %q is not a known chunker (known: %s)", k, v, v, knownChunkerNameList()))
		}
	}
	if len(errs) > 0 {
		return &ConfigError{Field: "chunker", Message: strings.Join(errs, "; ")}
	}
	return nil
}

func knownChunkerNameList() string {
	names := make([]string, 0, len(KnownChunkerNames))
	for n := range KnownChunkerNames {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// MCPConfig lists the external MCP servers the kernel connects to (ADR-0043).
type MCPConfig struct {
	Servers []MCPServerConfig `json:"servers,omitempty"`
	// DefaultSessionBudget is the per-session $ cap on MCP tool spend (ADR-0043
	// D5). 0 ⇒ MCP calls are tracked-but-unbounded (no per-session enforcement).
	DefaultSessionBudget float64 `json:"default_session_budget,omitempty"`
	// CallTimeoutMs bounds each MCP tool call (ADR-0043 D8). 0 ⇒ no per-call deadline.
	CallTimeoutMs int `json:"call_timeout_ms,omitempty"`
}

// MCPServerConfig describes one MCP server. Auth secrets are referenced by env
// var name, never inlined. Per-tool policy (pricing/dangerous/data kinds) is
// operator config keyed to the server's advertised tool names (A1.5 — the
// server's own metadata is never a trust input).
type MCPServerConfig struct {
	ID        string   `json:"id"`
	Transport string   `json:"transport"`      // "stdio" | "http" (Streamable HTTP) | "sse" (legacy HTTP+SSE)
	Endpoint  string   `json:"endpoint"`       // URL (http) or command (stdio)
	Args      []string `json:"args,omitempty"` // stdio command args (e.g. ["-y","firecrawl-mcp"])
	Auth      struct {
		Type     string `json:"type"`      // "none" | "bearer" | "header"
		Header   string `json:"header"`    // header name when type="header"
		TokenEnv string `json:"token_env"` // env var holding the credential
	} `json:"auth"`
	Tools []MCPToolConfig `json:"tools,omitempty"`

	// ClassificationTags apply to EVERY tool this server advertises (ADR-0085 D2 /
	// ADR-0090). A remote server's tools are discovered dynamically, so without a
	// server-level default they arrive UNTAGGED — and an untagged resource has no
	// tags for a policy to forbid, which means "only this path may use these tools"
	// cannot be expressed at all.
	//
	// Set on the server rather than per tool because a domain boundary is a property
	// of the whole integration: every tau2-airline tool touches airline data. A
	// per-tool list, when present, is used INSTEAD of this for that tool.
	ClassificationTags []string `json:"classification_tags,omitempty"`
}

// MCPToolConfig is operator policy for one tool the server advertises (ADR-0043).
type MCPToolConfig struct {
	Name           string   `json:"name"`
	Dangerous      bool     `json:"dangerous,omitempty"`
	DataWriteKinds []string `json:"data_write_kinds,omitempty"`
	// ClassificationTags name the domain this tool touches. Overrides the server's
	// default when set, so one tool in an otherwise-domain-bound server can be
	// classified differently.
	ClassificationTags []string `json:"classification_tags,omitempty"`
	Pricing            struct {
		Kind            string  `json:"kind"`                         // "flat" | "per_unit" | "token"
		UnitCost        float64 `json:"unit_cost"`                    // $ per unit / per call
		MaxUnitsPerCall int     `json:"max_units_per_call,omitempty"` // reservation cap
		ChargeOnFailure string  `json:"charge_on_failure,omitempty"`  // "none" | "cap"
	} `json:"pricing,omitempty"`
}

// ConfigError is returned by LoadConfig and Validate when the configuration is
// invalid. Field names the offending dot-path key; Message carries the human-readable
// description. Multiple failures are joined into a single ConfigError.
type ConfigError struct {
	Field   string
	Message string
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("config: field %q: %s", e.Field, e.Message)
}

// DefaultConfig returns a fully pre-filled *Config with every field set to its
// documented default value. It is the authoritative source of defaults and is used
// as the lowest-priority layer in the Koanf loading pipeline.
func DefaultConfig() *Config {
	cfg := &Config{
		Execution: ExecutionConfig{
			Plan: PlanConfig{
				StepTimeoutMultiplier:       2.0,
				StepTimeoutBaseBufferMs:     5000,
				PlanTimeoutMs:               120000,
				ContextGrowthK:              0.001,
				MaxRecursionDepth:           3,
				FallbackConfidenceThreshold: 0.4,
				FallbackEnabled:             true,
				MaxReplanAttempts:           2,
				MaxFanOutWidth:              64,
				MaxPartialContextBytes:      51200,
				PlanDriftDays:               7,
				MaxContextSlots:             20,
				ContextRefSnippetChars:      500,
				BudgetExhaustionAlarmRate:   0.05,
				MinStepEnergy:               256,
				MaxStepEnergy:               32768,
			},
			Gatekeeper: GatekeeperConfig{
				GatekeeperMaxCandidates:    5,
				GatekeeperW1:               0.4,
				GatekeeperW2:               0.4,
				GatekeeperW3:               0.2,
				GatekeeperW4:               0.15,
				ColdStartPenaltyMultiplier: 0.6,
				MinAuctionConfidence:       0.3,
			},
			Verification: VerificationConfig{
				VerifierPoolThreshold:      0.8,
				TrustBoostThreshold:        0.4,
				VerificationQueueCapacity:  256,
				MinVerifiedEvents:          3,
				VerifierRecencyWindow:      3,
				TrustScoreCalWeight:        0.6,
				TrustScoreAbsWeight:        0.4,
				VerifierPoolMinSize:        2,
				VerifierPoolThresholdStep:  0.05,
				VerifierPoolThresholdFloor: 0.6,
				CrossVerifyRate:            0.05,
			},
			Routing: RoutingConfig{
				// ADR-0100: default ON. Capability-typed dispatch gates on
				// step.required_capabilities, and this flag is what makes the planner emit
				// them — with it off, L1 is a no-op, the D5 ladder never fires, and
				// "dispatch" degrades to merit-ranking alone. It stops being an arm the
				// moment selection depends on it.
				CapabilityContract:                  true,
				AgentPinning:                        true, // honour an explicit "use agent X" directive
				RoutingTraceEnabled:                 true,
				ExplorationRate:                     0.05,
				AuctionBidTimeoutMs:                 2000,
				ProposalTimeoutMs:                   2000,
				BidRound:                            false, // ADR-0100: dispatch is the default; true = legacy auction (A/B only)
				DispatchCheapEnergyMax:              0,     // cheapest-competent branch OFF until the A/B lands (P0 = pure argmax)
				DispatchMeritFloor:                  0.5,   // neutral prior; a candidate must be at least average to be "competent"
				CalibratedBids:                      false, // ROUTE-05 arm toggle; OFF = raw self-reported confidence
				BidCalibrationMinSamples:            10,
				PerCapabilityMerit:                  false, // ROUTE-06 arm toggle; OFF = global merit + unconditional bypass
				LearnedScorer:                       false, // ROUTE-07 arm toggle; OFF = hand-weighted GatekeeperScore
				ProvisionalExplorationBudget:        3,
				ProvisionalExplorationWindowSeconds: 3600,
				ResourceSelector:                    "auction",
				EFETrafficPercent:                   0,
				EFEExplorationBonus:                 0.1,
			},
			Capability: CapabilityConfig{
				CapabilityClusterThreshold:       0.80,
				CapabilityClusterEpsilon:         0.02,
				CapabilityClusterMinAgents:       3,
				CapabilityClusterIntervalSeconds: 3600,
				CanonicalVocab:                   false,               // ROUTE-04 arm toggle; OFF = declared strings verbatim
				CapabilityResolution:             true,                // ADR-0100 D5 ladder; only fires for steps that declare requirements
				CapabilityAliases:                map[string]string{}, // authored, reviewed; add an entry only when a real step failed to route
			},
			Session: SessionConfig{
				SessionTTLDays:                  7,
				SessionIdleTimeoutMinutes:       1440, // 24h idle ⇒ dormant
				SessionIdleSweepIntervalSeconds: 300,
				// 30 days of completed history is generous for debugging and audit while
				// still bounding the table. Retention only ever touches COMPLETED sessions.
				SessionRetentionDays:                 30,
				SessionRetentionSweepIntervalSeconds: 3600,
				SessionTokenSweepIntervalSeconds:     30,
				SessionTokenTTLMultiplier:            5.0,
			},
			Memory: MemoryConfig{
				MemoryRelevanceThreshold:       0.70,
				MaxMemoryResults:               5,
				MaxNeighborExpansion:           3,
				MinGCAgeDays:                   30,
				Tier2MaxIdleSeconds:            300,
				Tier2LLMTimeout:                30,
				Tier2BatchSize:                 32,
				Tier1ChannelCapacity:           4096,
				CircadianStaleDocWarnThreshold: 50,
				RememberDefaultActivation:      0.5,
				ActivationThreshold:            0.1,
				// ADR-0049 A2.2 outcome records: ON (owner decision 2026-07-28). The RAW
				// path stays off — see ExperientialMemoryEnabled, which is deliberately
				// absent from this block and therefore false.
				ExperienceRecordsEnabled: true,
				ExperienceSurpriseFloor:  0.5,
			},
			Procedure: ProcedureConfig{
				ProcedureInductionIntervalHours: 0, // ADR-0094: 0 = scheduler disabled (default)
				ProcedureMinSamples:             2,
				ProcedureMaxEpisodesPerPass:     500,
				ProcedureDeprecateBelow:         0.3, // ADR-0049 A2.3 failure-precedent gate
			},
			Retrieval: RetrievalConfig{
				RetrievalFloor:           0.2,
				RecallSpreadingEnabled:   true, // ADR-0049 D10: spreading reads the Hebbian co_activated edges
				RecallSimilarityFloor:    0.25,
				HebbianEnabled:           true,
				HebbianLearningRate:      0.05,
				HebbianMaxWeight:         0.9,
				HebbianCoActivationFloor: 0.5,
				HebbianTopN:              5,
				HebbianDecayPerDay:       0.95,
				HebbianBaseWeight:        0.2,
				KG2RAGEnabled:            true,  // ADR-0053 D3: KG²RAG one-hop expansion; opt-out via config.json
				AnchorConstraintEnabled:  true,  // ADR-0053: document-local anchor promotion; opt-out via config.json
				StructureGraphEnabled:    true,  // ADR-0060: structure-aware ingestion (docling parse -> section graph -> structure retrieval) is the DEFAULT chunking pipeline; opt-out via config.json
				AgenticRetrievalEnabled:  false, // AGENTIC_RETRIEVAL_SPEC: opt-in agentic retrieval loop; A/B via config
				AgenticMaxHops:           1,     // Phase 2a: plan once, retrieve once
				KG2RAGMaxHops:            1,     // one-hop, KG²RAG paper default
				KG2RAGMaxExpanded:        20,    // cap on chunks added by expansion
				KG2RAGMaxEntities:        30,    // cap on entities walked per hop
				KG2RAGPerEntity:          5,     // cap on chunks pulled per entity
			},
			Workspace: WorkspaceConfig{
				WorkspaceMinFactCosine:    0.60,
				WorkspacePlanningSlots:    5,
				WorkspaceExecutionSlots:   5,
				WorkspaceDriftThreshold:   0.7,
				WorkspaceLRUCacheCapacity: 256,
				UseGlobalWorkspace:        true,
			},
			Ingestion: IngestionConfig{
				EdgeExtractionBatchSize:    16,
				EdgeExtractionMaxIdleMs:    2000,
				EdgeExtractionQueueSize:    4096,
				EdgeExtractionLLMTimeoutMs: 300000,
				IngestionHTTPPort:          0, // disabled by default; set to e.g. 8080 to enable
				InboxDir:                   "data/inbox",
				IngestionQueueSize:         1000,
				IngestionBatchSize:         5,
				IngestionWorkers:           5,
				IngestionBatchWaitMs:       1000,
			},
			Hippocampus: HippocampusConfig{
				HippocampusPolicies: map[string]domain.HippocampusPolicy{
					"codegen":   {SimilarityThreshold: 0.92, ConfidenceFloor: 0.85, MaxAgeHours: 24},
					"cognitive": {SimilarityThreshold: 0.85, ConfidenceFloor: 0.70, MaxAgeHours: 168},
					"tool":      {SimilarityThreshold: 0.80, ConfidenceFloor: 0.60, MaxAgeHours: 720},
					"research":  {SimilarityThreshold: 0.88, ConfidenceFloor: 0.75, MaxAgeHours: 72},
					"default":   {SimilarityThreshold: 0.85, ConfidenceFloor: 0.70, MaxAgeHours: 168},
					// ADR-0029: episodic lane — lower threshold (narrative text) + 1-year max age.
					"episodic": {SimilarityThreshold: 0.65, ConfidenceFloor: 0.0, MaxAgeHours: 8760},
				},
				HippocampusDefaultPolicy: "default",
			},
			Agents: AgentsConfig{
				DaemonRestartMaxAttempts:   5, // REACT-04: auto-restart on, crash-loop → quarantine
				DaemonRestartWindowSeconds: 300,
				DaemonRestartBaseBackoffMs: 1000,
				DaemonRestartMaxBackoffMs:  30000,
			},
			Supervision: SupervisionConfig{
				ProfileAggregatorIntervalSeconds: 300,
				EWMAAlpha:                        0.5,
				LatencyWindowSize:                50,
				SignalNoiseThreshold:             3,
				SignalNoiseWindowSecs:            10,
				HistogramMinSamples:              20,
				HistogramAlpha:                   0.2,
			},
			Router: RouterConfig{
				RouterMinClassificationConfidence: 0.5,
				RouterClassificationBodyChars:     500,
			},
			LLM: LLMConfig{
				LLMGatewayMaxConcurrency: 20,
				LLMGatewayRetryBackoffMs: 100,
			},
			Graph: GraphConfig{
				DecayFactor:            0.75,
				MaxDepth:               3,
				EnergyFloor:            0.15,
				WeightContradicts:      0.9,
				WeightSpecifies:        0.7,
				WeightCloses:           0.6,
				WeightDiscussedIn:      0.5,
				ConsolidatorLLMTimeout: 60,
			},
		},
		Telemetry: TelemetryConfig{
			TraceSamplingRate:            1.0,
			MetricsExportIntervalSeconds: 10,
		},
		AgentPool: AgentPoolConfig{
			DefaultAgentTimeoutMs: 30000,
		},
		Embedder: EmbedderConfig{
			SupportsLongContext: false,
		},
		Chunker: ChunkerConfig{
			Default:   "option_c",
			Routes:    map[string]string{},
			ExtRoutes: map[string]string{},
			Late:      LateChunkerConfig{Enabled: false, MaxDocTokens: 8192},
		},
	}
	// PLAT-02 / ADR-0064: migrate-on-boot is on by default (matches the historical
	// always-run ensureSchema behavior); set storage.auto_migrate=false to opt out.
	cfg.Storage.AutoMigrate = true
	return cfg
}

// LoadConfig reads configuration from an 11-layer pipeline (lowest → highest
// priority) and returns the merged *Config. All secondary paths
// (tuning.json, tuning.local.json, config.local.json, embedder.json,
// embedder.local.json, providers.json, providers.local.json, mcp.json) are
// derived relative to the directory containing the primary `path` argument —
// NOT relative to the process CWD — so tests can use t.TempDir() paths
// cleanly and the layering is independent of where the process was launched
// from.
//
// Pipeline:
//  1. Go defaults (DefaultConfig(), full hyperparameter set)
//  2. configs/tuning.json (committed, curated power-user starter — 13 fields)
//  3. configs/tuning.local.json (gitignored, per-machine tuning override)
//  4. configs/config.json (the `path` argument, user-facing infrastructure)
//  5. configs/config.local.json (gitignored, per-machine infra override)
//  6. configs/embedder.json (gitignored, embedder model config)
//  7. configs/embedder.local.json (gitignored, per-machine embedder override)
//  8. configs/providers.json (gitignored, LLM provider list)
//  9. configs/providers.local.json (gitignored, per-machine LLM provider override)
//
// 10. configs/mcp.json (gitignored, MCP server definitions; absent ⇒ no MCP)
// 11. CAMBRIAN_* env vars (highest priority, full override of any field)
//
// ADR-0024 (amended). The CAMBRIAN_* env-var convention (`__` separator,
// ToLower) is unchanged and now also applies to fields in every layer.
// ResolveBaseDir returns the directory the kernel should resolve its config bundle
// (configs/ + .env) against, so boot works regardless of the process working
// directory. This matters because a benchmark supervisor (or systemd, or an IDE)
// spawns the binary from an arbitrary cwd; when that cwd lacked configs/, every
// layered override — including execution.scout_enabled in tuning.local.json —
// silently fell back to DefaultConfig(), disabling the Scout without a trace.
//
// Resolution order:
//  1. CWD, when it already holds configs/tuning.json (committed sentinel) — this
//     preserves the historical behavior byte-for-byte for the normal launch.
//  2. Otherwise, walk up from the executable's own directory (bounded) looking for
//     the same sentinel — a binary under <repo>/bin finds <repo>/configs.
//  3. Fall back to "." so a genuinely absent bundle surfaces exactly as before.
func ResolveBaseDir() string {
	const sentinel = "configs/tuning.json" // committed; uniquely marks the kernel config bundle
	if _, err := os.Stat(sentinel); err == nil {
		return "."
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for range 8 {
			if _, err := os.Stat(filepath.Join(dir, sentinel)); err == nil {
				return dir
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break // reached the filesystem root
			}
			dir = parent
		}
	}
	return "."
}

func LoadConfig(path string) (*Config, error) {
	cfg, _, err := LoadConfigWithStore(path, nil)
	return cfg, err
}

// LoadConfigWithStore is LoadConfig plus the ADR-0101 embedded store layer and
// per-key provenance.
//
// `store` may be nil, which reproduces the pre-ADR-0101 pipeline exactly — that
// is how the OSS default, the tests and any benchmark arm that must ignore
// operator writes are configured.
//
// The returned Provenance records which layer supplied each flat key. It is
// gathered by observing the SAME merge that produces cfg (ADR-0101 D4), so it
// cannot disagree with the values the kernel goes on to use — a reconstruction
// that re-applied precedence separately could, and a provenance report that
// disagrees with the running config is worse than none.
func LoadConfigWithStore(path string, store Store) (*Config, Provenance, error) {
	// All secondary paths are derived from the directory of the primary `path`,
	// not from the process CWD. This keeps the layering deterministic across
	// invocation contexts (tests, systemd, dev shells).
	dir := filepath.Dir(path)

	k := koanf.New(".")
	t := newTracker()

	// Layer 1: Go defaults (lowest priority). Marshalled from the pre-filled
	// DefaultConfig() struct via rawbytes + JSON parser so the existing `json`
	// tags on every field continue to drive the merge.
	defaultBytes, err := stdjson.Marshal(DefaultConfig())
	if err != nil {
		return nil, nil, err
	}
	if err := k.Load(rawbytes.Provider(defaultBytes), kjson.Parser()); err != nil {
		return nil, nil, err
	}
	t.claim(scratchBytes(defaultBytes), SourceDefault)

	// Layer 2: tuning.json (committed curated power-user starter). Absent ⇒ skip.
	// Curated, NOT a full mirror of all hyperparameters — see configs/tuning.json
	// header comment and ADR-0024 §Curated tuning.json.
	loadIfPresent(k, filepath.Join(dir, "tuning.json"))
	t.claim(scratchFile(filepath.Join(dir, "tuning.json")), SourceTuning)

	// Layer 3: tuning.local.json (gitignored, per-machine). Wins over tuning.json.
	loadIfPresent(k, filepath.Join(dir, "tuning.local.json"))
	t.claim(scratchFile(filepath.Join(dir, "tuning.local.json")), SourceTuningLocal)

	// Layer 4: the primary `path` argument (configs/config.json in production).
	// ADR-0057: the OSS repo ships config.example.json (gitignored config.json
	// holds real user values). A missing primary file is fine: built-in
	// defaults + the other layers still produce a valid config.
	loadIfPresent(k, path)
	t.claim(scratchFile(path), SourceConfig)

	// Layer 5: config.local.json — silently skipped when absent.
	// Convention: .json → .local.json (e.g. config.json → config.local.json).
	localPath := strings.TrimSuffix(path, ".json") + ".local.json"
	loadIfPresent(k, localPath)
	t.claim(scratchFile(localPath), SourceConfigLocal)

	// Layer 6: embedder.json (gitignored, the embedding model). Absent ⇒ default
	// embedder from DefaultConfig() (bge-large via Ollama, localhost:11434).
	// If both config.json and embedder.json define an `embedder` block, embedder.json wins.
	loadIfPresent(k, filepath.Join(dir, "embedder.json"))
	t.claim(scratchFile(filepath.Join(dir, "embedder.json")), SourceEmbedder)

	// Layer 7: embedder.local.json (gitignored, per-machine embedder override).
	loadIfPresent(k, filepath.Join(dir, "embedder.local.json"))
	t.claim(scratchFile(filepath.Join(dir, "embedder.local.json")), SourceEmbedderLocal)

	// Layer 8: providers.json (gitignored, the LLM provider list). Absent ⇒
	// DefaultConfig() applies (empty generators, validate skipped). If both
	// config.json and providers.json define an `llm_provider` block, providers.json wins.
	loadIfPresent(k, filepath.Join(dir, "providers.json"))
	t.claim(scratchFile(filepath.Join(dir, "providers.json")), SourceProviders)

	// Layer 9: providers.local.json (gitignored, per-machine LLM provider override).
	loadIfPresent(k, filepath.Join(dir, "providers.local.json"))
	t.claim(scratchFile(filepath.Join(dir, "providers.local.json")), SourceProvidersLocal)

	// Layer 10: mcp.json (gitignored MCP servers). Absent ⇒ no MCP behavior.
	// If both config.json and mcp.json define an `mcp` block, mcp.json wins.
	// WARNING: an empty mcp.json ({"mcp": {}}) will silently wipe the `mcp`
	// block from config.json because Koanf merges per-key. Operators who
	// don't use MCP should simply not create mcp.json.
	loadIfPresent(k, filepath.Join(dir, "mcp.json"))
	t.claim(scratchFile(filepath.Join(dir, "mcp.json")), SourceMCP)

	// Layer 11 (ADR-0101 D1): the embedded store — durable operator writes.
	// Above every file so a console edit survives a restart and outranks the
	// shipped defaults; below the environment so a deployment still wins.
	if store != nil {
		if overrides, err := store.Overrides(); err == nil && len(overrides) > 0 {
			if b, mErr := stdjson.Marshal(expand(overrides)); mErr == nil {
				// The store is migrated exactly like a file layer. It holds keys an
				// operator wrote through the console, and a store populated before v2
				// holds them FLAT — so skipping it here would silently drop every
				// durable operator override across the upgrade, which is the one
				// layer whose whole purpose is to survive restarts.
				b = migrateJSON(b, "config store")
				_ = k.Load(rawbytes.Provider(b), kjson.Parser())
				t.claim(scratchBytes(b), SourceStore)
			}
		}
	}

	// Layer 12: environment variables (highest priority). The env-var
	// convention (`CAMBRIAN_` prefix, `__` as hierarchy separator, `_` literal
	// within a segment) is unchanged. Example: CAMBRIAN_EXECUTION__EWMA_ALPHA=0.7
	// overrides execution.ewma_alpha from every lower layer.
	envMap := func(s string) string {
		orig := s
		s = strings.TrimPrefix(s, "CAMBRIAN_")
		s = strings.ToLower(s)
		s = strings.ReplaceAll(s, "__", ".")
		// v1→v2: CAMBRIAN_EXECUTION__EWMA_ALPHA named `execution.ewma_alpha`, which
		// after the nesting is `execution.supervision.ewma_alpha`. The variable name
		// is part of a deployment's contract — it lives in systemd units, Helm
		// values and CI secrets, none of which are edited by upgrading a binary — so
		// the old name keeps working and is rewritten to the new path here.
		// CAMBRIAN_EXECUTION__SUPERVISION__EWMA_ALPHA also works and is preferred.
		if rest, ok := strings.CutPrefix(s, "execution."); ok {
			if block, known := legacyExecutionKey[rest]; known {
				s = "execution." + block + "." + rest
			}
		}
		// Record the variable name alongside the config key it maps to: this
		// callback is the only place both are visible at once, and the variable
		// name is the half that makes "something pins this" actionable.
		t.noteEnvKey(s, orig)
		return s
	}
	_ = k.Load(env.Provider("CAMBRIAN_", ".", envMap), nil)
	envScratch := koanf.New(".")
	_ = envScratch.Load(env.Provider("CAMBRIAN_", ".", envMap), nil)
	t.claimEnv(envScratch)

	var cfg Config
	if err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{
		Tag: "json",
		DecoderConfig: &mapstructure.DecoderConfig{
			// WeaklyTypedInput must be true: env.Provider delivers all values as
			// strings (e.g. "5000"), so int/float fields require string coercion.
			WeaklyTypedInput: true,
			TagName:          "json",
			Result:           &cfg,
		},
	}); err != nil {
		return nil, nil, &ConfigError{Field: "unmarshal", Message: err.Error()}
	}

	// ADR-0042 defaults for the llm_provider block.
	for i := range cfg.LLMProvider.Generators {
		if cfg.LLMProvider.Generators[i].TimeoutMs == 0 {
			cfg.LLMProvider.Generators[i].TimeoutMs = 60000
		}
	}
	if cfg.LLMProvider.configured() {
		if cfg.LLMProvider.Health.FailureThreshold == 0 {
			cfg.LLMProvider.Health.FailureThreshold = 3
		}
		if cfg.LLMProvider.Health.CooldownMs == 0 {
			cfg.LLMProvider.Health.CooldownMs = 30000
		}
	}

	if err := cfg.validateSecrets(); err != nil {
		return nil, nil, err
	}

	return &cfg, t.prov, nil
}

// loadIfPresent loads the JSON file at p into k when the file exists. A missing
// file is a silent no-op (per-layer skip, not an error). A present-but-malformed
// file surfaces the parser error to the caller, which is the right signal — the
// operator wrote a file but it's broken.
// loadIfPresent merges one config file into k.
//
// The bytes pass through migrateFile first, so a file still written against the
// v1 flat `execution` schema is converted to v2 in memory before koanf sees it.
// Migrating per LAYER rather than once over the merged result is what keeps the
// layering working: koanf merges by key PATH, so a v1 `execution.recall_top_k` in
// tuning.local.json and a v2 `execution.retrieval.recall_top_k` in config.json
// are different paths — both would survive the merge and the one the operator
// meant to override with would be silently ignored rather than losing.
//
// The file on disk is never modified. Rewriting an operator's config as a side
// effect of starting the kernel is a surprising thing for a read path to do;
// MigrateFileInPlace exists for when that is explicitly wanted.
func loadIfPresent(k *koanf.Koanf, p string) {
	b := migrateFile(p)
	if b == nil {
		return // missing file: skip this layer
	}
	_ = k.Load(rawbytes.Provider(b), kjson.Parser())
}

// scratchFile returns a throwaway Koanf holding ONLY the contents of the file at
// p, or nil when the file is absent or unreadable. It is how the provenance
// tracker learns which keys a layer states, without inferring it from a diff of
// the merged result (ADR-0101 D4). Parsing each small config file a second time
// at boot is a price worth paying for an answer that is right when a file
// restates a default.
// It migrates identically to loadIfPresent, and must: provenance has to describe
// the keys the kernel actually merged. Reporting that `execution.recall_top_k`
// came from tuning.json, when the value in effect is keyed
// `execution.retrieval.recall_top_k`, would make the provenance view claim a
// source for a key that no longer exists.
func scratchFile(p string) *koanf.Koanf {
	b := migrateFile(p)
	if b == nil {
		return nil
	}
	sk := koanf.New(".")
	if err := sk.Load(rawbytes.Provider(b), kjson.Parser()); err != nil {
		return nil
	}
	return sk
}

// scratchBytes is scratchFile for a layer already in memory (the Go defaults and
// the embedded store).
func scratchBytes(b []byte) *koanf.Koanf {
	sk := koanf.New(".")
	if err := sk.Load(rawbytes.Provider(b), kjson.Parser()); err != nil {
		return nil
	}
	return sk
}

// validateSecrets checks that required fields are non-empty after all layers merge.
func (c *Config) validateSecrets() *ConfigError {
	var errs []string

	if c.Database.Host == "" {
		errs = append(errs, "database.host is required")
	}
	if c.Database.User == "" {
		errs = append(errs, "database.user is required")
	}
	if c.Database.Password == "" {
		errs = append(errs, "database.password is required (set CAMBRIAN_DATABASE__PASSWORD)")
	}
	// ADR-0042: validate the llm_provider + embedder blocks (no-op until configured).
	errs = append(errs, c.LLMProvider.validate(c.Embedder)...)

	if len(errs) > 0 {
		return &ConfigError{
			Field:   "validation",
			Message: strings.Join(errs, "; "),
		}
	}
	return nil
}

// Validate checks that ExecutionConfig values are within acceptable ranges.
// Returns a multi-error describing all invalid fields.
// Note: zero-value fields are filled by LoadConfig defaults — a zero after
// LoadConfig means the field was explicitly set to zero in JSON and the
// implied default was skipped. This is a known limitation of Go zero-value
// semantics; use a small non-zero value (e.g. 0.0001) to effectively disable
// a float field like CrossVerifyRate.
func (c *ExecutionConfig) Validate() *ConfigError {
	var errs []string

	if c.Plan.StepTimeoutMultiplier < 0 {
		errs = append(errs, "step_timeout_multiplier must be >= 0")
	}
	if c.Plan.PlanTimeoutMs < 1000 {
		errs = append(errs, "plan_timeout_ms must be >= 1000")
	}
	if c.Supervision.EWMAAlpha < 0 || c.Supervision.EWMAAlpha > 1 {
		errs = append(errs, "ewma_alpha must be in [0, 1]")
	}
	if c.Gatekeeper.GatekeeperW1 < 0 || c.Gatekeeper.GatekeeperW2 < 0 || c.Gatekeeper.GatekeeperW3 < 0 {
		errs = append(errs, "gatekeeper_w1/w2/w3 must be >= 0")
	}
	sum := c.Verification.TrustScoreCalWeight + c.Verification.TrustScoreAbsWeight
	if sum < 0 || sum > 1 {
		errs = append(errs, fmt.Sprintf("trust_score_cal_weight + trust_score_abs_weight must be in [0, 1], got %.2f", sum))
	}
	if c.Verification.CrossVerifyRate < 0 || c.Verification.CrossVerifyRate > 1 {
		errs = append(errs, "cross_verify_rate must be in [0, 1]")
	}
	if c.Gatekeeper.MinAuctionConfidence < 0 || c.Gatekeeper.MinAuctionConfidence > 1 {
		errs = append(errs, "min_auction_confidence must be in [0, 1]")
	}
	if c.Plan.MaxRecursionDepth < 0 {
		errs = append(errs, "max_recursion_depth must be >= 0")
	}
	if c.Plan.FallbackConfidenceThreshold < 0 || c.Plan.FallbackConfidenceThreshold > 1 {
		errs = append(errs, "fallback_confidence_threshold must be in [0, 1]")
	}
	if c.Plan.MaxReplanAttempts < 0 {
		errs = append(errs, "max_replan_attempts must be >= 0")
	}
	if c.Plan.MaxPartialContextBytes < 0 {
		errs = append(errs, "max_partial_context_bytes must be >= 0")
	}
	if c.Supervision.SignalNoiseThreshold < 0 {
		errs = append(errs, "signal_noise_threshold must be >= 0")
	}
	if c.Supervision.SignalNoiseWindowSecs < 0 {
		errs = append(errs, "signal_noise_window_secs must be >= 0")
	}
	if c.Verification.VerifierRecencyWindow < 0 {
		errs = append(errs, "verifier_recency_window must be >= 0")
	}
	if c.Gatekeeper.GatekeeperW4 < 0 {
		errs = append(errs, "gatekeeper_w4 must be >= 0")
	}
	if c.Plan.MaxPlanCost < 0 {
		errs = append(errs, "max_plan_cost must be >= 0")
	}
	if c.Routing.ExplorationRate < 0 || c.Routing.ExplorationRate > 1 {
		errs = append(errs, "exploration_rate must be in [0, 1]")
	}
	if c.Session.SessionTTLDays < 0 {
		errs = append(errs, "session_ttl_days must be >= 0")
	}
	if c.Plan.PlanDriftDays < 0 {
		errs = append(errs, "plan_drift_days must be >= 0")
	}
	if c.Routing.AuctionBidTimeoutMs < 0 {
		errs = append(errs, "auction_bid_timeout_ms must be >= 0")
	}
	if c.Routing.ProposalTimeoutMs < 0 {
		errs = append(errs, "proposal_timeout_ms must be >= 0")
	}
	if c.Memory.MemoryRelevanceThreshold < 0 || c.Memory.MemoryRelevanceThreshold > 1 {
		errs = append(errs, "memory_relevance_threshold must be in [0, 1]")
	}
	if c.Memory.MaxMemoryResults < 0 {
		errs = append(errs, "max_memory_results must be >= 0")
	}
	if c.Memory.MaxNeighborExpansion < 0 {
		errs = append(errs, "max_neighbor_expansion must be >= 0")
	}
	if c.Hippocampus.HippocampusDefaultPolicy != "" {
		if _, ok := c.Hippocampus.HippocampusPolicies[c.Hippocampus.HippocampusDefaultPolicy]; !ok {
			errs = append(errs, fmt.Sprintf("hippocampus_default_policy %q is not a key in hippocampus_policies", c.Hippocampus.HippocampusDefaultPolicy))
		}
	}
	switch c.Routing.ResourceSelector {
	case "", "auction", "efe", "auto":
		// valid (empty resolves to auction)
	default:
		errs = append(errs, fmt.Sprintf("resource_selector %q must be one of auction|efe|auto", c.Routing.ResourceSelector))
	}
	if c.Routing.EFETrafficPercent < 0 || c.Routing.EFETrafficPercent > 100 {
		errs = append(errs, "efe_traffic_percent must be in [0, 100]")
	}
	if c.Routing.EFEExplorationBonus < 0 {
		errs = append(errs, "efe_exploration_bonus must be >= 0")
	}

	if len(errs) > 0 {
		return &ConfigError{
			Field:   "validation",
			Message: strings.Join(errs, "; "),
		}
	}
	return nil
}
