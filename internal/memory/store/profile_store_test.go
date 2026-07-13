package store

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// stubVectorStore is a minimal VectorStore stub for ProfileStore tests.
type stubVectorStore struct {
	savedDoc *domain.Document
}

func (s *stubVectorStore) Save(_ context.Context, doc *domain.Document) error {
	s.savedDoc = doc
	return nil
}
func (s *stubVectorStore) SaveBatch(_ context.Context, _ []*domain.Document) error { return nil }
func (s *stubVectorStore) Search(_ context.Context, _ []float32, _ domain.SearchOptions) ([]domain.SearchResult, error) {
	return nil, nil
}
func (s *stubVectorStore) GetByID(_ context.Context, _ string) (*domain.Document, error) {
	return nil, nil
}
func (s *stubVectorStore) GetBatch(_ context.Context, _ []string) ([]domain.Document, error) {
	return nil, nil
}
func (s *stubVectorStore) Delete(_ context.Context, _ string) error          { return nil }
func (s *stubVectorStore) DeleteBatch(_ context.Context, _ []string) error   { return nil }
func (s *stubVectorStore) IncrementAccess(_ context.Context, _ string) error { return nil }
func (s *stubVectorStore) GetStaleMemories(_ context.Context, _ int) ([]domain.Document, error) {
	return nil, nil
}
func (s *stubVectorStore) QueryByMetadata(_ context.Context, _ map[string]string, _ int) ([]domain.Document, error) {
	return nil, nil
}

// TestSaveProfile_SetsDocTypeAgentProfile verifies that the concrete ProfileStore
// writes DocumentType == domain.DocTypeAgentProfile when persisting a fingerprint.
func TestSaveProfile_SetsDocTypeAgentProfile(t *testing.T) {
	stub := &stubVectorStore{}
	store := NewProfileStore(stub)

	profile := domain.AgentProfile{
		AgentID:    "agent-z",
		SourceHash: "hash-z",
	}

	err := store.SaveProfile(context.Background(), "agent-z", "hash-z", []float32{0.1, 0.2, 0.3}, profile)
	if err != nil {
		t.Fatalf("SaveProfile returned error: %v", err)
	}

	if stub.savedDoc == nil {
		t.Fatal("expected Save to be called on the underlying VectorStore, but it was not")
	}
	if stub.savedDoc.DocumentType != domain.DocTypeAgentProfile {
		t.Errorf("expected DocumentType=%q, got %q", domain.DocTypeAgentProfile, stub.savedDoc.DocumentType)
	}
}

// TestSaveProfile_SetsAgentIDAndSourceHashInMetadata verifies that the saved
// document's Metadata contains the agentID and sourceHash.
func TestSaveProfile_SetsAgentIDAndSourceHashInMetadata(t *testing.T) {
	stub := &stubVectorStore{}
	store := NewProfileStore(stub)

	profile := domain.AgentProfile{
		AgentID:    "agent-meta",
		SourceHash: "hash-meta",
	}

	err := store.SaveProfile(context.Background(), "agent-meta", "hash-meta", []float32{0.5}, profile)
	if err != nil {
		t.Fatalf("SaveProfile returned error: %v", err)
	}

	agentID, ok := stub.savedDoc.Metadata["agent_id"].(string)
	if !ok || agentID != "agent-meta" {
		t.Errorf("expected Metadata[agent_id]=%q, got %v", "agent-meta", stub.savedDoc.Metadata["agent_id"])
	}

	sourceHash, ok := stub.savedDoc.Metadata["source_hash"].(string)
	if !ok || sourceHash != "hash-meta" {
		t.Errorf("expected Metadata[source_hash]=%q, got %v", "hash-meta", stub.savedDoc.Metadata["source_hash"])
	}
}
