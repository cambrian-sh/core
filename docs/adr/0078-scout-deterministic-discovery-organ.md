---
id: 0078
title: Scout as a Deterministic-First Environment & Foreign-Source Discovery Organ
status: Proposed
date: 2026-07-17
author: Afsin
amends:
  - 0051-grounded-planner-pre-plan-discovery
supersedes: []
superseded_by: []
depends_on:
  - 0051-grounded-planner-pre-plan-discovery
  - 0053-chunks-as-memory-unit
  - 0060-structure-aware-chunking
  - 0049-experiential-memory-world-model
  - 0075-agent-source-seam
  - 0043-mcp-tool-provider
  - 0044-find-tools-semantic-retrieval
---

# ADR-0078: Scout as a Deterministic-First Environment & Foreign-Source Discovery Organ

**Status:** Proposed (2026-07-17)
**Amends:** **ADR-0051** (Grounded Planner — Pre-Plan Discovery). Inverts **D1** (LLM-`run_think`-first → deterministic-probe-first, LLM demoted to opt-in), rewrites **D4** (MCP-only, LLM-found tools → a data-driven multi-source probe registry), replaces the **D3/D9** world-model write-back with **session-memory** persistence, and **reinforces D13** (the deterministic structured facts enlarge the trusted surface). D2 (always-Scout), D5 (bounded governors), D6 (`discovery-safe`), D8 (never discard / degrade to one-shot), D11 (Zero-Hardcode) are **kept**.
**Depends on:** ADR-0053 (the kg_extractor's LLM→deterministic-tier demotion — the precedent this ADR follows), ADR-0060 (`chunker_registry` — the data-driven-registry pattern), ADR-0049 (session/episodic memory substrate), ADR-0075 (AgentSource seam — the registry-of-sources shape), ADR-0043/0044 (MCP + `find_tools` — now one probe among many, no longer the only path).

---

## Context

ADR-0051 shipped the Scout as a privileged Python `run_think` agent: an **LLM ReAct loop** (`find_tools → tool_call → interpret`) that observes world-state before the planner commits, and hands the planner a `<DiscoveryLTM>` block. The design was right about the *need* (pre-plan perception) and the *trust boundary* (structured facts trusted, LLM text untrusted). Live testing (2026-07-17, `orchestration` suite) exposed three problems that trace to the same root — **the LLM is on the discovery hot path**:

1. **Never actually measured.** Every prior benchmark run recorded `scout_ran=0`. Root cause was operational (config resolved against cwd, so `scout_enabled` silently defaulted false when a supervisor launched the kernel from another directory) — fixed in this ADR's Phase 0. But it masked a deeper issue: the organ had no observed behavior to reason about.
2. **Net cost, not benefit, in practice.** Scout runs pre-plan and spends latency + tokens; then the planner's own LLM call times out (60s) on the same slow endpoint. On the measured run, 2/3 tasks died at planning *after* Scout had already paid — and the one that completed took 264s. An always-on organ that fronts an LLM call is fragile exactly when the lane is under load.
3. **Findings are ephemeral.** The `DiscoveryReport` is attached to ctx, rendered into the planner prompt, and **discarded**. `DiscoveredEntity.ContentCID` (the offload handle) is never populated. Nothing accumulates; every request re-scans from zero.

**The insight.** Almost everything the Scout should observe — filesystem state, listening ports and interfaces, HTTP/OpenAPI schemas, gRPC service reflection, DB `information_schema`, MCP server tool/resource lists, package-registry metadata — is **deterministically discoverable without an LLM**. The LLM in ADR-0051 D1 was doing two jobs: (a) *choosing what to look at and how*, and (b) *interpreting the result*. Job (a) is largely a deterministic mapping from what the request references to which probes to run; job (b) is a thin advisory. Deterministic probes are **faster, cheaper, testable, and — critically — trusted** (a structured probe result cannot carry a prompt injection; an LLM's free-form observation can).

**This is a road the codebase has already travelled and won.** ADR-0053 demoted the kg_extractor from a write-time LLM call to a frozen deterministic tiered pipeline (metadata + spaCy patterns), with the LLM as an **opt-in Tier-3**; ingestion cost dropped from ~37s/item to ~6.7s with no quality loss. The Scout should follow the identical arc.

**Scope correction (owner, 2026-07-17).** Scout findings do **not** need to persist to LTM — they are current-world-state for *this* session and belong in **session memory** (survives replans and multi-step within the session, dropped/consolidated at session end). This removes the ADR-0049 world-model write-back (0051 D9) and its `ContentCID`→ContentStore machinery from Scout's path. Scout may still `memory_query` LTM for genuinely durable facts.

---

## Decision

Invert the Scout from **LLM-first** to **deterministic-first**: a data-driven registry of deterministic discovery probes is the primary path; the LLM is demoted to opt-in probe-selection (only when the deterministic mapping is ambiguous) and a thin advisory interpretation. Findings land in session memory. Discovery is on-demand ("wherever the request needs"), not a fixed-directory watch.

### D1 — Deterministic-probe-first; the LLM is opt-in (inverts 0051 D1)

Discovery's primary path is a set of **deterministic `DiscoverySource` connectors** invoked directly by a thin Go dispatcher — no LLM turn on the common path. This directly removes problems (2) above: on a request that references only deterministically-probeable entities, Scout makes **zero LLM calls**, so it cannot time out and cannot compete with the planner for the endpoint.

The LLM is retained for exactly two opt-in jobs, both fail-open to the deterministic result:
- **Ambiguous probe selection** — when the deterministic request→probe mapping (D3) yields nothing but the request is plainly state-dependent, an optional LLM pass may propose probe targets. Default off; when off, unmatched requests simply produce an env-only report (degrade to one-shot, 0051 D8).
- **Interpretation** — the single advisory `interpretation` sentence (0051 D9). Untrusted (D8 below), scanned before it reaches the planner. Fallback model **mimo** (a fast/cheap variant), set where the LLM role is wired — the model choice is now a minor fallback detail, not a hot-path lever.

The Scout ReAct agent (`agents/system/scout_agent`) is **retained but demoted**: it becomes the opt-in LLM tier, not the default discovery engine.

### D2 — A data-driven probe registry (the `chunker_registry` pattern, ADR-0060)

Discovery sources live in a **registry keyed by probe kind**, not a `switch` — an unknown/unconfigured probe is a startup/skip decision recorded in config, never a silent code branch (Zero-Hardcode, 0051 D11). Each source implements a small port:

```
type DiscoverySource interface {
    Kind() string                                  // "filesystem" | "http" | "grpc" | ...
    Probe(ctx, target DiscoveryTarget) ([]DiscoveredEntity, error) // deterministic, read-only, bounded
}
```

Initial sources (all deterministic, read-only, per-probe timeout):

| Kind | What it observes (no LLM) |
|---|---|
| `filesystem` | dir listing / glob / stat for a referenced path — on-demand, replaces the fixed-dir watcher |
| `system` | OS facts, network interfaces, listening ports, env, DNS (extends the existing deterministic `EnvFacts`) |
| `http` / `openapi` | `GET /openapi.json`, `/.well-known/`, `HEAD`/`OPTIONS`, health endpoints |
| `grpc` | server reflection (service/method list) |
| `db` | `information_schema` introspection, version banner |
| `mcp` | the MCP protocol's own list-tools / list-resources (ADR-0043 as *one* probe, not the only path) |
| `package` | registry metadata (pip/npm), local manifests |

New foreign-source reach = a new registered source, no core edit — the same extensibility shape as ADR-0075's AgentSource.

### D3 — Deterministic probe selection from request-referenced entities

Which probes run is chosen deterministically: the request's referenced entities/kinds (paths, URLs, service names, DB DSNs — extracted with the existing deterministic entity/anchor machinery) map to probe kinds. No LLM decides the common case. This preserves 0051 D2 (always-Scout) cheaply — always-on is now near-free because the deterministic probes are cheap, so the "always-Scout adds an LLM call" cost in 0051 D2/Consequences **disappears** for the deterministic path.

### D4 — Multi-source, not MCP-only; sources are registered, not LLM-found (rewrites 0051 D4)

0051 D4 made MCP the *only* tool path and had the LLM *find* tools via `find_tools`. That is reversed: sources are **registered deterministic connectors**; MCP is one of them. `find_tools` (ADR-0044) remains available to the opt-in LLM tier (D1) but is no longer required for discovery to function. Consequence: the 0051 D4 "grounding is deployment-contingent on a read-MCP server" limitation is **lifted** — filesystem/system/http/grpc/db discovery works with no MCP server configured.

### D5 — Findings persist to session memory, not LTM (replaces 0051 D3/D9 world-model write-back)

Probe results are written to the **session-scoped store** keyed by session id: available to the planner (as today, via `<DiscoveryLTM>`) *and* to later steps/replans within the session, so mid-session work reuses discovery instead of re-scanning. Findings are **dropped or consolidated at session end** — no LTM write, no world-model `last_observed_at` staleness machinery on Scout's path (that machinery, 0051 D3 / ADR-0049 §A1, is no longer load-bearing for Scout and its Go modules stay retired). Durable facts remain reachable via `memory_query` against LTM when Scout's opt-in tier runs.

### D6 — On-demand discovery + reactive watch sources; the fixed-dir watcher is retired

Discovery happens where the request points (D3), not by watching one directory. Ongoing drift/staleness — when it is wanted — is the job of the **reactive watch sources** (ADR-0032 / REACT-06), not a bespoke Scout watcher. The legacy single-directory `DirectoryWatcher` (ADR-0028/0031) is removed from the boot path (Phase 0): it fed a `NoOpSignalReceiver` (dead weight) and its fixed-dir fsnotify watch errored at startup when the inbox was absent.

### D7 — Reliable enablement (Phase 0, implemented)

`config.ResolveBaseDir()` anchors the config bundle (`configs/` + `.env`) **and** the `agents_dir` + `data_dir` to the binary (walking up from the executable) when the working directory lacks them; a no-op for the normal in-tree launch. This is why `scout_enabled` (and the `scout_agent` process discovery) silently failed under a benchmark supervisor. Live-verified: launched from a foreign cwd, the kernel now registers `scout_agent` from the absolute agents path and logs `Scout ENABLED`, with no agents-walk or fsnotify error.

### D8 — Trust boundary reinforced (strengthens 0051 D13)

Deterministic-first **enlarges the trusted surface**: structured probe outputs (kind/id/exists/summary from a filesystem stat, an OpenAPI schema, a gRPC reflection) are structured facts that cannot carry instructions — trusted, no scan. Only the opt-in LLM `interpretation` (D1) and any raw fetched *body* remain untrusted; they pass the existing ADR-0043 injection scan before entering the planner prompt, and raw bodies stay behind a reference, never inlined. One boundary, now with a much larger trusted side.

### D9 — Governors kept, rarely binding

The 0051 D5 bounded-scan cap and per-probe error tolerance are kept (a failed probe → log, continue, stamp the entity `unobserved`; partial findings always emitted — never discard, 0051 D8). Because deterministic probes are cheap and parallelizable, the cap rarely binds; each probe carries its own timeout so one slow foreign host cannot stall planning.

---

## Consequences

### Good
- **Kills the net-cost/timeout failure** — the common discovery path makes zero LLM calls, so Scout can no longer time out or contend with the planner.
- **Larger trusted surface** (D8) — most findings are structured/trusted, shrinking the injection surface to the opt-in interpretation only.
- **Grounding no longer MCP-contingent** (D4) — fs/system/http/grpc/db work out of the box.
- **Testable + cacheable** — deterministic probes are unit-testable and cleanly written to session memory (D5).
- **Extensible** — a new foreign source is a new registered connector, no core edit (D2), mirroring ADR-0075.
- **Follows a proven arc** — the kg_extractor demotion (ADR-0053) is the direct precedent.

### Bad / Cost
- **Real rework** — a probe registry + connectors + session-memory write path + demoting (not deleting) the LLM ReAct agent. Sequenced below to stay shippable in slices.
- **Deterministic probes have narrower judgment** — they observe, they don't infer the *pattern* ("one file per section, 7 remain"). That inference stays with the thin LLM interpretation (D1), now opt-in — so on a request whose value is exactly that inference, the deterministic path degrades to structured facts + no interpretation unless the LLM tier is enabled.
- **Session-only findings** — cross-session reuse is deliberately dropped (owner scope call); a workload that would benefit from durable environment memory would need a separate, explicit LTM path (not this ADR).

### Neutral / Honesty
- This is a **measured** change: the benchmark gate below must show the cost drop with non-regressing plan quality before promotion past default-off.

## Benchmark gate

Measured via the corrected `orchestration` recipe (scout enabled + operator-feed auth so `scout_usefulness` is actually captured — both gaps identified 2026-07-17). Offline/deterministic-first per the cross-cutting rules.

1. **`scout_cost_share` drops sharply** (deterministic path removes the hot-path LLM turn) at **zero** plan-shape-correctness regression vs. the ADR-0051 LLM-first arm.
2. **`discovery_referenced_rate`** non-inferior — deterministic structured facts are referenced by the plan at least as often as the LLM report.
3. **Plan shape-correctness** on the helicopter/state-referencing class ≥ the LLM-first arm (the win ADR-0051 exists to deliver must survive the inversion).
4. **No new `critical_violations`** attributable to Scout; `discovery-safe` enforcement holds (0051 D6).
5. DECISIONS entry with run-manifest ids for both arms (LLM-first vs deterministic-first), either verdict.

## Sequencing

0. **Phase 0 (done)** — D7 config-robustness + D6 DirectoryWatcher retirement. Verified live; OSS + premium build.
1. **Slice 1** — the `DiscoverySource` registry (D2) with `filesystem` + `system` + `http/openapi` sources, deterministic selection (D3), session-memory write (D5). Deterministic path only; LLM tier off. *This is the correctness+cost core.*
2. **Slice 2** — `grpc` / `db` / `mcp` / `package` sources (D2), and the opt-in LLM tier (D1) behind a default-off flag, with mimo fallback.
3. **Gate** — run the benchmark A/B (above), DECISIONS entry, promote deterministic-first to default on a win.

## Related
- **Amends** ADR-0051 (D1/D3/D4/D9 revised, D13 reinforced; D2/D5/D6/D8/D11 kept). ADR-0051's Falsification/`scout_enabled` A/B mechanism is reused as the benchmark gate arm switch.
- **Precedent** ADR-0053 (LLM→deterministic-tier demotion), ADR-0060 (data-driven registry), ADR-0075 (registry-of-sources seam).
- **Reused** ADR-0043/0044 (MCP + find_tools, now one probe + the opt-in tier), ADR-0049 (session/episodic memory substrate), ADR-0032/REACT-06 (reactive watch sources for ongoing drift).
- CONTEXT.md sync: Module Breakdown (new `internal/.../discovery` sources; removed DirectoryWatcher boot), Implementation Status (this ADR), Known Gaps (session-only findings; cross-session environment memory deferred).
