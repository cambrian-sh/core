package memory

import (
	"context"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/scope"
)

// LLMGeneralizer is a scope.Generalizer that asks an LLM to distill one anonymized,
// aggregate insight from a theme cluster. The LLM is confined to GENERALIZATION; it
// is never the security boundary — the deterministic scope filter, k-anonymity
// floor, and mandatory regex PII scrub own promotion safety. ADR-0034 (D11).
type LLMGeneralizer struct {
	gen domain.Generator
}

// NewLLMGeneralizer builds a generalizer over an LLM generator.
func NewLLMGeneralizer(gen domain.Generator) *LLMGeneralizer {
	return &LLMGeneralizer{gen: gen}
}

// ExtractInsight prompts the LLM for a single anonymized, non-personal aggregate
// statement describing the common theme across the cluster's documents.
func (g *LLMGeneralizer) ExtractInsight(ctx context.Context, docs []domain.Document) (string, error) {
	var b strings.Builder
	b.WriteString("You are a privacy-preserving analyst. From the records below, write ONE concise, ")
	b.WriteString("aggregate insight describing the common theme. Do NOT include any names, emails, ")
	b.WriteString("phone numbers, IDs, or any detail specific to a single record. Output one sentence.\n\nRECORDS:\n")
	for i, d := range docs {
		if i >= 50 { // bound prompt size
			break
		}
		b.WriteString("- ")
		b.WriteString(d.Text)
		b.WriteString("\n")
	}
	return g.gen.Generate(ctx, b.String())
}

// ConsolidatorReader is a scope.Tier0Reader that reads raw Tier-0 documents under
// the kernel-defined ScopeConsolidator profile (ADR-0034 D11). Tag filtering is
// enforced by the scope predicate: only docs carrying a raw tag pass, and
// secrets/internal_only/PII are excluded from the read set entirely.
type ConsolidatorReader struct {
	store domain.VectorStore
	topK  int
}

// NewConsolidatorReader builds a reader. topK<=0 defaults to 500 (a batch scan).
func NewConsolidatorReader(store domain.VectorStore, topK int) *ConsolidatorReader {
	if topK <= 0 {
		topK = 500
	}
	return &ConsolidatorReader{store: store, topK: topK}
}

// ReadTier0 returns scope-filtered Tier-0 docs in the lookback window. The nil
// query vector makes this a recency-ordered batch scan (not ANN).
func (r *ConsolidatorReader) ReadTier0(ctx context.Context, since time.Time) ([]domain.Document, error) {
	eff := domain.NewEffectiveScope(domain.ScopeConfig{}, domain.ScopeConsolidator)
	results, err := r.store.Search(ctx, nil, domain.SearchOptions{
		Scope: &eff,
		Since: since,
		TopK:  r.topK,
	})
	if err != nil {
		return nil, err
	}
	docs := make([]domain.Document, 0, len(results))
	for _, res := range results {
		docs = append(docs, res.Document)
	}
	return docs, nil
}

// ConsolidatorWriter is a scope.PromotionWriter that embeds the derived insight and
// upserts it keyed by the cluster dedup key (doc ID = key → idempotent upsert). It
// writes through the ScopedStoreWriter under a ScopeConsolidator WriterScope, so
// the same write-side validation + provenance stamping that guards agents also
// guards promotion (ADR-0034 D8/R3).
type ConsolidatorWriter struct {
	store    domain.VectorStore // should be the ScopedStoreWriter
	embedder domain.Embedder
}

// NewConsolidatorWriter builds a writer over the (scoped) store and embedder.
func NewConsolidatorWriter(store domain.VectorStore, embedder domain.Embedder) *ConsolidatorWriter {
	return &ConsolidatorWriter{store: store, embedder: embedder}
}

// UpsertInsight embeds the insight text (if needed), keys the document by the
// dedup key for idempotency, and writes it under a ScopeConsolidator writer scope.
func (w *ConsolidatorWriter) UpsertInsight(ctx context.Context, key string, doc *domain.Document) error {
	doc.ID = key
	if doc.DocumentType == "" {
		doc.DocumentType = domain.DocTypeMnemonicFact
	}
	if len(doc.Embedding.Vector) == 0 && w.embedder != nil {
		vec, err := w.embedder.Embed(ctx, doc.Text)
		if err != nil {
			return err
		}
		doc.Embedding = domain.Embedding{Vector: vec}
	}
	// ADR-0035 C2: the kernel derives the promoted doc's classification from the
	// Consolidator's DefaultWriteTags (company_wide/analytics/derived) — not from
	// agent-chosen tags. No narrow hint is passed.
	wctx := scope.WithWriterScope(ctx, scope.WriterScope{
		WriterID:         "ConsolidatorAgent",
		DefaultWriteTags: domain.ScopeConsolidatorWriteTags,
	})
	return w.store.Save(wctx, doc)
}
