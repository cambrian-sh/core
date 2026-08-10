package memory

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

const defaultExternalActivation = 0.5

// sourceDocumentMarker announces that the NEXT tag is the caller's document id
// (see externalDocumentID rule 1). It is wire protocol, not a classification, and
// classificationTags removes it before the tags are stored as labels.
// One definition of the identity convention, shared with the write chokepoints via
// domain.ClassificationHint (ADR-0099). Two copies of a marker that decides what
// counts as a document's name is how the two paths drift apart.
const sourceDocumentMarker = domain.SourceDocumentMarker

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

	// The tag list is overloaded: it carries a CLASSIFICATION *and*, by the
	// `source_document` convention above, an IDENTITY. Only the first is a label.
	//
	// Identity has to stay on the wire — `externalDocumentID` has just read it, and
	// dropping it at the caller renames every document to `<tag>:<content-digest>`.
	// But it must not survive as a *classification*, because access policy acts on
	// tags and never on a document by name (ADR-0085): a tag naming exactly one
	// document is a term no rule can usefully match, and one per document buries the
	// controlled vocabulary. Measured on the live store: 726 distinct tags against a
	// 12-term vocabulary, 710 of them a document's own id.
	//
	// So the split happens HERE, at the write chokepoint, and not in any caller: it is
	// the one place that knows both the full request and which tag actually became the
	// id. Every ingest path gets it, including the six benchmark suites that use the
	// convention, without any of them changing.
	doc.Tags = classificationTags(doc.Tags, documentID)

	// ADR-0093: record the document itself FIRST. Chunks and sections carry a foreign
	// key to it, so the parent has to exist before its children — and this is also the
	// row that now owns the classification tags, rather than N copies scattered across
	// chunk metadata.
	//
	// A failure here is logged and not fatal: losing the ingest of a whole document
	// because its entity row could not be written would be a worse outcome than chunks
	// with an unresolved parent, which still retrieve correctly.
	if im.documentStore != nil {
		stored, derr := im.documentStore.SaveDocument(ctx, domain.SourceDocument{
			ID:         documentID,
			Title:      doc.Title,
			SourceType: doc.SourceType,
			Text:       doc.Body,
			Tags:       doc.Tags,
		})
		if derr != nil {
			slog.WarnContext(ctx, "IngestionManager: document entity not recorded; chunks will have no parent",
				"doc", documentID, "err", derr)
		} else {
			// ADR-0093 D4: the chunk copies are a DERIVED CACHE, so they are derived from
			// the row they cache — not re-derived independently from the same request.
			//
			// The document's tags have just been through the write chokepoint, which may
			// have narrowed them. Carrying the original request forward would leave the
			// document saying one thing and its chunks another, which is precisely the
			// split-brain this ADR removed at the schema level and would have quietly
			// reintroduced one layer up.
			doc.Tags = stored
		}
	}

	// ADR-0060 leaves-as-chunks: when structure parsing is on, the parser's leaves
	// ARE the chunk set, so chunk boundaries match the hierarchy exactly and every
	// chunk's section stamp is correct by construction. Falls back to the flat
	// chunker's chunks when parsing is off, fails, or yields no leaf content.
	var structuredDoc *StructuredDocument
	var structuredReps []StructNode
	if im.structureParser != nil && im.structureStore != nil {
		// A binary document (doc.Data non-empty) travels to the sidecar as base64 so
		// the docling_agent runs its Docling backend on the ORIGINAL bytes. Text
		// documents keep the Text path (no sidecar decode, no backend). The agent
		// gates on this: want_docling = bool(data_b64) and (...), so leaving DataB64
		// unset makes the Docling backend unreachable regardless of source type.
		dataB64 := ""
		if len(doc.Data) > 0 {
			dataB64 = base64.StdEncoding.EncodeToString(doc.Data)
		}
		if parsed, perr := im.structureParser.Parse(ctx, StructureParseRequest{
			DocID: documentID, Title: doc.Title, SourceType: doc.SourceType,
			Text: doc.Body, DataB64: dataB64,
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
		// A document with a body that produced NO stored chunk. Reported, not
		// swallowed.
		//
		// This returned `0, nil` silently, and the caller treats a zero count as
		// success — so an ingest could mint its source-doc entity, store no content
		// at all, and report success to the caller. Measured on the live store:
		// 248 drift-lane documents, 248 entity stubs, ZERO content chunks, and not
		// one log line. The entity's text is the TITLE, which for a short message
		// equals the body, so even a spot-check of the stub looked like the content
		// had landed.
		//
		// Every chunk was dropped by the `len(vec) == 0` guard above, which skips a
		// chunk whose embedding came back empty WITHOUT an error — so neither of the
		// embed-failure warnings fired either. Silence at three layers.
		if len(chunks) > 0 {
			empty := 0
			for _, v := range vectors {
				if len(v) == 0 {
					empty++
				}
			}
			slog.WarnContext(ctx, "IngestionManager: document produced NO stored chunks",
				"doc", documentID, "source_uri", doc.SourceURI, "source_type", doc.SourceType,
				"chunks", len(chunks), "empty_embeddings", empty, "body_len", len(doc.Body),
				"effect", "the source-doc entity exists but the CONTENT is not stored, "+
					"so this document is not retrievable")
		}
		return 0, nil
	}
	if err := im.agent.Manager.SaveBatch(ctx, docs); err != nil {
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
		// Phase 1: an ingestion thread is NOT a task session. This used to be written to
		// "session_id", which made every ingested corpus chunk look like the output of a
		// run to anything that filtered, counted or scoped by session.
		meta[domain.MetaIngestThreadID] = doc.ThreadID
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

// classificationTags strips the IDENTITY terms from a caller's tag list, leaving the
// classification.
//
// Removes exactly two things:
//
//  1. the `source_document` marker, which is wire protocol announcing "the next tag
//     is this document's id" and was never a label; and
//  2. a tag equal to the document id it produced.
//
// Everything else is preserved, in order. The id is matched against the RESOLVED
// documentID rather than blindly dropping whatever follows the marker, so a caller
// whose id came from somewhere else (a thread, a source URI, a content digest) keeps
// every tag it sent. A caller that never used the convention is unaffected.
//
// This is narrowing at the write chokepoint, which the document entity already does
// (ADR-0093 D4: SaveDocument returns what was actually stored, and the chunk cache is
// derived from that rather than from the request).
func classificationTags(tags []string, documentID string) []string {
	if len(tags) == 0 {
		return tags
	}
	out := make([]string, 0, len(tags))
	for i := 0; i < len(tags); i++ {
		if tags[i] == sourceDocumentMarker {
			// Consume the id it introduced, but only if that tag is genuinely the id.
			if i+1 < len(tags) && tags[i+1] == documentID {
				i++
			}
			continue
		}
		if tags[i] == documentID {
			continue
		}
		out = append(out, tags[i])
	}
	return out
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
		if tag == sourceDocumentMarker && i+1 < len(doc.Tags) && doc.Tags[i+1] != "" {
			return doc.Tags[i+1]
		}
	}
	// 1b. A bare classification tag, used as a WEAK id. A tag is not an identity —
	//     nothing stops a caller applying the same tag to a hundred documents, and
	//     ADR-0035 will in any case replace caller tags with kernel classification.
	//     So the tag only GROUPS; the content digest is what separates.
	//
	//     Without the digest this branch silently destroyed data. Ingesting N
	//     documents that share one tag collapsed them all onto "<tag>-chunk-K", each
	//     call overwriting the previous one's chunks, while every call returned a
	//     doc id and reported success. Measured: 8 documents in, 8 source-doc
	//     entities, ONE surviving fact chunk — the last document's. It is the same
	//     failure rule 2 below already names ("collapses every turn onto
	//     <source>-chunk-1 and each ingest overwrites the previous one") and fixes
	//     the same way; this branch simply never got the fix, and it SHADOWS rule 2,
	//     so a caller supplying a perfectly unique ThreadID still lost data as long
	//     as it also passed a tag.
	for _, tag := range doc.Tags {
		if tag == "" || tag == "document-qa" || tag == sourceDocumentMarker || strings.HasPrefix(tag, "chunker:") {
			continue
		}
		return tag + ":" + contentDigest(doc.Body)
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
