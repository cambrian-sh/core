// Package memory — Recursive Character Chunker.
//
// RecursiveCharacterChunker is the LangChain-style recursive separator
// splitter (ADR-0060 D2, T-1.5). It walks a four-level separator
// hierarchy — "\n\n" → "\n" → " " → "" — and at each level tries the
// current separator; if splitting on it produces only one piece, the
// chunker recurses to the next level on the whole text. When a piece
// still exceeds chunk_size after exhausting the separators, the
// chunker recurses on the piece at the next level. At the bottom
// (level 3, empty separator), it hard-cuts the text into chunk_size
// chunks.
//
// Tunables:
//   - chunkSize: target max chars per chunk. 0 → default 200.
//   - overlap: trailing chars of chunk[i] prepended to chunk[i+1]. 0
//     → no overlap (the spec default). overlap > 0 is the
//     small-to-big retrieval floor.
package memory

import (
	"context"
	"strings"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

const (
	recursiveDefaultChunkSize = 200
	recursiveDefaultOverlap   = 0
	recursiveSep0             = "\n\n"
	recursiveSep1             = "\n"
	recursiveSep2             = " "
	recursiveSep3             = "" // the hard-cut fallback
)

// RecursiveCharacterChunker splits text with the LangChain recursive
// strategy. Construct with NewRecursiveCharacterChunker; zero-value
// is treated as a "use defaults" instance by Chunk.
type RecursiveCharacterChunker struct {
	chunkSize int
	overlap   int
}

// NewRecursiveCharacterChunker returns a chunker with the given
// tunables. A chunkSize of 0 is replaced with the package default
// (200); overlap of 0 stays 0 (no overlap).
func NewRecursiveCharacterChunker(chunkSize, overlap int) RecursiveCharacterChunker {
	if chunkSize <= 0 {
		chunkSize = recursiveDefaultChunkSize
	}
	if overlap < 0 {
		overlap = 0
	}
	return RecursiveCharacterChunker{chunkSize: chunkSize, overlap: overlap}
}

// Name is the registry key. Stable.
func (RecursiveCharacterChunker) Name() string { return "recursive_character" }

// Supports returns true for every (sourceType, ext). Recursive is a
// general-purpose chunker and is safe to route any document to.
func (RecursiveCharacterChunker) Supports(sourceType, ext string) bool {
	_ = sourceType
	_ = ext
	return true
}

// Chunk applies the recursive split. Returns []Chunk{} for an empty
// or whitespace-only body (the spec: no chunks from a blank input,
// unlike Option C which returns one empty chunk).
func (c RecursiveCharacterChunker) Chunk(ctx context.Context, doc *domain.ExternalDocument) ([]domain.Chunk, error) {
	_ = ctx
	if doc == nil {
		return nil, nil
	}
	body := strings.TrimSpace(doc.Body)
	if body == "" {
		return []domain.Chunk{}, nil
	}
	if c.chunkSize <= 0 {
		c.chunkSize = recursiveDefaultChunkSize
	}
	pieces := splitRecursive(body, 0, c.chunkSize)
	if c.overlap > 0 && len(pieces) > 1 {
		pieces = applyOverlap(pieces, c.overlap)
	}
	total := len(pieces)
	out := make([]domain.Chunk, total)
	for i, p := range pieces {
		out[i] = domain.Chunk{
			Body: p,
			Metadata: map[string]any{
				"chunk_index":  i,
				"total_chunks": total,
			},
		}
	}
	return out, nil
}

// splitRecursive walks the separator hierarchy. level 0 → "\n\n",
// level 1 → "\n", level 2 → " ", level 3 → hard cut.
func splitRecursive(text string, level, chunkSize int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if level >= 3 {
		return hardCut(text, chunkSize)
	}
	sep := []string{recursiveSep0, recursiveSep1, recursiveSep2, recursiveSep3}[level]
	pieces := strings.Split(text, sep)
	if len(pieces) == 1 {
		// Separator not found at this level; recurse deeper on the
		// whole text.
		return splitRecursive(text, level+1, chunkSize)
	}
	var out []string
	for _, p := range pieces {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len(p) > chunkSize {
			out = append(out, splitRecursive(p, level+1, chunkSize)...)
			continue
		}
		out = append(out, p)
	}
	return out
}

// hardCut is the level-3 floor: slice text into chunkSize chunks.
// chunkSize <= 0 or text shorter than chunkSize → single chunk.
func hardCut(text string, size int) []string {
	if size <= 0 || len(text) <= size {
		return []string{text}
	}
	var out []string
	for i := 0; i < len(text); i += size {
		end := i + size
		if end > len(text) {
			end = len(text)
		}
		out = append(out, text[i:end])
	}
	return out
}

// applyOverlap prepends the last `overlap` chars of pieces[i-1] to
// pieces[i]. The first chunk is left untouched. If overlap >= len of
// the previous chunk, the whole previous chunk is prepended.
func applyOverlap(pieces []string, overlap int) []string {
	if overlap <= 0 || len(pieces) <= 1 {
		return pieces
	}
	out := make([]string, 0, len(pieces))
	out = append(out, pieces[0])
	for i := 1; i < len(pieces); i++ {
		prev := pieces[i-1]
		var prefix string
		if len(prev) <= overlap {
			prefix = prev
		} else {
			prefix = prev[len(prev)-overlap:]
		}
		out = append(out, prefix+pieces[i])
	}
	return out
}
