package memory

import (
	"context"
	"errors"
	"fmt"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/scope"

	"github.com/google/uuid"
)

// ErrUnknownPrincipal is returned when an authenticated agentID has no scope
// profile — a write by an unknown principal is fail-closed. ADR-0034 (D8).
var ErrUnknownPrincipal = errors.New("memory: unknown principal (fail-closed)")

// WriteScopeResolver resolves the writer's known-principal status and its
// operator-configured DefaultWriteTags. The ScopeResolver satisfies it.
type WriteScopeResolver interface {
	EffectiveForAgent(ctx context.Context, agentID string) (*domain.EffectiveScope, bool)
	DefaultWriteTags(ctx context.Context, agentID string) []string
}

// RememberService implements agent memory write-back (memory.remember() /
// IngestMemory). Classification is KERNEL-DERIVED (ADR-0035 C2): the doc is
// stamped with the agent's DefaultWriteTags, narrowed only by an optional hint,
// with provenance kernel-stamped by the ScopedStoreWriter. The agent cannot choose
// its own classification.
type RememberService struct {
	store    domain.VectorStore // the ScopedStoreWriter (read+write gated)
	embedder domain.Embedder
	scopes   WriteScopeResolver
	// bus is optional (ADR-0047 D3): when set, a successful write publishes a
	// MemoryWrittenEvent for the operator feed. nil ⇒ no-op.
	bus domain.EventBus
	// edgeBatcher is optional (ADR-0052): when set, a successful write also
	// enqueues the doc for LLM-based entity+relation extraction. The batcher
	// drives the LLM in N-doc batches (default 32) instead of one LLM call
	// per ingest. Enqueue is non-blocking; the graph is the lossy layer.
	edgeBatcher *EdgeBatcher
	// chunkTripletsBatcher is optional (ADR-0053 D2 revised): when set, a
	// successful write also enqueues the doc for per-chunk (h, r, t) extraction
	// that back-fills the chunk_triplets table. The producer is swapped at
	// bootstrap: the LLM adapter (legacy) or the kg_extractor system-agent
	// adapter (metadata + spacy_patterns, no LLM). Enqueue is non-blocking;
	// the enrichment is the lossy layer.
	chunkTripletsBatcher *ChunkTripletsBatcher
	// defaultActivation is the initial ActivationStrength stamped on a remembered
	// fact when the caller gives no importance hint (config RememberDefaultActivation,
	// ADR-0015). A fact written with activation 0 can never clear the recall floor
	// (cosine·α ≤ α < RecallSimilarityFloor), so this MUST be > the floor/α ratio to
	// be recallable. Defaults to 0.5; overridden from config at bootstrap.
	defaultActivation float64
}

// SetEventBus wires the operator-feed EventBus (ADR-0047 D3). Bootstrap-time.
func (s *RememberService) SetEventBus(bus domain.EventBus) { s.bus = bus }

// SetEdgeBatcher wires the LLM-based graph populator (ADR-0052). Bootstrap-time.
// Replaces the prior per-remember EdgeWriter wiring; the batcher drives N-doc
// batches through one LLM call each.
func (s *RememberService) SetEdgeBatcher(b *EdgeBatcher) { s.edgeBatcher = b }

// SetChunkTripletsBatcher wires the per-chunk (h, r, t) extractor (ADR-0053 D2
// revised). Bootstrap-time. The producer is the kg_extractor system agent when
// execution.kg_extractor_enabled is true, else the LLM residue adapter.
func (s *RememberService) SetChunkTripletsBatcher(b *ChunkTripletsBatcher) {
	s.chunkTripletsBatcher = b
}

// SetDefaultActivation sets the initial ActivationStrength for hint-less remembers
// (config RememberDefaultActivation). Bootstrap-time; a LoCoMo-tunable hyperparameter.
func (s *RememberService) SetDefaultActivation(a float64) { s.defaultActivation = a }

// NewRememberService builds the service over the scoped store, embedder, and resolver.
func NewRememberService(store domain.VectorStore, embedder domain.Embedder, scopes WriteScopeResolver) *RememberService {
	return &RememberService{store: store, embedder: embedder, scopes: scopes, defaultActivation: 0.5}
}

// Remember embeds text and writes a mnemonic-fact document for the agent. hint is a
// narrow-only classification hint (can only restrict the kernel-derived tags).
// Returns the new document ID. An unknown principal is rejected (ErrUnknownPrincipal);
// a coined hint tag is rejected (scope.ErrUnknownClassification).
func (s *RememberService) Remember(ctx context.Context, agentID, text string, hint []string, source, sessionID string, importance float64) (string, error) {
	if agentID == "" {
		return "", ErrUnknownPrincipal
	}
	if _, ok := s.scopes.EffectiveForAgent(ctx, agentID); !ok {
		return "", ErrUnknownPrincipal
	}
	vec, err := s.embedder.Embed(ctx, text)
	if err != nil {
		return "", fmt.Errorf("remember: embed: %w", err)
	}
	// ADR-0015 lifecycle metric: an explicit importance hint sets the initial
	// activation; otherwise fall back to the config default. A 0 here makes the fact
	// unrecallable (its floor-multiplier score can't clear RecallSimilarityFloor), so
	// the caller's intent ("remember this") MUST translate to a recallable activation.
	activation := importance
	if activation <= 0 {
		activation = s.defaultActivation
	}
	if activation < 0 {
		activation = 0
	} else if activation > 1 {
		activation = 1
	}
	docID := uuid.New().String()
	doc := &domain.Document{
		ID:                 docID,
		Text:               text,
		DocumentType:       domain.DocTypeMnemonicFact,
		ActivationStrength: activation,
		Embedding:          domain.Embedding{Vector: vec},
		Metadata: map[string]interface{}{
			"tags":            hint, // narrow-only hint; writer derives the real classification
			"source_agent_id": source,
			"session_id":      sessionID,
		},
	}
	// Seed the writer scope so the ScopedStoreWriter derives classification (C2) and
	// stamps provenance. DefaultWriteTags is the operator-configured ceiling.
	wctx := scope.WithWriterScope(ctx, scope.WriterScope{
		WriterID:         agentID,
		DefaultWriteTags: s.scopes.DefaultWriteTags(ctx, agentID),
	})
	if err := s.store.Save(wctx, doc); err != nil {
		return "", err
	}
	// ADR-0052: enqueue the doc for batched LLM-based graph enrichment.
	// The batcher is non-blocking; if the queue is full the doc is still
	// saved (durability path) and only the graph enrichment is dropped.
	if s.edgeBatcher != nil {
		s.edgeBatcher.Enqueue(doc)
	}
	// ADR-0053 D2 (revised): enqueue the doc for per-chunk (h, r, t)
	// extraction. The producer is the kg_extractor system agent when
	// execution.kg_extractor_enabled is true (metadata + spacy_patterns,
	// no LLM); otherwise the LLM residue adapter. Enqueue is non-blocking;
	// the chunk_triplets enrichment is the lossy layer.
	if s.chunkTripletsBatcher != nil {
		s.chunkTripletsBatcher.Enqueue(doc)
	}
	if s.bus != nil {
		_ = s.bus.Publish(domain.MemoryWrittenEvent{
			DocID:     docID,
			DocType:   string(domain.DocTypeMnemonicFact),
			SessionID: sessionID,
			Source:    source,
			Summary:   summarize(text),
		})
	}
	return docID, nil
}

// summarize returns a short, single-line preview of a memory's text for the
// operator feed (the full body is fetched on demand, never on the feed).
func summarize(text string) string {
	const max = 140
	s := text
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
