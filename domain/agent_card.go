package domain

import "context"

// AgentCard is the subset of a remote agent's A2A Agent Card (A2A 1.0 §4.4.1,
// served at <endpoint>/.well-known/agent-card.json) the kernel reads to
// register and seed the agent. The kernel's OWN cards are built and signed by
// internal/agentcard, which has the full A2A shape.
type AgentCard struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Version     string     `json:"version"`
	Skills      []A2ASkill `json:"skills"`
	// Raw is the card exactly as served, kept so its JWS signature (A2A §8.4)
	// can be verified over the bytes the server published, not a re-encoding.
	Raw []byte `json:"-"`
	// Verified reports that a signature by a trusted key verified, and
	// SignerKeyID names that key. Set only by a verifying fetcher.
	Verified    bool   `json:"-"`
	SignerKeyID string `json:"-"`
}

// A2ASkill describes a single capability declared on an Agent Card (AgentSkill).
type A2ASkill struct {
	ID          string   `json:"id,omitempty"`
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
	Examples    []string `json:"examples,omitempty"`
}

// CardFetcher fetches an AgentCard from a remote endpoint.
type CardFetcher interface {
	FetchCard(ctx context.Context, endpoint string) (*AgentCard, error)
}
