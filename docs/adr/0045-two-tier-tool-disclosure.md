# ADR-0045: Two-Tier Tool Disclosure — Terse Menu to Choose, Full Spec to Call

**Status:** Proposed (2026-06-13) — design recorded via a grilling session; not implemented. Extends ADR-0044.
**Date:** 2026-06-13
**Author:** Afsin
**Depends on:** ADR-0044 (Semantic Tool Retrieval — the `ToolRetriever` port, `DocTypeTool` index, `BuildToolDoc` seam, and the push/pull model this refines), ADR-0039 (kernel-owned tool registry — `ToolExecutor`, `AvailableTools`, grants, `ListTools`), ADR-0043 (MCP Tool Provider — the source of the verbose descriptions that motivate this).
**Relates to:** ADR-0041 (LRW — the agent loop that renders the menu and would gain the `describe_tool` action), the requirements doc `docs/requirements/capability-retrieval-tools-and-skills.md` (Part A).

---

## Context

ADR-0044 fixed retrieval **breadth** — serve the top-k task-relevant tools, not the whole granted registry. It did **not** fix retrieval **depth**: each served tool still dumps its *entire* description + full JSON arg schema, every turn.

Two concrete failures follow from this, and both are visible in the current code:

1. **Prompt bloat.** `ListTools` (`internal/substrate/network/execute_tool.go:92`) maps each `SystemTool` to a `ToolDescriptor` carrying the full `Description` + full `SchemaJson`, and the SDK renders all of it into the menu every turn (`build_output_schema`, `python-sdk/cambrian_agent_sdk/react.py:60`). Verbose MCP descriptions (Firecrawl) run ~1–2k tokens *each*; even top-k=3 is large.

2. **Diffuse-embedding mis-ranking.** `BuildToolDoc` (`internal/domain/tool_retriever.go:27`) embeds `name + full Description + arg names`. A 2k-token description averages into a vector spread across "files, transfer, parse, scrape, remote, PDF…", which spuriously matched "secure file **transfer**" and crowded out `execute_command` for a `terminal_agent` — the weak local model then confabulated a call on the wrong tool. **The bloat and the mis-ranking share one root cause: the full description is the embedded and served unit.**

The fix is a **depth control** complementing ADR-0044's breadth control: serve a terse capability summary by default, and deliver the full spec only for a tool the agent has actually committed to calling. The governing principle: **short to choose, full to call.**

This stays inside ADR-0044's stance — no LLM on the retrieval/disclosure hot path — and the Zero-Hardcode Rule (the deriver is deterministic; routing/need-phrasing stays in the awareness layer).

---

## Decision

Introduce **two-tier tool disclosure**. Tier-1 is a terse menu served always (and embedded for retrieval); Tier-2 is the full spec delivered on demand for one tool.

### D1 — The two tiers

- **Tier-1 — menu (always):** `name` + a **one-line** capability summary + arg **names**. Tens of tokens, not thousands. This is what the agent ranks and chooses over, and it is the unit embedded for retrieval.
- **Tier-2 — full spec (on demand):** the full prose `Description` + full JSON arg `Schema`. Needed only to *invoke correctly*, delivered only for the one tool the agent is about to call.

Tier-1 omits the **full schema** (keeps arg names only) and the **full prose**. Arg names are the high-signal-per-token backstop: even when the one-line summary is weak, `args: url, formats, onlyMainContent` discriminates.

### D2 — One deterministic `toolSummary()` deriver; no stored field

The one-liner is produced by a single **pure, deterministic function** `toolSummary(t SystemTool) string` (no LLM, ADR-0044-aligned), called in **both** places that need it:
- `BuildToolDoc` at **index time** (the embedded doc), and
- the `ListTools` handler at **serve time** (the menu line).

Calling one function in both guarantees the embedded vector matches the served line **by construction** — no embed-vs-menu drift class. No new field is added to `SystemTool`; nothing in the tool model or registry migrates. **Operator-authored summaries are deferred** — they layer on later as a trivial override (`if t.Summary != "" { use it } else { toolSummary(t) }`).

### D3 — The derivation algorithm

`toolSummary(t)` is a small deterministic hybrid:
1. **Normalize** — collapse whitespace; strip leading markdown header markers (`#`, `*`) and code fences (MCP descriptions are often markdown).
2. **First sentence-or-line** — the first non-empty segment terminated by `. `, `\n`, or end-of-string.
3. **Hard cap** at a named constant (`toolSummaryMaxChars`, default ~120 chars ≈ 30 tokens), truncated at a **word boundary** with `…`.
4. **Name fallback** — an empty/whitespace description humanizes the name (`firecrawl_scrape` → `"firecrawl scrape"`), so every tool gets a non-empty summary.

The cap is a constant (tunable later by measurement), not a config knob. First-sentence-or-line beats raw char-truncation on the Firecrawl "boilerplate title then real sentence" case; the arg-name backstop (D1) carries discriminating signal when the prose is thin, so the deriver need only be *short and non-misleading*, not perfect.

### D4 — `describe_tool(name)` owns Tier-2; `find_tools` stays Tier-1 discovery

Two orthogonal agent actions, each one job (Zero-Hardcode-clean):
- **`find_tools(need)`** — the relevance **discovery** pull (ADR-0044 D1), now returning **Tier-1 short forms** for its matches, merged into the menu uniformly with the push.
- **`describe_tool(name)`** — a new **by-name lookup** action returning **Tier-2** for the exact tool the agent has committed to calling.

`describe_tool` owns Tier-2 (rather than having `find_tools` return full detail) because **most calls target a tool already in the Tier-1 menu, not a freshly-discovered one** — `find_tools` only fires for capabilities *not* in the menu (`react.py:345`). A Tier-2 path that rides on `find_tools` would leave the common case (a menu tool needing its full schema) with no clean route. `describe_tool` is keyed by commitment: the agent has chosen tool X, so fetching exactly X's full spec is a deterministic lookup, and it doubles as a grounding gate — the agent cannot emit args it never saw, which structurally cuts the confabulation that motivated this ADR.

**Cost accepted:** `describe_tool` adds one round-trip per *distinct tool actually used* (1–3 per task), against a terse-menu saving that recurs *every turn* — net strongly positive on the token/latency budget. The optional eager-rank-1 prefetch that would remove this round-trip is deferred (D8).

### D5 — Wire format: extend the request, reuse the descriptor

- **`ListToolsRequest`** gains `names []string` + `full bool` (additive, backward-compatible). `describe_tool` calls it with the one name + `full=true`. The push and `find_tools` use the existing query+k path with `full=false`. **No new RPC.**
- **`ToolDescriptor`** is **unchanged**; its fields carry tier by request mode:
  - `full=false` (menu / `find_tools`): `Description = toolSummary(t)`; `SchemaJson` reduced to **arg-names-only** (`{"properties":{"url":{},…}}`).
  - `full=true` (`describe_tool`): `Description` = full prose; `SchemaJson` = full schema.
- The reduced short-mode schema **reuses the existing `toolArgNames` extraction** (`tool_retriever.go:44`) and the SDK's existing menu renderer — which receives the same fields, just shorter strings, so **Tier-1 needs no SDK rendering change**.
- The **embedding change is wire-free**: `BuildToolDoc` swaps full `Description` → `toolSummary(t)` (it already appends arg names). Embed and menu share the deriver per D2.

### D6 — `describe_tool` is grant-gated, fail-closed

`describe_tool(name)` resolves through the same `AvailableTools(agentID)` authority set (`tool_executor.go:206`): an ungranted or unknown name returns a **not-available** response (absence, no existence leak), mirroring `Execute`'s fail-closed denial. This reuses the existing grant path (no new permission logic) and keeps the invariant **what you can describe == what you can see in the menu == what you can call**. `find_tools` results are already grant-filtered, so every legitimately describable tool is in the available set by construction.

### D7 — Migration is self-applying on boot

No migration mechanism is built. Tool indexing is **unconditional + idempotent-upsert-by-name + runs every boot/re-sync**: native tools via `toolIndexer.IndexAll` at startup (`cmd/orchestrator/main.go:652`), MCP tools via `mcpToolSink.resync` (`main.go:911`). Shipping the new `BuildToolDoc` *requires* restarting the Go binary, and that restart re-embeds every tool under the short form — so the re-index is **atomic with the deploy**, with no stale-vector window and no backfill.

### D8 — v1 scope

In v1: the terse Tier-1 push, `find_tools` returning Tier-1, `describe_tool` Tier-2 on demand, short-form embedding, grant-gating, self-migration.

**Deferred** (measure first): **eager Tier-2 for rank-1** (pre-attaching the full spec of the top-ranked tool to the push to save the `describe_tool` round-trip — re-introduces a full-spec payload in the always-pushed menu on a speculative heuristic); **formal `describe_tool` result caching** (largely free already via working memory; the eviction concern belongs to the SDK dynamic-context work, not here); **operator-authored summaries** and **LLM enrichment** (ADR-0044 stance). ADR-0044-08's benchmark is the measurement that decides whether the round-trip is worth optimizing.

---

## Consequences

**Positive**
- The diffuse-embedding mis-ranking is fixed at its root: the embedded unit becomes the sharp short form (D2/D5), so the `terminal_agent`/Firecrawl failure class disappears.
- Per-turn menu cost drops from ~1–2k tokens/tool to tens of tokens/tool; the full schema is paid for only on tools actually invoked (1–3), not the whole granted set.
- A smaller, stable fixed scaffolding unblocks the SDK dynamic-context budgeter (sibling requirements doc): the token-aware assembly now sizes a small, stable per-turn cost.
- `describe_tool` is a grounding gate that reduces confabulation, not just a fetch.

**Negative / costs**
- One extra LLM round-trip per distinct tool used (D4) — accepted, net-positive, optionally removable later (D8).
- The agent action surface grows by one (`describe_tool`) — a mild cognitive-load increase, justified by its narrow, deterministic semantics.
- `ToolDescriptor.Description` is mode-dependent (D5) — bounded by the explicit `full` flag the caller sets; never a guess.

**Neutral**
- ADR-0044's retriever, `DocTypeTool` index, grant∩relevance filter, cosine floor, and asymmetric prefixes are all unchanged; this ADR only changes the *content* of the embedded/served doc and adds the Tier-2 fetch action.

---

## Alternatives considered

- **Stored `SystemTool.Summary` field (vs D2 pure function).** Rejected for v1: adds model/proto migration and a staleness class against changed descriptions, buying only operator-authored summaries — which layer on later as an override.
- **`find_tools`-returns-full as the Tier-2 path (vs D4 `describe_tool`).** Rejected: leaves the common case (a menu tool needing its full schema) with no direct Tier-2 route, since `find_tools` only fires for tools *not* in the menu.
- **New `ToolDescriptor.Summary` field (vs D5 field reuse).** Rejected: forces the SDK to branch on which field to read per mode; reuse keeps the existing renderer and one arg-name extraction path.
- **Pure char-cap deriver (vs D3 hybrid).** Rejected: truncates mid-clause and embeds chopped fragments on exactly the verbose MCP descriptions that motivate this.
- **Eager rank-1 Tier-2 in v1 (vs D8 defer).** Rejected for v1: re-introduces an unconditional full-spec payload in the pushed menu on a speculative "rank-1 will be used" heuristic; optimize only if the benchmark shows the round-trip hurts.

---

## Verification

- Re-run the ADR-0044-08 retrieval benchmark with the short-form embedding; confirm the `terminal_agent`/Firecrawl mis-ranking is corrected and measure top-k quality vs the full-description baseline.
- Measure per-turn menu token cost before/after on an MCP-heavy grant set.
- Assert the grant-gating invariant: `describe_tool` on an ungranted name returns not-available with no existence leak.
