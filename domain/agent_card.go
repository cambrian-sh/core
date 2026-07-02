package domain

import "context"

// AgentCard is the A2A identity document served by each agent's /.well-known/agent.json endpoint.
type AgentCard struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Version     string     `json:"version"`
	Skills      []A2ASkill `json:"skills"`
}

// A2ASkill describes a single capability declared on an Agent Card.
type A2ASkill struct {
	Description string `json:"description"`
}

// CardFetcher fetches an AgentCard from a remote endpoint.
type CardFetcher interface {
	FetchCard(ctx context.Context, endpoint string) (*AgentCard, error)
}
