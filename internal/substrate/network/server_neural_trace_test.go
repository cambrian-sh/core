package network

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// capturingVS is a VectorStore test double that records every Save call
// via a buffered channel, allowing tests to assert on document metadata
// without a real pgvector instance.
type capturingVS struct {
	saved chan *domain.Document
}

func (c *capturingVS) Save(_ context.Context, doc *domain.Document) error {
	c.saved <- doc
	return nil
}

func (c *capturingVS) SaveBatch(_ context.Context, _ []*domain.Document) error {
	return nil
}

func (c *capturingVS) Search(_ context.Context, _ []float32, _ domain.SearchOptions) ([]domain.SearchResult, error) {
	return nil, nil
}

func (c *capturingVS) GetByID(_ context.Context, _ string) (*domain.Document, error) {
	return nil, nil
}

func (c *capturingVS) GetBatch(_ context.Context, _ []string) ([]domain.Document, error) {
	return nil, nil
}

func (c *capturingVS) Delete(_ context.Context, _ string) error { return nil }

func (c *capturingVS) DeleteBatch(_ context.Context, _ []string) error { return nil }

func (c *capturingVS) IncrementAccess(_ context.Context, _ string) error { return nil }

func (c *capturingVS) GetStaleMemories(_ context.Context, _ int) ([]domain.Document, error) {
	return nil, nil
}
func (c *capturingVS) QueryByMetadata(_ context.Context, _ map[string]string, _ int) ([]domain.Document, error) {
	return nil, nil
}

// waitForDoc reads from ch with a 500ms timeout. It fails the test if nothing
// arrives in time, which proves the goroutine fired and called Save.
func waitForDoc(t *testing.T, ch chan *domain.Document) *domain.Document {
	t.Helper()
	select {
	case doc := <-ch:
		return doc
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for neural trace Save call")
		return nil
	}
}

func TestStoreNeuralTrace_SavesCorrectDocType(t *testing.T) {
	vs := &capturingVS{saved: make(chan *domain.Document, 1)}

	storeNeuralTrace(context.Background(), vs, "thought text", "trace-1", "plan-1", 0, 0, "agent-x")

	doc := waitForDoc(t, vs.saved)

	if doc.DocumentType != domain.DocTypeNeuralTrace {
		t.Errorf("DocumentType = %q, want %q", doc.DocumentType, domain.DocTypeNeuralTrace)
	}
	if doc.Text != "thought text" {
		t.Errorf("Text = %q, want %q", doc.Text, "thought text")
	}
}

func TestStoreNeuralTrace_MetadataFields(t *testing.T) {
	vs := &capturingVS{saved: make(chan *domain.Document, 1)}

	storeNeuralTrace(context.Background(), vs, "trace text", "trace-abc", "plan-xyz", 3, 1, "agent-foo")

	doc := waitForDoc(t, vs.saved)

	meta := doc.Metadata
	if meta == nil {
		t.Fatal("Metadata is nil")
	}

	if v, ok := meta["trace_id"]; !ok || v != "trace-abc" {
		t.Errorf("trace_id = %v, want %q", v, "trace-abc")
	}
	if v, ok := meta["plan_id"]; !ok || v != "plan-xyz" {
		t.Errorf("plan_id = %v, want %q", v, "plan-xyz")
	}
	if v, ok := meta["step_index"]; !ok || v != 3 {
		t.Errorf("step_index = %v, want %d", v, 3)
	}
	if v, ok := meta["agent_id"]; !ok || v != "agent-foo" {
		t.Errorf("agent_id = %v, want %q", v, "agent-foo")
	}
	if v, ok := meta["heal_attempt"]; !ok || v != 1 {
		t.Errorf("heal_attempt = %v, want %d", v, 1)
	}
}

func TestStoreNeuralTrace_EmptyTrace_NotCalled(t *testing.T) {
	vs := &capturingVS{saved: make(chan *domain.Document, 1)}

	storeNeuralTrace(context.Background(), vs, "", "trace-1", "plan-1", 0, 0, "agent-x")

	select {
	case <-vs.saved:
		t.Error("Save must not be called when trace is empty")
	case <-time.After(50 * time.Millisecond):
		// correct: nothing was saved
	}
}

func TestStoreNeuralTrace_NilVectorStore_NoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("storeNeuralTrace panicked with nil VectorStore: %v", r)
		}
	}()

	storeNeuralTrace(context.Background(), nil, "some trace", "trace-1", "plan-1", 0, 0, "agent-x")
}

func TestStoreNeuralTrace_HealAttempt_Nonzero(t *testing.T) {
	vs := &capturingVS{saved: make(chan *domain.Document, 1)}

	storeNeuralTrace(context.Background(), vs, "retried thought", "trace-2", "plan-2", 1, 2, "agent-y")

	doc := waitForDoc(t, vs.saved)

	if v := doc.Metadata["heal_attempt"]; v != 2 {
		t.Errorf("heal_attempt = %v, want %d", v, 2)
	}
}
