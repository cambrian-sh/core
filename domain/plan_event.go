package domain

import (
	"context"
	"time"
)

// PlanOutcome describes the final status of a plan execution.
type PlanOutcome string

const (
	PlanOutcomeSuccess         PlanOutcome = "success"
	PlanOutcomePartial         PlanOutcome = "partial"          // completed with errors, no replan possible
	PlanOutcomeReplanExhausted PlanOutcome = "replan_exhausted" // replanned but still failed
	PlanOutcomeBudgetExceeded  PlanOutcome = "budget_exceeded"
)

// PlanEvent aggregates per-step TaskEvents into a plan-level record.
// It is written once per DAGExecutor.Execute/ExecuteFrom call.
type PlanEvent struct {
	PlanID                string      `json:"plan_id"`
	Subject               string      `json:"subject,omitempty"`
	StepCount             int         `json:"step_count"`
	Outcome               PlanOutcome `json:"outcome"`
	TotalPromptTokens     int         `json:"total_prompt_tokens"`
	TotalCompletionTokens int         `json:"total_completion_tokens"`
	TotalTokens           int         `json:"total_tokens"`
	TotalEstimatedCost    float64     `json:"total_estimated_cost"`
	ReplanCount           int         `json:"replan_count"`
	FailedStepIndex       int         `json:"failed_step_index,omitempty"`
	FallbackCount         int         `json:"fallback_count"`
	BudgetOverrunCount    int         `json:"budget_overrun_count"`
	StartTime             time.Time   `json:"start_time"`
	EndTime               time.Time   `json:"end_time"`
	DurationMs            int64       `json:"duration_ms"`
	RetrievalSessionID    string      `json:"retrieval_session_id,omitempty"`
	PlannerPromptVersion  string      `json:"planner_prompt_version,omitempty"` // PROMPTREQ: 8-char hash of the static planner/replan prompt template
	CachePolicy           string      `json:"cache_policy,omitempty"`           // ADR-0027: policy name emitted by Planner for Hippocampus retrieval
}

// PlanEventWriter persists a PlanEvent after plan execution completes.
type PlanEventWriter interface {
	WritePlanEvent(ctx context.Context, event PlanEvent) error
}

// ContradictionResolution captures the feature vector of an LLM-arbitrated contradiction.
type ContradictionResolution struct {
	ResolutionID           string    `json:"resolution_id"`
	DocAID                 string    `json:"doc_a_id"`
	DocBID                 string    `json:"doc_b_id"`
	WinnerID               string    `json:"winner_id"`
	DocAAS                 float64   `json:"doc_a_as"`
	DocBAS                 float64   `json:"doc_b_as"`
	DocAAccessCount        int       `json:"doc_a_access_count"`
	DocBAccessCount        int       `json:"doc_b_access_count"`
	DocAAgeDays            int       `json:"doc_a_age_days"`
	DocBAgeDays            int       `json:"doc_b_age_days"`
	SemanticSimilarity     float64   `json:"semantic_similarity"`
	ConsolidatorAgentTrust float64   `json:"consolidator_agent_trust,omitempty"`
	VerifiedA              bool      `json:"verified_a,omitempty"`
	VerifiedB              bool      `json:"verified_b,omitempty"`
	Timestamp              time.Time `json:"timestamp"`
}

// ContradictionLogger persists contradiction resolution records.
type ContradictionLogger interface {
	LogContradiction(ctx context.Context, resolution ContradictionResolution) error
}
