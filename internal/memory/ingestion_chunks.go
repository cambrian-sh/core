package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

const defaultExternalActivation = 0.5

func (im *IngestionManager) persistChunks(
	ctx context.Context,
	doc domain.ExternalDocument,
	chunks []domain.Chunk,
	entityID string,
	scene string,
) (int, error) {
	if im.agent == nil || im.agent.Manager == nil || im.agent.Manager.Store == nil {
		return 0, fmt.Errorf("ingestion manager: vector store is not configured")
	}
	documentID := externalDocumentID(doc)

	// ADR-0060 leaves-as-chunks: when structure parsing is on, the parser's leaves
	// ARE the chunk set, so chunk boundaries match the hierarchy exactly and every
	// chunk's section stamp is correct by construction. Falls back to the flat
	// chunker's chunks when parsing is off, fails, or yields no leaf content.
	var structuredDoc *StructuredDocument
	var structuredReps []StructNode
	if im.structureParser != nil && im.structureStore != nil {
		if parsed, perr := im.structureParser.Parse(ctx, StructureParseRequest{
			DocID: documentID, Title: doc.Title, SourceType: doc.SourceType, Text: doc.Body,
		}); perr != nil {
			slog.WarnContext(ctx, "IngestionManager: structure parse failed; flat chunking", "doc", documentID, "err", perr)
		} else if parsed != nil {
			if lc, reps := ChunksFromLeaves(parsed); len(lc) > 0 {
				chunks = lc
				structuredDoc = parsed
				structuredReps = reps
			}
		}
	}

	ids := make([]string, len(chunks))
	for i := range chunks {
		ids[i] = externalChunkID(documentID, i)
	}
	vectors, err := im.embedChunkBodies(ctx, doc, chunks)
	if err != nil {
		return 0, err
	}

	docs := make([]*domain.Document, 0, len(chunks))
	for i, chunk := range chunks {
		vec := vectors[i]
		if len(vec) == 0 {
			continue
		}
		docs = append(docs, &domain.Document{
			ID:                 ids[i],
			DocumentType:       domain.DocTypeMnemonicFact,
			Text:               chunk.Body,
			ActivationStrength: externalActivation(doc.Importance),
			Embedding:          domain.Embedding{Vector: vec, Model: "dynamic", Size: len(vec)},
			Metadata:           chunkMetadata(doc, chunks, chunk, documentID, ids, i, entityID, scene),
		})
	}
	if len(docs) == 0 {
		return 0, nil
	}
	if err := im.agent.Manager.Store.SaveBatch(ctx, docs); err != nil {
		return 0, fmt.Errorf("ingestion manager: save chunk batch: %w", err)
	}
	// ADR-0053: enqueue each saved chunk for per-chunk (h, r, t) + anchor
	// extraction. Non-blocking + nil-safe; the chunk doc is already persisted, so
	// a dropped enqueue only loses KG enrichment, never the chunk itself.
	if im.tripletsBatcher != nil {
		for _, d := range docs {
			im.tripletsBatcher.Enqueue(d)
		}
	}
	// ADR-0060: build the document-structure graph and stamp each chunk with its
	// inherited section path. Best-effort — parse/persist failures log and leave
	// the (already-saved) chunks without structure.
	if structuredDoc != nil && im.structureStore != nil {
		im.persistStructure(ctx, structuredDoc, documentID, ids, structuredReps)
	}
	return len(docs), nil
}

// persistStructure persists the structure graph (section nodes + PART_OF/NEXT
// edges) and per-chunk section stamps from an ALREADY-parsed document. With
// leaves-as-chunks, ids align exactly to sd.OrderedLeaves(). Best-effort.
func (im *IngestionManager) persistStructure(ctx context.Context, sd *StructuredDocument, documentID string, ids []string, reps []StructNode) {
	sections, stamps, edges := BuildStructureGraph(sd, documentID, ids, reps)
	if len(sections) == 0 && len(stamps) == 0 {
		return // flat document, no hierarchy to persist
	}
	if err := im.structureStore.SaveSections(ctx, sections); err != nil {
		slog.WarnContext(ctx, "IngestionManager: SaveSections failed", "doc", documentID, "err", err)
		return
	}
	if err := im.structureStore.SaveStructuralEdges(ctx, edges); err != nil {
		slog.WarnContext(ctx, "IngestionManager: SaveStructuralEdges failed", "doc", documentID, "err", err)
	}
	if err := im.structureStore.StampChunks(ctx, stamps); err != nil {
		slog.WarnContext(ctx, "IngestionManager: StampChunks failed", "doc", documentID, "err", err)
	}
	slog.InfoContext(ctx, "IngestionManager: structure graph built", "doc", documentID,
		"sections", len(sections), "stamped_chunks", len(stamps), "edges", len(edges), "backend", sd.Backend)
}

func (im *IngestionManager) embedChunkBodies(ctx context.Context, doc domain.ExternalDocument, chunks []domain.Chunk) ([][]float32, error) {
	texts := make([]string, len(chunks))
	for i, chunk := range chunks {
		texts[i] = chunk.Body
	}
	if batch, ok := im.embedder.(domain.BatchEmbedder); ok {
		vectors, err := batch.EmbedBatch(ctx, texts)
		if err == nil && len(vectors) == len(texts) {
			return vectors, nil
		}
		slog.Warn("IngestionManager: batch embed failed, falling back to per-chunk embed", "source_uri", doc.SourceURI, "err", err, "got_vectors", len(vectors), "want_vectors", len(texts))
	}
	vectors := make([][]float32, len(chunks))
	for i, chunk := range chunks {
		vec, err := im.embedder.Embed(ctx, chunk.Body)
		if err != nil {
			slog.Warn("IngestionManager: embed failed, skipping chunk", "source_uri", doc.SourceURI, "err", err)
			continue
		}
		vectors[i] = vec
	}
	return vectors, nil
}

func chunkMetadata(
	doc domain.ExternalDocument,
	chunks []domain.Chunk,
	chunk domain.Chunk,
	documentID string,
	ids []string,
	index int,
	entityID string,
	scene string,
) map[string]any {
	meta := make(map[string]any, len(chunk.Metadata)+8)
	for k, v := range chunk.Metadata {
		meta[k] = v
	}
	var prevID, nextID, prevBody, nextBody string
	if index > 0 {
		prevID = ids[index-1]
		prevBody = chunks[index-1].Body
	}
	if index < len(ids)-1 {
		nextID = ids[index+1]
		nextBody = chunks[index+1].Body
	}
	meta["snapshot"] = scene
	meta["document_id"] = documentID
	meta["chunk_id"] = ids[index]
	meta["source_doc_entity_id"] = entityID
	if doc.Author != "" {
		meta["source_agent"] = doc.Author
		meta["source_agent_id"] = doc.Author
	}
	if doc.ThreadID != "" {
		meta["session_id"] = doc.ThreadID
	}
	if len(doc.Tags) > 0 {
		meta["tags"] = append([]string(nil), doc.Tags...)
	}
	data, _ := json.Marshal(ChunkRelations{
		ParentEntityID:   entityID,
		PrecedingChunkID: prevID,
		FollowingChunkID: nextID,
		SiblingContext: SiblingContext{
			ParentTitle:      doc.Title,
			ParentScene:      scene,
			PrecedingSnippet: clip(prevBody, precedingSnippetMaxBytes),
			FollowingSnippet: clip(nextBody, followingSnippetMaxBytes),
		},
	})
	meta["chunk_relations"] = json.RawMessage(data)
	return meta
}

// externalDocumentID derives the STABLE, UNIQUE document id an item's chunks hang
// off of (chunk ids are "<documentID>-chunk-<n>"). "Unique" so two distinct items
// never collide onto the same chunk id and overwrite each other; "stable" so
// re-ingesting the same item resolves to the same id (idempotent — the sibling of
// content dedup).
func externalDocumentID(doc domain.ExternalDocument) string {
	// 1. Explicit caller-supplied id (multi-chunk uploads tagged source_document).
	//    Preserves the "<doc_id>-chunk-N" contract downstream consumers match on.
	for i, tag := range doc.Tags {
		if tag == "source_document" && i+1 < len(doc.Tags) && doc.Tags[i+1] != "" {
			return doc.Tags[i+1]
		}
	}
	for _, tag := range doc.Tags {
		if tag == "" || tag == "document-qa" || tag == "source_document" || strings.HasPrefix(tag, "chunker:") {
			continue
		}
		return tag
	}
	// 2. Threaded/streamed items (e.g. conversation turns) share ONE SourceURI
	//    across many ingests, so SourceURI alone is not a unique id — using it
	//    collapses every turn onto "<source>-chunk-1" and each ingest overwrites
	//    the previous one. Key on the thread plus a content digest: distinct turns
	//    get distinct ids; re-ingesting the same turn resolves to the same id.
	if doc.ThreadID != "" {
		return doc.ThreadID + ":" + contentDigest(doc.Body)
	}
	// 3. A standalone item identified by its source (e.g. a watched file whose path
	//    is unique and stable across edits). Keep SourceURI so a re-ingest updates
	//    in place rather than orphaning the old chunks.
	if doc.SourceURI != "" {
		return doc.SourceURI
	}
	// 4. No identity at all — hash the body so two unrelated items still differ.
	return "external-document:" + contentDigest(doc.Body)
}

// contentDigest is a short, stable hex digest of an item's body, used to make an
// otherwise-nonunique document id unique per distinct content (16 hex chars of
// SHA-256 — ample within a single thread/source).
func contentDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:8])
}

func externalChunkID(documentID string, index int) string {
	return fmt.Sprintf("%s-chunk-%d", documentID, index+1)
}

func externalActivation(importance float64) float64 {
	if importance <= 0 {
		return defaultExternalActivation
	}
	if importance > 1 {
		return 1
	}
	return importance
}
