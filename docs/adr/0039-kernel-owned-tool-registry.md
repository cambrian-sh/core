# ADR-0039: Kernel-Owned Tool Registry & Schema Distribution

**Status:** Proposed (2026-06-05) — design recorded; not implemented. Gated on a falsification spike (tool-call success rate vs. the bare-binding baseline; see Falsification plan).
**Date:** 2026-06-05
**Author:** Afsin
**Depends on:** ADR-0036 (Trait-Aligned SDK — `@tool`, `think()` ReAct loop, `DeterministicAgent`), ADR-0037 (CE Planner / ResourceSelector — what binds a step), ADR-0034/0035 (scope / write classification — the enforcement boundary), ADR-0022 (ContentStore — large payloads), ADR-0018 (Managed LLM Gateway — the precedent for a kernel-owned capability gateway), ADR-0013 (PauseController — HITL approval)
**Amends:** the implicit "bind a `TraitTool` agent directly to a plan step" execution path (ADR-0023). That path survives only as a narrow optimization (D7).
**Theory basis:** least-privilege / reference-monitor (one enforcement chokepoint), and the marshalling/execution split that underlies every safe syscall interface.

---

## Amendment A1 (2026-06-05) — Tools are Python modules in a BBolt registry; `DeterministicAgent` is superseded

The original D1–D10 stand on the *authorization/trust* axis; this amendment changes the *implementation* axis after review. It supersedes the "in-kernel Go handler" half of D2 and the code-registration implied by D1.

- **A1.1 — A tool is a Python module + manifest, auto-discovered into a BBolt registry.** A tool is `tools/*tool.py` with a `TOOL_MANIFEST` (name, JSON schema, handler kind, `dangerous`, data-kinds), auto-scanned into a `tools` BBolt bucket by the existing `BBoltAdapter` — exactly like `agents/*agent.py` today. **No hand-written Go `Register()` calls.** The kernel loads the registry from BBolt at startup; an operator drops a tool file and it is live. (Replaces the accreting, error-prone Go registration in the composition root.)
- **A1.2 — All system tools execute as kernel-owned, confined Python processes.** The "in-kernel Go handler" kind is dropped; every system tool runs in a kernel-spawned, kernel-supervised, **confined child process** (cwd-jailed to allowed roots, env-scrubbed, timeout + resource caps). This generalizes D2's isolated-process handler to *all* tools, gives uniform isolation, and lets ADR-0040 **port Hermes' Python tools directly** rather than transliterate to Go. Dangerous tools get the strongest caps + approval; bounded tools get lighter caps.
- **A1.3 — `DeterministicAgent` (`TraitTool`) is superseded by tools.** It was *designed* as the deterministic-tool mechanism and is the exact thing that crashes on parameter-impedance (Context). The deterministic process now **becomes a tool handler** invoked via `ExecuteTool`, not an auction-bidding cell. The trait and the Auctioneer's Static-Bidder special-case are removed. (Cost is near-zero today: no `TraitTool` agents currently exist.)
- **A1.4 — THE FIRM LINE: authorization stays in the Go kernel, pre-invocation.** `ToolResourcePolicy`, `EffectiveScope`, `ApprovalController`, schema validation, and audit run **in the Go `ToolExecutor` before the Python tool is invoked**. The Python tool receives already-authorized, schema-validated args and merely executes (confined as a backstop). **No path/egress/command/approval/scope check may live in Python** — the safety boundary must not move into the process layer being contained. (Intrinsic guards like binary-detection-during-read may live in the tool; *authorization* does not.)
- **A1.5 — The manifest is not a trust input for safety.** A tool's manifest declares schema/kind/`dangerous`; the **resource policy (roots/egress/commands) stays operator-set on the grant**, never self-declared by the tool file — a tool cannot widen its own policy.

Unchanged by this amendment: the marshalling-agent-side / authorize-kernel-side split (D2 core), the two authorization regimes (D8), the D10 approval channel, belief-only selection (D5), the reference-monitor `ExecuteTool` chokepoint (D4), and the parameter-impedance fix (args are LLM-marshalled and Go-validated before the Python tool ever runs).

---

## Context

A plan step today binds to an agent, and the bound agent receives the upstream step's output as a raw `AgentTask`. For a **`DeterministicAgent` (`TraitTool`)** this is brittle: `DeterministicAgent.run(task)` is a typed handler with **no reasoning, no LLM, no tools** (`python-sdk/.../base.py`). It must turn whatever the previous step emitted into a result *deterministically* — but the upstream output almost never matches the tool's rigid parameter schema. There is **no layer that marshals unstructured context into structured tool parameters**, so the call mis-parses or crashes.

The contrast proves the point. `calculator_agent` solves "what is 47 times 89 plus 12?" reliably — **because it is a `CognitiveAgent`, not a `DeterministicAgent`**. Its `think()` loop reads the prose and emits `{"action":"tool_call","tool":"multiply","args":{"a":47,"b":89}}`. The `@tool` decorator is the registry; **the LLM is the parameter-marshalling layer**. A bare tool-step lacks exactly that adapter.

Two further facts shape the design:
- Cambrian already has the right *shape* for safe privileged operations: `MemoryClient` and `ArtifactManager` are SDK methods the agent calls, but the privileged op (LTM write, artifact store) **executes in the kernel under scope** (ADR-0034). The agent marshals; the kernel executes and enforces.
- Cambrian's threat model is **untrusted cells**. A tool like `read_file` or `terminal` that executed *inside the agent process* would hand an untrusted cell direct filesystem/shell access — the opposite of the model.

The system needs a way for tools to be (a) parameter-marshalled by a reasoning loop, and (b) executed and enforced by the kernel — without running privileged code in the agent process.

## Considered Options

- **A — Keep plan-time binding of bare `TraitTool` agents (status quo).** No marshalling layer; crashes on the common parameter mismatch. Rejected (this is the bug).
- **B — Run tools in-process in the agent (the Hermes model).** Gives the agent a rich tool surface, but executes privileged operations inside an untrusted cell — breaks the isolation invariant. Rejected.
- **C — A kernel-owned tool registry: agents receive tool *schemas* and marshal arguments in their reasoning loop; the *call* routes back to the kernel, which executes and enforces.** Chosen. Marshalling agent-side, execution kernel-side.

## Decision

Introduce a **kernel-owned tool registry**. Tools are kernel resources with a JSON schema; cognitive agents see the schemas as a callable menu, marshal arguments via their LLM, and invoke the tool through a single kernel RPC that executes the operation and enforces policy.

### D1 — The registry is kernel-owned (single source of truth + privilege boundary)

A `ToolRegistry` lives in the kernel (`internal/domain/` for the types, a kernel service for the registry + handlers). It is the **one** place tools are defined, the **one** place their schemas come from, and the **one** place they execute. Agents never own or execute system tools; they only call them. This makes the registry a reference monitor: every tool invocation passes one auditable chokepoint.

> The `@tool` *intra-agent* registry (ADR-0036) is unchanged and remains for an agent's own pure, in-process functions (e.g., `calculator.add`). System tools are a distinct, kernel-routed kind (D3).

### D2 — The marshalling/execution split (load-bearing)

Every system tool is split into two halves:
- **Marshalling — agent-side.** The agent's `think()` loop decides *which* tool and *what* arguments (`path=…`, `query=…`) from unstructured context. This is the LLM-as-adapter that fixes the crash.
- **Execution + enforcement — the kernel's *trust domain*, not necessarily its process.** The kernel owns the privilege decision: authorization (D8), deterministic guards (D6), and audit always run **in-kernel-process and pre-execution**. The *operation itself* may run in the kernel process **or** in a kernel-spawned, kernel-supervised child — see the handler kinds below.

This is the same shape `MemoryClient`/`ArtifactManager` already use. The line is absolute: **"registry distributed to agents" (schemas + marshalling) = allowed; "tools execute in the *agent* process" = forbidden.** A `read_file` that marshals agent-side but executes inside the kernel's trust domain keeps the untrusted-cell isolation; one that executes in the agent process destroys it.

**Handler kinds (per tool).** Execution location is an explicit property of each tool, because "kernel-side" is unsafe to read as "in the kernel's own process" for executing tools:
- **In-kernel handler** (Go func) — for bounded, non-executing ops: `file` read/write/search, `web` fetch, recall. A bug is a returned error, not a crash. Runs in the kernel process.
- **Isolated-process handler** — for `terminal` and `code-exec`: the kernel dispatches to a **dedicated, resource-capped, ephemeral child** (tempdir, `ulimit`/cgroup caps, hard timeout, killed after use), supervised by the kernel but not in it. A segfault, OOM, or fork-bomb dies with the child, **never the orchestrator**.

**Residual-risk honesty.** With Wasm cancelled (ADR-0004), the isolated child is **OS-process isolation + resource caps only — not a true sandbox.** `code-exec` is therefore the highest-residual-risk tool and SHOULD be **off by default** (no grant unless an operator opts in), distinct from the bounded in-kernel tools.

### D3 — Schema distribution into the reasoning loop

At dispatch, the kernel tells a cognitive agent which system tools it may call and ships their JSON schemas. The SDK injects them into the ReAct `<OutputSchema>` action menu alongside any `@tool`s, so the LLM sees one unified, closed tool menu. A `{"action":"tool_call","tool":"<system_tool>","args":{…}}` for a system tool is routed to the kernel (D4) rather than executed locally; a `@tool` is executed locally as today.

### D4 — `ExecuteTool` RPC (the chokepoint)

A new gRPC method:

```
ExecuteTool(ExecuteToolRequest) → ExecuteToolResponse
  ExecuteToolRequest{ tool_name, args_json, session_token_id, step_index }   // agent identity from x-agent-id metadata (ADR-0034)
  ExecuteToolResponse{ result_json | result_cid, error }
```

The kernel resolves the caller principal from `x-agent-id` (the same mechanism `QueryMemory` uses), looks up the tool, runs its safety guards (D6), enforces scope (D8), optionally gates on approval (D8), executes the handler, and writes an audit `TaskEvent` (D8). Errors are returned as structured, non-crashing results.

### D5 — Selection stays belief-only; the grant is an execution-time precondition

Because marshalling now lives in a reasoning loop, the resource the CE/ResourceSelector (ADR-0037) binds is a **cognitive agent**, not a bare tool — removing the crash-prone "bind a bare `TraitTool` to a step" path. But **tools are decided at think-time, not plan-time**: the planner drafts a capability intent ("research X"); the *bound agent's* `think()` loop is what later decides to call `web_search`. The plan step therefore does not know a tool is involved, so **selection cannot and does not filter on tool grants** — it remains purely capability-belief (ADR-0037), unchanged and Zero-Hardcode-clean. (No deterministic grant gate is added to routing.)

`AgentDefinition` still gains a **tool grant** per system tool — *which* tools the agent may call and the `ToolResourcePolicy` that bounds each (D8). But the grant is enforced **only at `ExecuteTool` time** (deny un-granted calls, fail-closed), never as a routing filter. An agent that attempts a tool it lacks receives a **clean structured denial — not a crash** — which it degrades on or surfaces as a failure. Capability-belief then **learns** the boundary from outcomes: an agent without the web grant fails web tasks → loses belief mass in that region → stops being selected for it. This is ADR-0037's own thesis (capability is *learned*, not declared), and a tool-denial is a cleaner, more attributable failure signal than today's crash (better cold-start, scored by the Verifier). An agent's manifest MAY advertise the tools it is built to use, seeding the belief prior / catalog — **advisory only, never a hard routing gate**.

### D6 — Capability gating + deterministic safety guards

Each tool carries:
- a **precondition** (`Available(ctx) bool`) — the tool appears in an agent's menu only when its config/env/scope preconditions hold (the Hermes `check_fn` analog);
- **deterministic safety guards** that run kernel-side before execution — binary detection, threat-pattern regexes, and the per-grant **resource-policy checks** of D8 (path-root containment, egress allowlist, command allowlist). **No LLM in the safety boundary** (Zero-Hardcode safety exception, like `isClassificationTag`). The marshalling LLM proposes args; the deterministic guard, parameterized by the agent's tool grant (D5), decides whether they are allowed.

### D7 — Plan-time bare-tool binding is narrowed, not deleted

Direct binding of a tool to a step survives **only** for the rare case where the upstream output is already structured to the tool's schema (e.g., one tool's structured output feeding another). The default is tool-behind-reasoning (D2/D5). Bare binding is an optimization an operator/planner opts into, not the fallback.

### D8 — Two authorization regimes (data-store scope ≠ system-resource policy)

Tool authorization spans **two distinct domains**, and the ADR keeps them separate rather than overloading the word "scope." ADR-0034 `EffectiveScope` is a predicate over **document/artifact tags**; it is necessary for data tools but says nothing about a raw filesystem path, URL, or shell command. System tools therefore carry a **second, deterministic resource-authorization layer**.

**Regime 1 — Data-store scope (ADR-0034, unchanged).** Tools that read the tagged stores — `memory`/recall, `artifact`, `episodic` — execute under the caller's `EffectiveScope`. `Allows(tags)` is the predicate; fail-closed on unknown principal. No change to ADR-0034.

**Regime 2 — System-resource policy (new; on the tool grant, D5).** Tools that touch the OS/network — `file`, `terminal`, `web`, `code-exec` — are bounded by a per-grant `ToolResourcePolicy`, enforced deterministically kernel-side **before** the handler runs:
- **Filesystem root allowlist.** The tool may only resolve paths under granted roots. Reject `..` escapes, symlink escapes (resolve real path, re-check containment), and device/special paths (ported Hermes blocklist). A grant with no roots = no filesystem access.
- **Network egress allowlist.** `web`/extract may only reach allowlisted domains/IPs. **Deny RFC-1918, loopback, link-local, and cloud-metadata (169.254.169.254) by default** — closing SSRF, which `EffectiveScope` cannot see. A grant with no egress list = no network.
- **Command allowlist.** `terminal` may only run allowlisted commands with blocklisted substrings (pipes/redirects/shell-metachars) rejected — the existing `terminal_agent` `ALLOWED_COMMANDS`/`BLOCKED_SUBSTRINGS` pattern, now kernel-side and per-grant.

`ToolResourcePolicy` is **operator-set, enumerable, and fail-closed** (empty ⇒ deny). It is *not* `EffectiveScope` and does not extend it; the two are checked independently and a tool call must pass **both** regimes that apply to it. A data tool that also touches the OS (e.g., exporting LTM to a file) is subject to both.

**Plus, for every tool regardless of regime:**
- **The static grant is the base gate (deny-by-default).** A dangerous tool runs only if an operator granted it to the agent with a resource policy; `code-exec` is off-by-default (D2). This is a configuration-time decision that exists independent of any interactive approver.
- **Interactive per-call approval via the channel built in D10** — *not* `PauseController`, whose only driver (TUI/ChatStream) was deleted (ADR-0030) and which currently reaches no human. Approval-required tools block on D10; **fail-closed** (no approver / timeout ⇒ deny).
- **Immutable audit.** Every `ExecuteTool` call writes a `TaskEvent` with tool name, arg hash, result hash, the authorization decision (scope + resource-policy outcomes), the approver identity (if any), and result, and emits a `TelemetryObserver` signal.
- **Large payloads via CAS.** Results above an inline threshold go to `ContentStore` (ADR-0022) and return a CID, bounding context growth.

### D9 — SDK surface

The Python SDK exposes system tools to `CognitiveAgent` as a kernel-provided menu (a `SystemToolRegistry` populated from the dispatch handshake). The author writes nothing per tool; the agent's existing ReAct loop gains the system tools automatically based on its grants. `DeterministicAgent` is unaffected — it has no reasoning loop and is not a system-tool *caller*. (Note: an isolated-process *handler* for `terminal`/`code-exec` (D2) is a kernel-spawned execution child, **not** a `DeterministicAgent`/cell on the auction path — it is an internal handler the kernel supervises, with no manifest, no bidding, and no agent identity of its own.)

### D10 — Build a real interactive approval channel (HITL revival)

Interactive HITL is currently **unwired** — `PauseController`'s only driver was the deleted TUI/ChatStream, and the unary `Execute` boundary never used it. This ADR **commits to building** a real, headless-friendly approval channel rather than depending on a dead one:

- **`domain.ApprovalController`** — `Request(ctx, ApprovalRequest) (ApprovalDecision, error)`, blocking until decided or timed out. `ExecuteTool` calls it for approval-flagged tools; the agent's `ExecuteTool` RPC blocks meanwhile (same callback shape as generation — proven safe, Q1).
- **Operator gRPC surface (two methods):**
  - `WatchApprovals(stream)` — a server-stream an **operator/automation client** subscribes to, receiving pending `ApprovalRequest`s (tool, args preview, agent, session, resource-policy decision).
  - `SubmitApprovalDecision(id, approve|deny, approver_id)` — resolves a pending request.
- **Operator-authenticated, never agent-reachable.** The approval surface is on the admin/operator plane behind an operator credential — **an agent can never approve its own (or any) tool call.** This is the load-bearing access boundary of the channel.
- **Fail-closed everywhere:** no subscriber within a grace window ⇒ deny; decision timeout ⇒ deny; ambiguous/duplicate ⇒ deny. The absence of a human can only block, never permit.
- **Automatable:** the same surface accepts a non-human approver (a policy oracle) for batch/headless deployments — opt-in, still fail-closed.
- **Reusable:** this channel is the general HITL primitive the system lost when the TUI was deleted; tool approval is its first consumer, but destructive-verb pauses (ADR-0013) and reactive `start_plan` gates can migrate onto it later.

## Consequences

### Good
- **Fixes the parameter-impedance crash** (D2): the LLM marshals messy upstream output into rigid tool params, the thing a bare `DeterministicAgent` step cannot do.
- **Preserves untrusted-cell isolation** (D2/D8): privileged ops execute kernel-side under scope; the agent never holds raw filesystem/shell/network access.
- **One reference monitor** (D1/D4/D8): every tool call passes a single auditable chokepoint that enforces *both* authorization regimes (data-store scope + system-resource policy).
- **SSRF/path-escape closed by construction** (D8 Regime 2): the resource policy denies private/metadata IPs and out-of-root paths deterministically — failures `EffectiveScope` structurally cannot catch.
- **Bounded blast radius for executing tools** (D2 handler kinds): a crash/OOM/fork-bomb in `terminal`/`code-exec` dies with its capped child, never the orchestrator.
- **Interactive HITL restored** (D10): the approval channel is a real, headless-friendly, fail-closed, operator-authenticated primitive — reviving the human-in-the-loop capability lost when the TUI was deleted, reusable beyond tools.
- **Cleaner selection that *strengthens* ADR-0037** (D5): removes the brittle bare-tool-step path and adds **no** routing filter — selection stays belief-only; the grant is enforced at execution and *learned* by belief from clean tool-denials (a better failure signal than today's crash). Tool capability collapses into the same region-resolved quantity as everything else.
- **Reuses proven shape** (D2): identical to `MemoryClient`/`ArtifactManager`; not a new paradigm.

### Bad / Cost
- **Latency.** Each system-tool call is an LLM marshalling turn + a gRPC round-trip. Inherent to adaptive marshalling; mitigated by batching where a tool is called repeatedly and by `@tool` for genuinely local pure functions.
- **Kernel surface growth.** The kernel now hosts tool handlers + guards — more code in the trusted base. Mitigated by keeping handlers thin and guards deterministic/tested.
- **Schema-menu size.** Many granted tools enlarge the agent prompt. Mitigated by per-agent grants (D5) — an agent sees only its tools.
- **Approval availability becomes a liveness dependency** (D10). Fail-closed means an approval-required tool with no operator/oracle subscribed will *stall then deny* — correct for safety, but it makes the approver an availability concern for deployments that rely on those tools. Mitigated by: static-grant tools needing no per-call approval, and an opt-in automated approver for headless runs.
- **New trusted surface** (D10): the operator approval plane is security-critical (it permits dangerous ops). It must be authenticated and isolated from the agent plane; a bug there is a privilege-escalation path. Accepted, and called out for focused review.

### Neutral / Honesty note
- The win (fewer tool failures, no isolation loss) must be **measured** against the bare-binding baseline, not assumed. A tool whose params the planner *can* produce structurally gains nothing from the loop (D7 keeps that path).

## Falsification plan (gate to acceptance)

A/B on `internal/benchmarks` with a tool-requiring task set:
1. **Tool-call success rate** — fraction of tool-invoking steps that complete without a parameter/parse failure — **materially higher** than the bare `DeterministicAgent`-bound baseline (the core defect).
2. **Both authorization regimes bite (D8).** Data-store: a recall/artifact tool never returns data outside the caller's `EffectiveScope` (extends ADR-0037 metric 9 / ADR-0034). System-resource: a `file` read outside granted roots (incl. `..`/symlink-escape), a `web` fetch to a private/metadata IP (SSRF), and a non-allowlisted `terminal` command are each **rejected deterministically**, with tests for each.
3. **Audit completeness** — every `ExecuteTool` call produces a `TaskEvent` with arg/result hashes.
4. **Latency** p50/p95 of a tool step within an accepted bound vs. the (crashing) baseline.
5. **Approval channel (D10)** — a pending approval is delivered over `WatchApprovals` to a subscribed operator; `approve` lets the tool proceed, `deny` blocks it, and **no-subscriber / timeout fail-closed to deny**. An agent calling the approval surface is rejected (operator-plane only). Decisions are audited with approver identity.
6. **Blast-radius isolation (D2 handler kinds)** — a `code-exec` payload that segfaults / OOMs / spawns runaway children is killed with its capped child; the orchestrator and all other sessions survive. `code-exec` is off-by-default and only runs under an explicit grant.

Accepted only if (1) shows a clear marshalling-success win with (2) holding absolutely.

## Related
- ADR-0040 (Hermes capability migration — the first tools that populate this registry; depends on this ADR)
- ADR-0036 (`@tool`/`think()`/`DeterministicAgent`), ADR-0037 (selection binds tool-capable agents), ADR-0034/0035 (scope enforcement), ADR-0022 (ContentStore), ADR-0018 (kernel gateway precedent), ADR-0013 (PauseController)
- `docs/hermes-tool-transplant-analysis.md`, `docs/cross-framework-synthesis.md`
