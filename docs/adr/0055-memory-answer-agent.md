# ADR-0055: MemoryAnswerAgent — A System Agent for Synthesis

**Status:** Proposed (2026-06-25) — proposes a new privileged system organ, exactly like the Scout (ADR-0051) and the kg_extractor (ADR-0053 D2 revised). The Awareness layer still owns routing; the new agent owns the synthesis LLM call.
**Date:** 2026-06-25
**Author:** Afsin
**Depends on:** ADR-0042 (centralized LLM provider — synthesis is a generator role), ADR-0051 (Scout system-agent pattern), ADR-0053 (chunks + chunk_triplets + KG²RAG), ADR-0054 (multi-signal ranking — feeds the input to synthesis).
**Foundational citations:** Microsoft GraphRAG's "map-reduce" pattern (Edge et al., 2024, arXiv:2404.16130) for community-summary synthesis; Cambrian's own system-agent pattern (ADR-0051 Scout, ADR-0053 kg_extractor) for the privileged-organ cut.

---

## Context

Today, every Cambrian agent that calls `QueryMemory` gets back `[]domain.SearchResult { Document, Score }` and must **synthesize its own answer** by calling the LLM with the chunk text. This is duplicated, inconsistent, and the LLM call is the same prompt template re-instantiated by every agent:

- **Duplication.** Every agent has its own copy of "here are some chunks, write an answer" prompt. Drift is inevitable.
- **Inconsistency.** Different agents format answers differently. Some include citations, some don't. Some refuse when evidence is thin, some hallucinate.
- **No central place for provenance.** The current `[]SearchResult` doesn't carry citation tags. Each agent invents its own citation format (or skips it).
- **No central place for refusal.** "I don't have enough context" is the right answer when the Substrate doesn't cover the question, but every agent has to implement that logic.
- **Repeated LLM costs.** N agents × M queries × synthesis LLM call. The synthesis is the same template; it should be cached, not re-prompted.

The fix is a **system agent** — `MemoryAnswerAgent` — that takes a `Handoff({question, retrieved_chunks, scores})` and returns a `Handoff({answer, citations, confidence})`. The kernel owns the synthesis LLM call; every Cambrian agent that wants an answer calls the same place.

This is the **same architectural pattern as Scout (ADR-0051) and kg_extractor (ADR-0053 D2 revised):** a privileged Python `DeterministicAgent`, registered in `domain.IsSystemAgent`, dispatched by a thin Go adapter, invoked directly via `Auctioneer.CallAgent` (no auction/EFE).

The Cambrian Hex is preserved: ingest is deterministic reflex; query routing is the Awareness layer; the synthesis is a system organ. The Awareness layer still decides WHEN to ask; the system agent answers HOW.

---

## Decision 1 — A new system agent, `MemoryAnswerAgent`

```
gRPC Ask(question, top_k, recall_options)
  └─→ MemoryService.Ask
        1. vector_seed (cosine, topK=20)              ── deterministic
        2. kgExpand (one-hop via chunk_triplets)      ── deterministic
        3. graphFilter (connected components)         ── deterministic
        4. spreading activation (flag-gated)          ── deterministic
        5. multi-signal rerank (ADR-0054)             ── deterministic
        6. Handoff({question, chunks, scores, opts})  ── kernel → agent
                → Auctioneer.CallAgent("memory_answer_agent", handoff)
                → MemoryAnswerAgent (Python)
                    - formats the synthesis prompt
                    - calls the LLM (ADR-0042 broker)
                    - parses the LLM response
                    - returns Handoff({answer, citations, confidence})
        7. MemoryService returns AskResponse{answer, citations, confidence, chunk_refs}
```

The new `MemoryAnswerAgent`:

- **Privileged** — registered in `domain.IsSystemAgent`, bypasses the auction, the Gatekeeper candidate pool, and the interview.
- **NO-LLM-extraction** in the hot path — it IS the LLM synthesis. The agent's whole purpose is to call the LLM once, deterministically, with a fixed prompt template.
- **Stateless** — every call is a fresh synthesis. No state between calls. (If caching is added later, it's a separate "summary cache" agent.)
- **Testable** — like every `DeterministicAgent`, it's testable in isolation with fakes for the LLM broker.

### 1.1 — The Handoff contract

```python
# Inbound
class SynthesisRequest:
    question: str
    chunks: list[RetrievalResult]   # {chunk_id, text, score, sources[], confidence}
    options: SynthesisOptions       # {max_answer_tokens, include_citations, refusal_threshold}

# Outbound
class SynthesisResponse:
    answer: str                     # the natural-language answer
    citations: list[Citation]       # [{chunk_id, claim, span_start, span_end}]
    confidence: float               # 0.0-1.0, model's self-reported confidence
    refused: bool                   # true if the LLM said "I don't have enough context"
    refusal_reason: str | None      # "no_evidence" | "low_evidence" | "out_of_scope"
```

The Handoff payload is JSON, parsed by the Go adapter (`MemoryAnswerDispatcher`) and the Python agent. The shape mirrors the existing `KgExtractorDispatcher` / `AgentScoutDispatcher` pattern.

---

## Decision 2 — The synthesis prompt template

The synthesis is one LLM call. The prompt has four parts:

```
1. SYSTEM (fixed)
   You are MemoryAnswerAgent. You answer questions about a user's memory
   substrate using ONLY the retrieved chunks provided. You cite every claim
   with the chunk_id. You refuse to answer when the chunks do not contain
   enough evidence.

2. CONTEXT (per-call)
   Retrieved chunks (most relevant first):
   [1] (chunk_id: aaa-111, confidence: 2) "Caroline went to an LGBTQ support
       group on 2023-05-08."
   [2] (chunk_id: bbb-222, confidence: 1) "Melanie is an ally of the
       LGBTQ+ community."
   ...

3. QUESTION (per-call)
   Who supports LGBT?

4. OUTPUT FORMAT (fixed)
   Reply as JSON:
   {
     "answer": "...",
     "citations": [{"chunk_id": "...", "claim": "..."}],
     "confidence": 0.0-1.0,
     "refused": false,
     "refusal_reason": null
   }
   - If the chunks do not contain enough evidence, set refused=true and
     refusal_reason to "no_evidence" or "low_evidence".
   - Do not invent facts beyond the chunks. Do not cite chunks you did not
     use.
```

The prompt is **fixed** (system + output format) + **per-call** (context + question). The fixed parts are in `agents/memory_answer_agent.py`; the per-call parts come from the Handoff.

The fixed parts are version-controlled. The synthesis prompt is the single most-edited file in the system; keeping it under `agents/` means it lives next to the agent code, not buried in a config file.

---

## Decision 3 — Citation and refusal are first-class

**Citation.** Every claim in the answer carries a `chunk_id` reference. The LLM is told "do not cite chunks you did not use". The Go side validates: any cited `chunk_id` must be in the retrieval set. Invalid citations are dropped with a warning. This catches LLM hallucinations before they reach the user.

**Refusal.** The LLM is told to set `refused=true` when the chunks don't contain enough evidence. The Go side respects the refusal: if `refused=true`, the `AskResponse.answer` is empty and the `refusal_reason` is surfaced to the calling agent. The agent can then route the question to a web search (ADR-0056 if adopted) or escalate to the operator.

**Confidence.** The LLM self-reports a confidence score (0.0-1.0). The Go side thresholds:
- `confidence >= 0.7` — return the answer.
- `0.3 <= confidence < 0.7` — return the answer with a `low_confidence: true` flag; the calling agent decides what to do (probably re-prompt or refuse).
- `confidence < 0.3` — treat as a refusal.

The threshold is config (`synthesis.confidence_threshold`, default 0.3). Operators tune per deployment.

---

## Decision 4 — The `Ask` RPC and the `QueryMemory` RPC coexist

`Ask` is the new high-level convenience. `QueryMemory` stays as the low-level "give me chunks" primitive. Both are on the gRPC `MemoryService`:

```protobuf
service MemoryService {
  // Low-level: returns raw chunks, no synthesis.
  rpc QueryMemory(QueryRequest) returns (QueryResponse);

  // High-level: returns a synthesized answer with citations.
  rpc Ask(AskRequest) returns (AskResponse);
}
```

**When to use which:**
- `QueryMemory` — agents that want to do their own synthesis (rare; only when the agent has a custom prompt template, e.g., a code-generation agent that wants to embed chunks in a different prompt).
- `Ask` — the default. Every cognitive agent that wants an answer calls `Ask`. The synthesis is centralized, the format is consistent, the citations are guaranteed.

The kernel implements both. The `Ask` path internally calls `QueryMemory` (or the same pipeline) then dispatches the synthesis. There's no double work.

---

## Decision 5 — Streaming for long answers

The synthesis LLM call streams the answer back to the calling agent. The gRPC `Ask` is a server-streaming RPC:

```protobuf
rpc Ask(AskRequest) returns (stream AskChunk);

message AskChunk {
  oneof payload {
    string text_delta = 1;     // streaming tokens
    Citation citation = 2;      // a citation as it becomes available
    AskComplete complete = 3;   // final response with confidence, refused
  }
}
```

The first chunks are `text_delta` (the answer being generated). `citation` chunks are interleaved as the LLM produces claims. `complete` is the final chunk with the full response.

**Why streaming:** the synthesis LLM call is ~1-2 seconds for a typical answer. Streaming gives the calling agent a 200-500ms time-to-first-token, so it can start displaying the answer as it arrives. The current `QueryMemory` is a unary RPC; `Ask` is the streaming one.

**Fail-soft:** if streaming fails partway, the kernel returns the partial answer with `complete.partial: true` and a `complete.error` describing the truncation. The calling agent can choose to display the partial or retry.

---

## Decision 6 — Caching the synthesis (optional, v2)

The same question, asked by two different agents, produces the same synthesis. A future Layer 5 enhancement (ADR-0053 D5) adds a synthesis cache:

```
Ask("Who supports LGBT?")
  → if cache hit: return cached {answer, citations, confidence}
  → else: run the full pipeline, cache the response with TTL=1h
```

The cache key is `(question_hash, corpus_version, top_k, blend_weights)`. Invalidation is on corpus change (chunk count delta, kg_extractor re-extraction).

**Not in v1.** The cache is a Layer 5 enhancement. v1 is just the system agent and the streaming RPC.

---

## Considered solutions

### C1 — Keep agent-side synthesis (current)

**Pros:** zero infra change, every agent can customize the prompt.
**Cons:** duplicated LLM calls, inconsistent format, no central citations, no central refusal logic, repeated LLM costs.

**Rejected.** The duplication is the problem; centralizing fixes it.

### C2 — MemoryAnswerAgent as a system agent (chosen)

**Pros:** single source of truth for the synthesis prompt, central citations, central refusal, one place to A/B prompt variants, fail-soft via the LLM broker (ADR-0042), testable in isolation.
**Cons:** one extra gRPC round-trip per `Ask`. Adds a privileged system organ (operational complexity).

**Chosen.** The architectural cut is the same as Scout and kg_extractor. The round-trip is ~10ms (Handoff marshaling) which is dwarfed by the synthesis LLM call (~1-2s). Operational complexity is bounded — one Python process, one config block, same fail-soft contract as the LLM broker.

### C3 — Synthesis as a Go function (not an agent)

**Pros:** no extra process, no gRPC round-trip, simpler.
**Cons:** the synthesis LLM call has to live somewhere. If it's a Go function, the LLM prompt is in Go code (not as easy to A/B as a Python agent's prompt). The function isn't testable in isolation without spinning up the whole kernel.

**Rejected.** The agent pattern is the right cut. The LLM prompt is a Python module that operators can edit and A/B without recompiling the Go binary.

### C4 — Synthesis as a streaming gRPC interceptor (not a separate agent)

**Pros:** no extra process, the synthesis is just a gRPC middleware.
**Cons:** the LLM prompt template lives in Go. The middleware is hard to test in isolation. The same "centralized prompt" benefit is achievable but with a worse code shape.

**Rejected.** A system agent is the right level of abstraction. The middleware pattern would couple the synthesis too tightly to the gRPC server's lifecycle.

### C5 — Use the LLM-as-reranker from ADR-0054 C6 for synthesis too

**Pros:** no new model, the LLM is already loaded.
**Cons:** conflates two different jobs. The reranker is a relevance scorer (small, fast, cross-encoder). The synthesis is a generation (large, slow, autoregressive). Different shapes, different costs.

**Rejected.** Keep them separate. The reranker is `bge-reranker-large` (Stage 5 of the retrieval). The synthesis is the LLM (Stage 6 of `Ask`). Same LLM broker, different roles.

---

## Implementation

### Phase 1 — System agent registration + dispatch

1. `agents/memory_answer_agent.py` — new file. The `MemoryAnswerAgent(DeterministicAgent)`. The fixed system prompt, the output format, the refusal logic.
2. `internal/domain/agent.go` — add `"memory_answer_agent": true` to the system-agent set.
3. `internal/substrate/network/memory_answer_dispatch.go` — new file. `MemoryAnswerDispatcher` builds the Handoff, calls `Auctioneer.CallAgent`, parses the response. Mirrors `AgentScoutDispatcher` and `KgExtractorDispatcher`.
4. `internal/domain/agent_test.go` — `TestIsSystemAgent` extended for `memory_answer_agent`.
5. `cmd/orchestrator/main.go` — wire the `MemoryAnswerDispatcher` into the `MemoryService.Ask` path.

**Estimate: 2 days.**

### Phase 2 — The `Ask` RPC

1. `api/proto/cambrian.proto` — add the `Ask` RPC + the `AskChunk` streaming message.
2. `api/proto/cambrian_grpc.pb.go` — regenerate.
3. `internal/memory/service.go` — implement the streaming `Ask` handler. Internally calls the existing `QueryMemory` pipeline (or the new multi-signal blend from ADR-0054), then dispatches to `MemoryAnswerAgent`.
4. `internal/memory/ask_handler.go` — the streaming writer.

**Estimate: 2 days.**

### Phase 3 — Validation on LoCoMo

1. Run `benchmarks/locomo/` with the existing grader + a new `MemoryAnswer` grader that scores:
   - answer correctness (LLM-as-judge, ADR-0037)
   - citation accuracy (every cited `chunk_id` is in the retrieval set; every used chunk is cited)
   - refusal precision/recall (the agent says "I don't know" when it should)
2. Compare `Ask` vs the current agent-side synthesis on the same LoCoMo questions.
3. Publish the sweep to `benchmarks/locomo/results/0055_memory_answer_agent.json`.

**Estimate: 2 days.**

---

## Migration plan

1. **`Ask` RPC is opt-in.** Agents keep using `QueryMemory` until they're migrated. The migration is per-agent: change `QueryMemory` calls to `Ask`, drop the local synthesis prompt.
2. **The default synthesis prompt is the v1 one.** Operators can override via the agent's config (`memory_answer_agent.prompt_override: "..."`).
3. **Fail-soft is identical to ADR-0042.** If the LLM is down, `Ask` returns a structured error; the calling agent falls back to `QueryMemory` (raw chunks) and does its own synthesis.
4. **Citations are guaranteed, not optional.** The `AskResponse` always carries a `citations` array, even if it's empty. Agents can rely on the field.

**Rollback:** agents that prefer the old behavior call `QueryMemory` directly. The `MemoryAnswerAgent` is one of many synthesis paths, not the only one.

---

## Open questions

1. **Multi-lingual synthesis.** The v1 prompt is English-only. For non-English Substrates, the prompt needs locale-aware variants. v1 + ADR-0053 Layer 5.
2. **JSON vs prose output.** The current design is JSON-only (machine-readable). For human-facing agents, a prose-only mode might be friendlier. v1: JSON only; v2: configurable.
3. **Citation granularity.** v1: chunk-level. v2: sentence-level or span-level (the LLM can be told to output `span_start` and `span_end` within a chunk).
4. **Caching.** v1: no cache. v2: synthesis cache (Decision 6).
5. **Multi-turn.** v1: each `Ask` is single-shot. v2: `Ask` accepts a `conversation_history` and the synthesis prompt includes it.

---

## Literature anchor

| Claim | Source |
|---|---|
| **Centralized synthesis with citations** | Microsoft GraphRAG (Edge et al., 2024, arXiv:2404.16130) — map-reduce with community summaries + citations |
| **Privileged system-agent pattern** | Cambrian ADR-0051 (Scout) + ADR-0053 D2 revised (kg_extractor) — the same pattern |
| **Streaming LLM synthesis** | SubstrateLLMGateway (ADR-0042) + `GenerateViaModelStream` — the streaming path is already wired |
| **Citation-validated answers** | RAGAS framework (Es et al., 2023, arXiv:2309.15217) — citation accuracy as a RAG metric |
| **Refusal as a first-class output** | Self-RAG (Asai et al., 2023, arXiv:2310.11511) — the LLM emits "is the evidence enough?" |
| **Synthesis prompt as a versioned artifact** | Cambrian's own (system prompts in `agents/`, config-overridable) |

---

## Appendix A — The synthesis prompt (v1, English)

```
SYSTEM
You are MemoryAnswerAgent, a privileged Cambrian system agent. You answer
questions about a user's memory substrate (a personal knowledge graph) using
ONLY the retrieved chunks provided below. You are precise, you cite every
claim, and you refuse to answer when the chunks do not contain enough
evidence.

RULES
- Use ONLY the retrieved chunks. Do not draw on outside knowledge.
- Every claim in your answer must cite a chunk_id from the retrieved set.
- If the chunks do not contain enough evidence, set refused=true and
  refusal_reason to "no_evidence" or "low_evidence".
- Do not invent facts, dates, or names that are not in the chunks.
- Confidence is your self-reported certainty in [0, 1]. Low confidence
  (under 0.3) means the chunks are ambiguous or only partially relevant.
- The answer should be concise (1-3 sentences) unless the question
  requires more.

OUTPUT FORMAT
Reply as JSON:
{
  "answer": "<the natural-language answer, with [chunk_id:xxx] citations inline>",
  "citations": [{"chunk_id": "xxx", "claim": "<the claim attributed to xxx>"}],
  "confidence": 0.0-1.0,
  "refused": false,
  "refusal_reason": null
}
```

## Appendix B — The Handoff wire shape

```json
// Inbound Handoff Payload
{
  "type": "synthesis_request",
  "data": {
    "question": "Who supports LGBT?",
    "chunks": [
      {
        "chunk_id": "aaa-111",
        "text": "Caroline went to an LGBTQ support group on 2023-05-08.",
        "score": 0.94,
        "confidence": 2,
        "sources": ["metadata", "spacy_patterns"]
      },
      {
        "chunk_id": "bbb-222",
        "text": "Melanie is an ally of the LGBTQ+ community.",
        "score": 0.87,
        "confidence": 1,
        "sources": ["spacy_patterns"]
      }
    ],
    "options": {
      "max_answer_tokens": 256,
      "include_citations": true,
      "refusal_threshold": 0.3
    }
  }
}

// Outbound Handoff Payload
{
  "type": "synthesis_response",
  "data": {
    "answer": "Caroline and Melanie both support the LGBT community. [chunk_id:aaa-111] [chunk_id:bbb-222]",
    "citations": [
      {"chunk_id": "aaa-111", "claim": "Caroline went to an LGBTQ support group"},
      {"chunk_id": "bbb-222", "claim": "Melanie is an ally of the LGBTQ+ community"}
    ],
    "confidence": 0.92,
    "refused": false,
    "refusal_reason": null
  }
}
```

## Appendix C — What stays the same

- **D1-D4 from ADR-0053** — unchanged. The `Ask` RPC is a wrapper around the existing pipeline.
- **The kg_extractor system agent** — unchanged. The synthesis is a separate organ; the kg_extractor writes to `chunk_triplets`, the MemoryAnswerAgent reads from it.
- **The Zero-Hardcode Rule** — preserved. The Awareness layer still decides when to call `Ask`. The synthesis prompt is config-overridable.
- **The Cambrian Hex** — preserved. Synthesis is a system organ; routing is the Awareness layer; ingest is the deterministic reflex.
