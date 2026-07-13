package memory

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// EdgeWriter is the LLM-driven edge populator. It runs synchronously inside
// IngestMemory, after the document is saved, and best-effort populates the
// `document_edges` table with the entities and relations the LLM observes in
// the fact text. Errors are logged, never returned: a fact must not fail to
// remember just because the graph extractor hiccuped.
type EdgeWriter struct {
	extractor  *EdgeExtractor
	graph      domain.GraphStore
	index      *EntityIndex
	embedder   domain.Embedder
	now        func() time.Time // injectable for tests
	enrichCost time.Duration    // warn threshold for slow extractions
}

// NewEdgeWriter builds the writer. embedder is optional; when set, the writer
// embeds entity names so the recall path can find the top-3 entity neighbors
// for a query (the T-Mem "first hop").
func NewEdgeWriter(extractor *EdgeExtractor, graph domain.GraphStore, index *EntityIndex, embedder domain.Embedder) *EdgeWriter {
	return &EdgeWriter{
		extractor:  extractor,
		graph:      graph,
		index:      index,
		embedder:   embedder,
		now:        time.Now,
		enrichCost: 2 * time.Second,
	}
}

// SetEnrichWarn sets the duration above which the writer logs a slow-extraction
// warning. Default 2s.
func (w *EdgeWriter) SetEnrichWarn(d time.Duration) { w.enrichCost = d }

// WriteForDoc runs entity+relation extraction on doc.Text, normalizes the
// output, writes the resulting edges to the graph store, and updates the
// in-memory EntityIndex. The function is best-effort: any non-fatal error is
// logged and swallowed. A nil doc or empty text is a no-op. PR1 legacy path:
// kept for the sync single-fact mode; the async batched path uses
// WriteExtraction directly after a batched LLM call.
func (w *EdgeWriter) WriteForDoc(ctx context.Context, doc *domain.Document) {
	if w == nil || w.extractor == nil || w.graph == nil || doc == nil || doc.Text == "" {
		return
	}
	ext, err := w.extractor.Extract(ctx, doc.Text)
	if err != nil {
		slog.Warn("EdgeWriter: extraction failed, continuing without edges",
			"doc_id", doc.ID, "err", err)
		return
	}
	w.WriteExtraction(ctx, doc, ext)
}

// WriteExtraction is the LLM-free half of WriteForDoc: given a pre-computed
// extraction, it writes the doc→entity edges to the graph store, updates the
// in-memory EntityIndex, and embeds entity names for top-K recall. Used by
// EdgeBatcher after a single batched LLM call produces N extractions.
//
// Best-effort: a nil doc, empty doc.Text, or empty extraction is a no-op.
// Slow-call warning is not emitted here (the batcher owns the LLM-cost
// timing, not the per-doc write cost).
func (w *EdgeWriter) WriteExtraction(ctx context.Context, doc *domain.Document, ext Extraction) {
	if w == nil || w.graph == nil || doc == nil || doc.Text == "" {
		return
	}
	if len(ext.Entities) == 0 && len(ext.Relations) == 0 {
		return
	}
	now := w.now()
	w.writeEntities(ctx, doc, ext, now)
	w.writeRelations(ctx, doc, ext, now)
	if w.index != nil {
		w.indexEntityEmbeddings(ctx, ext, now.UnixNano())
	}
}

// writeEntities updates the in-memory EntityIndex for every extracted entity.
// The persistent document_edges write is disabled — the kg_extractor system
// agent owns graph persistence via chunk_triplets (ADR-0053 D2 revised).
func (w *EdgeWriter) writeEntities(ctx context.Context, doc *domain.Document, ext Extraction, now time.Time) {
	if w.index == nil {
		return
	}
	for _, e := range ext.Entities {
		key := canonicalKey(e.Kind, e.Name)
		if key == "" {
			continue
		}
		w.index.Add(key, doc.ID, e.Confidence, e.Kind, now.UnixNano())
	}
}

// writeRelations records every extracted relation in the in-process audit
// trail (slog) but does NOT write a separate edge to the graph. The
// relation is implicit in the entity index: a doc that mentions both X and
// Y connects them; a query that finds X can hop to Y through the shared
// doc's entity edges. Writing a doc→entity "relation-source" edge would
// collide with the entity-mention edge (same (source, target, edgeType)
// key) and overwrite the mention's weight.
//
// The LLM's verb phrase (e.g. "researched", "is_friend_of") is logged
// here so the operator feed shows what the graph would have looked like
// if we had a richer label-aware edge model. ADR-0052.
func (w *EdgeWriter) writeRelations(ctx context.Context, doc *domain.Document, ext Extraction, now time.Time) {
	if len(ext.Relations) == 0 {
		return
	}
	seen := make(map[string]bool, len(ext.Relations))
	relCount := 0
	for _, r := range ext.Relations {
		if !IsEntityKey(r.Source) || !IsEntityKey(r.Target) {
			continue
		}
		dedupKey := r.Source + "\x00" + r.Target
		if seen[dedupKey] {
			continue
		}
		seen[dedupKey] = true
		relCount++
	}
	if relCount > 0 {
		slog.InfoContext(ctx, "EdgeWriter: implicit relations recorded (no separate edge)",
			"doc_id", doc.ID, "relation_count", relCount)
	}
}

// indexEntityEmbeddings embeds the canonical display name of every distinct
// entity the extraction emitted and stores the result in the in-memory index.
// Best-effort; missing embedder is a no-op (the recall path falls back to
// surface matching).
func (w *EdgeWriter) indexEntityEmbeddings(ctx context.Context, ext Extraction, nowNano int64) {
	if w.embedder == nil || w.index == nil {
		return
	}
	type distinct struct {
		key  string
		name string
	}
	seen := make(map[string]bool)
	for _, e := range ext.Entities {
		key := canonicalKey(e.Kind, e.Name)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		displayName := e.Name
		emb, err := w.embedder.Embed(ctx, displayName)
		if err != nil {
			slog.Warn("EdgeWriter: entity-name embed failed",
				"entity", key, "err", err)
			continue
		}
		w.index.SetNameEmbedding(key, displayName, domain.Embedding{Vector: emb})
	}
}

// canonicalKey builds the canonical entity key from a meta-kind and name.
// Empty / whitespace-only names return "". The recall path's IsEntityKey
// uses the meta-kind prefix to distinguish entity targets from doc targets.
func canonicalKey(kind EntityMetaKind, name string) string {
	name = trimSpace(name)
	if name == "" {
		return ""
	}
	if !ValidMetaKinds[kind] {
		return ""
	}
	return string(kind) + ":" + name
}

// trimSpace is a tiny local helper to avoid pulling strings in just for this.
func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// ErrExtractorMissing is returned by the writer's constructor when the
// extractor is nil. It's here so the boot path can fail loudly rather than
// silently producing an empty graph.
var ErrExtractorMissing = fmt.Errorf("memory: EdgeWriter requires a non-nil EdgeExtractor")
