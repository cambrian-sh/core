package storage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"go.etcd.io/bbolt"
)

// --- Test doubles ---

type mockA2ACardFetcher struct {
	card *A2ACard
	err  error
}

func (m *mockA2ACardFetcher) FetchCard(_ context.Context, _ string) (*A2ACard, error) {
	return m.card, m.err
}

type mockEnqueuer struct {
	enqueued []domain.AgentDefinition
}

func (m *mockEnqueuer) Enqueue(agent domain.AgentDefinition) {
	m.enqueued = append(m.enqueued, agent)
}

// newA2ATestAdapter creates a BBoltAdapter with both agentBucket and manifestBucket.
func newA2ATestAdapter(t *testing.T) (*BBoltAdapter, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := bbolt.Open(dbPath, 0600, &bbolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		t.Fatalf("open bbolt: %v", err)
	}
	_ = db.Update(func(tx *bbolt.Tx) error {
		_, _ = tx.CreateBucketIfNotExists(agentBucket)
		_, _ = tx.CreateBucketIfNotExists(manifestBucket)
		return nil
	})
	adapter := &BBoltAdapter{db: db}
	return adapter, func() {
		db.Close()
		os.RemoveAll(dir)
	}
}

// Cycle 1: Valid card → persisted AgentDefinition with RuntimeA2A, Enqueuer called once.
func TestRegisterA2AAgent_PersistsDefinitionAndEnqueues(t *testing.T) {
	adapter, cleanup := newA2ATestAdapter(t)
	defer cleanup()

	card := &A2ACard{Name: "Web Search Agent", Description: "Searches the web", Version: "1.0"}
	adapter.CardFetcher = &mockA2ACardFetcher{card: card}
	enqueuer := &mockEnqueuer{}
	adapter.Enqueuer = enqueuer

	err := adapter.RegisterA2AAgent(context.Background(), "http://a2a.example.com")
	if err != nil {
		t.Fatalf("RegisterA2AAgent returned error: %v", err)
	}

	// Enqueuer must have been called once.
	if len(enqueuer.enqueued) != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", len(enqueuer.enqueued))
	}
	enqueued := enqueuer.enqueued[0]
	if enqueued.Runtime != domain.RuntimeA2A {
		t.Errorf("expected Runtime=%q, got %q", domain.RuntimeA2A, enqueued.Runtime)
	}
	if enqueued.A2AEndpoint != "http://a2a.example.com" {
		t.Errorf("expected A2AEndpoint=%q, got %q", "http://a2a.example.com", enqueued.A2AEndpoint)
	}
	if !enqueued.Provisional {
		t.Error("expected Provisional=true")
	}

	// Agent must be persisted in bbolt.
	got, err := adapter.GetAgentRecord(enqueued.ID)
	if err != nil || got == nil {
		t.Fatalf("agent not found in bbolt: %v", err)
	}
	if got.Runtime != "a2a" {
		t.Errorf("persisted Runtime=%q, want a2a", got.Runtime)
	}
}

// Cycle 2: Idempotency — same SourceHash → no re-enqueue.
func TestRegisterA2AAgent_Idempotent_SameHash(t *testing.T) {
	adapter, cleanup := newA2ATestAdapter(t)
	defer cleanup()

	card := &A2ACard{Name: "My Agent", Description: "does stuff", Version: "2.0"}
	adapter.CardFetcher = &mockA2ACardFetcher{card: card}
	enqueuer := &mockEnqueuer{}
	adapter.Enqueuer = enqueuer

	// Register twice with same card.
	if err := adapter.RegisterA2AAgent(context.Background(), "http://agent.example.com"); err != nil {
		t.Fatalf("first registration failed: %v", err)
	}
	if err := adapter.RegisterA2AAgent(context.Background(), "http://agent.example.com"); err != nil {
		t.Fatalf("second registration failed: %v", err)
	}

	// Enqueuer called only once (second call was a no-op).
	if len(enqueuer.enqueued) != 1 {
		t.Errorf("expected 1 enqueue call (idempotent), got %d", len(enqueuer.enqueued))
	}

	// Only one agent in bbolt.
	agents, _ := adapter.GetAllAgentRecords()
	count := 0
	agentID := normalizeA2AAgentID(card.Name)
	for _, a := range agents {
		if a.ID == agentID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 agent in bbolt, found %d", count)
	}
}

// Cycle 3: Card version change → re-enqueue.
func TestRegisterA2AAgent_CardVersionChange_Reenqueues(t *testing.T) {
	adapter, cleanup := newA2ATestAdapter(t)
	defer cleanup()

	enqueuer := &mockEnqueuer{}
	adapter.Enqueuer = enqueuer

	// First registration — version 1.0
	cardV1 := &A2ACard{Name: "Versioned Agent", Description: "v1 features", Version: "1.0"}
	adapter.CardFetcher = &mockA2ACardFetcher{card: cardV1}
	if err := adapter.RegisterA2AAgent(context.Background(), "http://versioned.example.com"); err != nil {
		t.Fatalf("v1 registration failed: %v", err)
	}

	// Second registration — version 2.0 (card version bump)
	cardV2 := &A2ACard{Name: "Versioned Agent", Description: "v2 features", Version: "2.0"}
	adapter.CardFetcher = &mockA2ACardFetcher{card: cardV2}
	if err := adapter.RegisterA2AAgent(context.Background(), "http://versioned.example.com"); err != nil {
		t.Fatalf("v2 registration failed: %v", err)
	}

	// Enqueuer must have been called twice (once for each distinct SourceHash).
	if len(enqueuer.enqueued) != 2 {
		t.Errorf("expected 2 enqueue calls (once per version), got %d", len(enqueuer.enqueued))
	}

	// Persisted agent must have v2 SourceHash.
	agentID := normalizeA2AAgentID(cardV2.Name)
	got, err := adapter.GetAgentRecord(agentID)
	if err != nil || got == nil {
		t.Fatalf("agent not found: %v", err)
	}
	wantHash := ComputeSourceHash(cardV2.Version, []byte(cardV2.Description))
	if got.SourceHash != wantHash {
		t.Errorf("SourceHash = %q, want %q", got.SourceHash, wantHash)
	}
}

// Verify that the test file compiles correctly by referencing json package.
var _ = json.Marshal
