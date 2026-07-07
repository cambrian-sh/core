// Package domain holds the hexagonal ports for Cambrian's runtime.
//
// This file declares the Chunker port and the Chunk value type
// (ADR-0060 D2). Implementations live in
// cambrian-core/internal/memory/chunkers/; the registry at
// cambrian-core/internal/memory/chunker_registry.go routes a
// (SourceType, extension) pair to a registered Chunker. The
// IngestionManager is responsible for filling in the
// Metadata["chunk_relations"] subkey AFTER a chunker returns —
// Chunk itself is free to return chunks with a nil Metadata map,
// and the port contract requires that reading a missing key from
// such a map does not panic.
package domain

import "context"

// Chunker is the port for splitting a document into chunks. Implementations live in
// internal/memory/chunkers/; the registry at internal/memory/chunker_registry.go
// routes SourceType + extension to the right Chunker.
type Chunker interface {
	// Name returns the chunker's identifier (e.g. "option_c", "recursive_character").
	// Names are stable; renaming is a breaking change to the chunker config schema.
	Name() string

	// Supports returns true if this chunker can handle a document of the given
	// SourceType and/or file extension. The registry uses this for routing.
	// Either argument may be empty.
	Supports(sourceType string, ext string) bool

	// Chunk splits the document body into chunks. Each chunk carries a Body
	// (the text) and Metadata (a free-form map; the "chunk_relations" subkey
	// is set by the IngestionManager after Chunk returns).
	Chunk(ctx context.Context, doc *ExternalDocument) ([]Chunk, error)
}

// Chunk is one embeddable segment produced from an ExternalDocument.
// The Metadata map is intentionally free-form; the "chunk_relations" subkey
// is reserved for the parent_entity_id / linear / sibling_context data
// (set by IngestionManager; see internal/memory/chunk_relations.go).
type Chunk struct {
	Body     string
	Metadata map[string]any
}
