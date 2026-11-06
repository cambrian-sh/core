package domain

// AgentProfile holds the derived Merit state for an agent, computed by the
// ProfileAggregator from raw TaskEvent records. The Gatekeeper reads only this
// derived state — never the raw events — during Merit ranking.
//
// The primary key in pgvector is (AgentID, SourceHash). A new SourceHash starts a
// fresh profile on the neutral prior at registration; nothing is inherited from the
// previous version (the ADR-0002 D2 / ADR-0006 D5 release-notes merit decay was
// deleted with the LLM interview on 2026-11-05,
// T-20261105-delete-the-llm-agent-interview-now-owner). Rows written before then may
// still carry a `release_notes` key; it is ignored on read.
type AgentProfile struct {
	AgentID                    string  `json:"agent_id,omitempty"`
	SourceHash                 string  `json:"source_hash,omitempty"`
	SuccessRate                float64 `json:"success_rate,omitempty"`
	TrustScore                 float64 `json:"trust_score,omitempty"`
	NetworkLatencyMedianMs     int     `json:"network_latency_median_ms,omitempty"`
	ComputationLatencyMedianMs int     `json:"computation_latency_median_ms,omitempty"`
	ContextGrowthBytesMedian   int     `json:"context_growth_bytes_median,omitempty"`
	Provisional                bool    `json:"provisional,omitempty"`
	// RecentVerifierIDs holds the last VerifierRecencyWindow verifier agent IDs
	// that verified this agent's outputs. Used by VerifierPool.Select to enforce
	// verifier diversity (D4).
	RecentVerifierIDs []string `json:"recent_verifier_ids,omitempty"`
	// ModelMetrics holds per-model token and cost tracking for TraitModel agents.
	ModelMetrics *ModelMetrics `json:"model_metrics,omitempty"`
	// CapabilityStats holds capability-scoped success/trust (ROUTE-06 / ADR-0069),
	// keyed by canonical capability tag. Merit reads the tag-scoped record for a step's
	// required capability, falling back to the global SuccessRate/TrustScore above when
	// the tag has no history.
	CapabilityStats map[string]CapabilityStat `json:"capability_stats,omitempty"`
	// Outcomes is the agent's global outcome evidence (ADR-0139 D6), over every
	// step event regardless of capability. The posterior merit modes read it for a
	// step that declares no required capability; a step that does reads the
	// capability-keyed CapabilityStats[cap].Outcomes instead.
	Outcomes OutcomeEvidence `json:"outcomes,omitzero"`
}

// OutcomeEvidence is the raw evidence behind an ADR-0139 D6 Beta posterior.
//
// It stores EVIDENCE, not α/β: the prior (execution.gatekeeper.merit_prior_alpha /
// merit_prior_beta) and the weight of unverified outcomes (merit_unverified_weight)
// are applied at scoring time, so changing them takes effect on the next dispatch
// without re-aggregating, and a stored row never bakes in a prior. The posterior is
//
//	α = prior_α + VerifiedSuccess + unverified_weight·Unverified
//	β = prior_β + VerifiedFailure
//
// A verified event contributes its verifier score fractionally: VerifiedSuccess
// += score, VerifiedFailure += 1 − score (a pass/fail verifier reduces to counts).
// This is the standard fractional Bernoulli update and avoids re-binarising a
// graded score at 0.5, the distortion T-20261104-merit-math-distorts-routing named.
// An unverified event is a step that returned without error and that no verifier
// has scored; it moves the posterior only when merit_unverified_weight > 0
// (default 0), and only toward success, because a failed step writes no event.
type OutcomeEvidence struct {
	VerifiedSuccess float64 `json:"verified_success,omitempty"`
	VerifiedFailure float64 `json:"verified_failure,omitempty"`
	Unverified      int     `json:"unverified,omitempty"`
}

// IsZero reports whether there is no evidence at all.
func (o OutcomeEvidence) IsZero() bool {
	return o.VerifiedSuccess == 0 && o.VerifiedFailure == 0 && o.Unverified == 0
}

// CapabilityStat is an agent's success/trust for a single capability tag (ROUTE-06).
type CapabilityStat struct {
	SuccessRate float64 `json:"success_rate"`
	TrustScore  float64 `json:"trust_score"`
	// SampleCount is the number of verified events behind this stat (for shrinkage/fallback).
	SampleCount int `json:"sample_count"`
	// Outcomes is the capability-keyed ADR-0139 D6 evidence (verified and, when
	// counted, unverified events stamped with this capability). The legacy fields
	// above are unchanged by it.
	Outcomes OutcomeEvidence `json:"outcomes,omitzero"`
}

// ModelMetrics tracks token usage and cost for LLM inference providers.
type ModelMetrics struct {
	PromptTokensTotal     int64   `json:"prompt_tokens_total,omitempty"`
	CompletionTokensTotal int64   `json:"completion_tokens_total,omitempty"`
	EstimatedCostTotal    float64 `json:"estimated_cost_total,omitempty"`
	AvgCostPerTask        float64 `json:"avg_cost_per_task,omitempty"`
}

// ContextGrowthPenalty returns k * growthBytes as a float64 penalty value.
// It is a pure function shared by Gatekeeper and DAGExecutor.
func ContextGrowthPenalty(growthBytes int, k float64) float64 {
	return float64(growthBytes) * k
}
