package domain

// AgentProfile holds the derived Merit state for an agent, computed by the
// ProfileAggregator from raw TaskEvent records. The Gatekeeper reads only this
// derived state — never the raw events — during Merit ranking.
//
// The primary key in pgvector is (AgentID, SourceHash). A new SourceHash causes
// a fresh Interview; Merit is seeded from the previous version's history with
// trust decay proportional to the embedding distance between release notes.
type AgentProfile struct {
	AgentID                    string  `json:"agent_id,omitempty"`
	SourceHash                 string  `json:"source_hash,omitempty"`
	SuccessRate                float64 `json:"success_rate,omitempty"`
	TrustScore                 float64 `json:"trust_score,omitempty"`
	NetworkLatencyMedianMs     int     `json:"network_latency_median_ms,omitempty"`
	ComputationLatencyMedianMs int     `json:"computation_latency_median_ms,omitempty"`
	ContextGrowthBytesMedian   int      `json:"context_growth_bytes_median,omitempty"`
	Provisional                bool     `json:"provisional,omitempty"`
	// RecentVerifierIDs holds the last VerifierRecencyWindow verifier agent IDs
	// that verified this agent's outputs. Used by VerifierPool.Select to enforce
	// verifier diversity (D4).
	RecentVerifierIDs []string `json:"recent_verifier_ids,omitempty"`
	// ModelMetrics holds per-model token and cost tracking for TraitModel agents.
	ModelMetrics *ModelMetrics `json:"model_metrics,omitempty"`
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
