# ADR-0048: Working-Memory Context Hygiene

**Status:** Implemented (2026-06-20) — D1–D8 + follow-ups shipped; DB migration 007 applied to live `cambrian-db`. As-built detail in `CURRENT_CODEBASE_STATE.md` §ADR-0048. Residual: D2 spreading off by default; no agent-side `resolve` action for `content_cid`; truncation detection heuristic (no `finish_reason`); embed-summary-vs-full a flagged choice. (Original sequencing: D1 first, then D2, then the rest; D5 an independent live-security fix.)
**Date:** 2026-06-19
**Author:** Afsin
**Depends on:** ADR-0041 (Local Recurrent Workspace — typed working memory, `ToolCard` offload, recurrence gate), ADR-0022 (Global Workspace — `ContentStore` / `ContextRef` / `assemble_context`), ADR-0034 (tag-based read scoping — the predicate `GetContextNode` was bypassing), ADR-0036 (agent-pull ReAct loop — why PrimeForStep is unwired), ADR-0015/0029 (mnemonic_fact step-output recording; episodic `session_id` tag), ADR-0025 (heuristic memory pre-filter precedent), ADR-0044/0045 (semantic tool retrieval + two-tier disclosure).
**Relates to:** `docs/requirements/WORKING-MEMORY-CONDENSATION.md` (R2/R3 done, R7 seam done; this ADR records the grilled design for the rest).

---

## Context

A live `analyst_agent` trace showed `<LTMContext>` bloating every step — a 6.9 KB explanation carried forward, the prior step result appearing 3×, raw error JSON, and a multi-paragraph `describe_tool` spec persisting forever. The external diagnosis was "the prompt is badly authored." Investigation showed the opposite: `<LTMContext>` is the *runtime output* of the assembler (`WorkingMemory.assemble` + `ToolCard` summarization + `assemble_context`), so the fix is **make the existing condensation actually condense — and stop generating the redundancy at the source** — not rewrite the prompt.

The decisive root cause, confirmed in code during the grilling: **the agent's own step output is written to LTM and recalled back into the same run.** `dag_executor.go:906` feeds every step `Output` into `RecordExecution` (`memory/agent.go:264`), which stores `fmt.Sprintf("step_%d: %s", …)` as a `mnemonic_fact`. The mandatory `seed_recall` (`react.py:234`) then returns that `step_N:` blob — a **compounding feedback loop** (each step recalls a bigger accreted blob, and LTM fills with ever-larger snapshots). Verified: `QueryService.Search` applies only scope + ACL, **no session filter**, and the `session_id` is available on both ends (written at `agent.go:691`, readable at `query.go:112`).

---

## Decisions

### D1 — Same-session step-record exclusion in recall (the loop fix) — *ships first*

`QueryService.Search` excludes the current run's own auto-recorded step blobs. Read `sid` from `SessionIDFromContext(ctx)` (already present), **over-fetch** (`TopK` 10→~25), then drop results where `metadata["session_id"] == sid && metadata["source_agent"] == "System"` (the `step_N:` shape), return top 10. **Exclude** (not down-weight), **narrow** (step records only — a deliberate in-run `remember()` stays recallable), **post-hoc** (matches the existing `aclAllows` pattern). This is a deterministic cost/safety filter, not value-routing (Zero-Hardcode-clean, cf. ADR-0025's error pre-filter). Verified **single-path**: `PrimeForStep` has zero non-test call sites and the per-step workspace is built from `DependsOn` CIDs + 500-char snippets, so the workspace path is not a re-injection source.

### D2 — Associative pull: port `SpreadingEngine` into `QueryService` — *next*

The agent's single upfront `seed_recall` is a flat top-k; PrimeForStep's value was graph spreading (BFS over `document_edges`, ADR-0017). Bring that into the **pull** path: inject `SpreadingEngine` into `QueryService`, and after the seed `Search`, optionally `Spread` (bounded hops) → activation-rank → cap. The one per-plan-step recall becomes associatively rich → fewer ReAct inner iterations. Server-side only — the SDK is unchanged. **Flag-gated, bounded, measured** (spreading runs on every recall; quality depends on `document_edges`).

### D3 — `PrimeForStep` stays unwired

It is superseded by ADR-0036 (executor-push → agent-pull). Wiring it would (a) double-inject LTM (push + the agent's own pull), and (b) re-open the D1 loop (its seed search is `ScopeSystem`, unfiltered). **Do not re-wire the push.** Apply the D1 same-session exclusion to its seed search **defensively** (in case it is ever re-wired) and correct the stale "Phase 3: call PrimeForStep" comment in `dag_executor`.

### D4 — Agent content offload write path (R7 activation) → ContentStore

Add a `PutContextNode` RPC that writes to the **ephemeral `ContentStore`** (not the durable `ArtifactVault`) — R7 offloads are transient working memory, GC'd at plan end. Wire `agent.substrate.put_context_node` as `WorkingMemory.offload_fn`. **Session-gate reads:** add an explicit `OwnerSession` field to `nodeRecord`, derived from `ctx` at `Put` (the param is currently ignored); `GetContextNode` reads the caller session from `ctx` and returns not-found on mismatch. **Fail-open only when owner is empty** (system/legacy content), **fail-closed when an owner is set** (R7 agent content always gated). Single-owner for v1 — the content-addressed dedup-across-sessions quirk (second writer denied reading identical bytes) is flagged, owner-set deferred. GC is plan-scoped → R7 intra-step offload/drill-down is safe with **no keep-set change**.

### D5 — Remove the `GetContextNode` LTM fallback (live ADR-0034 hole)

`GetContextNode` (`server.go:645`) falls back to `VectorStore.GetByID`, returning raw LTM doc text **bypassing the `ScopedVectorStore`** every other read goes through — any agent can read any LTM doc by id, scope-free. **Remove the fallback**: `GetContextNode` serves **only** `ContentStore` cids; LTM is reached solely through scope-filtered `QueryMemory`. Independent of R7; ship whenever.

### D6 — Tool-output promotion to LTM (R8) — kernel-orchestrated, LLM-judged

Valuable tool outputs should reach durable LTM, not just the transient ContentStore. The **kernel orchestrates, the LLM judges** (Zero-Hardcode-clean): `ToolExecutor` feeds successful tool outputs into the existing **Tier-1 → Tier-2 curation** (`RecordExecution` pattern, one source over from step-results); the **Tier-2 LLM scorer** decides FULL/FACT_ONLY/DROP. A **deterministic pre-filter** (skip `error`/`denied`, size floor) sits in front for cost — cost control, not value-routing. No new RPC, no agent involvement (the `remember(cid)` agent-explicit variant was rejected). Tier-2 dedup absorbs the overlap with the agent's step-result summary.

### D7 — Supersession collapse (R4 + R5) — one render-time pass

R4 (a consumed `describe_tool` spec) and R5 (failed attempts superseded by a later success) are the same shape: an entry made **moot by a later event** still occupying the prompt. Add one `_collapse_superseded` pass in `WorkingMemory.assemble` (alongside `_dedup`), **non-destructive** (render-time only — the recurrence gate's full `wm.cards` evidence stays intact). Detection reuses the recurrence gate's existing `ToolCard` tracking. **R4 collapses immediately** on consumption (the tool was called); **R5 keeps failed attempts one turn** (hysteresis — the agent may still reason about *why*) then collapses to `<note>X failed ×N, then succeeded</note>`.

### D8 — (companion, prompt structure) Action protocol → dedicated `<ActionProtocol>`

Not strictly working memory, but grilled alongside it: the ReAct action menu + behavioral rules currently live inside `<OutputSchema>`, making the recency-anchored last line a `final_answer` format spec that biases premature termination. Move them to a **dedicated `<ActionProtocol>` section** (keep `<System>` lean); `<OutputSchema>` keeps only the per-turn action JSON. May be split into its own prompt-standard ADR.

---

## Consequences

- **Sequence:** D1 (loop fix, ~10 lines, high-value) → D2 (associative pull) → D4/D5/D6/D7/D8. D5 is a standalone security fix.
- **Already shipped** (recorded for completeness): R2 content-aware `_summarize`, R3 `assemble` dedup, R7 offload seam (inert until D4's RPC lands) — `python-sdk/cambrian_agent_sdk/working_memory.py`.
- **Tiered memory model made explicit:** live `WorkingMemory` buffer → ephemeral `ContentStore` (CAS, D4) → durable LTM (pgvector/RAG, D6). Two cid-based arrows: **offload** (buffer→CAS) and **promote** (CAS→LTM).
- **Risks to measure:** D2 spreading cost/latency per recall and `document_edges` quality; D6 doubled Tier-2 scoring load and step-result overlap (mitigated by dedup); D4's single-owner dedup quirk.
- **Out of scope:** ARD (Agentic Resource Discovery) interop — deliberately excluded; not the current subject.

---

## Amendment A1 — Recall provenance attribution & freshness (source monitoring) (2026-06-23)

**Status: D9 + D10 IMPLEMENTED (2026-06-23); D11 deferred to a follow-on ADR.** `go build ./...` / `go vet` clean; `internal/substrate/network` Go tests green (`TestQueryMemory_FoldsProvenanceAndFreshness`, `TestQueryMemory_OmitsZeroTimestamps`) + full Python SDK suite green (298 tests, incl. 9 new provenance/freshness tests in `test_working_memory.py`). As-built: D9 author attribution is pure-SDK (the kernel already shipped `source_agent`/`session_id`); D10 added a 3-key temporal fold in `querymemory.go` (no proto change, no migration) + a shared `memory_provenance_attrs()` renderer. D11 (`trust_at_write`) is unimplemented by design — it needs new write-path state + a DB migration. Motivated by the agent-memory literature audit (`docs/research/agent-memory/SUMMARY.md` §2.1, §2.12; MemIR arXiv:2605.25869). The audit's headline framing — "a recalled fact and a tool output are indistinguishable to the LLM; recall doesn't surface where a fact came from" — is **overstated and corrected below**: the *channel* tag (`source='LTM'`) already ships. The real, narrower gap is **provenance-of-author and freshness**: a recalled fact carries no *who wrote it / when / how trusted / how fresh*. This amendment surfaces that, in three phases ordered by cost, not by leverage.

**Why.** Source-monitoring errors (the agent confabulating a recalled fact with its own training, or trusting a stale/poisoned fact) are a documented failure mode. The cheapest mitigation is to *show the agent the provenance it's already being handed*. The kernel stamps non-forgeable provenance at write time (D1: `metadata["session_id"]` + `metadata["source_agent"]`; ADR-0035 kernel-derived classification); the recall path then **throws most of it away at render**.

**Implementation finding (corrects the audit).** Verified in code, the system is further along than the audit assumed:
- `<memory source='LTM'>` is already emitted — `react.py:1160` and `working_memory.py:496`. The *channel* is tagged; the audit's "where did this come from" is, at the channel level, already solved.
- `querymemory.go:69` marshals the **entire** `Document.Metadata` jsonb into `MemoryResult.metadata` (proto field 3), and `clients.py:63` already returns it to the SDK as `{"text","score","metadata"}`. So **author attribution (`source_agent`, `session_id`) already arrives at the SDK and is silently discarded** — `_render_memory_children` (`react.py:1150`) reads only `text` + `content_cid`.
- `written_at`, `last_accessed`, and `activation_strength` live on `Document` **struct columns** (`vector_store.go:67-71`), are selected from pgvector, but are **not** folded into the marshaled `metaJSON`, so they do not reach the SDK today.
- `trust_at_write` **does not exist** — no field on `Document`, no metadata key, no write-path stamp.

This finding sets the phasing: what's already on the wire is nearly free; what's on the struct is a small mapping touch; what doesn't exist is real work.

### D9 — Author attribution on recall render — *pure SDK, zero kernel/proto change* — ships first

`_render_memory_children` (`react.py:1150`) parses the `metadata` JSON string it **already receives** and renders the author provenance it contains:

```
<memory source="LTM" author="agent:planner_x" session="…" content_cid="…">…</memory>
```

`author` is derived from `metadata["source_agent"]` (kernel-stamped, D1); a `System` source_agent (the auto-recorded step blob) renders as `author="system"` so the agent can discount its own machine-generated echoes. **No proto change, no kernel change, no migration** — the data is on the wire and being dropped. This is the literal "fastest leverage-per-line" item. Bounded: attributes are rendered only when present; absent keys omit the attribute (no empty `author=""`).

### D10 — Freshness signal on recall render — *small kernel mapping, no migration*

Surface `written_at`, an `age`, and a derived `freshness` label so the agent can decide whether to re-verify before acting:

```
<memory source="LTM" author="…" written="2026-05-07" age="47d" freshness="stale">…</memory>
```

Kernel side: extend the `MemoryResult` mapping at `querymemory.go:80` to carry `CreatedAt` / `LastAccessedAt` / `ActivationStrength` — **either** folded into the existing `metaJSON` (smallest diff, no proto change) **or** as explicit typed proto fields (cleaner, costs a regen). Columns already exist (`vector_store.go:67-71`); **no DB migration**. SDK side: `freshness` is a render-time label derived from the effective activation (`activation × e^(-λ·age)`, the same formula as `temporal_decay.go:85`) crossing operator thresholds (e.g. `< 0.05 → stale`, `> 0.5 → fresh`, else `aging`).

**Zero-Hardcode boundary (non-negotiable, per CLAUDE.md).** The `freshness` label is threshold-deterministic, and that is *correct here*: it is a **rendering/lifecycle** concern, not value-routing. It states a fact about the memory ("this is 47 days old, low activation"); it does **not** decide which agent runs or which task is chosen. The agent's LLM still decides whether a stale fact warrants re-verification. No `freshness` threshold may ever gate agent-to-task routing — it lives in the memory layer (cf. the D1 same-session filter, which is likewise a deterministic cost/safety filter, Zero-Hardcode-clean).

### D11 — Write-time trust attestation (`trust_at_write`) — *deferred to a follow-on ADR (needs new state + migration)*

The fullest source-monitoring signal — "this fact was written by a high/low-trust channel" — requires state that **does not exist**: a kernel-derived trust level stamped at write time (extends ADR-0035 classification), persisted (a new `Document` field or metadata key → **DB migration**), and surfaced at recall (`trust="0.82"` / demote-in-rank for untrusted channels). This is the natural **read-side memory-poisoning defense** the audit raises separately (§2.6; arXiv:2606.04329, Trojan Hippo arXiv:2605.01970): a memory from an untrusted tool (MCP, user-supplied) is flagged or demoted at recall. **Out of scope for this amendment** — it is not additive rendering, it is a new write-path contract + migration + a re-rank term, and it deserves its own ADR co-scoped with the read-side poisoning defenses. Recorded here so D9/D10 are understood as the *cheap front half* of a larger source-monitoring story, not the whole of it.

### A1 sequencing note

D9 (pure SDK) ships first and standalone — it is the highest leverage-per-line and unblocked by everything. D10 (small kernel mapping, no migration) follows. D11 is deferred to a dedicated ADR that co-scopes write-time trust attestation with read-side memory-poisoning defenses (§2.6). None of the three touches agent-to-task routing. **All three are gated for *quality* impact by the LoCoMo-style memory benchmark** (the audit's §2.14 / the T-Mem note's regression bar) — the benchmark is a sibling work item, built first so the lift from provenance rendering is *measurable* rather than asserted.
