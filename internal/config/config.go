package config

import (
	stdjson "encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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

	// StepTimeoutFloorMs is the shortest per-step deadline the dispatcher will set
	// (ms). The ADR-0001 D5 formula scales the winner's OBSERVED median latency, so
	// an agent whose history is short answers (5 s median ⇒ 25 s at the defaults)
	// is cut off the first time it is asked for a long generation on a slow local
	// or proxied model. The floor keeps that healthy step alive; the formula can
	// still EXTEND past it for an agent that is genuinely slow. It applies only
	// where the formula applies (a winner with no latency profile still runs under
	// the plan's own limits alone), and the plan's absolute cap
	// (PlanMaxDurationMs) still bounds it. Default 180000 (3 min); 0 disables the
	// floor (T-20261105-slow-model-reliability-hardcoded-20-s).
	StepTimeoutFloorMs int `json:"step_timeout_floor_ms,omitempty"`

	// PlanIdleTimeoutMs cancels a plan that has made NO progress for this long
	// (ms). Progress is a step starting or finishing, a model or tool call in
	// flight on one of the plan's step leases (or an in-kernel thought/replan
	// call), or an ADR-0098 progress event for the plan's conversation. A plan
	// that keeps working is never cut by it, however long it runs. Default
	// 180000 (DefaultPlanIdleTimeoutMs); 0 disables the idle watchdog, leaving
	// only PlanMaxDurationMs. Replaces the fixed 120 s plan_timeout_ms (owner
	// decision 2026-11-05, T-20261105-owner-decision-needed-120-s-plan-timeout-ms).
	PlanIdleTimeoutMs int `json:"plan_idle_timeout_ms"`

	// PlanMaxDurationMs is the absolute safety cap (ms) on one plan, whatever
	// its progress. Default 1800000 (30 min). The retired v1/v2 key
	// plan_timeout_ms, when an operator still sets it, is loaded as this key
	// (it was their hard cap) — see renameDeprecatedExecutionKeys.
	PlanMaxDurationMs int `json:"plan_max_duration_ms"`

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

	// MaxReplanAttempts is the maximum number of times one plan may re-ask the
	// planner, whatever the reason: a retry after an invalid DAG and a replan after
	// intra-step retries and inter-step fallbacks are exhausted draw on the same
	// count, so a plan makes at most 1 + MaxReplanAttempts planner calls
	// (T-20261101-planner-validation-retries-and-replans-compound). 0 disables
	// both. Default: 2.
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
	ContextRefSnippetChars int     `json:"context_ref_snippet_chars"` // default 500

	// DependencyResultsMaxChars caps the upstream results a dependent plan step is
	// handed inline (domain.StepDependency, the executor's buildStepDependencies), in
	// bytes across all of its dependencies, for an agent step and an in-kernel thought
	// step alike: when they are larger, each is shortened to a fair share, marked as
	// shortened and keeps its content-store cid so the receiver can resolve the rest.
	// Default 24000 (about 6k tokens); <= 0 reads as the default. Replaces
	// thought_results_max_chars (2026-11-05, which bounded only the thought step's
	// request; T-20261105-a-planner-thought-step-that-depends-on-two).
	DependencyResultsMaxChars int `json:"dependency_results_max_chars,omitempty"`

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

	// RETIRED 2026-11-05 (T-20261105-delete-the-llm-agent-interview-now-owner):
	// `cold_start_penalty_multiplier`. It discounted an agent whose PROFILE was flagged
	// provisional; the only writer of that flag was the LLM interview's A2A path,
	// deleted with the interview. Agents with no evidence carry the neutral prior
	// instead. A layer that still sets it loads unchanged (the decoder ignores keys
	// with no field); retiredExecutionFields (execution_migration_test.go) records it.

	// RETIRED 2026-10-31 (T-20261031-min-auction-confidence-config-is-dead-after-the):
	// `min_auction_confidence`. It was the bid-confidence floor of the Auctioneer,
	// which ADR-0100 P3 deleted; nothing read it afterwards. A layer that still sets
	// it loads unchanged (the decoder ignores keys with no field);
	// retiredExecutionFields (execution_migration_test.go) records it.

	// GatekeeperW4 is the weight for the cost-penalty term in GatekeeperScore.
	// Set to 0 to disable cost-awareness entirely. Default: 0.15.
	GatekeeperW4 float64 `json:"gatekeeper_w4"`

	// MeritMode selects the Gatekeeper's L3 merit scorer (ADR-0139 D6, evolvable):
	//   - "posterior_lcb" (default since 2026-11-05, owner decision
	//     decisions/posterior-lcb-default-merit.md): rank by a lower quantile
	//     (MeritLCBQuantile) of the Beta posterior over the agent's verified outcomes for
	//     the step's capability, then by cost, then by latency (a missing latency or cost
	//     takes the slate mean);
	//   - "legacy": the hand-weighted GatekeeperScore
	//     w1·SR + w2·Trust + w3/latency − w4·cost (or the ROUTE-07 learned scorer when
	//     routing.learned_scorer is on), byte-identical to before D6;
	//   - "thompson": rank by one sample from that posterior per dispatch (Thompson
	//     sampling), same tie-breaks. Exploration is inherent, so the Dispatcher's
	//     epsilon branch (routing.exploration_rate) is skipped in this mode.
	// The posterior modes drop the SR binarisation and the always-zero calibration
	// term; they ignore the w1..w4 weights and the learned scorer. The owner promoted
	// posterior_lcb without an A/B/A study; that study stays open as an optional
	// validation (T-20261105-a-b-a-routing-study-legacy-merit-vs-posterior).
	MeritMode string `json:"merit_mode,omitempty"`

	// MeritPriorAlpha / MeritPriorBeta are the Beta prior of every (agent,
	// capability) posterior. Default 1/1 (uniform, neutral). No interview or
	// manifest seed feeds it: an agent with no verified outcome sits on the prior.
	// Both must be > 0; 0 (unset) reads as the default 1.
	MeritPriorAlpha float64 `json:"merit_prior_alpha,omitempty"`
	MeritPriorBeta  float64 `json:"merit_prior_beta,omitempty"`

	// MeritLCBQuantile is the posterior quantile posterior_lcb ranks by. Default 0.1
	// (a 90% one-sided lower bound). Must be in (0, 1); 0.5 ≈ the posterior median;
	// 0 (unset) reads as the default 0.1.
	MeritLCBQuantile float64 `json:"merit_lcb_quantile,omitempty"`

	// MeritUnverifiedWeight is how much an UNVERIFIED step outcome (the step
	// returned, no verifier scored it) adds to α in the posterior modes. Default 0:
	// unverified outcomes are neutral evidence (ADR-0139 D6). In [0, 1].
	MeritUnverifiedWeight float64 `json:"merit_unverified_weight,omitempty"`
}

// Merit modes of GatekeeperConfig.MeritMode (ADR-0139 D6).
const (
	MeritModeLegacy       = "legacy"
	MeritModePosteriorLCB = "posterior_lcb"
	MeritModeThompson     = "thompson"
)

// ActiveMeritMode is MeritMode with the empty string read as the default, posterior_lcb.
func (g GatekeeperConfig) ActiveMeritMode() string {
	if g.MeritMode == "" {
		return MeritModePosteriorLCB
	}
	return g.MeritMode
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
	// vocabulary, those flow into the DispatchTask, and L1 Declaration hard-gates
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
	// (Declaration→Semantic→Merit per-agent verdicts + winner margin) carried on
	// the auction event. Default true: the funnel only records values the
	// Gatekeeper already computes, so the cost is a per-auction slice allocation.
	// Toggle it off to measure that overhead as an A/B arm (acceptance: zero
	// auction-latency regression).
	RoutingTraceEnabled bool `json:"routing_trace_enabled"`

	// ExplorationRate is the probability of selecting a random TraitModel candidate
	// instead of the top-scored one (ε-greedy exploration). Default: 0.05.
	ExplorationRate float64 `json:"exploration_rate"`

	// DispatchCheapEnergyMax is the Step.MaxEnergy at or below which a VERIFIED step
	// (CheckpointAfter) takes the cheapest competent agent instead of merit-argmax
	// (ADR-0100 D4). 0 disables the branch entirely — every step takes argmax.
	// A step with no declared budget is never treated as cheap.
	DispatchCheapEnergyMax float64 `json:"dispatch_cheap_energy_max"`

	// DispatchMeritFloor is the minimum merit a candidate needs before the
	// cheapest-competent branch will consider it. Keeps "cheapest" from meaning
	// "worst". Ignored on the argmax path.
	DispatchMeritFloor float64 `json:"dispatch_merit_floor"`

	// YieldMaxDepth bounds an agent-yielded sub-goal chain (ADR-0041 D5, ADR-0100
	// D10; consumer internal/metabolism/yield): 1 lets a step's agent delegate but
	// not its delegate, and so on. ≤ 0 refuses every yield — the step then FAILS
	// with yield.ErrMaxDepth rather than returning an empty answer.
	YieldMaxDepth int `json:"yield_max_depth"`

	// YieldMaxSubgoals caps the total sub-goals one dispatched step (or one direct
	// CallAgent) may yield, across the whole tree and every resume round — the
	// fan-out bound that keeps a yield/resume/yield agent from looping forever.
	YieldMaxSubgoals int `json:"yield_max_subgoals"`

	// RETIRED with the auction (ADR-0100 P3): `calibrated_bids` and
	// `bid_calibration_min_samples` (ROUTE-05 / ADR-0068). They configured the isotonic
	// calibration of a BID, and there are no bids — the only reader was the auctioneer's
	// Calibrator, deleted with the mechanism. The keys are removed rather than left
	// declared-but-unread: a config key that silently does nothing is the same trap as
	// `resource_selector: "efe"`, which voided weeks of routing measurement because it
	// looked like it was configuring the thing it was quietly bypassing. A stale key in
	// someone's config file is now an unknown-key warning instead of a lie.

	// PerCapabilityMerit (ROUTE-06 / ADR-0069) makes L3 merit read the agent's
	// capability-scoped success/trust for the step's required capability, falling back
	// to the global profile when the tag has no history. OFF ⇒ global merit
	// (byte-identical to pre-ROUTE-06).
	//
	// It used to ALSO bound the provisional L2 bypass with a per-capability exploration
	// budget. That budget was removed 2026-08-08: its only recorder was the Auctioneer,
	// so after ADR-0100 P3 it could never reach its bound, and a bound that cannot bind
	// is worse than no bound — it reads as a guarantee. The provisional bypass is now
	// unconditional in both arm positions, which is what actually ran the whole time.
	PerCapabilityMerit bool `json:"per_capability_merit"`

	// LearnedScorer (ROUTE-07 / ADR-0076) replaces the hand-weighted GatekeeperScore with
	// a model learned offline from orchestration artifacts (routescorer.Model loaded from
	// LearnedScorerModelPath). OFF ⇒ hand weights (byte-identical). Adopt online only on a
	// published offline win over the calibrated hand-weights arm (offline-before-online).
	LearnedScorer bool `json:"learned_scorer"`

	// LearnedScorerModelPath points to the JSON model the learned scorer loads at boot.
	// Empty ⇒ no model wired (the arm stays inert even if the flag is on).
	LearnedScorerModelPath string `json:"learned_scorer_model_path,omitempty"`

	// ProposalTimeoutMs is the per-agent RPC timeout (ms) when calling RequestProposal.
	// Default: 2000.
	ProposalTimeoutMs int `json:"proposal_timeout_ms"`

	// BypassSelection (ADR-0050 D1) short-circuits Server.Execute past the
	// planner/auction/DAG path and dispatches the user's input verbatim to
	// SingleAgentID — the within-substrate no-orchestration control arm the
	// benchmark harness uses for routing A/Bs (ROUTE-01 baselines). Default
	// OFF; production behavior is unchanged when false.
	BypassSelection bool `json:"bypass_selection,omitempty"`

	// SingleAgentID is the agent the bypass path dispatches to. Required when
	// BypassSelection is true; Execute fails loud when it is empty or unknown.
	SingleAgentID string `json:"single_agent_id,omitempty"`

	// ADR-0057 (three-tier config): ReactiveEngine tuning is PREMIUM config and lives
	// in the premium repo's own config, fed in via the app.Options reactive hook.
	// It is intentionally absent from the OSS ExecutionConfig schema.

	// ADR-0037: Central-Executive Planner resource selection.
}

// CapabilityConfig — Capability vocabulary and resolution.
//
// Split out of the former 198-field flat ExecutionConfig (config schema v2).
// Field names, types and json tags are unchanged; only the nesting is new, so
// each field means exactly what it meant before.
//
// RETIRED 2026-10-31 (T-20261031-dead-capability-cluster-config-keys-and):
// `capability_cluster_threshold`, `capability_cluster_epsilon`,
// `capability_cluster_min_agents`, `capability_cluster_interval_seconds`. They
// tuned the ADR-0014 LLM CapabilityClusterer, which ADR-0067 deleted; nothing read
// them afterwards. A layer that still sets them loads unchanged;
// retiredExecutionFields (execution_migration_test.go) records them.
type CapabilityConfig struct {
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

	// MaxSessionTokens is the per-session LLM spend ceiling, in tokens (input +
	// output, provider-reported where the provider reports usage), enforced by the
	// ADR-0131 SpendMeter over EVERY LLM call made for the session: planner, replans,
	// thought steps, checkpoints, agent streams and the agentic retrieval chain. A
	// call that would take the session past it is refused with ErrSpendCeiling and
	// the plan ends with a visible ResourceExhausted failure plus a TaskEvent record.
	// 0 (the default) = metered but never refused, until worldsim yields a p95 to
	// set it from (ADR-0131 status_note); the benchmark rig sets it in
	// tuning.local.json. (Was an unread ADR-0030 scavenger threshold.)
	MaxSessionTokens int `json:"max_session_tokens,omitempty"`
}

// MemoryConfig — Long-term memory: experiential records, recall thresholds, consolidation and GC.
//
// Split out of the former 198-field flat ExecutionConfig (config schema v2).
// Field names, types and json tags are unchanged; only the nesting is new, so
// each field means exactly what it meant before.
type MemoryConfig struct {
	// RETIRED 2026-11-05 (T-20261105-remove-scenes-completely-owner-decision-2026-11):
	// `experiential_memory_enabled`, `tier1_channel_capacity`, `tier2_batch_size`,
	// `tier2_llm_timeout` and `tier2_max_idle_seconds`. They gated and tuned the RAW
	// capture path (step results and tool payloads embedded whole) and the ADR-0015
	// tier-2 memory judge that curated it; both were deleted by owner decision
	// (cambrian-knowledge/decisions/scenes-cancelled.md,
	// investigations/2026-11-05-memory-judge-assessment.md). A layer that still sets
	// them loads unchanged — the decoder ignores keys with no field.

	// ExperienceRecordsEnabled turns on the ADR-0049 §A2.2 experiential records: the
	// mutation action events and, per completed plan, the ADR-0095 `experiences` row
	// (episode open and close), the A2.3 failure record and procedure feedback. The
	// plan-level `mnemonic_scene` chunk it also wrote until 2026-11-05 was removed with
	// scenes. Nothing raw is ever embedded.
	//
	// DEFAULT TRUE as of 2026-07-28 (owner decision). The reasoning, recorded because
	// "we turned the removed thing back on" is the wrong summary:
	//
	//   - What broke in July was the RAW path. That stays false permanently, and this
	//     path cannot embed a tool payload by construction (§A2.2 hard constraint).
	//   - Defect 4 — experience crowding corpus recall — now has a structural guard
	//     (§A2.4 lane-aware truncation), measured to cost nothing.
	//   - The volume is bounded: one episode row per plan, one action event per
	//     mutation, and a failure record only for a surprising or replan-exhausted
	//     failure.
	//   - It is the only way to progress. `mean_foreign_share` cannot detect
	//     displacement until experience has volume, and volume only accrues if this is
	//     on. Enabling is how the measurement STARTS, not a substitute for it.
	//
	// The pre-enable baseline is runs/lg_1 and runs/lg_2 (hit 1.0, MRR ~0.60,
	// foreign_share ~0.43). Drift against those is the thing to watch.
	ExperienceRecordsEnabled bool `json:"experience_records_enabled"`

	// ExperienceSurpriseFloor is the ADR-0049 A2.3 gate: a failed episode whose
	// prediction error reaches this writes a FAILURE RECORD (a negative edge). Surprise is |merit expectation - actual|, so
	// 0.5 means "merit was wrong by half" — a confident agent that failed, or a
	// distrusted one that succeeded. Both are worth remembering; a routine failure by
	// an agent already known to fail is not.
	//
	// Default 0.5 is editorial, not measured.
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

	// CircadianStaleDocWarnThreshold is the count of low-activation-strength documents above
	// which CircadianRhythm logs a WARN on startup (indicating pg_cron may be disabled).
	// Default: 50. ADR-0015.
	CircadianStaleDocWarnThreshold int `json:"circadian_stale_doc_warn_threshold"`

	// RememberDefaultActivation is the initial ActivationStrength (ADR-0015 lifecycle
	// metric) stamped on a fact written via memory.remember()/IngestMemory when the
	// caller supplies no importance hint. Without it a remembered fact defaults to 0,
	// so its floor-multiplier recall score (cosine × (α + (1-α)·activation)) maxes at
	// cosine·α ≈ 0.2 and can never clear RecallSimilarityFloor (0.25) — the fact is
	// permanently unrecallable. A LoCoMo-tunable hyperparameter (sweep it alongside
	// RecallSimilarityFloor / RetrievalFloor). Range [0,1]; 0 reproduces legacy behavior.
	RememberDefaultActivation float64 `json:"remember_default_activation"`

	// RETIRED 2026-10-31 (owner decision, ADR-0029 / ADR-0030):
	// `consolidation_threshold_doc_count` and `consolidation_threshold_bytes`. They
	// configured the ADR-0030 lazy global-consolidation trigger of the episodic lane,
	// whose consolidator (EpisodicExtractor / MemoryLifecycleManager) was removed on
	// 2026-07-18; from then on nothing read either key. Removed rather than kept as
	// documented no-ops, for the same reason as `calibrated_bids`: a key that silently
	// does nothing reads as a control. A file, store or env layer that still sets them
	// loads unchanged — the decoder ignores keys with no field — and
	// retiredExecutionFields (execution_migration_test.go) records them.
	//
	// RETIRED 2026-10-31 (T-20261031-execution-memory-max-orphaned-documents-and):
	// `max_orphaned_documents`, the ADR-0030 MemoryPressureScavenger threshold.
	// Nothing constructed the scavenger from config, so the key had no reader.
}

// ProcedureConfig — ADR-0094 procedure induction.
//
// Split out of the former 198-field flat ExecutionConfig (config schema v2).
// Field names, types and json tags are unchanged; only the nesting is new, so
// each field means exactly what it meant before.
type ProcedureConfig struct {
	// RETIRED 2026-11-05 (T-20261105-remove-scenes-completely-owner-decision-2026-11):
	// `procedure_induction_interval_hours`, `procedure_min_samples` and
	// `procedure_max_episodes_per_pass`. They scheduled and tuned the ADR-0094 D3
	// induction pass, which read routines out of `mnemonic_scene` metadata; scenes and
	// induction were removed by owner decision (decisions/scenes-cancelled.md).
	// Procedures already stored are still served and still learn from outcomes (the key
	// below). A layer that still sets the retired keys loads unchanged.

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
	// re-ranks by activation. Default TRUE since ADR-0049 D10 (spreading reads the
	// Hebbian co_activated edges); set false to drop its per-recall latency and
	// cost. Requires a GraphStore-backed vector store. Also arms the ADR-0053 (T-Mem associative hop) entity
	// routing (see internal/kernel/memory_stack.go for its empty-index caveat).
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
	// synthesize loop. Default OFF since 2026-11-02 (owner decision; it was ON from
	// 2026-08-11). The query path is
	// unchanged when disabled or when the retrieval_agent is unreachable (fail-open
	// to searchByType), so a baseline arm that sets it false measures the true
	// single pass. A/B via config.
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

	// ChronologicalContextEnabled orders the passages handed to the FINAL answer
	// synthesis by event time, each prefixed with its date, its precision and its
	// source (ADR-0134 D5a): the cited synthesis of AnswerMemory / ask_memory and the
	// agentic loop's closing synthesis. Retrieval and ranking are unchanged — the
	// same passages are selected by relevance; only their order and dating in the
	// reader's context change. Default off until a LongMemEval temporal-reasoning A/B
	// is recorded (D5: each behaviour measured before it is turned on).
	ChronologicalContextEnabled bool `json:"chronological_context_enabled,omitempty"`

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
	// HnswIterativeScan turns on pgvector's iterative HNSW index scan (pgvector
	// >= 0.8): "" or "off" (pgvector's own default), "relaxed_order" (the shipped
	// default since 2026-11-02, owner decision), or "strict_order". Without it an
	// HNSW scan yields at most hnsw_ef_search tuples
	// and the read-scope / session / event-window predicates filter AFTER the
	// scan, so a selective predicate on a large shared store underfills top_k
	// (ADR-0134 P2, T-20261101-scoped-recall-may-underfill-top-k-on-a-large; the
	// measurement is recorded on ADR-0134). Read by the pgvector adapter's
	// per-connection settings. Part of the retrieval fingerprint.
	HnswIterativeScan string `json:"hnsw_iterative_scan,omitempty"`
	// HnswMaxScanTuples caps the tuples an iterative scan visits
	// (hnsw.max_scan_tuples); 0 = pgvector's default (20000). Only read when
	// HnswIterativeScan is on.
	HnswMaxScanTuples int `json:"hnsw_max_scan_tuples,omitempty"`

	// EmbeddingProjectionWrite dual-writes every chunk embedding into the
	// chunk_embeddings projection under the active embedder's (model, dims) —
	// ADR-0107 stage 3a. WRITE ONLY: retrieval keeps reading chunks.embedding,
	// so flipping this cannot move a recall number by construction; the read
	// switch is stage 3b's separately-gated flag. Default off.
	EmbeddingProjectionWrite bool `json:"embedding_projection_write,omitempty"`

	// EmbeddingProjectionRead switches dense chunk retrieval onto the
	// projection's rows for the ACTIVE model (ADR-0107 stage 3b). Requires
	// EmbeddingProjectionWrite (boot-enforced) and a completed backfill
	// (cmd/embed-backfill) — an unbackfilled chunk is invisible through the
	// projection. Part of the retrieval fingerprint: it changes where the
	// ranking's vectors come from. Default off.
	EmbeddingProjectionRead bool `json:"embedding_projection_read,omitempty"`

	// Hybrid retrieval (ADR-0054): fuse dense (pgvector cosine) with sparse/lexical
	// (Postgres full-text) via Reciprocal Rank Fusion, so exact-token chunks the
	// embedder misses (names, titles, places) still enter the candidate pool.
	// false ⇒ vector-only (legacy). HybridRRFK is the RRF constant (default 60).
	HybridSearchEnabled     bool    `json:"hybrid_search_enabled,omitempty"`
	HybridRRFK              int     `json:"hybrid_rrf_k,omitempty"`
	HybridLexicalWeight     float64 `json:"hybrid_lexical_weight,omitempty"`     // >1 leans RRF toward exact-term/entity matches; ≤0 ⇒ 1.0
	AgenticIrcotEnabled     bool    `json:"agentic_ircot_enabled,omitempty"`     // IRCoT: reason-then-retrieve loop (CoT step drives next retrieval)
	AgenticDecomposeEnabled bool    `json:"agentic_decompose_enabled,omitempty"` // up-front grounded decomposition: decompose whole question, retrieve+answer each sub-question

	// AgenticSubQuestionConcurrency bounds how many decomposition retrieval passes
	// run at once: the coverage-floor pass plus every sub-question whose {n}
	// placeholders are already resolved (dependent ones still wait for the answers
	// they substitute). Results are assembled in plan order, so the ranking is the
	// sequential loop's. <=1 ⇒ sequential. Default 4. The extraction LLM calls of
	// parallel passes still queue at the single-threaded retrieval_agent
	// (ADR-0036 D2); the saving is the retrieval passes.
	AgenticSubQuestionConcurrency int `json:"agentic_subquestion_concurrency,omitempty"`

	// AgenticAdaptiveRoutesEnabled executes the route the decomposition planner
	// chooses with its plan (ADR-0135): direct runs the single pass with no
	// extraction call, facets runs the coverage floor plus independent
	// sub-questions in one parallel wave, chain runs today's grounded chain; a
	// missing, unknown or inconsistent route runs as chain. false ⇒ the route is
	// logged per recall but every plan executes as before. Default off until the
	// ADR-0135 gate passes. Read only when agentic retrieval and decomposition are
	// on. Part of the retrieval fingerprint when on.
	AgenticAdaptiveRoutesEnabled bool `json:"agentic_adaptive_routes_enabled,omitempty"`

	// BlendEnabled (ADR-0054 Stage A) re-ranks recall candidates by the
	// multi-signal blend (cosine + recency + confidence + pagerank + activation)
	// instead of bare cosine. Default true; false ⇒ bare-cosine ranking. The bge
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

	// BlendRecencyMode chooses how the Stage-A recency signal is computed
	// (T-20261102-stage-a-recency-is-min-max-over-the-candidate, ADR-0054 A?):
	//   "minmax" (default, "" = minmax): (t - oldest) / (newest - oldest) over THIS
	//            candidate pool — the newest candidate scores 1, the oldest 0, however
	//            close their dates are;
	//   "decay": 2^(-age / BlendRecencyHalfLifeHours), age measured from the asker's
	//            reference_time when one is given, else from the NEWEST candidate — a
	//            fixed scale, so a session a week older loses the same credit whatever
	//            else is in the pool.
	// Min-max stretches a tight cluster of relevant dates to the full 0..1 range,
	// which let recency at weight 0.1 outvote cosine ~4:1 and evict the oldest
	// relevant session (LongMemEval gpt4_f420262c). Default unchanged until measured.
	BlendRecencyMode string `json:"blend_recency_mode,omitempty"`
	// BlendRecencyHalfLifeHours is the decay mode's half-life (default 2160 = 90 d).
	// Also the half-life of the calibrated Stage A's recency (StageACalibrated),
	// which always decays.
	BlendRecencyHalfLifeHours float64 `json:"blend_recency_half_life_hours,omitempty"`

	// ADR-0136 "Ranking v2", Phase 0. Every switch defaults to the pre-ADR-0136
	// ranking; each is measured on its own before any default moves.
	//
	// StageACalibrated (D1) scores Stage A as a convex combination of
	// POOL-INDEPENDENT features, with the blend weights above: lexical = the
	// adapter's ts_rank_cd saturated s/(s+LexicalSaturationK) instead of 1/RRF-rank;
	// coherence = IDF seed overlap over a fixed cap instead of min-max; PageRank = a
	// corpus-relative fixed scale instead of min-max; recency = the decay curve from
	// reference_time, active only when TemporalIntentEnabled finds an order in the
	// query. Default false.
	StageACalibrated bool `json:"stage_a_calibrated,omitempty"`
	// LexicalSaturationK is k in the calibrated lexical feature s/(s+k). Default 1.5:
	// half credit at the ts_rank_cd of a strong match, fitted to the legacy 1/rank
	// scale (memory.DefaultLexicalSaturationK says how). The first default, 0.1, gave
	// one matched word half the credit and lowered evidence recall on the ADR-0136
	// gate. Read only when StageACalibrated.
	LexicalSaturationK float64 `json:"lexical_saturation_k,omitempty"`
	// (ADR-0136 D2's max_per_document lived here; the owner rejected it on
	// 2026-11-03 and it was removed. A file or env var that still sets it is an
	// unknown key and is ignored.)
	// TemporalIntentEnabled (D3) parses the query's time phrase (go-dateparser)
	// against the request's reference_time into a target band (the temporal-fit
	// feature), an order (latest|current = recency decay, earliest = inverted,
	// history|none = recency off) and, for an absolute range only, a hard event-time
	// window. A query with no time phrase gets no time signal. Default false.
	TemporalIntentEnabled bool `json:"temporal_intent_enabled,omitempty"`
	// TemporalFitWeight is the Stage-A weight of the temporal-fit feature, applied
	// only to queries that name a time band. Default 0.2 — the lexical weight's
	// size, so a band match can lift a chunk past cosine near-ties without
	// overriding a clear relevance gap. Read only when TemporalIntentEnabled.
	TemporalFitWeight float64 `json:"temporal_fit_weight,omitempty"`

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

	// RerankerModel selects the cross-encoder the reranker_agent loads
	// (HuggingFace id). Injected into the agent's environment as RERANK_MODEL
	// via the SetEnvExtras seam — config-derived, never read from the kernel's
	// own .env (owner rule 2026-08-11). Empty ⇒ the agent's built-in default
	// (BAAI/bge-reranker-base). Production upgrade target: BAAI/bge-reranker-v2-m3
	// on GPU (~100–200 ms/100 pairs vs ~40 s CPU for the 2023 setup that got
	// Stage-B shelved).
	RerankerModel string `json:"reranker_model,omitempty"`

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

	// RETIRED 2026-11-05 (T-20261105-remove-scenes-completely-owner-decision-2026-11):
	// `workspace_enable_drift_guard` and `workspace_drift_threshold`. Their only reader
	// was the guard on the ADR-0016 scene-primed query vector in WorkspaceStage.enrich,
	// removed with scenes (decisions/scenes-cancelled.md). A layer that still sets them
	// loads unchanged.

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
// Document identity schemes (execution.ingestion.document_identity, ADR-0138 D2).
const (
	DocumentIdentityV2     = "v2"
	DocumentIdentityLegacy = "legacy"
)

type IngestionConfig struct {
	// EdgeExtractionBatchSize is the load-driven drain trigger for the
	// LLM-based entity+relation extractor (ADR-0053 D2). One LLM call extracts
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

	// RETIRED 2026-11-05 (T-20261105-remove-scenes-completely-owner-decision-2026-11):
	// `scene_gen_on_ingest_enabled`. Scenes were cancelled by the owner
	// (cambrian-knowledge/decisions/scenes-cancelled.md) and the ADR-0028 ingest
	// SceneGenerator it switched on was deleted with them. A layer that still sets
	// the key loads unchanged — the decoder ignores keys with no field.

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

	// RETIRED 2026-10-31 (T-20261031-three-config-fields-are-parsed-and-defaulted):
	// `inbox_dir`. The ADR-0028 DirectoryWatcher it pointed at was removed from the
	// boot path (see memory_stack.go); the only remaining reader was a log line.

	// IngestionQueueSize is the bounded document queue depth (ADR-0028). Default 1000.
	IngestionQueueSize int `json:"ingestion_queue_size,omitempty"`

	// IngestionBatchSize is the documents per pipeline batch (ADR-0028). Default 5.
	IngestionBatchSize int `json:"ingestion_batch_size,omitempty"`

	// IngestionWorkers is the number of concurrent pipeline workers (ADR-0028). Default 5.
	IngestionWorkers int `json:"ingestion_workers,omitempty"`

	// IngestionBatchWaitMs is the max batcher wait before flushing (ADR-0028). Default 1000.
	IngestionBatchWaitMs int `json:"ingestion_batch_wait_ms,omitempty"`

	// EventTimeFutureToleranceMs is how far past the ingest clock a stated event
	// time (IngestMemory occurred_at / part times, ADR-0134 Q4) may lie before it is
	// treated as skewed: a later time is still accepted, but its event time is
	// clamped to the ingest time, the claimed time is kept, and time_source becomes
	// "clamped_future". Source clocks are caller-controlled (ADR-0105 two-clock
	// discipline), so a future-dated memory must not outrank everything real on
	// recency. Read by both ingest planes through domain.ResolveIngestEventTime.
	// Default 300000 (5 minutes); 0 clamps anything later than the ingest clock.
	EventTimeFutureToleranceMs int `json:"event_time_future_tolerance_ms"`

	// ChunkContextPrepend prepends the document title and the chunk's section
	// breadcrumb to the chunk text at ingest, before embedding and storage. A
	// chunk whose body opens with a pronoun ("It couples proton flux to…") is
	// otherwise embedded — and FTS-indexed — with its subject in neither index;
	// the RAG Hardening Review (2026-08-12) identified this as the most
	// plausible mechanism behind gold documents that are never retrieved at any
	// depth (titles were reachable by NO retrieval lane). Because the stored
	// text changes, the lexical GIN index gains the same tokens for free.
	// Default false: it is a measurement-gated arm — flipping it changes what
	// every consumer of chunk text sees, and re-ingest is required for existing
	// corpora to benefit.
	ChunkContextPrepend bool `json:"chunk_context_prepend,omitempty"`

	// StructureJunkFilterBackends names the structure-parser backends whose
	// OCR/layout-junk leaves are DROPPED at ingest (ADR-0060). The docling_agent
	// only classifies a leaf as junk (a Docling layout marker such as "image not
	// available", or a prose paragraph with almost no letters); the kernel drops a
	// classified leaf only when the backend that produced it is listed here, and
	// logs every drop with a hash and a sample.
	//
	// Default ["docling"]: the classifier was built for Docling's extraction of
	// binary documents (PDF/DOCX/images). The "markdown" and "text" backends parse
	// chat sessions and every other text ingest, where a "paragraph" with few
	// letters is a markdown table, terminal output or a list of numbers — user
	// content, not noise — so they are NOT listed and drop nothing
	// (T-20261102-ingestion-structure-junk-filter-silently-drops: the filter had
	// been deleting such turns from LongMemEval chat sessions). [] disables
	// dropping everywhere. Valid names: docling, markdown, text.
	StructureJunkFilterBackends []string `json:"structure_junk_filter_backends"`

	// KgExtractorEnabled (ADR-0053 D2 revised) routes write-time chunk-triplet
	// extraction through the deterministic, NO-LLM kg_extractor system agent
	// (metadata + spacy_patterns) instead of the LLM. false ⇒ the LLM extractor
	// (legacy default); true ⇒ the rule-stack organ. The kg_extractor_agent
	// process must be registered + serving for true to take effect.
	KgExtractorEnabled bool `json:"kg_extractor_enabled,omitempty"`

	// SyncIngestWaitForKG makes a SYNC ingest (IngestMemory, the operator
	// IngestDocument plane — every IngestionManager.ProcessSync) return only after
	// its own chunks' KG triplets are extracted and persisted: read-your-writes
	// for the ADR-0053 KG lane (owner decision 2026-11-02). Without it the triplet
	// batcher catches up asynchronously, and under concurrent ingest (~50
	// chunks/s against ~30 chunks/s for the kg_extractor) a query issued right
	// after an ingest can see a partial chunk_triplets graph (KG2RAG expansion,
	// query-entity seeding, anchors). The batcher still batches across documents;
	// a waited document only makes it drain at once instead of idling. Async
	// ingests are unaffected. Default false: the product's ingest latency is
	// unchanged unless an operator (or a benchmark) asks for the guarantee.
	SyncIngestWaitForKG bool `json:"sync_ingest_wait_for_kg,omitempty"`

	// SyncIngestKGWaitTimeoutMs bounds the SyncIngestWaitForKG wait. On expiry
	// the ingest still SUCCEEDS (the content is stored; the KG lane is
	// enrichment) with a WARN and kg_pending in the ingest-timing line and
	// trailer. Default 30000; <=0 falls back to 30000.
	SyncIngestKGWaitTimeoutMs int `json:"sync_ingest_kg_wait_timeout_ms,omitempty"`

	// SyncIngestWaitForFacts is SyncIngestWaitForKG for the ADR-0136 D4 fact layer:
	// a SYNC ingest returns only after its document's facts are extracted and stored,
	// so a benchmark that measures the fact lane measures stored facts, not a race
	// with the extraction queue. No-op unless execution.facts.extract_enabled.
	// Default false: product ingest latency is unchanged.
	SyncIngestWaitForFacts bool `json:"sync_ingest_wait_for_facts,omitempty"`
	// SyncIngestFactsWaitTimeoutMs bounds that wait (one LLM call per extraction window, ADR-0136 A2.3). On
	// expiry the ingest still SUCCEEDS with a WARN and facts_pending in the timing
	// line. Default 120000; <=0 falls back to 120000.
	SyncIngestFactsWaitTimeoutMs int `json:"sync_ingest_facts_wait_timeout_ms,omitempty"`

	// DocumentIdentity selects how an ingested document's id is derived (ADR-0138 D1/D2):
	//   - "v2" (default): the natural key is (writer, source key). Content-addressed ids
	//     digest the writer and the binary payload (Data) too, so two principals, or two
	//     PDFs with the same tag or session, never share an id; a stored legacy id written
	//     by the same writer is reused (grandfathered), never re-keyed.
	//   - "legacy": the pre-ADR-0138 derivation byte for byte, to reproduce an old run's
	//     ids. It brings back both collisions and is logged as a WARN at boot.
	// "" reads as "v2". Read by NewIngestionManager through kernel.ingestionConfigFrom.
	DocumentIdentity string `json:"document_identity,omitempty"`

	// ADR-0136 Amendment A3.1: caps on the agent-plane IngestMemory's DESCRIPTIVE
	// source metadata (source_kind, title, author, and each part's speaker). The
	// kernel trims each value, rejects control characters, and cuts it to its cap
	// (in characters, at a rune boundary). Read by the agent-plane handler
	// (internal/substrate/network/ingest_memory.go) through SourceMetaCaps. Defaults
	// 64 / 256 / 128 / 64; each must be >= 0, and 0 means the default (the same rule
	// as sync_ingest_facts_wait_timeout_ms), so a value can never silently mean "no cap".
	SourceKindMaxChars   int `json:"source_kind_max_chars,omitempty"`
	SourceTitleMaxChars  int `json:"source_title_max_chars,omitempty"`
	SourceAuthorMaxChars int `json:"source_author_max_chars,omitempty"`
	PartSpeakerMaxChars  int `json:"part_speaker_max_chars,omitempty"`

	// KgExtractorCallAttempts is the total number of times one triplet batch's
	// call to the kg_extractor agent is attempted when the agent cannot be
	// booted, dialled or called (T-20261102-kg-extractor-cold-start-drops-the-first-kg:
	// the agent boots lazily and a cold spaCy load can miss the 10 s agent boot
	// timeout). The same retry covers the start-up readiness probe. Default 3;
	// <=0 means 1 (no retry). Read only when KgExtractorEnabled.
	KgExtractorCallAttempts int `json:"kg_extractor_call_attempts,omitempty"`

	// KgExtractorRetryBackoffMs is the sleep before the second call attempt,
	// doubled for each further one. Default 2000; <=0 retries at once.
	KgExtractorRetryBackoffMs int `json:"kg_extractor_retry_backoff_ms,omitempty"`

	// KgTripletsMaxRequeues bounds how many times a chunk whose triplet batch
	// failed because the extractor was UNAVAILABLE (after the call attempts
	// above) is put back into the batcher's pending buffer before it settles as
	// failed (kg_failed in the ingest-timing trailer, one WARN per batch). Re-
	// queueing re-appends items already accepted, so it adds no queue growth and
	// no goroutine. Default 1; 0 never re-queues.
	KgTripletsMaxRequeues int `json:"kg_triplets_max_requeues,omitempty"`

	// KgTripletsLLMFormat selects the reply format of the LLM triplet extractor (the
	// residue tier of the chunk_triplets lane): "completion" (default; "" reads as it)
	// is the KG²RAG base-model completion prompt with its <h##r##t>$$ grammar,
	// "structured" the library prompt kg_triplets as a cached system block, chunks
	// keyed by id and a response schema (ADR-0139 C1/C4). Both apply the same triplet
	// filters; the default moves only after an A/B/A retrieval run (AGENTS.md §0.5).
	KgTripletsLLMFormat string `json:"kg_triplets_llm_format,omitempty"`
}

// DefaultStepTimeoutFloorMs is the default of plan.step_timeout_floor_ms.
const DefaultStepTimeoutFloorMs = 180000

// DefaultPlanIdleTimeoutMs is the default of plan.plan_idle_timeout_ms: 3 min,
// equal to DefaultStepTimeoutFloorMs on purpose. A step that makes no kernel
// call at all (no model call, no tool call, no progress) is cut by its own
// per-step deadline first, or at the same moment — so a silent step reads as
// "step timed out" (its own fault) rather than as the plan going idle, and the
// idle watchdog only decides when NOTHING in the plan is running
// (T-20261105-owner-decision-needed-120-s-plan-timeout-ms).
const DefaultPlanIdleTimeoutMs = 180000

// DefaultPlanMaxDurationMs is the default of plan.plan_max_duration_ms: the
// absolute safety cap on one plan, 30 min (owner decision 2026-11-05).
const DefaultPlanMaxDurationMs = 1800000

// DefaultDependencyResultsMaxChars is the default of plan.dependency_results_max_chars.
const DefaultDependencyResultsMaxChars = 24000

// KgTripletsLLMFormat values.
const (
	KgTripletsFormatCompletion = "completion"
	KgTripletsFormatStructured = "structured"
)

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
	ToolsAutoApprove bool `json:"tools_auto_approve,omitempty"`

	// ToolRetrievalFloor (ADR-0044) is the minimum cosine similarity for a tool to
	// be served by semantic retrieval. Below it, "no tool fits" ⇒ an empty menu
	// (grounding safeguard). 0 ⇒ no floor (any top-k is returned). Tunable.
	ToolRetrievalFloor float64 `json:"tool_retrieval_floor,omitempty"`

	// ToolMenuK is how many tools/skills the SDK's ReAct loop lists per menu
	// query. 0 ⇒ the SDK's built-in default (3). The kernel INJECTS this into
	// every agent's environment (CAMBRIAN_TOOL_MENU_K) from config — it is a
	// config value, not something the kernel's own .env must carry and pass
	// through the SEC-01 allowlist (owner rule 2026-08-11: knobs never depend
	// on .env; the env layer is for secrets and deployment overrides).
	ToolMenuK int `json:"tool_menu_k,omitempty"`
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

// AgentsConfig — Agent process runtime: resource caps, env passthrough and daemon restart policy.
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

	// AgentBootTimeoutMs bounds how long the kernel waits for a spawned agent to
	// open its socket before it kills the child and fails the boot. A child that
	// exits early fails at once regardless; this bounds one that is alive but slow.
	// Default 30000: the built-in 10 s killed the kg_extractor agent (spaCy load)
	// three boots in a row while the kernel was booting its other agents, so the
	// KG lane had no extractor until the machine went idle
	// (T-20261102-kg-extractor-cold-start-drops-the-first-kg). <= 0 = 10 s.
	AgentBootTimeoutMs int `json:"agent_boot_timeout_ms,omitempty"`

	// AgentEnvPassthrough lists extra NON-SECRET environment variable names that
	// spawned agents may inherit beyond the OS base allowlist (SEC-01). Secret-
	// looking names (*_API_KEY, CAMBRIAN_*, provider prefixes, …) are stripped
	// regardless. Empty by default: agents get only OS essentials + the substrate
	// socket/addr (passed as flags), never the kernel's credentials.
	AgentEnvPassthrough []string `json:"agent_env_passthrough,omitempty"`

	// RETIRED 2026-11-05 (T-20261105-delete-the-llm-agent-interview-now-owner):
	// `disable_interviews`. The ADR-0037 graded LLM interview it switched off was
	// deleted (owner decision, ADR-0139 Q3): every agent is now seeded the way this
	// flag's `true` arm seeded it — SR=Trust=0.5 and its Description as the Layer-2
	// vector — and learns from verified outcomes (ADR-0139 D6). A layer that still
	// sets it loads unchanged; retiredExecutionFields records it.

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

	// ── ADR-0139 D12: in-agent subagents (Orchestrator.SpawnSubagent) ──
	// Every limit below is enforced by the kernel (internal/substrate/network/
	// subagent.go), never by the agent: a child is a separate kernel dispatch on
	// its own lease that rolls up to the parent's ROOT lease.

	// SubagentMaxDepth is how deep a spawn chain may go, counted from the lease
	// chain (a root step lease is depth 0, its child depth 1). A spawn that would
	// exceed it ends with exit_reason DEPTH and never runs. Default 1 (D12):
	// children may not spawn grandchildren. 0 disables spawning entirely.
	SubagentMaxDepth int `json:"subagent_max_depth"`
	// SubagentMaxConcurrency is how many children of one ROOT lease may run at
	// once. A spawn beyond it waits for a slot until its timeout, then ends with
	// TIMEOUT. Default 4 (D12). <= 0 is read as 1.
	SubagentMaxConcurrency int `json:"subagent_max_concurrency"`
	// SubagentTreeMaxToolRounds is the tree-wide iteration cap: the tool calls
	// all children of one root may make together. A child's max_tool_rounds is
	// clamped to what remains. Default 64. <= 0 = no tree cap.
	SubagentTreeMaxToolRounds int `json:"subagent_tree_max_tool_rounds"`
	// SubagentDefaultMaxToolRounds is a child's tool-call cap when the spawn
	// asks for 0. Default 16.
	SubagentDefaultMaxToolRounds int `json:"subagent_default_max_tool_rounds"`
	// SubagentTreeTokenBudget caps the model tokens all children of one root may
	// spend together. 0 (default) = no tree cap beyond the session spend ceiling
	// (ADR-0131), which still binds every child call. A spawn's token_budget is
	// clamped to what remains.
	SubagentTreeTokenBudget int `json:"subagent_tree_token_budget"`
	// SubagentDefaultTimeoutMs is a child's wall-clock limit when the spawn asks
	// for 0. Always clamped to the parent call's own deadline. Default 300000.
	SubagentDefaultTimeoutMs int `json:"subagent_default_timeout_ms"`
	// SubagentResultMaxChars caps the child's text / structured_json in the
	// result; a longer answer is cut and marked truncated. Default 16000.
	SubagentResultMaxChars int `json:"subagent_result_max_chars"`
	// SubagentWithheldTools are the tool-name globs a "*" toolset does NOT pass
	// to a child (D12: no clarify, memory write or messaging unless granted). A
	// child gets one only when the spawn names it explicitly.
	SubagentWithheldTools []string `json:"subagent_withheld_tools"`
	// SubagentMemoryWriteTools are the names (globs) that grant a child memory
	// write when the spawn's toolset names them explicitly: the lease is then
	// bound MemoryWriteGranted, which the D14 notes service reads. They are
	// control verbs, not registry tools, so they are not checked against the
	// parent's tool menu — only against the parent's own memory-write right.
	SubagentMemoryWriteTools []string `json:"subagent_memory_write_tools"`
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

	// RETIRED 2026-10-31 (T-20261031-three-config-fields-are-parsed-and-defaulted):
	// `k_anonymity_floor` (ADR-0034 D11). The insight-promotion pipeline it gated
	// was removed; nothing read it afterwards.
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

	// TracePipelinePayloads attaches a bounded preview of what each item carried
	// to the per-step flow view (ADR-0114 D38).
	//
	// Default OFF, for the same reason as the exchange capture above: an item's
	// value is the real data moving through the deployment, and shipping it to
	// the operator plane by default would make a diagnostic view an
	// exfiltration surface. The FLOW itself — which node, which port, how long
	// — is always emitted; only the contents are gated.
	TracePipelinePayloads bool `json:"trace_pipeline_payloads,omitempty"`

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

	// Capability vocabulary and resolution.
	Capability CapabilityConfig `json:"capability"`

	// Session lifecycle: idle timeout, retention sweeps and token TTL.
	Session SessionConfig `json:"session"`

	// Long-term memory: experiential records, recall thresholds, consolidation and GC.
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

	// Tool execution policy and retrieval.
	Tools ToolsConfig `json:"tools"`

	// Chat session worker pool.
	Chat ChatConfig `json:"chat"`

	// Agent process runtime: resource caps, env passthrough and daemon restart policy.
	Agents AgentsConfig `json:"agents"`

	// Supervision statistics: aggregation cadence, EWMA/latency windows and histogram sizing.
	Supervision SupervisionConfig `json:"supervision"`

	// Ingress classification routing.
	Router RouterConfig `json:"router"`

	// LLM gateway concurrency, retry and exchange capture.
	LLM LLMConfig `json:"llm"`

	// Graph holds spreading activation configuration (ADR-0017).
	Graph GraphConfig `json:"graph"`

	// Pipelines holds reactive-pipeline execution settings (ADR-0114).
	Pipelines PipelinesConfig `json:"pipelines"`

	// Facts is the ADR-0136 D4/D5 fact layer: per-document fact extraction, the fact
	// retrieval lane and valid-time supersession. Every switch defaults off.
	Facts FactsConfig `json:"facts"`

	// Hooks is the ADR-0139 D11 lifecycle-hook declarations: built-in parameterised
	// hooks and external command hooks. Empty by default (no hooks).
	Hooks HooksConfig `json:"hooks"`

	// MemoryNotes is ADR-0139 D14 principal memory notes: default budgets, the org
	// scope and the frozen-snapshot cache (internal/memorynotes; memory_notes.go).
	MemoryNotes MemoryNotesConfig `json:"memory_notes"`

	// Skills is the ADR-0139 D13 skill lifecycle: agent proposals and the
	// deterministic curator (internal/skill/lifecycle; skills_lifecycle.go).
	Skills SkillsConfig `json:"skills"`

	// StepOutput is the live step-output stream (ADR-0139 amendment 2026-11-06):
	// gateway deltas on both model lanes, agent-pushed output, caps and the MCP
	// progress wait (step_output.go).
	StepOutput StepOutputConfig `json:"step_output"`
}

// Hook kinds accepted in execution.hooks.entries[].kind (ADR-0139 D11).
const (
	// HookKindDenyArgsRegex denies a tool call whose arguments match Pattern.
	HookKindDenyArgsRegex = "deny_args_regex"
	// HookKindRequireApproval narrows the named tools' calls to ask (an approval).
	HookKindRequireApproval = "require_approval"
	// HookKindRedactOutput replaces Pattern matches in a tool result or model reply.
	HookKindRedactOutput = "redact_output"
	// HookKindCommand runs an external program: the JSON event on stdin, a JSON
	// result on stdout.
	HookKindCommand = "command"
)

// HooksConfig declares lifecycle hooks (ADR-0139 D11). Hooks can only narrow a
// decision (allow → ask → deny), never widen one; a rewrite needs may_rewrite and
// a rewritten tool call is re-authorized. Plugins add hooks through
// app.Registry.AddHook; these are the operator's.
type HooksConfig struct {
	// Entries are the declared hooks. Run order is priority (ascending), then name.
	Entries []HookConfig `json:"entries,omitempty"`
	// DefaultTimeoutMs bounds a hook whose timeout_ms is 0. 0 ⇒ 5000.
	DefaultTimeoutMs int `json:"default_timeout_ms,omitempty"`
	// GuardedArguments are the control-verb arguments post_model hooks see and may
	// rewrite or refuse besides the final answer (HookEvent.GuardedArgs): tool
	// calls the SDK handles itself, which never reach the ToolExecutor and so never
	// pass a pre_tool hook. Empty = DefaultGuardedArguments (the SDK's control
	// verbs); a non-empty list replaces the defaults. Read by the llm.Provider
	// (llm.WithGuardedArgs, app.go). T-20261106-post-model-hooks-do-not-guard-control-verb.
	GuardedArguments []GuardedArgumentConfig `json:"guarded_arguments,omitempty"`
}

// HookConfig is one declared hook.
type HookConfig struct {
	// Name is unique across all hooks (config and plugin); it is logged on every call.
	Name string `json:"name"`
	// Kind: deny_args_regex | require_approval | redact_output | command.
	Kind string `json:"kind"`
	// Points the hook attaches to (pre_tool, post_tool, pre_model, post_model,
	// pre_compact, post_compact, run_start, run_end, subagent_start, subagent_end).
	// Empty ⇒ the kind's natural point: deny_args_regex and require_approval →
	// pre_tool, redact_output → post_tool. A command hook must name its points.
	Points []string `json:"points,omitempty"`
	// Priority orders hooks; lower runs first.
	Priority int `json:"priority,omitempty"`
	// OnFailure is allow | block. Empty ⇒ block at pre_* and *_start points,
	// allow at post_* and *_end points.
	OnFailure string `json:"on_failure,omitempty"`
	// TimeoutMs bounds one invocation. 0 ⇒ default_timeout_ms.
	TimeoutMs int `json:"timeout_ms,omitempty"`
	// MayRewrite permits input/output rewrites. Required for redact_output; for a
	// command hook it decides whether its rewrites are honoured.
	MayRewrite bool `json:"may_rewrite,omitempty"`
	// Tools restricts tool-point hooks to these tool names (fnmatch globs, the
	// profile-allowlist syntax: `*` crosses `/`, so "mcp:*" covers every MCP
	// tool). Empty ⇒ every tool. Required for require_approval.
	Tools []string `json:"tools,omitempty"`
	// Pattern is the RE2 regex for deny_args_regex (matched against the raw
	// argument JSON) and redact_output.
	Pattern string `json:"pattern,omitempty"`
	// Replacement is redact_output's substitute. Empty ⇒ "[REDACTED]".
	Replacement string `json:"replacement,omitempty"`
	// Reason is the narrowing reason the agent sees (deny_args_regex, require_approval).
	Reason string `json:"reason,omitempty"`
	// Command is a command hook's argv: Command[0] is the program, the rest its
	// arguments. Never a shell string; no shell is involved.
	Command []string `json:"command,omitempty"`
	// Env names extra environment variables passed to a command hook, on top of
	// the OS essentials. Secret-shaped names are stripped regardless (procenv).
	Env []string `json:"env,omitempty"`
	// Agents scopes the hook to events of these agent ids (fnmatch globs, as
	// Tools). Applies at every point: tool, model and subagent events all carry
	// the acting agent. Empty = every event; a non-empty list never matches an
	// event with no agent (an organ call such as the planner's has none).
	Agents []string `json:"agents,omitempty"`
	// Purposes scopes the model points (pre_model, post_model) to calls with these
	// purpose labels (fnmatch globs): router, planner, verifier, memory, facts,
	// thought (an in-kernel thought step), agent_step (every agent-plane gateway
	// call), model (a model-bound handle). A deliverables scope is
	// [agent_step, thought]. Empty = every purpose. Only valid on a hook that
	// attaches to a model point.
	Purposes []string `json:"purposes,omitempty"`
}

// GuardedArgumentConfig names one guarded outbound argument: the string argument
// Arg of every call to Tool in a model reply (execution.hooks.guarded_arguments).
type GuardedArgumentConfig struct {
	Tool string `json:"tool"`
	Arg  string `json:"arg"`
}

// DefaultGuardedArguments are the SDK's control-verb arguments that leave a model
// reply without passing the ToolExecutor (sdk/cambrian_agent_sdk/react.py
// _YIELD_TOOL_DEF and _KERNEL_VERB_DEFS): a subgoal's intent becomes a plan step,
// a spawn's objective a child's task, a memory note's content and a proposed
// skill's body are persisted. spawn_subagent's context_summary is NOT a model
// argument (the SDK builds it for the SpawnSubagent RPC), so it is not here.
var DefaultGuardedArguments = []GuardedArgumentConfig{
	{Tool: "yield_subgoal", Arg: "intent"},
	{Tool: "spawn_subagent", Arg: "objective"},
	{Tool: "propose_memory_note", Arg: "content"},
	{Tool: "propose_skill", Arg: "skill_md"},
}

// GuardedArgumentList is GuardedArguments with its default applied, as the
// domain type the llm.Provider consumes (llm.WithGuardedArgs). The final answer
// (execution.step_output.final_answer_tool/arg) is always guarded on top.
func (h HooksConfig) GuardedArgumentList() []domain.GuardedArg {
	src := h.GuardedArguments
	if len(src) == 0 {
		src = DefaultGuardedArguments
	}
	out := make([]domain.GuardedArg, 0, len(src))
	for _, g := range src {
		out = append(out, domain.GuardedArg{Tool: g.Tool, Arg: g.Arg})
	}
	return out
}

// attachesToModelPoint reports whether a hook entry names a model point. No kind's
// natural point is a model point, so an entry with no points never does.
func (e HookConfig) attachesToModelPoint() bool {
	for _, p := range e.Points {
		if p == string(domain.HookPreModel) || p == string(domain.HookPostModel) {
			return true
		}
	}
	return false
}

// validate checks execution.hooks. A bad hook fails the load: a safety
// control that silently did not install is worse than a boot error.
func (h HooksConfig) validate() []string {
	var errs []string
	if h.DefaultTimeoutMs < 0 {
		errs = append(errs, "hooks.default_timeout_ms must be >= 0")
	}
	guarded := map[GuardedArgumentConfig]bool{}
	for i, g := range h.GuardedArguments {
		at := fmt.Sprintf("hooks.guarded_arguments[%d]", i)
		if strings.TrimSpace(g.Tool) == "" || strings.TrimSpace(g.Arg) == "" {
			errs = append(errs, at+": tool and arg are both required")
			continue
		}
		if guarded[g] {
			errs = append(errs, fmt.Sprintf("%s: %s.%s is listed twice", at, g.Tool, g.Arg))
		}
		guarded[g] = true
	}
	seen := map[string]bool{}
	for i, e := range h.Entries {
		at := fmt.Sprintf("hooks.entries[%d]", i)
		if strings.TrimSpace(e.Name) == "" {
			errs = append(errs, at+".name is required")
		} else if seen[e.Name] {
			errs = append(errs, fmt.Sprintf("%s.name %q is declared twice", at, e.Name))
		}
		seen[e.Name] = true
		for _, p := range e.Points {
			if !domain.HookPoint(p).Valid() {
				errs = append(errs, fmt.Sprintf("%s.points: unknown point %q", at, p))
			}
		}
		switch e.OnFailure {
		case "", "allow", "block":
		default:
			errs = append(errs, fmt.Sprintf("%s.on_failure must be allow or block (got %q)", at, e.OnFailure))
		}
		if e.TimeoutMs < 0 {
			errs = append(errs, at+".timeout_ms must be >= 0")
		}
		for _, t := range e.Tools {
			if strings.TrimSpace(t) == "" {
				errs = append(errs, at+".tools must not contain an empty name")
			}
		}
		for _, a := range e.Agents {
			if strings.TrimSpace(a) == "" {
				errs = append(errs, at+".agents must not contain an empty glob")
			}
		}
		for _, p := range e.Purposes {
			if strings.TrimSpace(p) == "" {
				errs = append(errs, at+".purposes must not contain an empty glob")
			}
		}
		if len(e.Purposes) > 0 && !e.attachesToModelPoint() {
			errs = append(errs, at+".purposes scope model calls: the hook must attach to pre_model or post_model")
		}
		needPattern := func() {
			if e.Pattern == "" {
				errs = append(errs, at+".pattern is required for kind "+e.Kind)
				return
			}
			if _, err := regexp.Compile(e.Pattern); err != nil {
				errs = append(errs, fmt.Sprintf("%s.pattern does not compile: %v", at, err))
			}
		}
		switch e.Kind {
		case HookKindDenyArgsRegex:
			needPattern()
		case HookKindRequireApproval:
			if len(e.Tools) == 0 {
				errs = append(errs, at+".tools is required for require_approval")
			}
		case HookKindRedactOutput:
			needPattern()
			if !e.MayRewrite {
				errs = append(errs, at+": redact_output rewrites output and needs may_rewrite: true")
			}
		case HookKindCommand:
			if len(e.Command) == 0 || strings.TrimSpace(e.Command[0]) == "" {
				errs = append(errs, at+".command is required (argv; command[0] is the program)")
			}
			if len(e.Points) == 0 {
				errs = append(errs, at+".points is required for a command hook")
			}
		default:
			errs = append(errs, fmt.Sprintf("%s.kind must be one of %s, %s, %s, %s (got %q)", at,
				HookKindDenyArgsRegex, HookKindRequireApproval, HookKindRedactOutput, HookKindCommand, e.Kind))
		}
	}
	return errs
}

// FactsConfig is the ADR-0136 fact layer (D4 fact-augmented keys into chunks, D5
// valid-time supersession). Owner-assumed defaults (2026-11-03): the whole layer is
// DEFAULT OFF (its extraction spends one LLM call per ingested document, ADR-0131), the
// attribute registry is empty so every attribute is multi (nothing supersedes until an
// operator declares it single).
type FactsConfig struct {
	// ExtractEnabled runs asynchronous LLM extraction (one call per A2.3 window) per ingested document after
	// its chunks are stored (never on the ingest RPC path), writing each fact as a
	// memory_fact knowledge item (ADR-0136 A1) through the write chokepoint. The generator is the "facts" system role
	// (llm_provider.roles.facts; unset = the default generator). Default false.
	ExtractEnabled bool `json:"extract_enabled,omitempty"`
	// LaneEnabled adds the fact lane to retrieval: a fact vector search under the
	// caller's read predicate whose hits contribute their SOURCE CHUNKS as primary hits,
	// plus the fact_match Stage-A feature and the fact text as hit metadata. Default false.
	LaneEnabled bool `json:"lane_enabled,omitempty"`
	// SupersessionEnabled (D5, as amended by A1) seeds the declared predicate profiles,
	// lets a change-language cue (supersedes_prior) give an undated fact the session time
	// as its start, and lets the lane rank by the derived chains — the substrate's
	// valid_time_supersession authority closes an older value by dates, never deleting
	// it. The judge call is a separate switch (assessments_enabled). The lane ranking
	// is current_fact for current/latest queries and the value chain for
	// history/earliest ones (those need
	// retrieval.temporal_intent_enabled). Default false.
	SupersessionEnabled bool `json:"supersession_enabled,omitempty"`

	// QueueSize is the extraction queue bound (documents). Full = the document's facts
	// are dropped with a WARN; its chunks are stored. Default 1024.
	QueueSize int `json:"queue_size"`
	// Workers is the extraction worker count (concurrent LLM calls). Default 2.
	Workers int `json:"workers"`
	// LLMTimeoutMs is the base bound of one extraction (or judge) call. Default 120000.
	LLMTimeoutMs int `json:"llm_timeout_ms"`
	// LLMTimeoutPerKCharMs adds this much per 1000 prompt characters to llm_timeout_ms,
	// so a full max_prompt_chars window gets the time its longer answer needs (a 48k
	// window ran past a flat 120 s even alone, T-20261104-fact-extractor-fails-
	// permanently-on-some). 0 = a flat llm_timeout_ms. Default 2500.
	LLMTimeoutPerKCharMs int `json:"llm_timeout_per_kchar_ms"`
	// LLMTimeoutMaxMs caps the scaled timeout, so a call stays bounded. Must be >=
	// llm_timeout_ms. Default 300000.
	LLMTimeoutMaxMs int `json:"llm_timeout_max_ms"`
	// ParseRetries is how many times an extraction call whose answer cannot be parsed
	// is asked again with the same prompt (the model intermittently wraps or trails the
	// object; a plain re-ask fixed 6 of 7). 0 = no retry; at most 3. Default 1.
	ParseRetries int `json:"parse_retries"`
	// MaxFactsPerDocument caps the facts kept from ONE extraction call — one window of
	// a source (ADR-0136 A2.3; the name predates windows). Default 64.
	MaxFactsPerDocument int `json:"max_facts_per_document"`
	// MaxPromptChars caps one extraction prompt. A2.3: a longer source is packed in order
	// into several windows of at most this size, one call each (up to
	// max_calls_per_document). Default 48000.
	MaxPromptChars int `json:"max_prompt_chars"`
	// MaxCallsPerDocument caps the extraction windows (LLM calls) one source costs per
	// run (A2.3, owner answer A2-Q1). A source with more windows is extracted up to the
	// cap and recorded as a PARTIAL extraction marker that cmd/facts-backfill resumes
	// from the first uncovered window. 0 = no cap. Default 8 (about 384k characters).
	MaxCallsPerDocument int `json:"max_calls_per_document"`
	// MaxFactsPerSource bounds the facts kept from one source across all its windows
	// (A2.3); once reached, the rest of the source is not extracted. Default 512
	// (64 x 8).
	MaxFactsPerSource int `json:"max_facts_per_source"`
	// LaneTopK is how many facts the lane fetches per query. Default 20.
	LaneTopK int `json:"lane_top_k"`
	// LaneScopedDocuments is N of the ADR-0136 A4 document-scoped fact lane
	// (experimental): after the fused dense+lexical pool is formed, the lane also
	// searches only the facts whose source chunk belongs to the first N DISTINCT source
	// documents of that pool, so an incidental fact of a document the kernel already
	// found is not outranked by the principal's whole fact set. 0 = off (the default).
	// Needs lane_enabled; without it the setting is a no-op (WARN at boot).
	LaneScopedDocuments int `json:"lane_scoped_documents"`
	// LaneScopedTopK is how many facts one scoped search keeps (A4). Default 20; > 0
	// when lane_scoped_documents > 0.
	LaneScopedTopK int `json:"lane_scoped_top_k"`
	// FactMatchWeight is the Stage-A weight of fact_match (needs blend_enabled), joined
	// only for a query the lane matched. Default 0.2.
	FactMatchWeight float64 `json:"fact_match_weight"`
	// CurrentFactWeight is the Stage-A weight of current_fact, joined only for a
	// current/latest query with supersession on. Default 0.1.
	CurrentFactWeight float64 `json:"current_fact_weight"`
	// SingleAttributes declares predicates that hold one value at a time and therefore
	// supersede ("home_city", "employer"). Matched after normalisation (lower case, words
	// joined by '_'). ADR-0136 A1.4: seeded at boot as DECLARED predicate profiles
	// (predicate_profiles, source=declared), which learned votes never overrule — config
	// is v1's only override channel (A1-Q5). An undeclared predicate's cardinality is
	// learned from assessments, and is `unknown` until then. Read only with
	// supersession_enabled.
	SingleAttributes []string `json:"single_attributes"`
	// MultiAttributes declares predicates whose values accumulate ("likes", "owns_pet")
	// — the same seeding as SingleAttributes with cardinality multi. A name in both lists
	// is multi. Default [].
	MultiAttributes []string `json:"multi_attributes"`
	// AssessmentsEnabled (ADR-0136 A1.2 rules 2-4, gate arm F2b) adds at most ONE
	// batched judge call per session over the candidate pairs the cues and profiles
	// cannot decide — exact-key pairs of an unknown-cardinality predicate and
	// fuzzy_candidates same-subject facts under other predicates. Each verdict is stored
	// as a keyed assessment (knowledge_assessments) before it has any effect, so a replay
	// over the same candidates makes no call; exact-key verdicts vote on the predicate's
	// learned cardinality. Needs supersession_enabled. Default false (F2a: cues + exact
	// match + declared/learned cardinality, no judge call).
	AssessmentsEnabled bool `json:"assessments_enabled,omitempty"`
	// FuzzyCandidates is k of A1.2 rule 3 — same-subject candidates per new fact for the
	// batched assessment call. 0 = exact-key pairs only. Default 10.
	FuzzyCandidates int `json:"fuzzy_candidates"`
	// CardinalityMinVotes is the A1.4 learning threshold: a predicate becomes learned
	// single (or multi) once that many assessments voted that way with no opposing vote;
	// a split vote stays unknown. Default 3.
	CardinalityMinVotes int `json:"cardinality_min_votes"`
	// BackfillGroupConcurrency is how many documents of ONE ordered classification group
	// cmd/facts-backfill runs at once (its -group-concurrency flag overrides it;
	// T-20261103-fact-backfill-processes-each-principal-s). 1 is the serial ordered
	// path: one document at a time in event-time order. N > 1 runs the group in two
	// phases — every document's extraction and every window's judge call N at a time,
	// then the assessments, votes and markers committed one window at a time in
	// event-time order — with the same final items, assessments, resolutions and
	// predicate profiles as the serial path (ADR-0136 A2 "Backfill concurrency"). It
	// multiplies with the command's -concurrency (groups in flight). Read only by the
	// backfill, never by live ingest. Default 1; 0 reads as 1; negative is refused.
	BackfillGroupConcurrency int `json:"backfill_group_concurrency"`
}

// PipelinesConfig governs whether authored pipelines actually EXECUTE.
type PipelinesConfig struct {
	// DrainerEnabled starts the worker that claims and runs queued pipeline
	// tasks. Default FALSE.
	//
	// Not a performance switch — without it, routing enqueues work that nothing
	// ever claims, and the runs sit in `running` forever while the console
	// reports them as live. Off by default because turning it on for the first
	// time does not resume a paused system, it starts one: everything already
	// queued executes at once, each run against the plan revision it was pinned
	// to rather than any correction made since. Inspect the queue, then enable.
	DrainerEnabled bool `json:"drainer_enabled,omitempty"`
}

// GraphConfig defines spreading activation parameters (ADR-0017).
//
// RETIRED 2026-10-31 (T-20261031-graph-edge-weight-and-consolidator-timeout):
// `weight_contradicts`, `weight_specifies`, `weight_closes`, `weight_discussed_in`
// (ADR-0053 D2 replaced the per-type weight map with the edge's own confidence; the
// spreader reads DocumentEdge.Weight and the kernel only discarded these) and
// `consolidator_llm_timeout` (its ConsolidatorAgent was removed 2026-07-18). A
// layer that still sets them loads unchanged; retiredExecutionMapEntries
// (execution_migration_test.go) records them.
type GraphConfig struct {
	DecayFactor float64 `json:"decay_factor"`
	MaxDepth    int     `json:"max_depth"`
	EnergyFloor float64 `json:"energy_floor"`
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
		// BunExecutable / NodeExecutable are the JS agent-runtime interpreters
		// (ADR-0125). Empty ⇒ the kernel falls back to `bun`/`node` on $PATH at
		// spawn time; a spawn of a bun/node agent errors (naming the key) only
		// when neither resolves. Additive keys — python_executable is untouched.
		BunExecutable  string `json:"bun_executable"`
		NodeExecutable string `json:"node_executable"`
		AgentsDir      string `json:"agents_dir"`
	} `json:"metabolism"`
	Storage struct {
		DataDir string `json:"data_dir"`
		DBName  string `json:"db_name"`
		// AutoMigrate runs the DB migration runner at boot (PLAT-02 / ADR-0064:
		// stamp baseline + apply pending forward-delta migrations + refuse if the DB
		// is ahead of the binary). Default true — set false to manage migrations only
		// via the `migrate` CLI / external tooling.
		AutoMigrate bool `json:"auto_migrate"`
		// CompactMinBytes / CompactFreeRatio gate the boot-time offline compaction
		// of the bbolt file (ADR-0061 A2.8): before the store is opened, a file
		// larger than CompactMinBytes of which more than CompactFreeRatio is free
		// pages is rewritten (bbolt never returns freed pages to the file system,
		// so this is the only way the file shrinks). Defaults 256 MiB / 0.5;
		// CompactMinBytes <= 0 disables it. Read by kernel.BootstrapStorage.
		CompactMinBytes  int64   `json:"compact_min_bytes"`
		CompactFreeRatio float64 `json:"compact_free_ratio"`
	} `json:"storage"`
	Database struct {
		Host     string `json:"host"`
		Port     string `json:"port"`
		User     string `json:"user"`
		Password string `json:"password"`
		DBName   string `json:"dbname"`
		// Migrations is the boot-time migration guard (see MigrationsConfig).
		Migrations MigrationsConfig `json:"migrations"`
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
		//
		// ADR-0130 D2: the flag ALSO DISABLES AGENT-PLANE AUTHENTICATION. With it
		// set, x-agent-id is trusted as the principal and no per-agent credential
		// (agent:<id>:token) is required — any process that reaches the port can
		// act as any agent. The loopback benchmark harness and the integration
		// tests run under it; a deployment should not.
		InsecureLocalhost bool `json:"insecure_localhost"`
		// HealthzPort, when > 0, starts an HTTP /healthz shim on that port for dumb
		// probes (PLAT-03 / ADR-0065). Off by default; the gRPC grpc.health.v1 service
		// is always on the main listener.
		HealthzPort int `json:"healthz_port"`
		// HealthCheckIntervalSeconds is how often the readiness probe (DB ping) runs.
		// Default 10.
		HealthCheckIntervalSeconds int `json:"health_check_interval_seconds"`
		// MCP is the INBOUND Cambrian MCP endpoint (ADR-0126 D3) — the surface
		// external agents dial. It is NOT the top-level `mcp` block, which is the
		// OUTBOUND connector dialling foreign servers (ADR-0043); the two live
		// apart on purpose, because reusing one name for both directions makes
		// every comment and console label ambiguous.
		MCP MCPEndpointConfig `json:"mcp"`
		// A2A is the kernel's A2A Agent Card listener (ADR-0139 D4/C2): signed
		// cards for the registered agents at the A2A well-known path. Like MCP,
		// the INBOUND side lives here and the OUTBOUND side (remote agents the
		// kernel registers) is the top-level `a2a` block.
		A2A A2AEndpointConfig `json:"a2a"`
		// RateLimit is the per-principal token bucket on the gRPC planes
		// (rate_limit.go). Default on and generous.
		RateLimit RateLimitConfig `json:"rate_limit"`
	} `json:"server"`
	// ADR-0042: new model-provisioning spine. Additive for now — the legacy LLM
	// block and Models array above remain until the cutover (slice 0042-07).
	Embedder    EmbedderConfig    `json:"embedder"`
	LLMProvider LLMProviderConfig `json:"llm_provider"`
	Execution   ExecutionConfig   `json:"execution"`
	Telemetry   TelemetryConfig   `json:"telemetry"`
	// RETIRED 2026-10-31 (T-20261031-agent-pool-default-agent-timeout-ms-is): the
	// `agent_pool` block and its only key `default_agent_timeout_ms`. No manifest
	// field or call path ever read it. A file or env layer that still sets it loads
	// unchanged (the decoder ignores keys with no field).
	// ADR-0043: external MCP servers the kernel consumes tools from. Opt-in —
	// absent/empty ⇒ no MCP behaviour.
	MCP MCPConfig `json:"mcp,omitempty"`
	// A2A lists the remote A2A agents the kernel registers and how their cards'
	// signatures are checked (ADR-0139 D4). The inbound card listener is
	// server.a2a.
	A2A     A2AConfig     `json:"a2a"`
	Chunker ChunkerConfig `json:"chunker"`
	// Relations declares the identity plane's link verbs (five-planes step 2,
	// D-W5-1). Absent/empty is a working deployment: the built-in seeds
	// `same_as` and `preceded_and_shares_entities` are what the kernel's own
	// lanes need, and a verb nobody declared is refused at the write rather
	// than admitted at some default. See relations.go.
	Relations []RelationConfig `json:"relations,omitempty"`
}

// MCPEndpointConfig configures the inbound Cambrian MCP endpoint (ADR-0126 D3),
// served on its own listener from the Published Tool Surface.
//
// DEFAULT DISABLED. The endpoint is the one surface strangers are meant to
// reach, so it opens because a deployment said so — never because a kernel was
// upgraded.
type MCPEndpointConfig struct {
	// Enabled switches the endpoint on. Off ⇒ no listener is bound and nothing
	// else in this block is read.
	Enabled bool `json:"enabled"`
	// Port is the TCP port the endpoint binds, on the same interface as the
	// gRPC plane (server.bind_address) and under the same SEC-03 discipline.
	//
	// There is deliberately NO implied default: which port a deployment's public
	// front door occupies is a deployment decision, and a kernel that picked one
	// for you would open a port nobody chose. Enabled with no port is a boot
	// error naming this key.
	Port int `json:"port"`
	// Clients names the external clients allowed to dial. Each name resolves to
	// a stored credential under the ADR-0101 named secret `mcp:client:<name>`;
	// the VALUE never appears here or in any log.
	//
	// A name with no stored credential can never authenticate — that is the
	// fail-closed direction, and it is why the list is config while the secrets
	// are not: the roster is reviewable, the tokens are not readable.
	Clients []string `json:"clients"`
	// MaxConcurrentPerClient bounds in-flight requests per client credential;
	// over-cap requests are refused with 429 rather than queued.
	//
	// It exists because coding agents retry aggressively and the answer tools
	// spend LLM budget. Default 4; a value ≤ 0 falls back to that default rather
	// than removing the cap — an unmetered public endpoint is not a mode.
	MaxConcurrentPerClient int `json:"max_concurrent_per_client"`
}

// DefaultMCPMaxConcurrentPerClient is the per-credential in-flight bound applied
// when server.mcp.max_concurrent_per_client is unset or non-positive.
const DefaultMCPMaxConcurrentPerClient = 4

// validate reports the endpoint's configuration errors. It is a no-op while the
// endpoint is disabled: an unconfigured block on a kernel that never serves it
// is not a mistake, and failing a boot over one would be.
func (m MCPEndpointConfig) validate(grpcPort string) []string {
	if !m.Enabled {
		return nil
	}
	var errs []string
	if m.Port <= 0 || m.Port > 65535 {
		errs = append(errs, fmt.Sprintf(
			"server.mcp.port must be 1..65535 when server.mcp.enabled is true (got %d); "+
				"the endpoint is the deployment's public front door and no port is implied for it", m.Port))
	}
	if p, err := strconv.Atoi(strings.TrimSpace(grpcPort)); err == nil && p == m.Port {
		errs = append(errs, fmt.Sprintf(
			"server.mcp.port %d is server.port: the MCP endpoint gets its OWN listener (ADR-0126 D3) "+
				"and cannot share the gRPC plane's", m.Port))
	}
	if len(m.Clients) == 0 {
		errs = append(errs, "server.mcp.enabled requires at least one server.mcp.clients entry: "+
			"the bearer token is never optional (ADR-0126 D4), so an endpoint with no named client "+
			"could serve nobody")
	}
	seen := make(map[string]bool, len(m.Clients))
	for _, c := range m.Clients {
		name := strings.TrimSpace(c)
		if name == "" {
			errs = append(errs, "server.mcp.clients contains an empty name")
			continue
		}
		if seen[name] {
			errs = append(errs, fmt.Sprintf("server.mcp.clients lists %q twice", name))
		}
		seen[name] = true
	}
	return errs
}

// A2AEndpointConfig configures the kernel's Agent Card listener (ADR-0139
// D4/C2; internal/agentcard). It serves, read-only and unauthenticated:
//
//	/a2a/{agent_id}/.well-known/agent-card.json  the agent's kernel-signed card
//	/a2a/agents                                  the card index
//	/.well-known/jwks.json                       the card-signing public key
//
// DEFAULT DISABLED, like server.mcp: a listener opens because a deployment said
// so. It binds through the one listener policy (server.bind_address, TLS,
// timeouts; ADR-0130 D3).
type A2AEndpointConfig struct {
	// Enabled switches the listener on. Off: nothing in this block is read and
	// no signing key is loaded or minted.
	Enabled bool `json:"enabled"`
	// Port is the listener's TCP port. No implied default (the server.mcp rule).
	Port int `json:"port"`
	// PublicBaseURL is where clients reach THIS listener (e.g. behind a proxy).
	// Optional; when set, cards carry its JWKS URL as the JWS `jku` and the
	// index returns absolute card URLs.
	PublicBaseURL string `json:"public_base_url"`
	// Interfaces are the public A2A interfaces a deployment fronts its agents
	// with (card.supportedInterfaces). The kernel does NOT itself serve the A2A
	// task protocol, so the default is none and cards say so; list one only
	// when something at that URL serves it. url_template may hold {agent_id}.
	Interfaces []A2AInterfaceConfig `json:"interfaces"`
	// ProviderOrganization / ProviderURL become card.provider (A2A requires
	// both members, so both or neither).
	ProviderOrganization string `json:"provider_organization"`
	ProviderURL          string `json:"provider_url"`
	// DocumentationURL, optional, becomes card.documentationUrl.
	DocumentationURL string `json:"documentation_url"`
	// SigningKeyFile, when set, holds the Ed25519 card-signing key (hex seed;
	// created 0600 on first boot when absent). Empty: the key is the ADR-0101
	// named secret `a2a:card-signing-key` in the encrypted config store, minted
	// on first boot. The key is never logged.
	SigningKeyFile string `json:"signing_key_file"`
	// IncludeSystemAgents publishes cards for system agents too. Off: they are
	// kernel organs never assigned a user task.
	IncludeSystemAgents bool `json:"include_system_agents"`
}

// A2AInterfaceConfig is one public A2A interface (AgentInterface).
type A2AInterfaceConfig struct {
	URLTemplate     string `json:"url_template"`
	ProtocolBinding string `json:"protocol_binding"`
}

// A2AProtocolBindings are the A2A 1.0 protocol bindings.
var A2AProtocolBindings = []string{"JSONRPC", "GRPC", "HTTP+JSON"}

func (a A2AEndpointConfig) validate(grpcPort string, mcp MCPEndpointConfig) []string {
	if !a.Enabled {
		return nil
	}
	var errs []string
	if a.Port <= 0 || a.Port > 65535 {
		errs = append(errs, fmt.Sprintf("server.a2a.port must be 1..65535 when server.a2a.enabled is true (got %d)", a.Port))
	}
	if p, err := strconv.Atoi(strings.TrimSpace(grpcPort)); err == nil && p == a.Port {
		errs = append(errs, fmt.Sprintf("server.a2a.port %d is server.port: the card listener gets its own port", a.Port))
	}
	if mcp.Enabled && mcp.Port == a.Port {
		errs = append(errs, fmt.Sprintf("server.a2a.port %d is server.mcp.port", a.Port))
	}
	if (a.ProviderOrganization == "") != (a.ProviderURL == "") {
		errs = append(errs, "server.a2a.provider_organization and server.a2a.provider_url are set together or not at all "+
			"(A2A AgentProvider requires both)")
	}
	for i, it := range a.Interfaces {
		if strings.TrimSpace(it.URLTemplate) == "" {
			errs = append(errs, fmt.Sprintf("server.a2a.interfaces[%d].url_template is empty", i))
		}
		ok := false
		for _, b := range A2AProtocolBindings {
			ok = ok || it.ProtocolBinding == b
		}
		if !ok {
			errs = append(errs, fmt.Sprintf("server.a2a.interfaces[%d].protocol_binding %q must be one of %v",
				i, it.ProtocolBinding, A2AProtocolBindings))
		}
	}
	return errs
}

// A2AConfig is the OUTBOUND A2A side: remote agents the kernel registers.
type A2AConfig struct {
	// Endpoints are the remote A2A agents' base URLs. At boot the kernel fetches
	// each card (<endpoint>/.well-known/agent-card.json), applies the signature
	// policy and registers the agent as runtime a2a at trust tier third-party.
	// Empty: no remote agents (the default).
	Endpoints []string `json:"endpoints"`
	// SignaturePolicy is off | optional | required (default optional):
	//   - optional: a card signed by a trusted key must verify, else it is
	//     refused; an unsigned card, or one signed only by unknown keys, is
	//     registered with a WARN;
	//   - required: only cards that verify against a trusted key register;
	//   - off: signatures are ignored.
	// A broken signature by a trusted key is refused under optional AND required.
	SignaturePolicy string `json:"signature_policy"`
	// TrustedJWKSFile is a JSON Web Key Set (Ed25519 OKP or P-256 EC keys) of
	// the card signers this deployment trusts. A card's own `jku` is never
	// fetched: it is served by the party the signature is meant to check.
	TrustedJWKSFile string `json:"trusted_jwks_file"`
}

// DefaultA2ASignaturePolicy applies when a2a.signature_policy is unset.
const DefaultA2ASignaturePolicy = "optional"

func (a A2AConfig) validate() []string {
	var errs []string
	switch a.SignaturePolicy {
	case "off", "optional", "required":
	default:
		errs = append(errs, fmt.Sprintf("a2a.signature_policy %q must be off, optional or required", a.SignaturePolicy))
	}
	if a.SignaturePolicy == "required" && strings.TrimSpace(a.TrustedJWKSFile) == "" && len(a.Endpoints) > 0 {
		errs = append(errs, "a2a.signature_policy=required needs a2a.trusted_jwks_file: with no trusted key "+
			"every remote card would be refused")
	}
	for i, e := range a.Endpoints {
		u := strings.TrimSpace(e)
		if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			errs = append(errs, fmt.Sprintf("a2a.endpoints[%d] %q must be an http(s) URL", i, e))
		}
	}
	return errs
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

	// The ADR-0127 CL-2 contribution-lane knobs. All three use 0 ⇒ documented
	// fallback semantics (no DefaultConfig pin), so an explicit 0 and "unset"
	// behave identically and a committed tuning file cannot shadow them.
	//
	// WorkerCallTimeoutMs bounds one relayed contributed step (dispatch →
	// worker report). 0 ⇒ falls back to call_timeout_ms — the CL-1 behaviour
	// (one remote call's bound) preserved exactly when unset.
	WorkerCallTimeoutMs int `json:"worker_call_timeout_ms,omitempty"`
	// WorkerConsentTimeoutMs bounds a consent / "which machine?" prompt
	// awaiting a human answer on the initiating conversation surface (ADR-0127
	// D6/D7). 0 ⇒ 120000 (2 min). Fail-closed: an unanswered prompt DENIES.
	WorkerConsentTimeoutMs int `json:"worker_consent_timeout_ms,omitempty"`
	// WorkerParkDeadlineMs bounds how long a contributed step targeting an
	// offline machine stays parked awaiting its return (ADR-0127 D6). 0 ⇒
	// 86400000 (24h). Parked steps are memory-resident with the task lane —
	// a kernel restart drops them (the known phase-4 residual); expiry fails
	// the step visibly with a recorded decision, never silently.
	WorkerParkDeadlineMs int `json:"worker_park_deadline_ms,omitempty"`
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
	// DefaultToolPolicy applies to every advertised tool with no `tools` entry
	// (ADR-0139 D8). Absent, an unlisted tool FAILS CLOSED: dangerous, with
	// effects read+write+egress. Set it to mark a whole server read-only, e.g.
	// {"effects":["read"]}. Its `name` and `pricing` are ignored.
	DefaultToolPolicy *MCPToolConfig `json:"default_tool_policy,omitempty"`
	// Env names the kernel environment variables a stdio server may inherit, on
	// top of the OS essentials (ADR-0139 D8). Everything else is withheld; a
	// credential-shaped name passes only when listed here, and CAMBRIAN_* /
	// LANGFUSE_* never pass.
	Env []string `json:"env,omitempty"`

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
	// Effects declare the tool's effect classes (ADR-0086 closed set: read,
	// write, egress, spend, admin). Declared effects win over inference.
	Effects []string `json:"effects,omitempty"`
	Pricing struct {
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
				StepTimeoutMultiplier: 2.0,
				// 15s buffer / 10min plan ceiling (owner defaults, 2026-08-11:
				// promoted from the operating config — local models routinely
				// blow a 2-minute plan ceiling, and a timeout mid-plan wastes
				// everything already spent).
				StepTimeoutBaseBufferMs: 15000,
				// 3 min: comfortably fits one long generation on a slow local or
				// proxied model (the 2026-11-05 E2E saw a healthy 96 s step).
				StepTimeoutFloorMs: DefaultStepTimeoutFloorMs,
				// The fixed 120 s plan ceiling (owner decision 2026-10-31, ADR-0131
				// D7) is superseded by a progress-aware one (owner decision
				// 2026-11-05): a plan is cut when it goes idle, or at the 30 min
				// safety cap. A call in flight counts as progress, so ONE hung call is
				// bounded by the per-step deadline (step_timeout_* with its floor) and
				// the provider's own request timeout, not by the idle window; a
				// winner with no latency profile gets no step deadline and falls to
				// the 30 min cap.
				PlanIdleTimeoutMs:           DefaultPlanIdleTimeoutMs,
				PlanMaxDurationMs:           DefaultPlanMaxDurationMs,
				ContextGrowthK:              0.001,
				MaxRecursionDepth:           3,
				FallbackConfidenceThreshold: 0.4,
				FallbackEnabled:             true,
				MaxReplanAttempts:           2,
				MaxFanOutWidth:              64,
				MaxPartialContextBytes:      51200,
				PlanDriftDays:               7,
				ContextRefSnippetChars:      500,
				DependencyResultsMaxChars:   DefaultDependencyResultsMaxChars,
				BudgetExhaustionAlarmRate:   0.05,
				MinStepEnergy:               256,
				MaxStepEnergy:               32768,
			},
			Gatekeeper: GatekeeperConfig{
				GatekeeperMaxCandidates: 5,
				GatekeeperW1:            0.4,
				GatekeeperW2:            0.4,
				GatekeeperW3:            0.2,
				GatekeeperW4:            0.15,
				// ADR-0139 D6: posterior_lcb by owner decision (2026-11-05); legacy stays selectable.
				MeritMode:             MeritModePosteriorLCB,
				MeritPriorAlpha:       1,
				MeritPriorBeta:        1,
				MeritLCBQuantile:      0.1,
				MeritUnverifiedWeight: 0,
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
				CapabilityContract:     true,
				AgentPinning:           true, // honour an explicit "use agent X" directive
				RoutingTraceEnabled:    true,
				ExplorationRate:        0.05,
				ProposalTimeoutMs:      2000,
				DispatchCheapEnergyMax: 0,     // cheapest-competent branch OFF until the A/B lands (P0 = pure argmax)
				DispatchMeritFloor:     0.5,   // neutral prior; a candidate must be at least average to be "competent"
				YieldMaxDepth:          3,     // step agent → delegate → delegate's delegate; deeper is a decomposition the planner should own
				YieldMaxSubgoals:       8,     // matches the deleted YieldDriver's MaxDepth of 8 as the per-step budget
				PerCapabilityMerit:     false, // ROUTE-06 arm toggle; OFF = global merit
				LearnedScorer:          false, // ROUTE-07 arm toggle; OFF = hand-weighted GatekeeperScore
			},
			Capability: CapabilityConfig{
				CanonicalVocab:       false,               // ROUTE-04 arm toggle; OFF = declared strings verbatim
				CapabilityResolution: true,                // ADR-0100 D5 ladder; only fires for steps that declare requirements
				CapabilityAliases:    map[string]string{}, // authored, reviewed; add an entry only when a real step failed to route
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
				CircadianStaleDocWarnThreshold: 50,
				RememberDefaultActivation:      0.5,
				// ADR-0049 A2.2 experiential records: ON (owner decision 2026-07-28).
				ExperienceRecordsEnabled: true,
				ExperienceSurpriseFloor:  0.5,
			},
			Procedure: ProcedureConfig{
				ProcedureDeprecateBelow: 0.3, // ADR-0094 D4 retirement floor for stored routines
			},
			Retrieval: RetrievalConfig{
				RetrievalFloor:         0.2,
				RecallSpreadingEnabled: true, // ADR-0049 D10: spreading reads the Hebbian co_activated edges
				// 0 = no similarity floor (owner default 2026-08-11, promoted with
				// the whole tuned retrieval stack below: the floor cost recall and
				// the blend re-ranks what a floor used to cut).
				RecallSimilarityFloor:    0,
				HebbianEnabled:           true,
				HebbianLearningRate:      0.05,
				HebbianMaxWeight:         0.9,
				HebbianCoActivationFloor: 0.5,
				HebbianTopN:              5,
				HebbianDecayPerDay:       0.95,
				HebbianBaseWeight:        0.2,
				KG2RAGEnabled:            true, // ADR-0053 D3: KG²RAG expansion; opt-out via config.json
				AnchorConstraintEnabled:  true, // ADR-0053: document-local anchor promotion; opt-out via config.json
				StructureGraphEnabled:    true, // ADR-0060: structure-aware ingestion (docling parse -> section graph -> structure retrieval) is the DEFAULT chunking pipeline; opt-out via config.json

				// ── The tuned retrieval stack, promoted to shipped defaults ──
				// Owner decision 2026-08-11: the operating config's measured-best
				// values become the product defaults (they had lived only in a
				// gitignored local file). The experimental-off era of these flags
				// ended with that call; opt-OUT is via config or the store.
				// Owner decision 2026-11-02 (decisions/agentic-retrieval-default-off.md):
				// OFF until a multi-hop dataset shows it earns its cost. The ADR-0135 gate
				// measured a plain pass ~10x faster at equal accuracy within noise on
				// LongMemEval-S. Opting in restores decomposition (the flag below).
				AgenticRetrievalEnabled:       false,
				AgenticMaxHops:                4,
				AgenticDecomposeEnabled:       true, // grounded decomposition — the one agentic lever that measured a win
				AgenticSubQuestionConcurrency: 4,    // T-20261102: independent sub-questions retrieved in parallel
				AgenticPlannerModel:           "llm:deepseek",
				HybridSearchEnabled:           true, // dense+lexical RRF fusion (ADR-0054)
				HybridRRFK:                    60,
				HybridLexicalWeight:           1,
				HnswEfSearch:                  120,
				// Owner decision 2026-11-02 (ADR-0134 P2): ON by default. Every principal
				// read carries a scope predicate, which filters after an HNSW scan, so
				// without the iterative scan a selective scope underfills top_k silently.
				HnswIterativeScan:         "relaxed_order",
				QueryEntitySeedingEnabled: true,
				RecallTopK:                100,
				RecallOverFetch:           100,
				RerankerTopK:              100, // the TechQA global-rerank depth; reranker itself stays opt-in
				RerankerWeight:            0.5,
				BlendEnabled:              true, // ADR-0054 Stage-A blend
				BlendWeightCosine:         0.4,
				BlendWeightLexical:        0.2,
				BlendWeightCoherence:      0.05,
				BlendWeightConfidence:     0.1,
				// Owner decision 2026-11-03: recency 0. The pool-relative (min-max)
				// recency term cost retrieval recall on LongMemEval-S temporal +
				// knowledge-update (benchmark longmemeval-s-temporal-ku: weight 0 =
				// 195/211, recall 0.9749; 0.1 = 188-190, 0.9528; 0.3 = 164), and no
				// variant (decay curves incl.) beat 0. It was 0.1 for one day
				// (2026-11-02); memory.DefaultBlendWeights' 0.05 is only the all-zero
				// fallback. The ranking redesign is
				// T-20261102-research-and-redesign-stage-a-ranking-improve.
				BlendWeightRecency:        0,
				BlendRecencyMode:          "minmax",
				BlendRecencyHalfLifeHours: 2160, // 90 days; read in decay mode and by the calibrated Stage A
				// ADR-0136 Phase 0: all off. The two values below are read only
				// when their switch is on.
				LexicalSaturationK: 1.5, // memory.DefaultLexicalSaturationK; was 0.1 (T-20261103-adr-0136-d1-stage-a-calibrated-lowers-evidence)
				TemporalFitWeight:  0.2,

				KG2RAGMaxHops:     2,
				KG2RAGMaxExpanded: 40,
				KG2RAGMaxEntities: 30, // cap on entities walked per hop
				KG2RAGPerEntity:   10,
			},
			Workspace: WorkspaceConfig{
				WorkspaceMinFactCosine:  0.60,
				WorkspacePlanningSlots:  5,
				WorkspaceExecutionSlots: 5,
				UseGlobalWorkspace:      true,
			},
			Ingestion: IngestionConfig{
				// Owner defaults 2026-08-11: content-first evidence capture
				// (ADR-0105) and the deterministic no-LLM triplet extractor are
				// part of the product, not options a fresh install must discover.
				EvidenceCaptureEnabled:     true,
				KgExtractorEnabled:         true,
				EdgeExtractionBatchSize:    16,
				EdgeExtractionMaxIdleMs:    2000,
				EdgeExtractionQueueSize:    4096,
				EdgeExtractionLLMTimeoutMs: 300000,
				IngestionHTTPPort:          0, // disabled by default; set to e.g. 8080 to enable
				IngestionQueueSize:         1000,
				IngestionBatchSize:         5,
				IngestionWorkers:           5,
				IngestionBatchWaitMs:       1000,
				EventTimeFutureToleranceMs: 300000, // ADR-0134 Q4: 5 minutes
				// Owner decision 2026-11-02: read-your-writes for the KG lane is an
				// option, off by default; the benchmark turns it on.
				SyncIngestWaitForKG:       false,
				SyncIngestKGWaitTimeoutMs: 30000,
				// ADR-0136 D4: same option for the fact layer, off by default.
				SyncIngestWaitForFacts:       false,
				SyncIngestFactsWaitTimeoutMs: 120000,
				// ADR-0138 D2: writer-qualified, binary-aware document ids.
				DocumentIdentity: DocumentIdentityV2,
				// ADR-0136 A3.1: descriptive source-metadata caps (agent-plane ingest).
				SourceKindMaxChars:   DefaultSourceKindMaxChars,
				SourceTitleMaxChars:  DefaultSourceTitleMaxChars,
				SourceAuthorMaxChars: DefaultSourceAuthorMaxChars,
				PartSpeakerMaxChars:  DefaultPartSpeakerMaxChars,
				// T-20261102-kg-extractor-cold-start-drops-the-first-kg: absorb a
				// cold kg_extractor boot instead of dropping the first batches.
				KgExtractorCallAttempts:   3,
				KgExtractorRetryBackoffMs: 2000,
				KgTripletsMaxRequeues:     1,
				KgTripletsLLMFormat:       KgTripletsFormatCompletion,
				// Drop OCR/layout junk only from Docling output; chat and other text
				// ingests keep every leaf (T-20261102-ingestion-structure-junk-filter-silently-drops).
				StructureJunkFilterBackends: []string{"docling"},
			},
			Hippocampus: HippocampusConfig{
				HippocampusPolicies: map[string]domain.HippocampusPolicy{
					"codegen":   {SimilarityThreshold: 0.92, ConfidenceFloor: 0.85, MaxAgeHours: 24},
					"cognitive": {SimilarityThreshold: 0.85, ConfidenceFloor: 0.70, MaxAgeHours: 168},
					"tool":      {SimilarityThreshold: 0.80, ConfidenceFloor: 0.60, MaxAgeHours: 720},
					"research":  {SimilarityThreshold: 0.88, ConfidenceFloor: 0.75, MaxAgeHours: 72},
					"default":   {SimilarityThreshold: 0.85, ConfidenceFloor: 0.70, MaxAgeHours: 168},
					// The ADR-0029 "episodic" policy was removed 2026-10-31 (owner
					// decision): its only consumer, the WorkspaceStage episodic lane, was
					// deleted with its writer on 2026-07-18. An operator file that still
					// declares `hippocampus_policies.episodic` loads fine — the map takes
					// any name — it simply has no reader.
				},
				HippocampusDefaultPolicy: "default",
			},
			Agents: AgentsConfig{
				// AgentEnvPassthrough deliberately stays EMPTY (owner rule
				// 2026-08-11): a knob that reaches agents through the kernel's
				// own environment depends on .env, and knobs never do. The one
				// entry the rig used it for (CAMBRIAN_TOOL_MENU_K) became the
				// real config key execution.tools.tool_menu_k, which the kernel
				// injects into agent environments itself. The seam remains for
				// genuine deployment extras.
				DaemonRestartMaxAttempts:   5, // REACT-04: auto-restart on, crash-loop → quarantine
				DaemonRestartWindowSeconds: 300,
				DaemonRestartBaseBackoffMs: 1000,
				DaemonRestartMaxBackoffMs:  30000,
				// T-20261102-kg-extractor-cold-start-drops-the-first-kg: 10 s killed
				// the kg_extractor (spaCy load) while the kernel booted its agents.
				AgentBootTimeoutMs: 30000,
				// ADR-0139 D12 subagent limits (depth 1 and concurrency 4 are the
				// ADR's and the proto's stated defaults).
				SubagentMaxDepth:             1,
				SubagentMaxConcurrency:       4,
				SubagentTreeMaxToolRounds:    64,
				SubagentDefaultMaxToolRounds: 16,
				SubagentDefaultTimeoutMs:     300000,
				SubagentResultMaxChars:       16000,
				SubagentWithheldTools: []string{
					"clarify", "clarify_*", "ask_user*",
					"memory_write*", "memory_note*", "propose_memory_note", "ingest_memory*",
					"send_message*", "message_*", "*_send_message", "notify*",
				},
				SubagentMemoryWriteTools: []string{"memory_write*", "memory_note*", "propose_memory_note"},
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
				// TracePipelinePayloads deliberately NOT promoted with the rest of
				// the 2026-08-11 defaults: its own doc comment names the reason —
				// item values are real deployment data, and a default that ships
				// them to the operator plane turns a diagnostic view into an
				// exfiltration surface. The rig keeps it on via tuning.local.json.
			},
			Chat: ChatConfig{
				// ADR-0084 conversation lane ON by default (owner 2026-08-11):
				// a kernel with chat_pool_size 0 hides the whole chat surface.
				ChatPoolSize:      2,
				ChatPoolQueueSize: 8,
				ChatPoolAgentID:   "chat_agent",
			},
			Pipelines: PipelinesConfig{
				DrainerEnabled: true, // owner default 2026-08-11
			},
			Graph: GraphConfig{
				DecayFactor: 0.75,
				MaxDepth:    3,
				EnergyFloor: 0.15,
			},
			// ADR-0136 D4/D5: the fact layer is off; the tunables below are read only
			// when a switch is on.
			Facts: FactsConfig{
				QueueSize:            1024,
				Workers:              2,
				LLMTimeoutMs:         120000,
				LLMTimeoutPerKCharMs: 2500,
				LLMTimeoutMaxMs:      300000,
				ParseRetries:         1,
				MaxFactsPerDocument:  64,
				MaxPromptChars:       48000,
				MaxCallsPerDocument:  8,
				MaxFactsPerSource:    512,
				LaneTopK:             20,
				LaneScopedTopK:       20,
				FactMatchWeight:      0.2,
				CurrentFactWeight:    0.1,
				SingleAttributes:     []string{},
				MultiAttributes:      []string{},
				FuzzyCandidates:      10,
				CardinalityMinVotes:  3,
				// T-20261103-fact-backfill-processes-each-principal-s: 1 = the serial
				// ordered backfill, unchanged.
				BackfillGroupConcurrency: 1,
			},
			// ADR-0139 D13: proposals on (inert until approved), curator off.
			Skills: DefaultSkillsConfig(),
		},
		Telemetry: TelemetryConfig{
			TraceSamplingRate:            1.0,
			MetricsExportIntervalSeconds: 10,
		},
		// A fresh install now embeds out of the box (owner default 2026-08-11:
		// promoted from the operating embedder.json). Before this the default
		// embedder was EMPTY and dimensions 0 — which the vector store refuses
		// to boot on — so a kernel without an embedder.json could not start.
		// bge-large is asymmetric: the query prefix goes on QUERY text only.
		Embedder: EmbedderConfig{
			Provider:    "ollama",
			Model:       "bge-large",
			Endpoint:    "http://localhost:11434",
			Dimensions:  1024,
			TimeoutMs:   10000,
			QueryPrefix: "Represent this sentence for searching relevant passages: ",
		},
		LLMProvider: LLMProviderConfig{
			// The circuit-breaker values LoadConfig was already filling post-merge,
			// stated where every other default lives; and the measured-safe global
			// concurrency cap for local endpoints (owner default 2026-08-11).
			// Deliberately NO default generator id and NO generator list here — a
			// default id would boot-brick any deployment declaring generators
			// without that id, and generators are deployment facts, not defaults.
			Health:         HealthConfig{FailureThreshold: 3, CooldownMs: 30000},
			MaxConcurrency: 3,
		},
		MCP: MCPConfig{
			CallTimeoutMs: 60000, // owner default 2026-08-11; 0 meant NO per-call deadline
		},
		// ADR-0139 D4: verify remote card signatures when they are by a trusted key.
		A2A: A2AConfig{SignaturePolicy: DefaultA2ASignaturePolicy},
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
	// The boot-time guard defaults to the historical behaviour (apply, nothing
	// protected); an operator protects a database by naming it in
	// database.migrations.protected_dbnames (ADR-0064 amendment, 2026-11-04).
	cfg.Database.Migrations.Mode = "auto"
	// ADR-0061 A2.8: compact the bbolt file at boot when it is over 256 MiB and
	// more than half free pages.
	cfg.Storage.CompactMinBytes = 256 << 20
	cfg.Storage.CompactFreeRatio = 0.5
	// ADR-0126 D3: the inbound MCP endpoint is DEFAULT DISABLED and has no
	// implied port. Only the storm-control cap carries a default, because a
	// deployment that switches the endpoint on should not also have to think
	// about concurrency before it is safe.
	cfg.Server.MCP.MaxConcurrentPerClient = DefaultMCPMaxConcurrentPerClient
	cfg.Server.RateLimit = DefaultRateLimitConfig()
	return cfg
}

// LoadConfig reads configuration from a 12-layer pipeline (lowest → highest
// priority) and returns the merged *Config. Layer 11 is the ADR-0101 embedded
// store, which is where durable operator writes land; it was added after this
// comment first said 11, so the count is stated here to match the numbered
// layers below rather than the history. All secondary paths
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
// layered override — including nested execution blocks in tuning.local.json —
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
	// All secondary paths are derived from the directory of the primary `path`
	// (FileLayers), not from the process CWD. This keeps the layering
	// deterministic across invocation contexts (tests, systemd, dev shells).
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

	// Layers 2-10: the JSON files, in the precedence order FileLayers states —
	// tuning.json (committed curated starter, ADR-0024 §Curated tuning.json) →
	// tuning.local.json → the primary `path` (config.json; ADR-0057 ships
	// config.example.json) → config.local.json → embedder{,.local}.json →
	// providers{,.local}.json → mcp.json. Each is skipped when absent. A later
	// file wins per key, so e.g. providers.json's `llm_provider` block beats one in
	// config.json.
	//
	// WARNING (mcp.json): an empty mcp.json ({"mcp": {}}) silently wipes the `mcp`
	// block from config.json because Koanf merges per-key. Operators who don't use
	// MCP should simply not create mcp.json.
	//
	// The SAME list drives the ADR-0101 D8 write-time resolver
	// (ResolveFileSetting), so the merge and "which file supplies this key" share
	// one statement of precedence.
	for _, layer := range FileLayers(path) {
		loadIfPresent(k, layer.Path)
		t.claim(scratchFile(layer.Path), layer.Source)
	}

	// Layer 11 (ADR-0101 D1): the embedded store — durable operator writes.
	// Above every file so a console edit survives a restart and outranks the
	// shipped defaults; below the environment so a deployment still wins.
	//
	// Generators are the exception to "the store is one more layer" (ADR-0101 D9):
	// they are stored per id and merged BY ID over the file list after unmarshal
	// (storedGens below), because a stored list would replace the file list
	// wholesale and hide every generator added to a file.
	var storedGens map[string]StoredGenerator
	if store != nil {
		if overrides, err := store.Overrides(); err == nil && len(overrides) > 0 {
			gens, rest, legacy := SplitGeneratorOverrides(overrides)
			if legacy {
				// Normally migrated (persisted) at boot by app.Run; a read-only
				// reader (CLI) still sees the right merge from the in-memory split.
				slog.Info("config store holds the pre-ADR-0101-D9 generator list; merging it per id",
					slog.Int("generators", len(gens)))
			}
			storedGens = gens
			if len(gens) > 0 {
				t.prov[GeneratorsKey] = SourceStore
			}
			if b, mErr := stdjson.Marshal(expand(rest)); mErr == nil && len(rest) > 0 {
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
		// A deprecated key's variable keeps working as its successor
		// (CAMBRIAN_EXECUTION__PLAN__PLAN_TIMEOUT_MS → plan_max_duration_ms), per
		// layer like the file and store migrations (deprecatedExecutionKey).
		s = deprecatedEnvKey(s, os.LookupEnv)
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

	// ADR-0101 D9: stored generators merge by id over the file layers' list. An
	// env var pinning the whole list (not a realistic deployment, but the D1 rule)
	// still wins: the merge is skipped when the env layer claimed the key.
	if len(storedGens) > 0 && !strings.HasPrefix(t.prov[GeneratorsKey], SourceEnvPrefix) {
		cfg.LLMProvider.Generators = MergeGenerators(cfg.LLMProvider.Generators, storedGens)
		t.prov[GeneratorsKey] = SourceStore
	}

	// ADR-0042 defaults for the llm_provider block.
	ApplyGeneratorDefaults(cfg.LLMProvider.Generators)
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
	errs = append(errs, c.Database.Migrations.validate()...)
	// ADR-0042: validate the llm_provider + embedder blocks (no-op until configured).
	errs = append(errs, c.LLMProvider.validate(c.Embedder)...)
	// ADR-0126 D3/D4: validate the inbound MCP endpoint (no-op until enabled).
	errs = append(errs, c.Server.MCP.validate(c.Server.Port)...)
	// ADR-0139 D4: the card listener (no-op until enabled) and remote agents.
	errs = append(errs, c.Server.A2A.validate(c.Server.Port, c.Server.MCP)...)
	errs = append(errs, c.A2A.validate()...)
	errs = append(errs, c.Server.RateLimit.validate()...)

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
	if c.Plan.PlanMaxDurationMs < 1000 {
		errs = append(errs, "plan_max_duration_ms must be >= 1000")
	}
	if c.Plan.PlanIdleTimeoutMs < 0 {
		errs = append(errs, "plan_idle_timeout_ms must be >= 0 (0 disables the idle watchdog)")
	}
	if c.Plan.StepTimeoutFloorMs < 0 {
		errs = append(errs, "step_timeout_floor_ms must be >= 0 (0 disables the floor)")
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
	switch c.Gatekeeper.ActiveMeritMode() {
	case MeritModeLegacy, MeritModePosteriorLCB, MeritModeThompson:
	default:
		errs = append(errs, fmt.Sprintf("gatekeeper.merit_mode %q must be one of legacy, posterior_lcb, thompson", c.Gatekeeper.MeritMode))
	}
	// The posterior parameters are validated even under legacy, so a layer that
	// prepares a posterior arm fails at load rather than on the flip.
	// 0 reads as the default (prior 1, quantile 0.1), as in the scorer.
	if c.Gatekeeper.MeritPriorAlpha < 0 || c.Gatekeeper.MeritPriorBeta < 0 {
		errs = append(errs, "gatekeeper.merit_prior_alpha and merit_prior_beta must be > 0 (0 = default 1)")
	}
	if c.Gatekeeper.MeritLCBQuantile < 0 || c.Gatekeeper.MeritLCBQuantile >= 1 {
		errs = append(errs, "gatekeeper.merit_lcb_quantile must be in (0, 1) (0 = default 0.1)")
	}
	if c.Gatekeeper.MeritUnverifiedWeight < 0 || c.Gatekeeper.MeritUnverifiedWeight > 1 {
		errs = append(errs, "gatekeeper.merit_unverified_weight must be in [0, 1]")
	}
	if c.Plan.MaxPlanCost < 0 {
		errs = append(errs, "max_plan_cost must be >= 0")
	}
	if c.Retrieval.AgenticSubQuestionConcurrency < 0 {
		errs = append(errs, "agentic_subquestion_concurrency must be >= 0 (<=1 retrieves sub-questions sequentially)")
	}
	if c.Routing.ExplorationRate < 0 || c.Routing.ExplorationRate > 1 {
		errs = append(errs, "exploration_rate must be in [0, 1]")
	}
	if c.Session.MaxSessionTokens < 0 {
		errs = append(errs, "max_session_tokens must be >= 0 (0 disables the ADR-0131 spend ceiling)")
	}
	if c.Session.SessionTTLDays < 0 {
		errs = append(errs, "session_ttl_days must be >= 0")
	}
	if c.Plan.PlanDriftDays < 0 {
		errs = append(errs, "plan_drift_days must be >= 0")
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
	switch c.Retrieval.BlendRecencyMode {
	case "", "minmax":
	case "decay":
		if c.Retrieval.BlendRecencyHalfLifeHours <= 0 {
			errs = append(errs, "retrieval.blend_recency_half_life_hours must be > 0 when blend_recency_mode is decay")
		}
	default:
		errs = append(errs, fmt.Sprintf("retrieval.blend_recency_mode must be minmax or decay (got %q)", c.Retrieval.BlendRecencyMode))
	}
	// ADR-0136: a value the consumer would silently replace is refused instead.
	if c.Retrieval.StageACalibrated && c.Retrieval.LexicalSaturationK <= 0 {
		errs = append(errs, "retrieval.lexical_saturation_k must be > 0 when stage_a_calibrated is on")
	}
	if c.Retrieval.StageACalibrated && c.Retrieval.BlendRecencyHalfLifeHours <= 0 {
		errs = append(errs, "retrieval.blend_recency_half_life_hours must be > 0 when stage_a_calibrated is on")
	}
	if c.Retrieval.TemporalFitWeight < 0 {
		errs = append(errs, "retrieval.temporal_fit_weight must be >= 0")
	}
	// ADR-0136 D4/D5: the fact layer's tunables. Each is checked only when the switch
	// that reads it is on, as the consumer would otherwise silently replace it.
	if f := c.Facts; f.ExtractEnabled {
		if f.QueueSize <= 0 {
			errs = append(errs, "facts.queue_size must be > 0 when extract_enabled is on")
		}
		if f.Workers <= 0 {
			errs = append(errs, "facts.workers must be > 0 when extract_enabled is on")
		}
		if f.LLMTimeoutMs <= 0 {
			errs = append(errs, "facts.llm_timeout_ms must be > 0 when extract_enabled is on")
		}
		if f.LLMTimeoutPerKCharMs < 0 {
			errs = append(errs, "facts.llm_timeout_per_kchar_ms must be >= 0 (0 = a flat llm_timeout_ms)")
		}
		if f.LLMTimeoutMaxMs < f.LLMTimeoutMs {
			errs = append(errs, "facts.llm_timeout_max_ms must be >= llm_timeout_ms when extract_enabled is on")
		}
		if f.ParseRetries < 0 || f.ParseRetries > 3 {
			errs = append(errs, "facts.parse_retries must be in [0, 3]")
		}
		if f.MaxFactsPerDocument <= 0 {
			errs = append(errs, "facts.max_facts_per_document must be > 0 when extract_enabled is on")
		}
		if f.MaxPromptChars < 2000 {
			errs = append(errs, "facts.max_prompt_chars must be >= 2000 when extract_enabled is on")
		}
		if f.FuzzyCandidates < 0 {
			errs = append(errs, "facts.fuzzy_candidates must be >= 0 (0 = exact supersession only)")
		}
		if f.MaxCallsPerDocument < 0 {
			errs = append(errs, "facts.max_calls_per_document must be >= 0 (0 = no cap)")
		}
		if f.MaxFactsPerSource <= 0 {
			errs = append(errs, "facts.max_facts_per_source must be > 0 when extract_enabled is on")
		}
	}
	if f := c.Facts; f.LaneEnabled && f.LaneTopK <= 0 {
		errs = append(errs, "facts.lane_top_k must be > 0 when lane_enabled is on")
	}
	if c.Facts.LaneScopedDocuments < 0 {
		errs = append(errs, "facts.lane_scoped_documents must be >= 0 (0 = off)")
	}
	if f := c.Facts; f.LaneScopedDocuments > 0 && f.LaneScopedTopK <= 0 {
		errs = append(errs, "facts.lane_scoped_top_k must be > 0 when lane_scoped_documents > 0")
	}
	if c.Facts.BackfillGroupConcurrency < 0 {
		errs = append(errs, "facts.backfill_group_concurrency must be >= 0 (0 and 1 = the serial ordered backfill)")
	}
	if c.Facts.FactMatchWeight < 0 || c.Facts.CurrentFactWeight < 0 {
		errs = append(errs, "facts.fact_match_weight and facts.current_fact_weight must be >= 0")
	}
	for _, a := range c.Facts.SingleAttributes {
		if strings.TrimSpace(a) == "" {
			errs = append(errs, "facts.single_attributes must not contain an empty name")
			break
		}
	}
	for _, a := range c.Facts.MultiAttributes {
		if strings.TrimSpace(a) == "" {
			errs = append(errs, "facts.multi_attributes must not contain an empty name")
			break
		}
	}
	errs = append(errs, c.Hooks.validate()...)
	errs = append(errs, c.MemoryNotes.validate()...)
	errs = append(errs, c.StepOutput.validate()...)
	errs = append(errs, c.Skills.validate()...)
	if c.Facts.CardinalityMinVotes < 0 {
		errs = append(errs, "facts.cardinality_min_votes must be >= 0 (0 = the default 3)")
	}
	if c.Ingestion.SyncIngestFactsWaitTimeoutMs < 0 {
		errs = append(errs, "ingestion.sync_ingest_facts_wait_timeout_ms must be >= 0")
	}
	for _, f := range []struct {
		name string
		v    int
	}{
		{"source_kind_max_chars", c.Ingestion.SourceKindMaxChars},
		{"source_title_max_chars", c.Ingestion.SourceTitleMaxChars},
		{"source_author_max_chars", c.Ingestion.SourceAuthorMaxChars},
		{"part_speaker_max_chars", c.Ingestion.PartSpeakerMaxChars},
	} {
		if f.v < 0 {
			errs = append(errs, fmt.Sprintf("ingestion.%s must be >= 0 (0 = the default; got %d)", f.name, f.v))
		}
	}
	switch c.Retrieval.HnswIterativeScan {
	case "", "off", "relaxed_order", "strict_order":
	default:
		errs = append(errs, fmt.Sprintf("retrieval.hnsw_iterative_scan must be one of off, relaxed_order, strict_order (got %q)", c.Retrieval.HnswIterativeScan))
	}
	switch c.Ingestion.KgTripletsLLMFormat {
	case "", KgTripletsFormatCompletion, KgTripletsFormatStructured:
	default:
		errs = append(errs, fmt.Sprintf("ingestion.kg_triplets_llm_format must be %q or %q (got %q)", KgTripletsFormatCompletion, KgTripletsFormatStructured, c.Ingestion.KgTripletsLLMFormat))
	}
	switch c.Ingestion.DocumentIdentity {
	case "", DocumentIdentityV2, DocumentIdentityLegacy:
	default:
		errs = append(errs, fmt.Sprintf("ingestion.document_identity must be %q or %q (got %q)", DocumentIdentityV2, DocumentIdentityLegacy, c.Ingestion.DocumentIdentity))
	}
	for _, b := range c.Ingestion.StructureJunkFilterBackends {
		switch b {
		case "docling", "markdown", "text":
		default:
			errs = append(errs, fmt.Sprintf("ingestion.structure_junk_filter_backends: unknown backend %q (valid: docling, markdown, text)", b))
		}
	}
	if c.Retrieval.HnswMaxScanTuples < 0 {
		errs = append(errs, "retrieval.hnsw_max_scan_tuples must be >= 0")
	}
	if c.Hippocampus.HippocampusDefaultPolicy != "" {
		if _, ok := c.Hippocampus.HippocampusPolicies[c.Hippocampus.HippocampusDefaultPolicy]; !ok {
			errs = append(errs, fmt.Sprintf("hippocampus_default_policy %q is not a key in hippocampus_policies", c.Hippocampus.HippocampusDefaultPolicy))
		}
	}

	if len(errs) > 0 {
		return &ConfigError{
			Field:   "validation",
			Message: strings.Join(errs, "; "),
		}
	}
	return nil
}
