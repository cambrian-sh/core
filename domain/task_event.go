package domain

import "time"

// TaskEventWriter persists a TaskEvent after each DAG step completes.
// Writes are best-effort; implementations must be safe for concurrent use.
type TaskEventWriter interface {
	WriteTaskEvent(event TaskEvent) error
}

// TaskEventReadWriter extends TaskEventWriter with a read-back method so that
// VerificationWorker can update an existing event in place.
type TaskEventReadWriter interface {
	TaskEventWriter
	ReadTaskEvent(taskID string) (*TaskEvent, error)
}

// TaskEvent is the raw per-task record written to bbolt after every verified task
// completion. It is the source of truth for all Merit metric computation.
// The ProfileAggregator reads these records to derive AgentProfile metrics.
// The Gatekeeper never reads TaskEvent directly.
type TaskEvent struct {
	TaskID               string    `json:"task_id,omitempty"`
	AgentID              string    `json:"agent_id,omitempty"`
	SourceHash           string    `json:"source_hash,omitempty"`
	BidConfidence        float64   `json:"bid_confidence,omitempty"`
	VerifierScore        float64   `json:"verifier_score,omitempty"`
	NetworkLatencyMs     int       `json:"network_latency_ms,omitempty"`
	ComputationLatencyMs int       `json:"computation_latency_ms,omitempty"`
	ContextGrowthBytes   int       `json:"context_growth_bytes,omitempty"`
	PromptTokens         int       `json:"prompt_tokens,omitempty"`
	CompletionTokens     int       `json:"completion_tokens,omitempty"`
	TotalTokens          int       `json:"total_tokens,omitempty"`
	EstimatedCost        float64   `json:"estimated_cost,omitempty"`
	Timestamp            time.Time `json:"timestamp,omitempty"`
	Verified             bool      `json:"verified,omitempty"`
	// Capability is the (first) required capability tag this step was routed for
	// (ROUTE-06 / ADR-0069). Empty when the step declared none. Powers per-capability
	// merit aggregation.
	Capability string `json:"capability,omitempty"`
	BudgetOverrun        bool      `json:"budget_overrun,omitempty"`      // server-authoritative: ActualTokensUsed > TokenLimit (ADR-0018)
	FallbackModelUsed    bool      `json:"fallback_model_used,omitempty"` // health cache circuit breaker engaged (ADR-0018)
	ActualModelID        string    `json:"actual_model_id,omitempty"`     // the TraitModel that actually served the call (ADR-0018)
	// Mechanism records which resource-selection mechanism bound this step:
	// "auction" or "efe" (ADR-0037). The A/B benchmark harness partitions all
	// falsification metrics by this field without external bookkeeping.
	Mechanism string `json:"mechanism,omitempty"`
}
