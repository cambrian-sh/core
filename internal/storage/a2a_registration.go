package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"go.etcd.io/bbolt"
)

// A2ACard is a minimal card representation for registration purposes.
type A2ACard struct {
	Name        string
	Description string
	Version     string
}

// A2ACardFetcher fetches an A2A Agent Card from a base endpoint URL.
type A2ACardFetcher interface {
	FetchCard(ctx context.Context, endpoint string) (*A2ACard, error)
}

// Enqueuer enqueues an agent for asynchronous Interview processing.
type Enqueuer interface {
	Enqueue(agent domain.AgentDefinition)
}

// RegisterA2AAgent fetches the Agent Card from endpoint, builds an AgentDefinition
// with Runtime=RuntimeA2A, persists it to bbolt, and enqueues it in InterviewWorker.
// Idempotent: if the SourceHash is unchanged the call is a no-op.
func (b *BBoltAdapter) RegisterA2AAgent(ctx context.Context, endpoint string) error {
	card, err := b.CardFetcher.FetchCard(ctx, endpoint)
	if err != nil {
		return fmt.Errorf("a2a: fetch card from %s: %w", endpoint, err)
	}

	sourceHash := ComputeSourceHash(card.Version, []byte(card.Description))
	agentID := normalizeA2AAgentID(card.Name)

	// Read existing agent from bbolt.
	var existing *domain.AgentDefinition
	readErr := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(agentBucket)
		if bucket == nil {
			return nil
		}
		data := bucket.Get([]byte(agentID))
		if data == nil {
			return nil
		}
		var a domain.AgentDefinition
		if err := json.Unmarshal(data, &a); err != nil {
			return err
		}
		existing = &a
		return nil
	})
	if readErr != nil {
		return fmt.Errorf("a2a: read existing agent %s: %w", agentID, readErr)
	}

	// Idempotency guard: same SourceHash → no-op.
	if existing != nil && existing.SourceHash == sourceHash {
		return nil
	}

	// Build the AgentDefinition (new or updated).
	agent := domain.AgentDefinition{
		ID:          agentID,
		Name:        card.Name,
		Description: card.Description,
		Runtime:     domain.RuntimeA2A,
		A2AEndpoint: endpoint,
		SourceHash:  sourceHash,
		Provisional: true,
	}
	if existing != nil {
		// Preserve fields that should persist across version bumps.
		agent.Dir = existing.Dir
		agent.ExecPath = existing.ExecPath
	}

	// Persist to bbolt.
	data, err := json.Marshal(agent)
	if err != nil {
		return fmt.Errorf("a2a: marshal agent %s: %w", agentID, err)
	}
	if err := b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(agentBucket)
		if bucket == nil {
			return fmt.Errorf("agents bucket not found")
		}
		return bucket.Put([]byte(agentID), data)
	}); err != nil {
		return fmt.Errorf("a2a: persist agent %s: %w", agentID, err)
	}

	// Enqueue for Interview processing.
	b.Enqueuer.Enqueue(agent)
	return nil
}

func normalizeA2AAgentID(name string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), " ", "-"))
}
