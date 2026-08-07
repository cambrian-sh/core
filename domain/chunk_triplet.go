package domain

import "context"

// ChunkTriplet is one (h, r, t) triple extracted from a chunk's text. This is the
// per-chunk KG that the KG²RAG retrieval pattern walks. The h and t are free-form
// entity strings (canonicalized to lowercase on insert); the r is a free-form verb
// phrase.
//
// It lives in the domain, not in the memory adapter, because it is an ENTITY that
// crosses three packages: the extraction adapter produces it, the persistence
// adapter stores it, and the retrieval path reads it. Two of those adapters
// (internal/substrate/network, internal/infrastructure/postgres) previously
// imported internal/memory purely to reach this type and the two ports below —
// an adapter depending on another adapter for a domain concept.
type ChunkTriplet struct {
	H      string  // head entity (canonicalized: lowercase, trimmed)
	R      string  // relation (free-form verb phrase; e.g., "researched", "born in")
	T      string  // tail entity (canonicalized)
	Weight float64 // extractor's per-triple weight, [0, 1]; 1.0 if not reported
	// ADR-0053 D2 (revised): provenance + agreement tier from the tiered extractor.
	// Sources: producers, subset of {metadata, spacy_patterns, llm}; nil = legacy.
	// Confidence: 2=high / 1=low / 0=filler; nil = unset/legacy (persisted NULL).
	Sources    []string `json:"sources,omitempty"`
	Confidence *int     `json:"confidence,omitempty"`
}

// TripletExtractor is the port the chunk-triplet batcher depends on to turn a
// batch of chunk texts into per-chunk (h, r, t) triplets (ADR-0053 D2 revised).
//
// Its adapter is the kg_extractor system-agent dispatcher
// (internal/substrate/network) — the deterministic metadata + spacy_patterns
// tiers, injected at bootstrap.
//
// ExtractBatch returns one []ChunkTriplet per input text, positionally aligned
// (texts[i] -> out[i]); a blank/failed position yields an empty slice. ids[i] is
// the chunk's document id, positionally aligned with texts — the deterministic
// adapter uses it to anchor structural (metadata) triplets to the real chunk.
type TripletExtractor interface {
	ExtractBatch(ctx context.Context, texts []string, ids []string) [][]ChunkTriplet
}

// ChunkTripletsStore is the storage port for the per-chunk triplets produced at
// write time. The KG²RAG retrieval pattern (ADR-0053 D3) reads these at query
// time for one-hop chunk expansion.
//
// The implementation lives in the postgres adapter; this port is what the
// retrieval path depends on so it can be faked in unit tests.
type ChunkTripletsStore interface {
	// SaveChunkTriplets persists a list of triplets for a single chunk. Idempotent
	// on (chunk_id, h, r, t) — repeated inserts are no-ops.
	SaveChunkTriplets(ctx context.Context, chunkID string, triplets []ChunkTriplet) error

	// ForChunk returns the triplets extracted from a chunk (h, r, t, weight).
	ForChunk(ctx context.Context, chunkID string) ([]ChunkTriplet, error)

	// ForChunks batches the above — returns a map chunkID -> []ChunkTriplet.
	// Used by the KG expansion post-processor to walk many seed chunks in
	// one query.
	ForChunks(ctx context.Context, chunkIDs []string) (map[string][]ChunkTriplet, error)

	// ChunksMentioningEntity returns the chunk IDs that have a triplet
	// referencing the given entity (as either head or tail). This is the
	// "entity → chunks" lookup that powers the KG expansion.
	// Matching is case-insensitive (entities are stored lowercase).
	//
	// `scope` is the caller's read predicate and is REQUIRED (ADR-0095 D9):
	// chunk_triplets carries no classification, so the implementation applies the
	// predicate by joining the chunk rows. A nil predicate returns nothing — this
	// lookup is reached directly from kgExpand, with no chokepoint in front of it.
	ChunksMentioningEntity(ctx context.Context, entity string, limit int, scope *TagPredicate) ([]string, error)
}
