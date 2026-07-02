# ADR 0006 — Capability-Based Orchestration and A2A External Agent Integration

**Status:** Implemented  
**Date:** 2026-05-07  
**Deciders:** Afsin  
**Source Plan:** `plans/3- Interview.md`

---

## Context

The current orchestration model is **tool-centric**: the Planner emits `required_tools: [...]` per step, the Gatekeeper's Declaration Filter hard-matches those tool names against `AgentManifest.Tools`, and the auction selects among the survivors. This produces three structural problems:

1. **Single-name bottleneck.** The Planner can only express capability needs through exact tool identifiers. If an agent names its tool `"csv_writer"` and the Planner emits `"csv-writer"`, the agent is eliminated before it ever bids. The Planner becomes an implicit router, which violates the Zero-Hardcode Rule's spirit.

2. **Interview Vector is inert.** ADR 0002 introduced cognitive fingerprints (Interview Vectors) stored in pgvector per agent. The Gatekeeper was specified to gate on semantic similarity between the task and the agent's fingerprint at auction time (ADR 0002 §Layer 2). This gate was never implemented; the vector is written but never queried during candidate selection.

3. **No external agent interoperability.** All agents are internal Python/binary gRPC processes. There is no path for external agents — agents built in other frameworks, languages, or organisations — to participate in auctions.

---

## Decision

### 1. Semantic Intent Replaces Tool Names

The Planner no longer emits `required_tools`. The step's natural-language `Query` field is the **Semantic Intent** — the embedding substrate for capability matching. No new field is added to `domain.Step` or the proto.

The Planner prompt is updated to:
- Show each agent's `AGENT_DESCRIPTION` free-text (already parsed and stored by `BBoltAdapter`) instead of `AgentManifest.Tools` lists.
- Instruct the LLM to emit only `query` and `depends_on` per step.
- Remove all reference to `required_tools` from the output schema.

`RequiredTools []string` is removed from both `domain.Step` and `domain.AuctionTask`. The proto freeze (Issue #016) is preserved — `RequiredTools` was never in the proto wire format; it lived in domain structs only.

### 2. Gatekeeper Layer Restructure

The three-layer Gatekeeper (ADR 0002) is retained but reshaped:

**Layer 1 — Declaration (format-only hard filter):**  
`PassesDeclaration` is narrowed to `RequiredFormats` compatibility only. The tool-name check is removed entirely. Agents with no manifest still pass unconditionally (legacy behaviour preserved).

**Layer 2 — Semantic Similarity Gate (ANN search, replaces tool gate):**  
The `AuctionTask.Description` (the step's `Query`) is embedded once per `FindCandidates` call. A single pgvector ANN search retrieves the top-K agents whose Interview Vectors are nearest to the task embedding. Agents whose cosine similarity falls below `DefaultSimilarityThreshold` (0.5) are eliminated. This is the Interview gate that ADR 0002 specified but left unimplemented.

The Gatekeeper gains two new dependencies via narrow interfaces:
- `Embedder` — embeds the task description (same interface defined in `interview_worker.go`).
- `InterviewSearcher` — wraps `VectorStore.Search`; returns `(AgentID, SourceHash, similarity)` tuples for agent-profile documents above the threshold.

**Layer 3 — Merit ranking (unchanged):**  
Survivors of Layers 1 and 2 are ranked by `GatekeeperScore` as before. The similarity score is a gate, not a ranking signal — it does not merge into `GatekeeperScore`.

**Homeostat:**  
`validatePlan` loses the capability check (`buildKnownTools` + tool-name validation). It becomes structural-only: DAG cycle detection and index-bounds checking. This is correct because capability matching has moved from Planner-time (static tool names) to Gatekeeper-time (dynamic ANN search).

### 3. ANN-First Candidate Discovery

The Gatekeeper queries pgvector before it queries bbolt. The pipeline becomes:

```
Embed(task.Description)
  → InterviewSearcher.Search(embedding, threshold=0.5, topK=N)
  → for each result: BBoltAdapter.GetManifest → PassesDeclaration(RequiredFormats)
  → computeMeritScore → sort → top-K candidates
```

This trades N sequential point-lookups (one `GetInterviewVector` per candidate) for a single ANN query whose result set is already similarity-ranked. On edge hardware with a large agent registry this is the correct trade-off: one pgvector round-trip dominates N bbolt reads.

### 4. A2A External Agent Transport

External agents — those living outside the runtime, built in any language or framework — are first-class citizens via the **Google A2A protocol** as a second transport alongside gRPC.

**`domain.RuntimeA2A`** is added alongside `RuntimePython` and `RuntimeBinary`. `domain.AgentDefinition` gains `A2AEndpoint string` — the base URL of the external agent (e.g. `https://agent.example.com`). The Agent Card is fetched from `{A2AEndpoint}/.well-known/agent.json`.

**`A2AClient`** (`internal/metabolism/a2a_client.go`) owns all A2A protocol concerns:
- Fetches Agent Cards at registration time.
- Translates `Handoff` → A2A `Task` (flat mapping: `Payload.Data` → `message.parts[0].text`, `Context` → `task.metadata`).
- Translates A2A `Task` response → `Handoff` (`message.parts[0].text` → `Payload.Data`).
- Delivers tasks via HTTP POST per the A2A spec.

`AgentManager.CallAgent` dispatches to `A2AClient` when `def.Runtime == domain.RuntimeA2A`. The gRPC path is unchanged. The `Handoff` envelope remains the universal internal representation — `A2AClient` is the sole translation boundary.

The proto freeze is preserved. `A2AClient` introduces HTTP as a transport; it does not modify any proto message.

### 5. External Agent Interview — Agent Card Embedding

External A2A agents acquire an Interview Vector at registration time without scenario generation or `RequestProposalFrom` calls. `BBoltAdapter` fetches the Agent Card and concatenates:

```
{card.description}\n{card.skills[0].description}\n{card.skills[1].description}...
```

This text is embedded by `Embedder` and stored in pgvector as the agent's cognitive fingerprint via `ProfileStore.SaveProfile`. The result is identical in shape to an internal agent's Interview Vector. The ANN search in Layer 2 treats internal and external agents uniformly.

`computeDecay` applies on re-registration: if the Agent Card's version field changes, the old profile is archived and the new card text is embedded. Merit inherits with decay proportional to embedding distance between old and new card descriptions — the same decay formula as internal agents.

`buildScenarios` and `RequestProposalFrom` are bypassed entirely for `RuntimeA2A` agents. `InterviewWorker.processAgent` branches on `agent.Runtime`.

### 6. Package Structure

**New files:**

```
internal/metabolism/
  a2a_client.go           — HTTP client; Handoff↔A2A Task translation; Agent Card fetcher
```

**Modified files:**

```
internal/domain/agent.go         — Add RuntimeA2A, A2AEndpoint string
internal/domain/plan.go          — Remove RequiredTools []string from Step
internal/domain/auction.go       — Remove RequiredTools []string from AuctionTask
internal/metabolism/gatekeeper.go — Add Embedder + InterviewSearcher deps;
                                    Layer 1 = format-only; Layer 2 = ANN similarity gate
internal/metabolism/interview_worker.go — RuntimeA2A branch: Card embed, skip buildScenarios
internal/metabolism/executer.go  — RuntimeA2A dispatch to A2AClient
internal/storage/bbolt_adapter.go — A2A registration: Card fetch → embed → enqueue Interview
internal/awareness/planner.go    — Prompt: AGENT_DESCRIPTION only; no tool listing;
                                    no required_tools output instruction
internal/substrate/server.go     — Remove buildKnownTools call
internal/substrate/dag_executor.go — validatePlan: remove capability check; structural only
```

**New interfaces (consumer-side, in `internal/metabolism/`):**

```go
type InterviewSearcher interface {
    SearchByEmbedding(ctx context.Context, embedding []float32, threshold float64, topK int) ([]AgentSearchResult, error)
}

type AgentSearchResult struct {
    AgentID    string
    SourceHash string
    Similarity float64
}
```

---

## Alternatives Considered

| Decision | Alternative Rejected | Reason |
|---|---|---|
| Semantic Intent | New `SemanticIntent` field on `Step` | Proto-adjacent field risks LLM inconsistency between `query` and `intent`; `Query` already encodes intent; proto freeze applies |
| Semantic Intent | `RequiredCapabilities []string` replaces `RequiredTools` | Still a structured vocabulary the Planner must know; same naming fragility as tool names |
| Gatekeeper Layer 2 | Remove Declaration entirely | Format constraints (e.g. JSON output) are hard requirements with no semantic proxy; the hard filter earns its place |
| Gatekeeper Layer 2 | Similarity score folds into `GatekeeperScore` | Gate + rank conflates two distinct decisions; threshold eliminates genuinely unqualified agents; ranking is about preference among qualified ones |
| ANN search | N point-lookups (`GetInterviewVector` per candidate) | N pgvector round-trips on edge hardware; single ANN query returns a pre-ranked result set |
| ANN search | Embed vector in `AgentProfile` struct | `AgentProfile` loaded on every `FindCandidates` call; a 1 536-dim float slice per agent is wasteful when only a similarity score is needed |
| A2A placement | A2A replaces gRPC for all agents | Full Python SDK migration; proto effectively broken; edge-latency requirement violated by HTTP overhead on internal fast path |
| A2A placement | A2A as inbound Substrate interface only | Solves discoverability but not external agent invocation; half the integration story |
| A2A task delivery | Structured Handoff → A2A parts mapping | Requires A2AClient to understand Cambrian context key semantics; couples two vocabularies |
| A2A task delivery | `domain.A2AHandoff` type | Two diverging internal envelope standards; every pipeline stage would need to branch on envelope type |
| External agent Interview | Full Interview via A2A HTTP scenario calls | A2A task calls during registration block the registration path; Agent Card is the A2A-standard capability declaration — using it is architecturally consistent |
| External agent Interview | Agent self-declares capability vector in Card | Substrate trusts external vector without verification; security violation |
| Planner prompt | Keep tool listings, stop requiring output | Dead context in the prompt; LLM trained on tool-name vocabulary will continue emitting tool references |

---

## Consequences

- **`RequiredTools` is a breaking domain change.** Any code reading `step.RequiredTools` or `task.RequiredTools` must be updated. The Homeostat's capability check is removed; plan validity is structural only from this point forward.
- **Gatekeeper gains two new dependencies.** `Embedder` and `InterviewSearcher` must be wired at construction time in `cmd/orchestrator/main.go`. The initialisation order in CONTEXT.md §3 is updated accordingly.
- **Every agent needs an Interview Vector for Layer 2 to admit it.** Agents registered before this ADR have no vector; they will fail the ANN search and never enter auctions. A migration pass must run `InterviewWorker.processAgent` against all existing agents on first boot after deployment.
- **A2A agents are first-class.** An operator registers an external agent by adding its `A2AEndpoint` to config or via a future registration API; `BBoltAdapter` handles discovery and Interview automatically.
- **Planner quality becomes the capability signal.** There is no longer a structural check that a planned step can be executed by any registered agent. A step whose `Query` describes capability absent from the registry will find zero candidates and fail with `NO_WINNER`. The Planner prompt quality and the richness of `AGENT_DESCRIPTION` fields are now the primary defence against unresolvable plans.
