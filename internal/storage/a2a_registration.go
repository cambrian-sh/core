package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"go.etcd.io/bbolt"
)

// Enqueuer enqueues an agent for asynchronous profile seeding (registration.ProfileSeeder).
type Enqueuer interface {
	Enqueue(agent domain.AgentDefinition)
}

// A2ATrustTier is the ADR-0139 D8 tier every remote A2A agent registers at.
// It is stamped here, by the kernel; a card cannot claim another.
const A2ATrustTier = "third-party"

// ErrA2AIDTaken reports a remote card whose name maps onto an agent id the
// kernel already uses for a different agent.
var ErrA2AIDTaken = errors.New("a2a: agent id already belongs to another agent")

// RegisterA2AAgent fetches the Agent Card from endpoint, registers the agent as
// RuntimeA2A at the third-party trust tier, persists it to bbolt, and enqueues
// it for profile seeding. Idempotent: an unchanged SourceHash is a no-op.
//
// The fetcher is the kernel's verifying fetcher (internal/agentcard
// RemoteFetcher), so a card the a2a.signature_policy refuses never reaches here.
//
// A remote card cannot take over a local agent's id: its name is normalised to
// an id, and an existing record that is not the same remote endpoint is a
// refusal, not an overwrite. Overwriting would hand a stranger's card the local
// agent's routing history, grants and tool ceiling.
func (b *BBoltAdapter) RegisterA2AAgent(ctx context.Context, endpoint string) error {
	if b.CardFetcher == nil {
		return errors.New("a2a: no card fetcher wired")
	}
	card, err := b.CardFetcher.FetchCard(ctx, endpoint)
	if err != nil {
		return fmt.Errorf("a2a: fetch card from %s: %w", endpoint, err)
	}

	sourceHash := ComputeSourceHash(card.Version, []byte(card.Description))
	agentID := normalizeA2AAgentID(card.Name)
	if agentID == "" {
		return fmt.Errorf("a2a: card from %s has no usable name", endpoint)
	}

	existing, err := b.agentRecordByID(agentID)
	if err != nil {
		return fmt.Errorf("a2a: read existing agent %s: %w", agentID, err)
	}
	if existing != nil && (existing.Runtime != string(domain.RuntimeA2A) || existing.A2AEndpoint != endpoint) {
		return fmt.Errorf("%w: card from %s names %q (registered runtime %q, endpoint %q)",
			ErrA2AIDTaken, endpoint, agentID, existing.Runtime, existing.A2AEndpoint)
	}
	// Idempotency guard: same SourceHash → no-op.
	if existing != nil && existing.SourceHash == sourceHash && existing.TrustTier == A2ATrustTier {
		return nil
	}

	// Persisted as the AgentRecord the bucket holds (it used to marshal a
	// domain.AgentDefinition here, readable only through encoding/json's
	// case-insensitive key match).
	rec := AgentRecord{
		ID:          agentID,
		Name:        card.Name,
		Description: card.Description,
		Runtime:     string(domain.RuntimeA2A),
		A2AEndpoint: endpoint,
		SourceHash:  sourceHash,
		Provisional: true,
		TrustTier:   A2ATrustTier,
	}
	data, err := json.Marshal(rec)
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

	// Enqueue for profile seeding (card ingestion + neutral prior; no interview).
	if b.Enqueuer != nil {
		b.Enqueuer.Enqueue(domain.AgentDefinition{
			ID:          rec.ID,
			Name:        rec.Name,
			Description: rec.Description,
			Runtime:     domain.RuntimeA2A,
			A2AEndpoint: rec.A2AEndpoint,
			SourceHash:  rec.SourceHash,
			Provisional: true,
			TrustTier:   rec.TrustTier,
		})
	}
	return nil
}

// agentRecordByID reads one record by its exact key; (nil, nil) when absent.
// Not GetAgentRecord: that one falls back to a Name match and reports absence
// as an error.
func (b *BBoltAdapter) agentRecordByID(id string) (*AgentRecord, error) {
	var rec *AgentRecord
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(agentBucket)
		if bucket == nil {
			return nil
		}
		data := bucket.Get([]byte(id))
		if data == nil {
			return nil
		}
		var r AgentRecord
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		rec = &r
		return nil
	})
	return rec, err
}

// normalizeA2AAgentID maps a card name onto the kernel's agent-id alphabet:
// lower case, runs of anything outside [a-z0-9_-] become one '-'.
func normalizeA2AAgentID(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}
