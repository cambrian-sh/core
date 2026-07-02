package domain

import (
	"context"
	"time"
)

// Payload is the domain-layer mirror of proto's Object message.
type Payload struct {
	ID       string
	Type     string
	Data     []byte
	Metadata map[string]string
}

// Handoff is the domain-layer mirror of proto's Handoff message.
// It carries a task between agents through the execution DAG.
type Handoff struct {
	ID            string
	FromAgent     string
	ToAgent       string
	Payload       *Payload
	Confidence    float32
	Uncertainties []string
	Context       map[string]string // Phase 0/1/2 and circuit-breaker fallback
	SessionToken  *SessionToken     // per-step managed LLM session (ADR-0018); nil-safe
	// WorkingMemory is populated in Phase 3 (use_global_workspace=true).
	// When non-nil, Context is empty — the two fields are mutually exclusive.
	// Use assemble_context() in the Python SDK to consume this field.
	WorkingMemory []ContextRef
}

// AuctionResult bundles the three values returned by Auctioneer.Execute.
type AuctionResult struct {
	Handoff        *Handoff
	Confidence     float64
	RunnerUps      []ScoredCandidate
	StepAllocation *StepAllocation // populated by Auctioneer TraitModel sub-selection (ADR-0018); nil-safe
}

// Auctioneer runs the full auction pipeline: Gatekeeper → ConductAuction → CallAgent.
type Auctioneer interface {
	Execute(ctx context.Context, task *AuctionTask, in *Handoff) (*AuctionResult, error)
	CallAgent(ctx context.Context, agentID string, handoff *Handoff, excludeInstanceID string) (*Handoff, error)
}

// AgentProposal represents a bid from an agent to execute a specific task.
// As defined in AGENT_COORDINATION_MECHANISM.md
type AgentProposal struct {
	AgentID      string            `json:"agent_id"`
	TaskID       string            `json:"task_id"`
	Confidence   float64           `json:"confidence"`   // 0.0 - 1.0 (Based on LTM and Tool capability)
	Rationale    string            `json:"rationale"`    // Reasoning for the confidence score
	Requirements []string          `json:"requirements"` // Dependencies, e.g., "BrowserAgent's 'search_results'"
	Latency      int               `json:"latency_ms"`   // Estimated processing time
	Metadata     map[string]string
	IsTool       bool              `json:"is_tool,omitempty"` // true when produced by the Static Bidder path
}

// AuctionTask represents a task/RFP (Request For Proposal) broadcast to agents.
type AuctionTask struct {
	ID              string    `json:"id"`
	Description     string    `json:"description"`
	Context         string    `json:"context"`
	Deadline        time.Time `json:"deadline"`
	RequiredFormats []string  `json:"required_formats,omitempty"`
}
