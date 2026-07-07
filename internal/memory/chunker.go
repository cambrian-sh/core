// This file is a back-compat shim. The real chunker is in
// internal/memory/option_c.go. New code should use OptionCChunker
// directly via the chunker_registry (see T-1.8).
package memory

import (
	"context"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// ChunkDocument is the legacy back-compat entry point. It delegates
// to OptionCChunker (ADR-0060 D2, T-1.4). In-tree callers should
// migrate to chunker_registry.Resolve(sourceType, ext).Chunk(...).
func ChunkDocument(doc domain.ExternalDocument) []domain.Chunk {
	chunks, _ := OptionCChunker{}.Chunk(context.Background(), &doc)
	return chunks
}
