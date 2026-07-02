package domain

import (
	"context"
	"time"
)

// ProposalRequest is the domain-layer mirror of proto's ProposalRequest.
type ProposalRequest struct {
	TaskID         string
	Description    string
	Context        string
	Deadline       time.Time
	ConfidenceHint float32
}

// ProposalResponse is the domain-layer mirror of proto's ProposalResponse.
type ProposalResponse struct {
	Confidence         float32
	Rationale          string
	Requirements       []string
	EstimatedLatencyMs int32
	Metadata           map[string]string
}

// VerifyRequest is the domain-layer mirror of proto's VerifyRequest.
type VerifyRequest struct {
	TaskID        string
	OriginalQuery string
	WinnerOutput  string
	WinnerAgentID string
	BidConfidence float32
}

// VerifyResponse is the domain-layer mirror of proto's VerifyResponse.
type VerifyResponse struct {
	QualityScore float32
	Critique     string
}

// VerificationRequest carries one verification task from DAGExecutor to
// VerificationWorker after each successful DAG step.
type VerificationRequest struct {
	TaskID        string
	AgentID       string
	SourceHash    string
	BidConfidence float64
	Request       *Handoff
	Response      *Handoff
}

// ProposalRequester sends a RequestProposal RPC to a specific agent.
type ProposalRequester interface {
	RequestProposalFrom(ctx context.Context, agent AgentDefinition, req ProposalRequest) (ProposalResponse, error)
}

// VerifyRequester sends a VerifyOutput RPC to a verifier agent.
type VerifyRequester interface {
	VerifyOutput(ctx context.Context, agent AgentDefinition, req VerifyRequest) (VerifyResponse, error)
}

// VerifierProfileStore is the narrow profile read/write interface used by
// VerificationWorker for RecentVerifierIDs maintenance.
type VerifierProfileStore interface {
	GetProfile(ctx context.Context, agentID, sourceHash string) (*AgentProfile, error)
	SaveProfile(ctx context.Context, agentID, sourceHash string, embedding []float32, profile AgentProfile) error
}
