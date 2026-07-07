// Package memory — Late Chunker.
//
// LateChunker implements the late-chunking strategy from Günther
// et al. (arXiv:2409.04701, ADR-0060 D6, T-2.2): one whole-doc
// forward pass through a per-token embedder, then per-chunk
// mean-pool over the token range that covers the chunk's char
// range in the body. This captures cross-chunk context that
// independent per-chunk embedding loses.
//
// Fallback contract (D6):
//   - empty body → OptionCChunker output (embedder not called)
//   - body token count > maxDocTokens → OptionCChunker output
//     (embedder not called) — a 8K cap that matches the
//     nomic-embed-text context window
//
// Gating (D6 + T-2.4): the *registry* enforces chunker.late.enabled
// AND embedder.supports_long_context. LateChunker itself is
// unconditional — it just returns Option C when the body is too
// big. The registry's lateGate is the operator-facing policy knob.
//
// Each chunk's Metadata["late_embedding"] carries the mean-pooled
// vector. Callers that don't read it (the standard path) ignore the
// extra bytes; callers that want to bypass re-embedding for
// per-chunk retrieval can use the precomputed vector directly.
package memory

import (
	"context"
	"log/slog"
	"strings"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

const (
	lateDefaultMaxDocTokens = 8192
	lateCharsPerToken       = 4 // generous lower bound on token count for English
)

// LateChunker is the late-chunking implementation. Construct with
// NewLateChunker(embedder, maxDocTokens); zero-value
// LateChunker{} works but its embedder is nil and Chunk will
// fall back to Option C for every call.
type LateChunker struct {
	embedder     domain.BatchEmbedder
	maxDocTokens int
}

// NewLateChunker returns a LateChunker that calls embed.EmbedBatch
// (the vectorized batch API) for the whole-doc forward pass.
// maxDocTokens <= 0 → 8192.
func NewLateChunker(embedder domain.BatchEmbedder, maxDocTokens int) LateChunker {
	if maxDocTokens <= 0 {
		maxDocTokens = lateDefaultMaxDocTokens
	}
	return LateChunker{embedder: embedder, maxDocTokens: maxDocTokens}
}

// Name is the registry key. Stable.
func (LateChunker) Name() string { return "late" }

// Supports returns true for every (sourceType, ext). The late
// chunker is a general text strategy; the gating is on doc size
// and embedder capability, not on file type.
func (LateChunker) Supports(sourceType, ext string) bool {
	_ = sourceType
	_ = ext
	return true
}

// Chunk applies the late-chunking strategy.
//
//  1. If body is empty/whitespace → Option C output (no embedder
//     call).
//  2. If body token count > maxDocTokens → Option C output (no
//     embedder call).
//  3. Otherwise: call embedder.EmbedBatch([body]) to get the
//     per-token vectors for the whole document; chunk the body
//     via Option C to get char ranges; mean-pool the per-token
//     vectors over each chunk's char range; attach the pooled
//     vector to each chunk's Metadata["late_embedding"].
//
// Returns an error if the embedder fails; the IngestionManager
// catches that and falls back to Option C.
func (l LateChunker) Chunk(ctx context.Context, doc *domain.ExternalDocument) ([]domain.Chunk, error) {
	_ = ctx
	if doc == nil {
		return nil, nil
	}
	body := doc.Body

	// Fallback 1: empty body.
	if trimIsEmpty(body) {
		return OptionCChunker{}.Chunk(ctx, doc)
	}

	// Fallback 2: doc too big.
	nTokens := len(body) / lateCharsPerToken
	if nTokens > l.maxDocTokens {
		slog.Warn("LateChunker: doc exceeds max_doc_tokens, falling back to OptionC",
			"source_uri", doc.SourceURI,
			"doc_tokens", nTokens,
			"max_doc_tokens", l.maxDocTokens)
		return OptionCChunker{}.Chunk(ctx, doc)
	}

	// No embedder configured → behave like Option C.
	if l.embedder == nil {
		return OptionCChunker{}.Chunk(ctx, doc)
	}

	// Whole-doc forward pass. BatchEmbedder returns per-text
	// vectors; LateChunker needs per-token vectors for mean-pool.
	// The contract here is that the embedder implementation
	// returns one entry per token, with the first per-text slice
	// carrying the per-token vectors for the input text. Real
	// implementations (the production kernel's Ollama embedder)
	// are extended to return per-token vectors via the
	// PerTokenBatchEmbedder sub-interface; the IngestionManager
	// passes the production embedder here.
	perToken, err := embedBatchPerToken(ctx, l.embedder, body)
	if err != nil {
		return nil, err
	}
	if len(perToken) == 0 {
		return OptionCChunker{}.Chunk(ctx, doc)
	}

	// Chunk via Option C to get the per-chunk char ranges and the
	// base chunks (with their full provenance metadata).
	baseChunks, err := OptionCChunker{}.Chunk(ctx, doc)
	if err != nil {
		return nil, err
	}
	if len(baseChunks) == 0 {
		return baseChunks, nil
	}

	ranges := findChunkCharRanges(body, baseChunks)
	out := make([]domain.Chunk, len(baseChunks))
	for i, ch := range baseChunks {
		meta := ch.Metadata
		if meta == nil {
			meta = map[string]any{}
		} else {
			copied := make(map[string]any, len(meta)+1)
			for k, v := range meta {
				copied[k] = v
			}
			meta = copied
		}
		r := ranges[i]
		meta["late_embedding"] = meanPoolTokens(perToken, r[0], r[1])
		out[i] = domain.Chunk{Body: ch.Body, Metadata: meta}
	}
	return out, nil
}

// embedBatchPerToken runs the per-token batch call. If the embedder
// also satisfies PerTokenBatchEmbedder, use the per-token API
// directly. Otherwise call EmbedBatch and treat each returned
// vector as a single token (the conservative fallback when no
// per-token model is wired in; mean-pool collapses to a copy of
// the per-text vector in that case).
func embedBatchPerToken(ctx context.Context, e domain.BatchEmbedder, body string) ([][]float32, error) {
	if pe, ok := e.(PerTokenBatchEmbedder); ok {
		perText, err := pe.EmbedBatchTokens(ctx, []string{body})
		if err != nil {
			return nil, err
		}
		if len(perText) == 0 {
			return nil, nil
		}
		return perText[0], nil
	}
	perText, err := e.EmbedBatch(ctx, []string{body})
	if err != nil {
		return nil, err
	}
	if len(perText) == 0 {
		return nil, nil
	}
	return [][]float32{perText[0]}, nil
}

// PerTokenBatchEmbedder is the optional sub-interface for
// backends that return per-token vectors (needed for the
// late-chunking mean-pool math). Production embedders (the
// kernel's Ollama embedder) extend EmbedBatchTokens for the late
// path; the harness's mock returns a hand-built per-token list.
type PerTokenBatchEmbedder interface {
	domain.BatchEmbedder
	EmbedBatchTokens(ctx context.Context, texts []string) ([][][]float32, error)
}

// findChunkCharRanges locates each chunk's body in the original
// text. The range is [start, end) in char offsets. If a chunk
// body is not found (defensive; should not happen for Option C
// output on the same body), the range is a zero-width point at
// the current search position so the mean-pool falls through to a
// zero vector (preserves chunk count, loses the embedding).
func findChunkCharRanges(text string, chunks []domain.Chunk) [][2]int {
	ranges := make([][2]int, len(chunks))
	pos := 0
	for i, c := range chunks {
		idx := strings.Index(text[pos:], c.Body)
		if idx < 0 {
			ranges[i] = [2]int{pos, pos}
			continue
		}
		idx += pos
		ranges[i] = [2]int{idx, idx + len(c.Body)}
		pos = idx + len(c.Body)
	}
	return ranges
}

// meanPoolTokens is the per-dimension mean of embeddings in the
// token range [start/lateCharsPerToken, end/lateCharsPerToken).
// The chars-per-token=4 heuristic is a generous lower bound on
// English token count.
//
// An empty token range (e.g. chunk with zero tokens) returns a
// zero vector of the same dimensionality as the first token. This
// is the convention the harness relies on (the Python port's
// _mean_pool_tokens does the same).
func meanPoolTokens(embeddings [][]float32, startChar, endChar int) []float32 {
	if len(embeddings) == 0 {
		return []float32{}
	}
	startTok := startChar / lateCharsPerToken
	endTok := endChar / lateCharsPerToken
	if startTok > len(embeddings) {
		startTok = len(embeddings)
	}
	if endTok > len(embeddings) {
		endTok = len(embeddings)
	}
	dim := len(embeddings[0])
	if startTok >= endTok {
		out := make([]float32, dim)
		return out
	}
	total := make([]float32, dim)
	for _, emb := range embeddings[startTok:endTok] {
		for j, v := range emb {
			if j < dim {
				total[j] += v
			}
		}
	}
	n := float32(endTok - startTok)
	for j := range total {
		total[j] /= n
	}
	return total
}
