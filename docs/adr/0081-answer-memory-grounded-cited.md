---
id: 0081
title: AnswerMemory — Grounded, Span-Cited Answers on the Operator Plane
status: Proposed
date: 2026-07-23
supersedes: []
superseded_by: []
depends_on:
  - 0047-operator-transport-plane
  - 0054-automated-tuning-runtime-config
  - 0057-open-core-boundary
  - 0060-structure-aware-chunking
---

# ADR-0081: AnswerMemory — Grounded, Span-Cited Answers on the Operator Plane

## Status

Proposed (2026-07-23). Motivated by the operator UI Memory page (`docs/MEMORY_PAGE_MVP.md` §3): the
operator asks a question and wants a single grounded answer whose sentences are attributed to the
exact passages they came from — a NotebookLM-style read where clicking a cited sentence reveals its
source chunk.

## Context

`QueryMemory` (ADR-0047 amendment A2) is the operator plane's memory read. It calls
`QueryService.SearchSystem` → `searchByType` — a **single pass** that returns ranked evidence and
**never synthesizes**. This is deliberate: the operator plane is single-pass by invariant, so a
human can inspect what retrieval found without the planner/auction/agentic machinery running behind
a human principal.

The grounded synthesis the UI wants already exists, but on a different entry point:

- `QueryService.agenticSearch` / `decompSearch` run the multi-hop loop (plan → retrieve →
  `Synthesize`), producing a **composed answer extracted from retrieved chunks** — grounded, never
  parametric (the hard constraint in `AGENTIC_RETRIEVAL_FINDINGS.md` §4). The answer is carried
  today as a synthetic control `SearchResult` (`AgenticControlID`, text in `AgenticTextKey`).
- This path is reached only via the agent/benchmark `.Search()` lane and is gated on
  `execution.agentic_retrieval_enabled` (default off).

So the retrieval agent the UI wants is built; it is simply unreachable from the operator plane.

Two gaps block a NotebookLM read even once it is reachable:

1. **No attribution.** `RetrievalDispatcher.Synthesize(query, chunks) → (status, text)` returns flat
   prose. Nothing says which sentence came from which chunk.
2. **Metadata is lost.** `Synthesize` is handed `[]string` (chunk texts only). A citation needs the
   chunk's `doc_id`, `section_path`, and `source`, which live on the `SearchResult`, not the string.

## Decision

Add **`AnswerMemory`**, a capability-gated operator-plane read RPC that runs the agentic retrieval
loop and returns a grounded answer with **inline, index-based citations** plus the resolved evidence
each index points to. The UI renders the citations as coloured, clickable spans.

### D1 — A new RPC, not an overload of `QueryMemory`

`QueryMemory` stays exactly as it is: deterministic, single-pass, evidence-only — the source of the
UI's browse/inspect and of any benchmark that depends on stable ranking. `AnswerMemory` is a
distinct RPC with a distinct contract (`{status, answer, citations[]}`). Overloading one RPC with
two retrieval regimes would make its behaviour depend on a flag, which the operator cannot see.

### D2 — Crossing the single-pass invariant, deliberately and narrowly

`AnswerMemory` runs the multi-hop agentic loop on the operator plane — exactly what the single-pass
invariant otherwise forbids. This ADR sanctions that as a **named, bounded exception**: it is a
**read** RPC (no `command_id`, no mutation, no audit-write), it never carries an `x-agent-id`
principal (ADR-0047 D1 holds — the caller is the human operator, scope `ScopeSystem`), and it runs
no auction and spawns no agent. It is retrieval synthesis, not task execution. `QueryMemory` remains
the single-pass lane; the invariant is narrowed, not abandoned.

### D3 — Capability-gated and opt-in

`agentic_retrieval_enabled` stays off by default (CPU cost; the reranker caveats in
`cambrian-failure-archaeology`). `AnswerMemory` advertises the capability **`memory-answer`** only
when the agentic path is available, and returns `Unimplemented` otherwise. The UI lights up the
answer lane only on the capability and otherwise falls back to evidence-only. No default retrieval
behaviour changes.

### D4 — Index-based inline citations (the wire shape)

The synthesizer cites claims by the **1-based index of the chunk** that supports them, inline in the
answer text: e.g. `"The little prince came from asteroid B612 [3]. He met a geographer [1]."` The
kernel resolves each index against the ordered evidence list it passed to `Synthesize`, so every
marker maps to a real chunk with full metadata.

```proto
message AnswerMemoryRequest {
  string query = 1;
  int32  top_k = 2;          // evidence pool cap (0 = kernel default)
  string source = 3;         // optional filter
  string session = 4;        // optional filter
  double min_importance = 5; // optional filter
}

message MemoryCitation {
  int32  marker = 1;        // the [n] used in `answer`
  string doc_id = 2;
  string text = 3;          // the verbatim chunk — what a citation quotes
  string section_path = 4;  // ADR-0060 breadcrumb (may be empty)
  string source = 5;
  double score = 6;
  double importance = 7;
  repeated string tags = 8;
}

message AnswerMemoryResponse {
  string status = 1;                    // answer | abstention | clarification
  string answer = 2;                    // grounded prose with inline [n] markers
  repeated MemoryCitation citations = 3;// referenced by marker; the evidence pool
}
```

Rationale for markers over structured spans: a marker convention is far more robust for an LLM to
emit than clean per-sentence JSON, and it keeps the proto flat. **Sentence-level colouring is a UI
concern**, not a kernel one: the webview segments the answer into sentences, associates each
sentence's trailing `[n]` markers with citations, colours the sentence by its citation(s), and on
click reveals the exact chunk(s). Pushing segmentation to the UI means the kernel never has to
promise a brittle span structure, and the rendering can improve without a contract change.

### D5 — Grounded-only preserved

The synthesizer already extracts sub-answers from retrieved chunks and never from parametric
knowledge (unwritten rule #1). Index citation strengthens this: a claim with no supporting chunk
gets no marker, and the UI renders uncited connective text in a neutral colour, making ungrounded
sentences visually obvious. Abstention (`status = abstention`) remains the correct output when the
evidence does not answer the question — the UI must show it as such, never as an empty answer.

## Consequences

**Kernel.**
- `RetrievalDispatcher.Synthesize` (and its Python `retrieval_agent` `synthesize` op) gains a
  citation instruction: cite each grounded claim with the 1-based index of the chunk it came from.
  Output envelope stays `{status, text}` (text now carries markers). Fail-open unchanged.
- New `QueryService.AnswerSystem(ctx, query) → (status, answer string, evidence []SearchResult)`:
  runs the agentic loop and returns the synthesized answer plus the **ordered evidence** passed to
  `Synthesize`, so the operator handler can resolve markers to citations. This replaces the
  control-result hack for this path; the `AgenticControlID` mechanism stays for the benchmark lane.
- Operator `Service.AnswerMemory` handler maps evidence → `MemoryCitation[]`, applies the
  `source`/`session`/`min_importance` filters to the evidence pool, and returns the response.
- Wired in `app.go` on the same memory port that already backs `QueryMemory`; `memory-answer` is
  appended to `operatorCaps` when the agentic path is enabled.

**Contract.** New RPC + messages + `memory-answer` capability ⇒ `contract_version` bump and
`make proto-breaking`/`proto`/`proto-check`; re-vendor `ui/proto` + `ui/src-tauri/src/pb.rs`
(`PINNED_CONTRACT_VERSION`). CLI vendored proto records the skew (unchanged).

**UI.** `AskPane` drops the option-(A) chat-lane hack (which routed questions through the planner
and produced hollow answers, per ADR-0080) and calls `answerMemory`. It renders the answer with
NotebookLM-style sentence colouring: cited sentences are clickable and coloured per citation;
clicking reveals the exact chunk with its breadcrumb. Abstention and clarification are first-class
states.

**Benchmark (DDD).** The `agentic-retrieval` suite already exercises `Synthesize`; add an
operator-answer check that asserts `AnswerMemory` returns a grounded answer whose citations resolve
to real chunks, and that abstention fires when the corpus cannot answer. Measure with
`agentic_retrieval_enabled` on vs off as the arm.

## Alternatives rejected

- **Extend `QueryMemory`** (D1): behaviour-by-flag on a contract the operator can't see.
- **Structured span JSON from the LLM** (D4): brittle output, more failure modes, no better UX than
  UI sentence-segmentation over markers.
- **Post-hoc attribution by embedding each answer sentence against chunks**: adds per-sentence
  similarity work and is less exact than LLM self-citation; keep it as a fallback only if marker
  emission proves unreliable in measurement.
- **Option (A), chat lane + parallel `queryMemory`** (the shipped stopgap): citations are not
  causally linked to the answer, and the answer inherits the planner's hollow-turn failure
  (ADR-0080). Replaced by this ADR.
