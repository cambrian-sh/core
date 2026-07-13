package memory

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// mockGenerator implements domain.Generator for Tier-2 scorer tests.
type mockGenerator struct {
	response string
	err      error
	calls    atomic.Int32
}

func (m *mockGenerator) Generate(_ context.Context, _ string) (string, error) {
	m.calls.Add(1)
	return m.response, m.err
}

// capturingStore records all Save calls.
type capturingStore struct {
	fakeVectorStore
	mu    sync.Mutex
	saved []*domain.Document
}

func (c *capturingStore) Save(_ context.Context, doc *domain.Document) error {
	c.mu.Lock()
	c.saved = append(c.saved, doc)
	c.mu.Unlock()
	return nil
}

func (c *capturingStore) Search(_ context.Context, _ []float32, _ domain.SearchOptions) ([]domain.SearchResult, error) {
	return nil, nil
}

func testAgentWithMocks(gen domain.Generator) *Agent {
	mgr := NewMemoryManager(&capturingStore{fakeVectorStore: fakeVectorStore{docs: []domain.SearchResult{}}}, &fakeEmbedder{})
	a := NewAgent(mgr, gen, 0.70, 5, 3, 64, 32, 300, 30)
	return a
}

// Cycle 1: FULL tier commits FACT + SCENE; FACT_ONLY commits only FACT; DROP commits nothing.
func TestTier2_CommitTiers(t *testing.T) {
	gen := &mockGenerator{
		response: `[
			{"relevance":8,"specificity":8,"explicitness":8,"tier":"FULL"},
			{"relevance":5,"specificity":5,"explicitness":5,"tier":"FACT_ONLY"},
			{"relevance":2,"specificity":2,"explicitness":2,"tier":"DROP"}
		]`,
	}
	agent := testAgentWithMocks(gen)

	// Put 3 items in the channel (one of each tier expected)
	stepDocs := []string{"important result", "average result", "noise result"}
	for i, text := range stepDocs {
		agent.pendingMu.Lock()
		agent.pendingItems = append(agent.pendingItems, pendingItem{
			Embedding: []float32{float32(i + 1), 0, 0},
			Doc: &domain.Document{
				ID:   text,
				Text: text,
				Metadata: map[string]interface{}{
					"snapshot":   "ctx for " + text,
					"step_index": i,
				},
			},
		})
		agent.pendingMu.Unlock()
	}

	// Drain the batch synchronously
	agent.drainBatch(context.Background())

	store := agent.Manager.Store.(*capturingStore)
	if gen.calls.Load() == 0 {
		t.Error("Generator.Generate was not called")
	}

	// Count saved documents by type
	factCount := 0
	sceneCount := 0
	for _, doc := range store.saved {
		switch doc.DocumentType {
		case domain.DocTypeMnemonicFact:
			factCount++
		case domain.DocTypeMnemonicScene:
			sceneCount++
		}
	}

	// FULL (important result) → 1 FACT + 1 SCENE
	// FACT_ONLY (average result) → 1 FACT
	// DROP (noise result) → 0 docs
	if factCount != 2 {
		t.Errorf("FACT document count = %d, want 2 (FULL + FACT_ONLY)", factCount)
	}
	if sceneCount != 1 {
		t.Errorf("SCENE document count = %d, want 1 (FULL only)", sceneCount)
	}

	// Verify scoring_prompt_version is set on committed documents
	for _, doc := range store.saved {
		if doc.ScoringPromptVersion == "" {
			t.Errorf("document %s has empty scoring_prompt_version", doc.ID)
		}
	}
}

// Cycle 1b: a reasoning model (e.g. qwen3) wraps its answer in <think>…</think>
// — often with stray brackets inside — before the JSON array. The scorer must
// strip the reasoning block and parse the real array, NOT collapse to the
// all-FACT_ONLY heuristic fallback (which would give factCount=3, sceneCount=0).
func TestTier2_ParsesReasoningModelOutput(t *testing.T) {
	gen := &mockGenerator{
		response: "<think>\nLet me score these. The first is important [high relevance]; " +
			"I'll emit an array like [{...}]. The third is noise.\n</think>\n" +
			`[
				{"relevance":8,"specificity":8,"explicitness":8,"tier":"FULL"},
				{"relevance":5,"specificity":5,"explicitness":5,"tier":"FACT_ONLY"},
				{"relevance":2,"specificity":2,"explicitness":2,"tier":"DROP"}
			]`,
	}
	agent := testAgentWithMocks(gen)

	for i, text := range []string{"important result", "average result", "noise result"} {
		agent.pendingMu.Lock()
		agent.pendingItems = append(agent.pendingItems, pendingItem{
			Embedding: []float32{float32(i + 1), 0, 0},
			Doc: &domain.Document{
				ID:       text,
				Text:     text,
				Metadata: map[string]interface{}{"snapshot": "ctx for " + text, "step_index": i},
			},
		})
		agent.pendingMu.Unlock()
	}

	agent.drainBatch(context.Background())

	store := agent.Manager.Store.(*capturingStore)
	factCount, sceneCount := 0, 0
	for _, doc := range store.saved {
		switch doc.DocumentType {
		case domain.DocTypeMnemonicFact:
			factCount++
		case domain.DocTypeMnemonicScene:
			sceneCount++
		}
	}
	// FULL → FACT+SCENE, FACT_ONLY → FACT, DROP → nothing. Heuristic fallback
	// would instead yield 3 FACTs / 0 SCENEs — so these counts prove the LLM
	// array (behind the <think> wrapper + stray brackets) was actually parsed.
	if factCount != 2 {
		t.Errorf("FACT count = %d, want 2 (reasoning wrapper not stripped → heuristic fallback?)", factCount)
	}
	if sceneCount != 1 {
		t.Errorf("SCENE count = %d, want 1 (DROP/FULL tiers lost → heuristic fallback?)", sceneCount)
	}
}

// Cycle 2: Error pre-filter — error items routed to negative_edge; clean items still scored.
func TestTier2_ErrorPreFilter_RoutesToNegativeEdge(t *testing.T) {
	gen := &mockGenerator{
		response: `[{"relevance":5,"specificity":5,"explicitness":5,"tier":"FACT_ONLY"}]`,
	}
	agent := testAgentWithMocks(gen)

	errorItem := pendingItem{
		Embedding: []float32{1, 0, 0},
		Doc: &domain.Document{
			ID:   "err-item",
			Text: "BLOCKED: 'write' is not in ALLOWED_COMMANDS",
			Metadata: map[string]interface{}{
				"agent_id":   "terminal_agent",
				"step_index": 0,
			},
		},
	}
	cleanItem := pendingItem{
		Embedding: []float32{0, 1, 0},
		Doc: &domain.Document{
			ID:   "clean-item",
			Text: "The Sieve of Eratosthenes runs in O(n log log n).",
			Metadata: map[string]interface{}{
				"agent_id":   "analyst_agent",
				"step_index": 1,
			},
		},
	}

	agent.pendingMu.Lock()
	agent.pendingItems = append(agent.pendingItems, errorItem, cleanItem)
	agent.pendingMu.Unlock()

	agent.drainBatch(context.Background())

	store := agent.Manager.Store.(*capturingStore)

	if gen.calls.Load() != 1 {
		t.Errorf("Generator call count = %d, want 1 (error item must not reach LLM)", gen.calls.Load())
	}

	var negEdges, facts []*domain.Document
	for _, doc := range store.saved {
		switch doc.DocumentType {
		case domain.DocTypeNegativeEdge:
			negEdges = append(negEdges, doc)
		case domain.DocTypeMnemonicFact:
			facts = append(facts, doc)
		}
	}
	if len(negEdges) != 1 {
		t.Fatalf("negative_edge doc count = %d, want 1", len(negEdges))
	}
	if len(facts) != 1 {
		t.Fatalf("mnemonic_fact doc count = %d, want 1 (clean item)", len(facts))
	}
	if errorType, _ := negEdges[0].Metadata["error_type"].(string); errorType == "" {
		t.Error("negative_edge doc missing error_type metadata")
	}
}

// Cycle 3: Generator timeout → entire batch committed as FACT-only via heuristic.
func TestTier2_Timeout_FallbackToHeuristic(t *testing.T) {
	gen := &mockGenerator{
		err: context.DeadlineExceeded,
	}
	agent := testAgentWithMocks(gen)

	items := []string{"result a", "result b", "result c"}
	for i, text := range items {
		agent.pendingMu.Lock()
		agent.pendingItems = append(agent.pendingItems, pendingItem{
			Embedding: []float32{float32(i + 1), 0, 0},
			Doc: &domain.Document{
				ID:   text,
				Text: text,
				Metadata: map[string]interface{}{
					"snapshot": "ctx",
				},
			},
		})
		agent.pendingMu.Unlock()
	}

	agent.drainBatch(context.Background())

	store := agent.Manager.Store.(*capturingStore)

	// All 3 items should be committed as FACT-only (no drops, no scenes)
	if len(store.saved) != 3 {
		t.Errorf("saved count = %d, want 3 (all FACT-only fallback)", len(store.saved))
	}
	for _, doc := range store.saved {
		if doc.DocumentType != domain.DocTypeMnemonicFact {
			t.Errorf("document %s type = %q, want DocTypeMnemonicFact (FACT-only fallback)", doc.ID, doc.DocumentType)
		}
	}

	// Fallback count should be incremented
	if agent.tier2FallbackCount.Load() != 1 {
		t.Errorf("tier2_llm_fallback_count = %d, want 1", agent.tier2FallbackCount.Load())
	}

	// Generator should have been called exactly once (no per-item retry)
	if gen.calls.Load() != 1 {
		t.Errorf("Generator calls = %d, want 1 (no per-item retry)", gen.calls.Load())
	}
}

// Cycle 3: Verify Tier-2 goroutine drains on shutdown (drains remaining items).
func TestTier2_DrainOnShutdown(t *testing.T) {
	gen := &mockGenerator{
		response: `[{"relevance":8,"specificity":8,"explicitness":8,"tier":"FULL"}]`,
	}
	agent := testAgentWithMocks(gen)

	// Put an item in the channel
	agent.pendingMu.Lock()
	agent.pendingItems = append(agent.pendingItems, pendingItem{
		Embedding: []float32{1, 0, 0},
		Doc: &domain.Document{
			ID:   "drain-on-shutdown",
			Text: "drain on shutdown test",
			Metadata: map[string]interface{}{
				"snapshot": "ctx",
			},
		},
	})
	agent.pendingMu.Unlock()

	// Start the goroutine and immediately stop it.
	ctx, cancel := context.WithCancel(context.Background())
	agent.StartTier2Drain(ctx)
	time.Sleep(50 * time.Millisecond) // let goroutine start
	cancel()
	agent.StopTier2Drain()
	time.Sleep(50 * time.Millisecond) // let drain complete

	store := agent.Manager.Store.(*capturingStore)
	store.mu.Lock()
	savedCount := len(store.saved)
	store.mu.Unlock()
	if savedCount == 0 {
		t.Error("no documents saved — drain-on-shutdown did not commit pending items")
	}
}

// Cycle 4: Malformed JSON in LLM response → heuristic fallback.
func TestTier2_MalformedJSON_HeuristicFallback(t *testing.T) {
	gen := &mockGenerator{
		response: "not valid json at all",
	}
	agent := testAgentWithMocks(gen)

	agent.pendingMu.Lock()
	agent.pendingItems = append(agent.pendingItems, pendingItem{
		Embedding: []float32{1, 0, 0},
		Doc: &domain.Document{
			ID:   "malformed-json",
			Text: "some useful information here for scoring",
		},
	})
	agent.pendingMu.Unlock()

	agent.drainBatch(context.Background())

	store := agent.Manager.Store.(*capturingStore)
	if len(store.saved) != 1 {
		t.Errorf("saved count = %d, want 1 (FACT-only fallback for malformed JSON)", len(store.saved))
	}
	if agent.tier2FallbackCount.Load() != 1 {
		t.Errorf("fallback count = %d, want 1", agent.tier2FallbackCount.Load())
	}
}

// Cycle 5: scoringPromptHash is computed and non-empty.
func TestScoringPromptHash_NonEmpty(t *testing.T) {
	if scoringPromptHash == "" {
		t.Error("scoringPromptHash is empty")
	}
	if len(scoringPromptHash) != 8 {
		t.Errorf("scoringPromptHash length = %d, want 8", len(scoringPromptHash))
	}
	if scoringPromptHash != domain.PromptHashOf(scoringPromptTemplate) {
		t.Error("scoringPromptHash is not deterministic")
	}
}

// Cycle 6: Tier-1 channel items carry pre-computed embeddings (RecordExecution test).
func TestRecordExecution_EmbedsAndPlacesInChannel(t *testing.T) {
	store := &capturingStore{fakeVectorStore: fakeVectorStore{docs: []domain.SearchResult{}}}
	mgr := NewMemoryManager(store, &fakeEmbedder{})
	agent := NewAgent(mgr, nil, 0.70, 5, 3, 64, 0, 0, 0)

	err := agent.RecordExecution(context.Background(), domain.StepResult{
		Index:    0,
		Output:   "test step output for embedding",
		Snapshot: map[string]string{"key": "value"},
	})
	if err != nil {
		t.Fatalf("RecordExecution failed: %v", err)
	}

	agent.pendingMu.RLock()
	n := len(agent.pendingItems)
	agent.pendingMu.RUnlock()

	if n != 1 {
		t.Errorf("pending channel length = %d, want 1", n)
	}

	agent.pendingMu.RLock()
	if n > 0 {
		item := agent.pendingItems[0]
		if len(item.Embedding) == 0 {
			t.Error("pending item has no pre-computed embedding")
		}
		if item.Doc.DocumentType != domain.DocTypeMnemonicFact {
			t.Errorf("pending doc type = %q, want DocTypeMnemonicFact", item.Doc.DocumentType)
		}
	}
	agent.pendingMu.RUnlock()
}

// Cycle 7: Query merges Tier-1 channel items with pgvector results.
func TestQuery_MergesPendingAndStore(t *testing.T) {
	// Seed pgvector store with one document
	store := &capturingStore{fakeVectorStore: fakeVectorStore{
		docs: []domain.SearchResult{
			{
				Document: domain.Document{ID: "pg-doc", Text: "pgvector document", DocumentType: domain.DocTypeMemory},
				Score:    0.85,
			},
		},
	}}
	mgr := NewMemoryManager(store, &fakeEmbedder{})
	agent := NewAgent(mgr, nil, 0.70, 5, 3, 64, 0, 0, 0)

	// Pre-populate Tier-1 channel with one item
	agent.pendingMu.Lock()
	agent.pendingItems = append(agent.pendingItems, pendingItem{
		Embedding: []float32{1, 0, 0, 0, 0},
		Doc:       &domain.Document{ID: "pending-doc", Text: "pending channel document"},
	})
	agent.pendingMu.Unlock()

	results, err := agent.Query(context.Background(), "query text", 5)
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("Query returned no results")
	}

	// Should contain both pending and pgvector results
	hasMerged := false
	for _, r := range results {
		if strings.Contains(r.Document.Text, "pending") {
			hasMerged = true
		}
	}
	if !hasMerged {
		t.Error("Query did not merge Tier-1 pending items with pgvector results")
	}
}
