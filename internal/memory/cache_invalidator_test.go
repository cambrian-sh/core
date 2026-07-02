package memory

// Tests for CacheInvalidator registration and firing in MemoryAgent.
//
// These tests assert that commitBatch notifies registered invalidators.
// They do NOT test internal batch processing or LLM scoring logic.

import (
	"context"
	"sync/atomic"
	"testing"
)

// countingInvalidator records how many times InvalidateContextRefCache is called.
type countingInvalidator struct {
	calls atomic.Int32
}

func (c *countingInvalidator) InvalidateContextRefCache() {
	c.calls.Add(1)
}

// ── Tracer bullet: invalidator called after RegisterCacheInvalidator ────────

func TestMemoryAgent_RegisterCacheInvalidator_CalledOnCommitBatch(t *testing.T) {
	agent := newMinimalAgent()
	invalidator := &countingInvalidator{}

	agent.RegisterCacheInvalidator(invalidator)

	// commitBatch with empty scored items still fires invalidators.
	agent.commitBatch(context.Background(), nil, 0)

	if invalidator.calls.Load() == 0 {
		t.Error("CacheInvalidator must be called after commitBatch")
	}
}

// ── Multiple invalidators all get called ───────────────────────────────────

func TestMemoryAgent_MultipleInvalidators_AllCalled(t *testing.T) {
	agent := newMinimalAgent()
	a := &countingInvalidator{}
	b := &countingInvalidator{}

	agent.RegisterCacheInvalidator(a)
	agent.RegisterCacheInvalidator(b)
	agent.commitBatch(context.Background(), nil, 0)

	if a.calls.Load() == 0 {
		t.Error("first invalidator must be called")
	}
	if b.calls.Load() == 0 {
		t.Error("second invalidator must be called")
	}
}

// ── Without registration, commitBatch is a no-op for invalidators ──────────

func TestMemoryAgent_NoInvalidators_CommitBatchSafe(t *testing.T) {
	agent := newMinimalAgent()
	// Must not panic with no registered invalidators.
	agent.commitBatch(context.Background(), nil, 0)
}

// newMinimalAgent builds a minimal MemoryAgent suitable for invalidator tests.
func newMinimalAgent() *Agent {
	mgr := NewMemoryManager(&fakeVectorStore{}, &fakeEmbedder{})
	return NewAgent(mgr, nil, 0.5, 10, 3, 32, 8, 5, 10)
}
