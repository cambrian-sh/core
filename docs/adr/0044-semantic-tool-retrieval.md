# ADR-0044: Semantic Tool Retrieval — Serving Task-Relevant Tools, Not the Whole Registry

**Status:** Proposed (2026-06-11) — design recorded via a grilling session; not implemented. Note: ADR-0043 D10 phases native `tools/` toward MCP-sourced tools, so the registry this retrieves over is increasingly MCP-populated.
**Date:** 2026-06-11
**Author:** Afsin
**Depends on:** ADR-0039 (kernel-owned tool registry — `ToolExecutor`, `ToolRegistry`, grants, `AvailableTools`), ADR-0043 (MCP Tool Provider — dynamic discovery adds many tools, which is what makes the registry large enough to need this), ADR-0022 (Global Workspace — the push/pull retrieval pattern this mirrors), ADR-0015/0025 (the pgvector `VectorStore` + `DocType` model reused for the tool index).
**Relates to:** ADR-0041 (LRW — the `memory_query` pull is the direct analogue of the `find_tools` pull; the `substrate.embed` RPC is reused).

---

## Context

With the kernel-owned tool registry (ADR-0039) and dynamic MCP discovery (ADR-0043), an agent can be granted many tools (native + every MCP server's tools). Today `AvailableTools` returns the **entire** granted set, and the SDK injects all of it into the prompt's tool menu — observed at **~15k tokens per LLM call**, dominated by large tool schemas (MCP tools especially). Every call carries every tool, relevant or not.

This is a retrieval problem, and Cambrian already solved the equivalent problem for *context*: the Global Workspace **pushes** a relevance-assembled seed and `memory_query` **pulls** more on demand. Tools need the same treatment — serve only the tools relevant to the task, and let the agent ask for more when a need arises.

The hard part is **query–tool semantic alignment**: an agent's need is phrased as a *goal* ("find out who afsin.asf is") but a tool is described as a *capability* ("search the web"). Naive cosine between them retrieves the wrong tool. Getting this right — without adding LLM calls to the hot path or a hallucination surface — is the substance of this ADR.

---

## Decision

Add **semantic tool retrieval**: the kernel ranks granted tools by embedding similarity to a query and serves only the top-k relevant ones, in a **hybrid push/pull** model mirroring the memory architecture.

### D1 — Hybrid retrieval: ranked push + `find_tools` pull

Two modes, mirroring Global-Workspace-push + `memory_query`-pull:
- **Push:** at task start, the kernel ranks tools against the **task** and serves a small default menu.
- **Pull:** a new `find_tools(need)` ReAct action — when the agent hits a concrete capability need mid-loop, it queries and gets matching tool descriptors back, exactly like `memory_query` returns facts.

The push keeps trivial/obvious tasks from needing a round-trip; the pull is the safety net when the default ranking missed a tool the task later needs. Because the pull exists, the push can be tiny.

### D2 — One query-aware retrieval RPC

`ListTools` gains optional `query` + `k`. **Empty query ⇒ the full granted menu** (backward compatible); **query present ⇒ ranked top-k**. The push is `ListTools(task, k_small)` at menu-build; the pull (`find_tools`) is `ListTools(need, k)`. One ranking path, two call sites — there is only one ranking algorithm, so there is one RPC. `find_tools` is the agent-facing *action* name that maps to it.

### D3 — pgvector-backed, behind a `ToolRetriever` port (hexagonal)

- Tool vectors live in **pgvector** as a new **`DocTypeTool`** — reusing the native `<=>` cosine search (no hand-rolled cosine in Go) and persisting across restarts (no re-embed every boot).
- The tool domain depends on a **`ToolRetriever` port**: `Rank(ctx, query, grantedNames, k) → []toolName`. An infrastructure **`VectorToolRetriever` adapter** implements it via `VectorStore` (`DocTypeTool`). `ToolExecutor` never imports `VectorStore`, `DocType`, or cosine — they are contained in the adapter.
- **Framing:** tools are **not** memory. pgvector is a *shared associative index*; memory (`mnemonic_fact`), agents (`agent_profile`, already indexed this way), and tools (`tool`) are sibling domains that share the index through ports, each keeping its own authority and lifecycle. `DocType` namespaces them.

### D4 — Grant ∩ relevance: authorized-by-construction

The grant check is expressed **inside** the pgvector query: `ToolExecutor` resolves the agent's granted tool names and passes them through the port; the adapter renders them as `SearchOptions.Filter`, so the cosine top-k is already authorized — an ungranted tool is excluded by the `WHERE` before ranking and can never appear. `tools_unrestricted` ⇒ no filter (rank over all `DocTypeTool` rows). Grants are a *query-time* parameter (per-agent, dynamic); they are never baked into the tool document.

### D5 — Deterministic tool docs + asymmetric prefixes (the matchability fix)

- **Asymmetric `nomic-embed-text` prefixes:** the need is embedded as `search_query: <need>` and each tool doc as `search_document: <doc>` — the aligned-space fix for query/document asymmetry. Mandatory.
- **Deterministic tool doc** (v1): `name + description + arg names (+ category)`, assembled by a **`ToolDocBuilder`** seam — **no LLM, zero hallucination surface**.
- **LLM enrichment is deferred** behind the same seam: a gated upgrade that generates capability statements + synonyms + example needs per tool (one-time, persisted), turned on only if measurement shows deterministic docs under-retrieve. If enabled, it is grounded on the real description+schema (cannot invent capabilities) and affects *ranking only* — never authorization (A1.5).

### D6 — Pure cosine + a similarity floor; no LLM in the retrieval hot path

- **Top-k** (small, configurable; ~5 for the push) with a **tunable cosine floor**: return up to k tools clearing the threshold, and **empty when none clear it**. An empty menu — "no tool fits" — is a **grounding safeguard**: it forces the agent to yield or answer honestly instead of being handed a misleading tool to confabulate with. Floor is config (tuned by measurement), never a Go constant.
- **HyDE** (query-side hypothetical-document rewrite) and **LLM re-ranking** are **deferred** behind seams. Rationale: both add a per-`find_tools` LLM call on the hot path with a hallucination surface, and their value is largely pre-empted by prefixes + deterministic docs + a grant-filtered candidate set that is already tiny (re-ranking earns its keep at top-50-of-millions, not top-k-of-a-handful).

### D7 — Need phrasing: trust + steer the agent

The `find_tools` prompt steers the agent to phrase the need as a **verb-first capability** ("search…", "scrape…", "read a file…"), embedded as-is. The push query is necessarily the raw task (a goal — coarse, but it's only the default); the pull query is the agent's sharper capability phrasing. **No kernel-side LLM normalization** (that would be HyDE by another name, and need-phrasing is the awareness layer's job — Zero-Hardcode). If weak-model phrasing drags pull recall down, the floor catches the bad matches and the deferred normalization/HyDE is the measured upgrade.

### D8 — Index lifecycle

Tool docs are embedded at **discovery** — native tools at `LoadRegistry`, MCP tools at `tools/list` — and upserted into pgvector. **MCP re-sync** (a server reconnecting and re-advertising) recomputes that server's tool docs and drops stale ones. Persistence means re-embedding happens once per tool version, not every boot.

---

## Consequences

**Positive**
- Prompt size drops from "every granted tool" to a task-sized menu (target: well under the ~15k-token observed bloat).
- The fix lives in the **kernel** (one place, every agent/SDK benefits), behind a port (hexagon intact), reusing pgvector cosine + the LRW embedder.
- The floor doubles as a grounding safeguard against the confabulation failure mode (an honest empty menu beats a wrong tool).
- No LLM added to the retrieval hot path in v1; the precision upgrades (enrichment, HyDE, re-rank) are seams, switched on by data.

**Negative / risks**
- Re-embedding tool docs at discovery adds boot/connect cost (bounded; persisted, so one-time per tool version).
- A mis-tuned floor starves the agent (too high) or pollutes the menu (too low) — mitigated by it being config + measurable.
- Weak-model need phrasing can hurt pull recall — mitigated by the floor and the deferred normalization upgrade.
- `tools_unrestricted` ranks over all tools (no grant filter) — acceptable; that is exactly the bloat case this fixes.

---

## Testing decisions

Hermetic, behavior-level (a fake/in-memory `ToolRetriever` or a test pgvector):
- **D1/D2 push:** `ListTools(task, k)` returns ≤ k tools ranked by relevance; empty query returns the full menu (backward compat).
- **D1 pull:** a `find_tools(need)` action returns matching tool descriptors that become callable.
- **D4 authorization:** a relevance query never returns an ungranted tool (grant filter); `tools_unrestricted` ranks over all.
- **D5:** the embedded doc includes name+args; `search_query:`/`search_document:` prefixes applied.
- **D6 floor:** a need with no relevant tool (e.g. web-search need over a file-tool-only registry) returns an **empty** menu, not a wrong tool; a relevant need returns the right tool above the floor.
- **D8:** MCP re-sync recomputes a server's tool docs and drops removed ones.

## Falsification

Accepted when, on the local benchmark: (1) median prompt tokens/call drop materially versus the full-menu baseline; (2) for a set of single-capability tasks, the correct tool is in the served top-k; (3) a task with no relevant tool yields an empty menu rather than a misleading one. Until then, **Proposed**.

## Out of scope (deferred behind seams)
- LLM tool-doc **enrichment** (D5), **HyDE** (D6), **LLM re-ranking** (D6) — gated upgrades, switched on by measurement.
- Kernel-side **need normalization** (D7).
- A **pgvector→in-memory** swap of the `ToolRetriever` (the port allows it; not needed).
- Ranking the agent's own in-process `@tools` (few; left in the menu as-is).
