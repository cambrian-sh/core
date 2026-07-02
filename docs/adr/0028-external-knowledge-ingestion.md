# ADR-0028: External Knowledge Ingestion via Tier-2 Dual-Coding Pipeline

## Status

Accepted

## Context

Cambrian's memory system only learns from its own step outputs. It cannot ingest Slack messages, emails, PDFs, web pages, or manual file drops. A previous design (`DocTypeExternalFact`) would store raw external blobs as second-class citizens, bypassing the dual-coding (FACT + SCENE) architecture that makes Cambrian's retrieval powerful.

`REQ-CACHE-1` (exact-match plan fast-path) is already implemented. `REQ-CACHE-2` and `REQ-CACHE-3` are accepted via ADR-0026 and ADR-0027. Now we need the Company Brain to actually acquire knowledge from the outside world.

## Decision

External documents are **NOT stored as-is**. They flow through the existing Tier-2 dual-coding pipeline, producing `DocTypeMnemonicFact` and `DocTypeMnemonicScene` documents with full provenance metadata.

### Pluggable Adapter Interface

```go
type IngestionAdapter interface {
    Name() string
    Poll(ctx context.Context, since time.Time) ([]ExternalDocument, error)
    Stream(ctx context.Context) (<-chan ExternalDocument, error) // for real-time sources
}

type ExternalDocument struct {
    SourceURI   string
    SourceType  string // "slack", "email", "web", "jira", "pdf", "file_drop"
    Title       string
    Body        string
    Author      string
    Timestamp   time.Time
    ThreadID    string // for conversational context
    Attachments []Attachment
}
```

**Initial Adapters:**
1. **WebhookReceiver:** HTTP endpoint `/v1/ingest` accepting JSON POSTs. Auth via `X-Ingest-Token`.
2. **DirectoryWatcher:** Watch `data/inbox/` for new `.md`, `.txt`, `.json` files. Auto-import on `fsnotify`.
3. SlackImporter, WebCrawler (deferred)

### Scene Generation: Async LLM Batching

**One scene per document**, generated before chunking. Scene captures episodic context (WHO, WHERE, WHEN, WHY, social dynamics), not semantic content.

**Prompt (canonical PromptBuild):**

```go
scenePrompt := domain.PromptBuild(
    domain.PromptSystem(
        "You are Cambrian's episodic context extractor.",
        "Generate a concise SCENE description capturing WHO, WHERE, WHEN, WHY, and social dynamics.",
        "Max 2 sentences. No explanation. No bullet points.",
        "SCENE answers: 'In what context did this information appear?' not 'What does it say?'.",
    ),
    domain.PromptContext(fmt.Sprintf(`
<DocumentMeta>
  <SourceType>%s</SourceType>
  <Title>%s</Title>
  <Author>%s</Author>
  <Timestamp>%s</Timestamp>
</DocumentMeta>
<BodyPreview>%s</BodyPreview>`, doc.SourceType, doc.Title, doc.Author, doc.Timestamp, buildBodyPreview(doc.Body, 150))),
    domain.PromptTask("Generate the SCENE description for this document."),
    domain.PromptOutputSchemaString(300, "A concise 1-2 sentence description of the episodic context."),
)
```

**BodyPreview generation (Option B with Option A fallback):**

```go
func buildBodyPreview(body string, maxChars int) string {
    // 1. Try first paragraph
    para := strings.TrimSpace(strings.SplitN(body, "\n\n", 2)[0])
    if len(para) <= maxChars {
        return para
    }
    // 2. Fallback: truncate at word boundary
    preview := para[:maxChars]
    if i := strings.LastIndexByte(preview, ' '); i > 0 {
        preview = preview[:i]
    }
    return preview + "..."
}
```

**Fallback placeholder (when qwen:8b is slow/unavailable):**

```
External {source_type} document '{title}' from {source_uri} by {author}, received {timestamp} (scene generation pending)
```

### Batching Architecture

To handle 100 documents/minute within qwen:8b's token budget:

```
Adapter → IngestionQueue (buffered, size 1000)
        → Batcher (default batch size 5, 1s max wait)
        → Jobs channel
        → Worker Pool (5 goroutines, each calls qwen:8b once per batch)
        → Results channel
        → Chunker → Tier-2 pending channel
```

**Token safety:**
- Per-document input: ~80 tokens (truncated metadata + 150-char preview)
- 5-document batch: ~400 tokens input + 200 tokens output = **~600 total**
- Well within qwen3:8b's 8,192-token context window
- Dynamic splitting: if estimated prompt > 6,000 tokens, split batch in half

**Latency:**
- Batcher wait: ≤1s
- qwen:8b batched call: ~2s
- Total per 5-doc batch: ~3s
- 100 docs/minute = 20 batches ÷ 5 workers = 4 calls per worker → **~12s total**

### Chunking Strategy (Option C)

```go
func chunkDocument(doc ExternalDocument) []Chunk {
    // 1. Split by paragraph/section
    paragraphs := splitByParagraph(doc.Body)
    var chunks []Chunk
    for _, para := range paragraphs {
        if len(para) <= maxChunkChars {
            chunks = append(chunks, Chunk{Body: para})
        } else {
            // 2. Paragraph too long: split by sentence
            chunks = append(chunks, splitBySentence(para, maxChunkChars)...)
        }
    }
    return chunks
}
```

- `maxChunkChars = 1000` (safe for most embedding models)
- Each chunk carries: `chunk_index`, `total_chunks`, `source_type`, `source_uri`, `author`, `timestamp`
- The generated scene is attached to every chunk's `metadata["snapshot"]`

### Ingestion Flow

1. Adapter polls / receives push → produces `ExternalDocument`
2. Document queued → batched (size 5, 1s timeout)
3. Worker generates scene via qwen:8b (or fallback placeholder)
4. Document chunked into paragraph/section segments
5. Each chunk's `Metadata["snapshot"] = generatedScene`
6. Chunks enter **Tier-2 pending channel** (`pendingItems`) alongside step outputs
7. `tier2DrainLoop` scores chunks via LLM: relevance, specificity, explicitness
8. Classification:
   - `FULL` → 1 `DocTypeMnemonicFact` (chunk body) + 1 `DocTypeMnemonicScene` (snapshot)
   - `FACT_ONLY` → 1 `DocTypeMnemonicFact`, snapshot stored as `EdgeDiscussedIn` reference
   - `DROP` → discarded
9. Graph edges: `EdgeDiscussedIn` links chunks from same source; `EdgeSpecifies` links attachments to parent
10. Emits `DomainEvent{ExternalDocumentIngested}` for audit

### Why Tier-2 Routing (Not IngestSync)

`IngestSync` (the existing synchronous ingestion path) stores everything as bare `DocTypeMnemonicFact` with hardcoded `ActivationStrength=0.1`. It bypasses:
- LLM quality scoring (relevance, specificity, explicitness)
- Dual-coding separation (FACT vs SCENE)
- Deduplication against existing memories
- Graph edge creation

The dual-coding theory is Cambrian's core memory architecture. External knowledge must flow through the **same** quality control as internally-generated knowledge. The source (agent vs Slack vs PDF) is just metadata; the representation in memory must be uniform.

## Consequences

### Positive

- A Slack message like "We decided to use PostgreSQL over MySQL because of JSONB support" produces:
  - **FACT**: "PostgreSQL chosen over MySQL due to JSONB support"
  - **SCENE**: "Engineering team decision in #database-migration on Jan 15, 2024; Alice proposed, Bob and Carol agreed"
- Both participate in retrieval; SCENE gets higher activation for temporal/social queries; FACT for technical queries
- Dropping a `.md` file into `data/inbox/` appears in `SearchResults` within 30 seconds
- Pipeline processes 100 documents/minute without dropping items
- No new infrastructure — reuses existing BBolt, qwen:8b, and Tier-2 pipeline

### Negative

- qwen:8b scene generation adds ~3s per 5-document batch
- If qwen:8b is unavailable, all documents get placeholder scenes (still functional, lower quality)
- BBolt write load increases from `step_cache` (ADR-0026) + `step_cache` bucket for external docs
- Ingestion queue (size 1000) may drop documents under extreme burst load (>1000 docs in <12s)

### Deferred Work

- In-memory LRU hot layer for scene generation results (same-channel Slack messages often share scene structure)
- Event-driven invalidation: when `ExternalDocumentIngested` arrives, invalidate related `StepCache` entries
- PDF-specific parser (Marker) for structured section extraction before paragraph splitting
- Cross-instance ingestion sharing (requires distributed queue like Redis Streams)
