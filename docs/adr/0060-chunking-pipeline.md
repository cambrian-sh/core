---
id: 0060
title: Multi-Strategy Chunking Pipeline (Pluggable Chunker Port + Source-Document Entity + Chunk Relations)
status: Accepted
date: 2026-07-03
supersedes: []
superseded_by: []
depends_on:
  - 0028-external-knowledge-ingestion
  - 0048-working-memory-context-hygiene
  - 0049-experiential-memory-world-model
---

# ADR-0060: Multi-Strategy Chunking Pipeline

## Status

Accepted

## Context

Cambrian's external-knowledge ingestion pipeline (ADR-0028) has shipped with a
single, hard-coded chunker — the "Option C" paragraph-first splitter at
`cambrian-core/internal/memory/chunker.go:1-115` — and the call site in
`cambrian-core/internal/memory/ingestion_manager.go:144`. The splitter has
three structural limits that have become load-bearing problems:

1. **No SourceType awareness.** A `.go` file, a `.md` file, a Slack message,
   and a `data/inbox/` file drop all flow through the same
   paragraph-or-sentence splitter. Code chunks are mid-function slices
   (the documented load-bearing failure mode of paragraph splitting over
   source code — see `docs/research/document-chunking/SUMMARY.md` §1.12
   citing the cAST paper, Findings of EMNLP 2025). Markdown sections
   collapse into a single chunk regardless of heading depth. PDF text is
   impossible to ingest at all (deferred by ADR-0028 §Deferred Work).

2. **No A/B testability.** The chunker is a Go function call, not a
   port; "we picked Option C" is the entire empirical record. The
   research survey (`SUMMARY.md` §2.6, Chroma 2024) reports
   `RecursiveCharacterTextSplitter` at `chunk_size=200` is one of the
   top-3 strategies across domains — we have no way to validate that
   claim on Cambrian's corpora because the strategy isn't pluggable.

3. **Whole documents are lost on chunk.** Once `ChunkDocument` returns,
   the original body is gone from LTM. The `R7` content-offload pattern
   (ADR-0048 D4) and the entity offload pattern (ADR-0049 D6) both
   describe `content_cid` as the general offload mechanism, but no caller
   uses it for ingested documents. Drill-down from a recalled chunk to
   the source document is impossible without a re-read from the upstream
   adapter.

The literature (`SUMMARY.md` §6 *Strategic Recommendations for Cambrian*)
makes a clean Tier 1 / Tier 2 / Tier 3 split: ship the no-LLM strategies
immediately, gate the long-context encoder strategy on the embedder
selection, and explicitly defer the LLM-driven strategies. This ADR
codifies the Tier 1 + Tier 2 ship set as a hexagonal port
(`domain.Chunker`), five implementations, a config-driven registry, and
a new per-chunk relation data structure; Tier 3, hierarchical section
relations, full graph edges, and an auto-router are explicitly
deferred to the **Future improvements** section.

Two vocabulary collisions must be avoided up front:

- **`contextual enrichment`** is the term used in ADR-0048 D6 for the
  recall-time "promote a tool output to LTM" mechanism. The new
  per-chunk relation data is *not* that. We name it **`chunk_relations`**
  to keep the two concepts lexically distinct; the §1.7 *Contextual
  Retrieval* strategy (Anthropic 2024) is a future-improvement,
  not part of v1.
- **A new `DocType` constant** for "source document" was the name an
  early draft used for a parallel document type. Metis's audit (in
  the plan `.omo/drafts/chunking-pipeline.md`) found the existing
  `DocTypeMnemonicEntity` primitive (ADR-0049 D8) already covers this
  case with a discriminator field; we use that primitive and do not
  introduce a parallel `DocType` constant.

The byte-oriented `domain.ContentStore.Put` / `Get` is the only
content-offload contract (ADR-0022 / ADR-0048 D4). We do not add a
parallel text-oriented content API for the source-doc offload; the
existing byte-oriented API is the only contract.

## Decision

### D1 — Source documents are `DocTypeMnemonicEntity` records (not a new type)

Each ingested external document is materialised as a single
`DocTypeMnemonicEntity` row (ADR-0049 D8) with discriminator
`kind: "source_document"`. The entity record carries:

- `SourceURI`, `SourceType`, `Title`, `Author`, `Timestamp` (mirroring
  the `ExternalDocument` shape in `cambrian-core/internal/memory/ingestion_manager.go:120-179`).
- `ContentCID` — the content-addressed ID returned by
  `domain.ContentStore.Put(ctx, []byte(doc.Body), "source_document", nil, snippet)`
  (`cambrian-core/domain/content_store.go:56-67`). The full body is
  offloaded to CAS once per document, not once per chunk; the entity's
  `content_cid` is the stable handle for drill-down.

**Rationale.** A `DocTypeMnemonicEntity` is the existing primitive for
"a typed record of a real thing with provenance and a content
reference" (ADR-0049 D8: *First-class entity records*). Reusing it
avoids a parallel doc-type surface, inherits the entity pipeline's
canonicalization (`dir:` / `file:` / `api:` / `url:` namespace),
materialized current view, field-level LWW updates, and the
`last_observed_at` staleness contract (A1.1, 2026-06-22). **We do
not introduce a parallel source-doc `DocType` constant** — the
source doc is a `mnemonic_entity` with a discriminator, not a new
type.

The entity write uses the existing entity write path
(`cambrian-core/internal/memory/agent.go` — the
`DocTypeMnemonicEntity`-minting site; verified by
`grep -r "MnemonicEntity" cambrian-core/internal/memory/agent.go`).
Exactly one entity is minted per document, not per chunk.

### D2 — The `domain.Chunker` port + `domain.Chunk` struct

`cambrian-core/domain/chunker.go` (NEW) defines the hexagonal port:

```go
type Chunker interface {
    Name() string
    Supports(sourceType string, ext string) bool
    Chunk(ctx context.Context, doc *ExternalDocument) ([]Chunk, error)
}

type Chunk struct {
    Body     string
    Metadata map[string]any
}
```

`Metadata["chunk_relations"]` carries the JSON-marshaled
`ChunkRelations` (D4). Implementations live in
`cambrian-core/internal/memory/chunkers/`. The existing
`internal/memory/chunker.go` becomes a thin shim that calls
`chunkers.OptionCChunker.Chunk(ctx, &doc)` (for any external callers
that may still reach for the bare function — the in-tree caller at
`ingestion_manager.go:144` is updated in T-1.10 to use the registry).

### D3 — Five chunker implementations

| Chunker | File | Tier | What it does | `Supports` |
| --- | --- | --- | --- | --- |
| `OptionCChunker` | `internal/memory/chunkers/option_c.go` | 1 (back-compat) | The current 115-line `ChunkDocument` logic, extracted verbatim. Same `\n\n`-first paragraph split, same 1000-char sentence fallback. | `sourceType == "file_drop" \|\| sourceType == ""` (always supported; the back-compat floor) |
| `RecursiveCharacterChunker` | `internal/memory/chunkers/recursive_character.go` | 1 | LangChain-style recursive separator splitter with `["\n\n", "\n", " ", ""]` (per-language overrides for Go, Python, Rust, Markdown). Default `chunk_size=200` per Chroma 2024 §1.2. | `ext ∈ {.txt, .md, .py, .ts, .rs, .go}` (broadest prose+code coverage) |
| `ASTGoChunker` | `internal/memory/chunkers/ast_go.go` | 1 | `go/ast`-based extractor for `.go` files. Top-level decls (functions, types, consts, vars) become chunks; each chunk's body is the decl's source span (`Pos()` → `End()`). **Pure Go stdlib; no cgo, no tree-sitter dependency.** | `ext == ".go"`; non-`.go` files return `ErrUnsupportedExtension` and the registry falls back |
| `MarkdownHeaderChunker` | `internal/memory/chunkers/markdown_header.go` | 1 | `MarkdownHeaderTextSplitter`-style: splits on `^(#{1,6})\s+`, retains parent header path in `Metadata["section_path"]`. | `ext == ".md"` |
| `LateChunker` | `internal/memory/chunkers/late.go` | 2 | Whole-doc long-context encoder + per-chunk mean-pool over masked token embeddings (Günther et al., arXiv:2409.04701). Gated (D6). | `sourceType == "file_drop" && ext ∈ {.md, .txt}` (long-form docs only) |

**Naming note.** The first version of this ADR used the term
"contextual enrichment" for the per-chunk relation payload. The term
collides with ADR-0048 D6's "contextual enrichment" semantic (the
R8 tool-output promotion to LTM) and is renamed to `chunk_relations`.
The Anthropic *Contextual Retrieval* strategy (SUMMARY §1.7) is a
*future-improvement* Tier 3 strategy and is not part of v1.

### D4 — `chunk_relations` (the per-chunk relation data, 512-byte budget)

`cambrian-core/internal/memory/chunk_relations.go` (NEW) defines:

```go
type ChunkRelations struct {
    ParentEntityID    string         `json:"parent_entity_id"`
    PrecedingChunkID  string         `json:"preceding_chunk_id,omitempty"`
    FollowingChunkID  string         `json:"following_chunk_id,omitempty"`
    SiblingContext    SiblingContext `json:"sibling_context"`
}

type SiblingContext struct {
    ParentTitle      string `json:"parent_title"`      // ≤ 80B
    ParentSummary    string `json:"parent_summary"`    // ≤ 120B
    ParentScene      string `json:"parent_scene"`      // ≤ 120B
    PrecedingSnippet string `json:"preceding_snippet"` // ≤ 96B
    FollowingSnippet string `json:"following_snippet"` // ≤ 96B
}
```

`SiblingContext.MarshalJSON` enforces a strict **≤ 512-byte total**
budget. Over-budget fields are trimmed at the right (never truncated
mid-rune); the marshaled JSON is the unit of budget. The per-field
caps are 80 + 120 + 120 + 96 + 96 = 512, which is the designed fit;
the marshaled form adds only a fixed ~120 bytes of JSON scaffolding,
so the per-field caps leave the strict 512-byte headroom. This budget
keeps the relation payload predictable for downstream embedding
budgets and avoids the per-chunk bloat that erodes retrieval
quality (the SUMMARY §2.4 *HOPE* finding: *don't optimize for tight,
monolithic chunks* — keep them independent and distinguishable).

Each chunk's `Metadata["chunk_relations"]` is the JSON-marshaled
`ChunkRelations`. The parent-entity link is what makes drill-down
possible: given a recalled chunk, the agent follows
`chunk_relations.parent_entity_id` → source-document entity →
`content_cid` → full body via `ContentStore.Get`.

### D5 — Chunker registry: data-driven routing, precedence `SourceType → extension → default`

`cambrian-core/internal/memory/chunker_registry.go` (NEW) defines:

```go
type Registry struct {
    routes          map[string]string // sourceType → chunker name
    extRoutes       map[string]string // ext → chunker name
    defaultChunker  string
    chunkers        map[string]domain.Chunker
}
```

`Resolve(sourceType, ext)` returns the chunker in this strict
precedence:

1. `routes[sourceType]` if non-empty.
2. `extRoutes[ext]` if non-empty.
3. `defaultChunker` (the configured default; defaults to `"option_c"`).

**The default name is a config value, not a Go constant** — this
honors the **Zero-Hardcode Rule** (`cambrian-core/AGENTS.md`): agent-
to-task routing lives in the Awareness layer (config / data), never
as Go `if/else` / `switch`. The registry's `Resolve` is a pure map
lookup; no `switch sourceType` or `switch ext` exists in the registry
or the chunker implementations. The default
`chunker.default = "option_c"` lives in
`internal/config/config.go` and is overridable per-deployment.

`NewRegistry(cfg ChunkerConfig)` validates that every name in
`routes`, `extRoutes`, and `default` resolves to a registered
chunker; an unknown name is a config error, not a silent fallback.

### D6 — Late chunking is gated and sized

`LateChunker` is **opt-in**, not the default. It is selected only
when **all** of the following hold:

- `chunker.late.enabled = true` (config; default `false`).
- `embedder.supports_long_context = true` (config; default `false`
  until the embedder-selection ADR resolves).
- The document body is within `chunker.late.max_doc_tokens` (default
  `8192`, matching the existing `nomic-embed-text` 8K context).

When the gate evaluates true and the doc fits, `LateChunker`:

1. Calls `embedder.EmbedBatch(ctx, []string{doc.Body})` to get
   per-token contextualised vectors.
2. Tokenizes the body to map char ranges → token indices.
3. Re-derives the chunk boundaries via the same Option-C rules
   (consistent chunk *boundaries* across arms).
4. Mean-pools the per-token vectors over each chunk's token range.
5. Returns the chunks with the pooled vector in
   `Metadata["late_embedding"]`.

**Late-fallback contract.** When the gate evaluates true but the
doc body exceeds `chunker.late.max_doc_tokens` estimated tokens,
`LateChunker` falls back to `OptionCChunker` and emits:

```go
slog.Warn("LateChunker: doc exceeds max_doc_tokens, falling back to OptionC",
    "source_uri", doc.SourceURI,
    "doc_tokens", n,
    "max_doc_tokens", cfg.Late.MaxDocTokens)
```

The run manifest records this as `metric=late_fallback` (the
benchmark's `chunking_sweep.yaml` arm name `late_8k` carries the
cap as a parameter). The fallback is **silent only at the data
layer** — it is loud at the log/manifest layer so the operator can
see when the gate let through an oversized doc.

`domain.Embedder` gains an additive `EmbedBatch(ctx, []string) ([][]float32, error)`
with a free-function default forwarder
(`EmbedBatchForwarder`) that loops over `Embed`. The
`OllamaEmbedder` (the only live impl) overrides `EmbedBatch` to use
the vectorized Ollama `/api/embeddings` `input: texts` endpoint
(one request, not N). The default forwarder is the back-compat
floor; no embedder is *forced* to override.

### D7 — Config block `chunker.*` and `embedder.supports_long_context`

`cambrian-core/internal/config/config.go` adds:

```go
type ChunkerConfig struct {
    Default string                       // default "option_c"
    Routes  map[string]string            // sourceType → chunker name
    ExtRoutes map[string]string          // ext → chunker name (NEW; second precedence)
    Late    LateChunkerConfig
}

type LateChunkerConfig struct {
    Enabled      bool // default false
    MaxDocTokens int  // default 8192
}

type EmbedderConfig struct {
    // ... existing fields ...
    SupportsLongContext bool // default false
}
```

`DefaultConfig()` populates the empty `Routes` and `ExtRoutes` maps
and the `Late.Enabled = false` / `Late.MaxDocTokens = 8192` /
`SupportsLongContext = false` defaults. `Validate()` rejects any
route name that doesn't resolve to a registered chunker.

### D8 — Scavenger exemption for `source_document` entities

The existing scavenger pass GC's `DocTypeMnemonicEntity` rows that
have no remaining provenance. Source-document entities
(`kind: "source_document"` AND `content_cid` set) are **GC-exempt**:
they are the *drill-down targets* for chunk recall, not the
chunk-level recall targets themselves. Deleting them would break
the parent link in `chunk_relations.parent_entity_id`. The
exemption is keyed on the discriminator + the presence of a
non-empty `content_cid`, not on a hard-coded list of `SourceType`
values (the data-driven way).

### D9 — Ingestion flow (the wire of it)

`internal/memory/ingestion_manager.go::processBatch` becomes:

1. For each `ExternalDocument` in the batch:
   1. Resolve the chunker: `chunkName, _ := registry.Resolve(doc.SourceType, extOf(doc.SourceURI))`.
   2. Offload the body: `cid, _ := contentStore.Put(ctx, []byte(doc.Body), "source_document", nil, snippet)`.
   3. Mint the source-document entity (via the existing entity write path): `kind: "source_document"`, `SourceURI`, `SourceType`, `Title`, `Author`, `Timestamp`, `ContentCID = cid`.
   4. Chunk: `chunks, _ := chunker.Chunk(ctx, &doc)`.
   5. For each chunk `i`, build `ChunkRelations` with `ParentEntityID` (the entity id from 1.iii), `PrecedingChunkID`/`FollowingChunkID` (the prev/next chunk ids from the loop), and `SiblingContext` (parent title, parent summary, parent scene, first 96B of prev/next chunk bodies — all trimmed to the per-field caps; over-budget fields are trimmed at the right).
   6. Embed the chunk bodies as one batch when the embedder implements `domain.BatchEmbedder`, falling back to per-chunk embedding only when the batch capability is unavailable or fails.
   7. Save the chunks immediately as `DocTypeMnemonicFact` rows in one store batch, with raw chunk text, preserved tags/provenance, deterministic `{document_id}-chunk-N` IDs, and `ch.Metadata["chunk_relations"]` populated with the JSON-marshaled `ChunkRelations`.
2. The synchronous `IngestMemory` path returns only after these chunk facts are durable and queryable. Document-ingest chunks do **not** enter the Tier-2 pending channel; Tier-2 summaries would hide exact source text from document QA and make successful ingest invisible to `QueryMemory`.

The two operations per doc are sequential: the entity is minted
*before* chunking so the chunker (and `chunk_relations`) can refer
to the entity ID. The `cid` is the durable handle; the entity row
is the indexable record. The two are written in the same
`processBatch` call — no asynchronous fan-out, no risk of an entity
existing without a body or vice versa.

## Consequences

### Positive

- **Pluggable chunking.** Five strategies ship in v1; the registry
  is data-driven (no Go `if/else`); new chunkers are config-registered
  without code changes to `processBatch`.
- **Back-compat preserved.** `OptionCChunker` is the default and
  reproduces the existing 115-line logic verbatim
  (`TestChunkRegistry_OptionC_MatchesChunkDocument` is the
  regression bar).
- **Source-document drill-down.** The `chunk_relations.parent_entity_id`
  + `content_cid` pair gives the agent a deterministic handle to
  the full source body, even after chunking.
- **A/B testability.** `cambrian-bench run --suite chunking` sweeps
  all five arms against the new `structured-code-corpus` and emits
  `nDCG@10` + `Recall@10` + `MRR` (the harness had no `nDCG` before
  this work; it's net-new in the chunking suite's `retrieval.py`).
- **Zero-Hardcode-clean.** Routing, default selection, and the
  late-chunking gate are all data-driven from the `chunker.*` config
  block; no `switch sourceType` or `switch ext` exists in
  Go code (`grep -r "switch.*[sS]ource[tT]ype\|switch.*[eE]xt"
  cambrian-core/internal/memory/chunkers/ cambrian-core/internal/memory/chunker_registry.go`
  returns nothing).
- **No new doc type.** The source doc is a
  `DocTypeMnemonicEntity` with `kind: "source_document"`. The
  entity pipeline's canonicalization, supersession, and
  `last_observed_at` (A1.1) semantics are inherited for free.
- **No new content API.** The byte-oriented `ContentStore.Put` /
  `Get` is the only contract; the source-doc offload uses it as-is.
- **Late-chunking is honest about its cost.** Gate + size cap +
  explicit `late_fallback` log means the operator sees when a doc
  was too big, not just when late chunking was off.

### Negative

- **Five chunkers to test, benchmark, and document.** Each carries
  its own unit test surface and the benchmark suite has 5 arms.
  The `internal/memory/chunkers/` package is the new home; long-term
  maintenance is the cost.
- **`chunk_relations` is a per-chunk metadata tax.** Even the
  minimal parent-only payload adds ~30 bytes per chunk; the full
  ≤ 512B budget is the upper bound. Embedding-budget impact is
  measured by the benchmark.
- **Source-document entity rows are GC-exempt.** They accumulate;
  operators must size the `ContentStore` (CAS) and the entity table
  accordingly. A dedicated size-cap / tiered-storage ADR is a
  follow-up.
- **Late chunking is gated off by default.** Operators who want it
  must set `chunker.late.enabled = true` AND
  `embedder.supports_long_context = true`; getting either wrong
  silently routes to `OptionCChunker` (a logged warn at resolve
  time, not a hard error). The intent is "fail to the known-good
  default" rather than "fail to a confusing late-but-broken state",
  but it does mean the gate is *implicit* for any operator who
  only sets one of the two flags.
- **Re-chunking backfill is out of scope.** When the operator
  updates the chunker config, existing documents are not
  re-chunked in v1. A backfill path is future work.

### Future improvements

These are explicitly **deferred** from v1 and recorded here so the
work is not lost. Each item has an explicit non-goal statement in
the v1 code (no `// TODO` markers; the absence is the gate).

1. **B2 — Hierarchical chunk relations.** `section_id` + `level` +
   `sibling_index` exposure. The `markdown_header` and `ast_go`
   chunkers *implicitly* produce section structure (header path,
   top-level decls) but do not expose it as a typed relation in
   `chunk_relations` in v1. B2 is the natural extension of D4 once
   the *Auto-merging retrieval* pattern (SUMMARY §1.10) is adopted
   — the chunker emits a section_id; the retriever walks the
   parent on majority-leaf-match. **Deferred** because (a) the
   v1 retrieval path doesn't yet consume the section_id, and (b)
   the per-chunk budget (512B) doesn't have headroom for a
   section_id field without dropping one of the existing fields.

2. **B-full — Chunk-level graph edges in `document_edges`.** Add
   `chunk_precedes`, `chunk_follows`, `chunk_part_of` edges in the
   existing `document_edges` table (ADR-0017). These overlap
   structurally with the linear `preceding_chunk_id` /
   `following_chunk_id` in `chunk_relations` (D4) — the linear
   IDs are *intra-document sequential*, while the proposed graph
   edges would be *typed, edge-typed, and queryable by SpreadingEngine
   BFS* (ADR-0017). **Deferred** because (a) the existing
   `document_edges` already handles cross-chunk relations via
   `discussed_in` / `follows` (ADR-0049 D10), and (b) adding
   chunk-level edges is duplicative until the retriever has a
   concrete query that needs them (no such query exists in v1).

3. **Tier 3 — Contextual Retrieval / Propositions / Small-to-big
   (LLM-driven chunkers).** The Anthropic Contextual Retrieval
   strategy (SUMMARY §1.7, Sep 2024) gives −35% top-20 retrieval
   failure at the cost of *one LLM call per chunk*. Propositions
   (Dense X Retrieval, §1.8) and small-to-big (§1.9) are the
   proposition- and sentence-window variants. **All three are
   deferred** because the qwen3:8b budget cannot sustain
   1 LLM-call-per-chunk at 100 docs/min without aggressive
   batching and prompt-cache amortization. The research's own
   recommendation is to *gate on the embedder selection and
   reproduce Chroma 2024 first* before adopting these. When the
   gating pre-conditions hold (a vLLM/SGLang scheduler with
   prompt-cache reuse, a HOPE-based eval harness), a future ADR
   can introduce a `Tier3Chunker` that wraps the existing
   `SceneGenerator` (ADR-0028) and the LLM-driven split prompt.
   **Recorded for parity with SUMMARY §6 "Tier 3 — Evaluate, then
   ship selectively."**

4. **Auto-router (LLM picks chunker per document).** A learned /
   prompted router that examines the document (file extension,
   `sourceType`, a quick embed of the first paragraph) and picks
   one of the five chunkers. This is the *MoG* pattern
   (Zhong et al., arXiv:2406.00456) at a per-document level rather
   than per-query. **Deferred** because the v1 routing precedence
   (`SourceType → extension → default`) is *data-driven* (the
   operator's config is the source of truth) and there is no
   empirical evidence in v1 that an LLM router would beat it.
   A future ADR can add a learned router **if** the benchmark
   shows a non-trivial recall gap on a corpus the operator
   config can't express. The opt-in is *not* free: it adds
   one LLM call per document to the hot path.

5. **Tree-sitter-based multi-language AST chunker.** Extend
   `ASTGoChunker` (D3) to a tree-sitter-driven multi-language
   chunker covering Python, TypeScript, and Rust — the cAST
   pattern (SUMMARY §1.12, EMNLP 2025 Findings, +4.3 Recall@5
   on RepoEval). **Deferred** because tree-sitter grammars
   ship as a cgo dependency, and `cambrian-core`'s build
   pipeline is pure-Go (verified: `grep -r "cgo" cambrian-core/go.mod`
   returns nothing). The deferred trigger is *cgo being enabled
   in CI* — at that point, tree-sitter is a clean addition.
   The v1 AST path is `go/ast`-only, which covers `.go` files
   (the highest-value code chunking case for Cambrian's
   self-inges­ting code paths) and falls back to
   `RecursiveCharacterChunker` for everything else.

6. **PDF structure-aware parsing (Marker / LayoutLMv3).** PDF
   ingest is already deferred by ADR-0028 §Deferred Work. The
   Tier 1 implementation is Marker (`marker-pdf`) for
   rule-based structure extraction; the Tier 2 path is
   LayoutLMv3 (arXiv:2204.08387) for table/image-aware
   chunking (SUMMARY §1.18). **Deferred** with the trigger
   being either (a) a corpus of PDFs the operator needs to
   ingest, or (b) the LayoutLMv3 model fitting in the 12 GB
   GPU budget (current numbers: 50–200 ms/page; a 50-page
   paper is 2.5–10 s of GPU time per ingest). The
   research-flagged blocker remains: 100M–500M-param encoder
   per page is not in the v1 budget.

The literature basis for the v1 ship set and the future
improvements is `docs/research/document-chunking/SUMMARY.md`
**§6 — Strategic Recommendations for Cambrian**, which lays out
the Tier 1 / Tier 2 / Tier 3 ship ordering and the "do not
ship" list (GraphRAG at full Microsoft spec, LayoutLMv3 for
general PDF, RST parsing, agentic / LumberChunker at ingest
throughput). This ADR follows that ordering verbatim.

## References

- **ADR-0028** — External Knowledge Ingestion via Tier-2 Dual-Coding
  Pipeline. Parent ADR; mirrors its structure. Establishes
  `ExternalDocument` shape, the Tier-2 ingestion flow, the
  scene generation prompt, and the `qwen:8b` / `qwen3:8b` LLM
  budget assumption (`cambrian-core/docs/adr/0028-external-knowledge-ingestion.md:1-192`).
- **ADR-0048** — Working-Memory Context Hygiene. Confirms
  `chunk_relations` is *not* the "contextual enrichment" term
  (D6) and reuses the byte-oriented `ContentStore.Put` for
  the source-doc offload (`cambrian-core/docs/adr/0048-working-memory-context-hygiene.md:1-107`).
- **ADR-0049** — Experiential Memory — Typed Records, Online
  Graph, and Scenes as a World Model. Establishes
  `DocTypeMnemonicEntity` (D8) and the `content_cid` offload
  pattern (D6) that D1 and D9 of this ADR reuse
  (`cambrian-core/docs/adr/0049-experiential-memory-world-model.md:69-76, A1.1`).
- **`docs/research/document-chunking/SUMMARY.md` §6** — *Strategic
  Recommendations for Cambrian*. The Tier 1 / Tier 2 / Tier 3
  ship ordering; the citation backbone for the five v1
  chunkers and the six future improvements.
- **arXiv:2409.04701** — Günther et al., *Late Chunking:
  Contextual Chunk Embeddings Using Long-Context Embedding
  Models* (Jina AI, Sep 2024). The math behind D6's
  per-chunk mean-pool.
- **arXiv:2005.11401** — Lewis et al., *Retrieval-Augmented
  Generation for Knowledge-Intensive NLP Tasks* (NeurIPS 2020).
  The DPR/RAG baseline that defines the fixed-size chunker
  reference.
- **Smith & Troynikov, Chroma 2024** — *Evaluating Chunking
  Strategies for Retrieval*. The empirical basis for
  `RecursiveCharacterChunker` at `chunk_size=200`.
- **LangChain `RecursiveCharacterTextSplitter`** — the
  design pattern D3's `RecursiveCharacterChunker` mirrors
  (default separators `["\n\n", "\n", " ", ""]`).
- **cAST (EMNLP 2025 Findings)** — *Enhancing Code Retrieval-
  Augmented Generation with Structural Chunking via Abstract
  Syntax Tree*. The basis for the future-improvement #5
  tree-sitter path and the motivation for the v1
  `go/ast`-only AST chunker.
- **Plan** — `.omo/plans/chunking-pipeline.md` (the work plan;
  T-1.1 is this ADR).
- **Draft** — `.omo/drafts/chunking-pipeline.md` (the planning
  draft; the resolved forks F1, F9, F10).
