package network

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("storeNeuralTrace panicked with nil VectorStore: %v", r)
		}
	}()

	storeNeuralTrace(context.Background(), nil, "some trace", "trace-1", "plan-1", 0, 0, "agent-x")
}

func TestStoreNeuralTrace_HealAttempt_Nonzero(t *testing.T) {
	t.Parallel()
	vs := &capturingVS{saved: make(chan *domain.Document, 1)}

	storeNeuralTrace(context.Background(), vs, "retried thought", "trace-2", "plan-2", 1, 2, "agent-y")

	doc := waitForDoc(t, vs.saved)

	if v := doc.Metadata["heal_attempt"]; v != 2 {
		t.Errorf("heal_attempt = %v, want %d", v, 2)
	}
}

// BRAIN-01: a neural trace must carry the SESSION that produced it.
//
// A trace is one run's reasoning — the archetypal "another task's evidence".
// Without a session stamp it was retrievable by every other run forever, and
// nothing downstream could tell whose run it belonged to: `trace_id` looks
// session-shaped but is a plan or lease id.
func TestStoreNeuralTrace_StampsTheSession(t *testing.T) {
	t.Parallel()
	vs := &capturingVS{saved: make(chan *domain.Document, 1)}
	ctx := domain.WithSessionID(context.Background(), "sess-owner")

	storeNeuralTrace(ctx, vs, "thought text", "trace-1", "plan-1", 0, 0, "agent-x")

	doc := waitForDoc(t, vs.saved)
	got, _ := doc.Metadata[domain.MetaSessionID].(string)
	if got != "sess-owner" {
		t.Fatalf("session_id = %q, want %q — an unstamped trace is invisible to "+
			"isolation and uncountable by cross_session_retrieval_rate", got, "sess-owner")
	}
	// Stored as a plain string: Metadata is an untyped JSON edge, and every reader
	// does a .(string) assertion that would silently miss a typed value.
	if _, ok := doc.Metadata[domain.MetaSessionID].(string); !ok {
		t.Fatalf("session_id is not a plain string: %T", doc.Metadata[domain.MetaSessionID])
	}
}

// No session on the context (a kernel-internal or unattended write) must NOT
// invent one. An empty-string stamp would be worse than none: it is a value that
// every predicate then has to special-case.
func TestStoreNeuralTrace_NoSessionLeavesTheKeyAbsent(t *testing.T) {
	t.Parallel()
	vs := &capturingVS{saved: make(chan *domain.Document, 1)}

	storeNeuralTrace(context.Background(), vs, "thought text", "trace-1", "plan-1", 0, 0, "agent-x")

	doc := waitForDoc(t, vs.saved)
	if v, present := doc.Metadata[domain.MetaSessionID]; present {
		t.Fatalf("session_id present as %#v with no session on the context", v)
	}
}

// Two different exchanges under ONE lease must not overwrite each other: the id
// used to be `trace-<lease>`, so only a lease's last model call survived. The
// same trace written twice (a retry) must land on the same row (AGENTS.md §2.6;
// T-20261113-agent-neural-traces-full-prompt-response-are).
func TestNeuralTraceDocID_DeterministicAndDistinctPerExchange(t *testing.T) {
	t.Parallel()
	a := neuralTraceDocID("lease-1", "PROMPT:\nq1\n\nRESPONSE:\na1")
	b := neuralTraceDocID("lease-1", "PROMPT:\nq2\n\nRESPONSE:\na2")
	if a == b {
		t.Fatalf("two exchanges under one lease share id %q", a)
	}
	if again := neuralTraceDocID("lease-1", "PROMPT:\nq1\n\nRESPONSE:\na1"); again != a {
		t.Fatalf("same trace gave ids %q and %q; a retry would duplicate the row", a, again)
	}
}

// blockingVS holds every Save until released, to fill the in-flight bound.
type blockingVS struct {
	capturingVS
	release chan struct{}
	started chan struct{}
}

func (b *blockingVS) Save(ctx context.Context, doc *domain.Document) error {
	b.started <- struct{}{}
	<-b.release
	return nil
}

// A slow store must not turn into unbounded goroutine growth (AGENTS.md §3.3):
// past maxInFlightNeuralTraces, a trace write is dropped rather than queued on a
// new goroutine. Not parallel: it fills the package-wide bound.
func TestStoreNeuralTrace_BoundsInFlightWrites(t *testing.T) {
	vs := &blockingVS{release: make(chan struct{}), started: make(chan struct{}, maxInFlightNeuralTraces+8)}
	defer close(vs.release)
	for i := 0; i < maxInFlightNeuralTraces+8; i++ {
		storeNeuralTrace(context.Background(), vs, fmt.Sprintf("t%d", i), "lease", "", 0, 0, "a")
	}
	// Every admitted write reaches Save; give stragglers a moment, then count.
	deadline := time.After(2 * time.Second)
	for n := 0; n < maxInFlightNeuralTraces; n++ {
		select {
		case <-vs.started:
		case <-deadline:
			t.Fatalf("only %d writes started, want %d", n, maxInFlightNeuralTraces)
		}
	}
	select {
	case <-vs.started:
		t.Fatal("a write beyond the in-flight bound was started")
	case <-time.After(100 * time.Millisecond):
	}
}
