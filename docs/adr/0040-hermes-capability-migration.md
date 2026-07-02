# ADR-0040: Hermes Capability Migration (Tools, Agents, Skills)

**Status:** Proposed (2026-06-05) — design recorded; not implemented. **Depends on ADR-0039** (kernel-owned tool registry) landing first.
**Date:** 2026-06-05
**Author:** Afsin
**Depends on:** ADR-0039 (Kernel-Owned Tool Registry — the execution model these tools plug into), ADR-0036 (SDK traits — `CognitiveAgent`/`DeterministicAgent`), ADR-0037 (CE selection binds tool-capable agents), ADR-0034/0035 (scope/classification), ADR-0013 (PauseController approval), ADR-0023 (real agents — the registration path)
**Reference material:** `docs/external-examples/hermes-agent-main/` (Hermes Agent, **MIT licensed**, © 2025 Nous Research), `docs/hermes-tool-transplant-analysis.md`, `docs/cross-framework-synthesis.md`
**Goal stated by the operator:** transfer as many capabilities/agents/skills/tools from Hermes into Cambrian's agents as is sound, via the kernel-owned tool registry.

---

## Context

Cambrian agents cannot currently touch the filesystem, the shell, the web, or execute code — there are **no general-purpose tools** in the SDK, and `agents/terminal_agent.py` / `agents/code_executor_agent.py` do not exist (they are precisely the 26 failing SDK tests: `ModuleNotFoundError: terminal_agent`/`code_executor`). Hermes, by contrast, ships ~85 tool files (file/terminal/web/browser/vision/image/code-exec/…) and ~150 free-text skills, battle-tested but **woven into Hermes' single-process, trusted-in-process runtime** (`file_tools.py` alone is 1,533 lines and imports `agent.file_safety`, `agent.redact`, `tools.registry`, …; `terminal_tool.py` is 2,597 lines).

ADR-0039 establishes the safe execution model (kernel-owned registry; marshal agent-side, execute kernel-side). This ADR decides **what to bring across, and how** — maximizing transferred capability without importing Hermes' trust model or its coupling.

## Considered Options

- **A — Port Hermes' tool code.** Drags in Hermes' `agent.*`/`tools.registry` web and its in-process trusted-tool model (the opposite of untrusted cells); creates a drifting fork of a foreign runtime in a language that is only half of Cambrian. Rejected.
- **B — Bulk-migrate everything (all ~85 tools + ~150 skills).** Scope creep that turns the *kernel* into a tool *application*; imports an ungoverned free-text skill/prompt-injection surface. Rejected.
- **C — Migrate *capabilities*, not code: reimplement a prioritized tool set natively as ADR-0039 registry tools, porting Hermes' MIT-licensed *safety patterns* verbatim (with attribution); add a small set of tool-using cognitive agents; curate, do not bulk-import, skills.** Chosen.

## Decision

Transfer Hermes capability into Cambrian by **native reimplementation behind the ADR-0039 registry**, prioritized by genuine gap, with Hermes' safety hygiene ported as deterministic kernel-side guards. Skills are curated into governed procedural memory, not bulk-imported.

### D1 — Capabilities, not code (with attribution)

For each adopted capability, write a Cambrian-native kernel tool handler + schema (ADR-0039). **Do not** copy Hermes' tool plumbing. **Do** port Hermes' MIT-licensed safety constants/algorithms verbatim where they are pure and valuable — device-path blocklist, binary-extension detection, URL safety, command allow/blocklist, threat-pattern regexes — each with a source attribution comment (`// ported from hermes-agent tools/<file>.py (MIT, © 2025 Nous Research)`).

### D2 — P0 tool set (the real gap)

The first tools to populate the ADR-0039 registry are exactly the missing primitives, each kernel-executed and scope-gated:
- **file** — `read_file`, `write_file`, `patch_file`, `search_files`. Guards: device-path blocklist, max-read-chars, binary detection, scope-gated paths.
- **shell/terminal** — `execute_command`, `list_processes`. Guards: command allowlist + blocklist (incl. pipes/redirects), **PauseController approval** for dangerous verbs.
- **web** — `web_search`, `web_extract`. Guards: URL safety, egress policy; large results → `ContentStore` CID.
- **code-exec** — `execute_python` in a tempdir with a hard timeout. Honesty: with Wasm cancelled (ADR-0004), isolation is **OS-process only** — this is a stated limitation, not a sandbox.

These are *the most dangerous possible operations*, which is exactly why they land behind ADR-0039's reference monitor (scope + approval + audit), never in-process.

### D3 — Tracer bullet: restore `terminal_agent` + `code_executor_agent` first

Reintroduce both as the first adopters of the registry. This (a) closes a real gap, (b) **clears the 26 failing SDK tests** that already expect these modules, and (c) exercises the full ADR-0039 path (schema → marshal → `ExecuteTool` → guard → execute → audit) end-to-end before broadening.

### D4 — Tool-using cognitive agents (a small set, not twelve)

Add a *handful* of capability-bundled `CognitiveAgent`s that are *granted* the P0 tools (ADR-0039 D5) and marshal them in their `think()` loop — e.g. a **research agent** (web + file), a **coding agent** (file + code-exec), a **data agent** (file + code-exec). The kernel ships these as a **reference tool-pack**; the broader catalog (browser/vision/media/devops/…) is left to the ecosystem, not bundled into the kernel.

### D5 — Skills are curated, not bulk-migrated

Hermes' ~150 free-text markdown skills are an **ungoverned prompt surface**, contrary to Cambrian's *learned* (Hippocampus procedural templates) and *governed* (PolicyTemplates, if adopted) procedural-memory thesis. Therefore:
- **Do not** bulk-import skills.
- **Do** hand-curate a small seed set as either `DocTypeProceduralTemplate` references or operator-authored policy/procedure docs, each operator-approved. New procedural memory requires approval, never free-text agent self-authoring.

### D6 — Phasing, demand-gated

- **Phase 0 (P0, this ADR's core):** file, shell, web, code-exec; restore terminal/code-exec agents; the 3-ish tool-using agents.
- **Phase 1 (only on measured demand):** browser (Playwright, accessibility-tree snapshots), vision (a vision-capable model call — note this is a *model* concern, ADR-0037 D16, not a new agent kind), image-gen.
- **Phase 2+ (ecosystem / deferred):** TTS, kanban, computer-use, smart-home, messaging, MCP, enterprise connectors. The kernel does not bundle these.

### D7 — Explicitly NOT adopted (guardrails)

- **Blocking `delegate_task`** → keep non-blocking `yield_subgoal` (ADR-0037 D10).
- **In-process tool execution** → forbidden by ADR-0039 D2.
- **Per-provider tool gateways** → the kernel is the gateway (ADR-0018 pattern).
- **Free-form, agent-authored skills** → D5 (governed, approved).
- **Hermes thread-local approval callbacks** → use the kernel `PauseController` arbiter (ADR-0039 D8).

### D8 — Dependency hygiene is a prerequisite

Any new tool dependency (e.g., a web-search client, Playwright) must land **with upper bounds + a hash-pinned lockfile + the CI bounds gate** (the synthesis-doc #3 item). New tools must not regress the supply-chain posture of processes that hold UDS access to the kernel.

### D9 — Each migrated tool ships complete

Definition of done per tool: JSON schema, kernel handler, ported deterministic safety guards (attributed), scope policy, dangerous-flag + approval wiring where relevant, audit `TaskEvent`, and isolation/behavior tests. No tool lands without its guards.

## Consequences

### Good
- **Closes the real capability gap** — agents can finally do file/web/code work, the limitation that made them "useful without tools."
- **Clears 26 failing tests** (D3) as a side effect of the tracer bullet.
- **Keeps the kernel a kernel** (D4/D6) — a reference tool-pack, not a bundled tool app; the dangerous surface stays small and gated.
- **Inherits Hermes' hard-won safety** (D1) without inheriting its trust model or coupling.

### Bad / Cost
- **Trusted-base growth.** Each kernel-side handler is code in the trusted base; mitigated by thin handlers + deterministic, tested guards.
- **Real-world danger.** file/shell/web/code-exec materially expand blast radius; mitigated by ADR-0039's scope + approval + audit, but **OS-process isolation only** (Wasm cancelled) is an honest ceiling on code-exec safety.
- **Reimplementation effort.** Native rewrite is more work than copy-paste; the payoff is no foreign-runtime fork and a security model that actually fits.

### Neutral
- This ADR is deliberately *less* than Hermes. The "as many as possible" goal is bounded by *soundness*: a capability is adopted only when it passes through ADR-0039 cleanly and earns its place by demand (D6).

## Falsification plan (gate to acceptance)

1. **Capability unlock** — on a task set requiring external I/O (read a file, search the web, run a snippet), agents complete tasks they provably could not before (0% → meaningful success).
2. **Regression cleared** — the 26 `terminal_agent`/`code_executor` SDK tests pass.
3. **Safety guards bite** — known-bad inputs (blocked device path, blocklisted command, unsafe URL, binary read) are rejected deterministically, with tests.
4. **No scope-leak** — tools never act outside the caller's effective scope (ADR-0034/0039 tests).
5. **Approval works** — a dangerous shell command blocks on `PauseController` and resumes/aborts on operator response.

Phase 0 is accepted on (1)–(5); Phase 1 begins only on measured demand.

## Related
- ADR-0039 (the registry/execution model — hard dependency)
- ADR-0036 (traits/SDK), ADR-0037 (selection), ADR-0034/0035 (scope), ADR-0013 (approval), ADR-0023 (agent registration), ADR-0004 (Wasm cancellation — the isolation ceiling)
- `docs/hermes-tool-transplant-analysis.md` (the per-tool inventory + priorities this ADR distills), `docs/external-examples/hermes-agent-main/` (MIT source)
