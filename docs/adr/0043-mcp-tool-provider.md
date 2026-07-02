# ADR-0043: MCP Tool Provider — Consuming External Tools Under the Reference Monitor

**Status:** Proposed (2026-06-10) — design recorded via a grilling session; not implemented. v1 is gated on the test suite below and ships behind operator config (no MCP servers configured ⇒ zero behaviour change).
**Date:** 2026-06-10
**Author:** Afsin
**Depends on:** ADR-0039 (kernel-owned tool registry — `ToolExecutor` reference monitor, `ToolHandler`, grant/policy/scope/approval; this ADR adds a second `ToolHandler` and a budget regime), ADR-0034 (Tag-Based Data Access Scoping — Regime-1 `DataReadKinds`/`DataWriteKinds`, reused for egress), ADR-0011 (`MaxEnergy` per-step $ budget — extended to tool spend), ADR-0042 (Centralized LLM Provider — health/circuit-breaker + provider-reported usage patterns, mirrored for MCP).
**Relates to:** ADR-0041 (Local Recurrent Workspace — the recurrence gate is the retry-storm backstop that lets failure-cost stay lenient; the `SessionTokenID` it threads is reused for budget attribution), ADR-0037 (interview/evaluation sessions auto-approve dangerous tools — unaffected; MCP tools inherit it).

---

## Context

Cambrian's tools are either **kernel-owned system tools** (ADR-0039 Python modules in `tools/`, run as confined subprocesses) or an agent's **own in-process `@tools`**. The deprecated `TraitTool` auction-agent model is gone (`agent.go:16`). All external capability today must be re-implemented as a native tool.

The **Model Context Protocol (MCP)** is the emerging standard for exposing tools over JSON-RPC (stdio or Streamable HTTP). Supporting MCP **clients** lets Cambrian consume the growing ecosystem of external/vendor tools (search, scrape, SaaS) without bespoke glue per service. The motivating case: a hosted/remote search-and-scrape server (e.g. Firecrawl).

But MCP introduces things native tools never did, and the design must answer them without punching a hole in the ADR-0039 firm authorization boundary:

1. **No pricing channel.** MCP has no billing primitive. A paid MCP tool's cost is known only out-of-band (the provider bills your account). For Cambrian to "act as an OS distributing resources," it must maintain its own price model and meter calls locally.
2. **Untrusted in both directions.** Remote responses are untrusted content (injection); remote **arguments leave the trust boundary** (data egress / exfiltration) — a risk native local tools don't have.
3. **Dynamic, networked, authenticated.** Tools are discovered at runtime (`tools/list`); remote servers need credentials and a connection lifecycle.

---

## Considered Options

- **A — Hand-roll an MCP client + treat MCP tools as auction agents.** Rejected: duplicates JSON-RPC/transport/handshake work, and `TraitTool` (the auction-agent-as-tool model) is deprecated — tools are not auction participants.
- **B — Bolt Firecrawl's MCP server on as a dedicated client path.** Rejected: bypasses the `ToolExecutor` reference monitor (grant/policy/scope/approval/confinement), a second tool paradigm with its own auth story = drift, and a side door through the firm boundary.
- **C — MCP-ify Cambrian's own `tools/` so everything is MCP.** Rejected: protocol overhead talking to your own code with no interop payoff, and it trades away the per-call subprocess confinement that protects dangerous tools (`execute_command`/`execute_python`).
- **D — A generic MCP *client* behind the existing `ToolExecutor`, as a second `ToolHandler`, built on the official MCP Go SDK.** Chosen.

---

## Decision

Add an **`MCPHandler` implementing the domain `ToolHandler`** alongside `ProcessHandler`, so external MCP tools run under the single ADR-0039 reference monitor. Native `tools/` keep their confined-subprocess path. Add a **budget/metering regime** to `ToolExecutor`. Built on the **official MCP Go SDK**, confined to an infrastructure adapter.

### D1 — An MCP tool is a kernel-owned SystemTool, not an auction agent

An MCP tool is registered in the `ToolRegistry` as a `SystemTool` and invoked the ADR-0039 way: a cognitive agent selects it via `tool_call` in its ReAct loop → `ExecuteTool` → `MCPHandler`. It is **not** an auction participant (no `TraitTool`). Consequence: **tool cost never enters EFE/auction selection** — the auction picks *agents for steps*; a tool is chosen *within* a step by the agent's reasoning. Tool cost has exactly two levers: **budget admission** (D5) and **the agent's menu choice** (price surfaced in the tool menu).

### D2 — Transports: stdio (local) + Streamable HTTP (remote), both in v1

Both transports ship in v1 (remote is the primary target). The client is the **official `modelcontextprotocol/go-sdk`**, imported **only** inside one infrastructure adapter (`internal/infrastructure/mcp/`) that implements `ToolHandler` + a discovery port. `internal/domain` and `ToolExecutor` stay SDK-agnostic — honoring the separability rule (`check-separability.ps1`), so a future SDK swap touches one package. *Implementation note to verify at build: exact SDK module/maturity and that its client covers both transports.*

### D3 — Dynamic discovery, namespaced identity, operator-config policy

- **Identity = `mcp:<server-id>/<tool-name>`** (operator-assigned server id + server-advertised tool name). Prevents collisions; gives operator config a stable key.
- **Discovery is dynamic:** `tools/list` at connect, re-synced on reconnect (D8). The server's advertised metadata is **descriptive only** — per A1.5 it is never a trust input for policy.
- **All policy comes from operator config keyed by `mcp:<server>/<tool>`:** pricing, dangerous-flag, `DataWriteKinds`, grants.
- **Unpriced discovered tools are callable** at a **per-server operator default price (default 0)** and **flagged `unpriced` in the audit/ledger** — low friction (consistent with D4), with any unmetered spend made *visible* rather than hidden behind a hard "hide until priced" gate.

### D4 — Trust boundary: operator is the authority; egress audited, not enforced

Wiring a server is the operator's vouching act (same posture as ADR-0039 grants / `tools_unrestricted`).

- **Availability/grants are enforced** — an agent only sees/calls tools it is granted (unless `tools_unrestricted`).
- **Data egress is softer:** if the operator declared `DataWriteKinds`, ADR-0034 Regime-1 (`scopeAllows`) applies; if **unset**, the call **proceeds but emits an audited egress event** recording `{agent_id, mcp:<server>/<tool>, data_class_if_known, ts}`. The audit is the forensic record of what left the box.
- **Remote is NOT dangerous-by-default** — a normal tool unless the operator flags a specific one dangerous (the operator owns endpoint trust). Dangerous MCP tools still inherit the ADR-0037 evaluation-session auto-approve.
- **Responses are always untrusted** — the `_THREAT_PATTERNS` injection scan runs on MCP output regardless of transport.

### D5 — Budget: per-step + per-session in v1, a new ToolExecutor regime

A **budget regime** (admission + reconcile) is added to `ToolExecutor`, attributed via the **`SessionTokenID` already threaded through `ExecuteTool`** (the eval-session plumbing — no new identity wiring).

- **Inner = per-step `MaxEnergy`** ($, ADR-0011): tool estimate and LLM token cost draw against the *same* step budget, unifying tool + model spend into one number.
- **Outer = per-session ledger** (`SessionState`, extended to include tool spend) with a session cap, bounding a runaway agent across steps.
- **Deferred = per-tenant/caller pool** (ADR-0034 `caller_scope`) — the full top-down "OS quota" vision, a follow-up.
- Over-budget ⇒ deny `budget_exhausted`.

### D6 — Estimate: reserve-then-reconcile, cap-on-unmeasurable

Mirrors the LLM Pass-1 estimate → Pass-2 reconcile (`ConsumedTokens` → `ActualTokensUsed`).

- Operator declares per priced tool: `pricing_kind ∈ {flat, per_unit, token}`, `unit_cost`, and a **`max_units_per_call`** cap.
- **Admission reserves** the exact cost (`flat`) or `max_units_per_call × unit_cost` (`per_unit`) — a hold that guarantees the step/session budget is never overspent.
- **Reconcile** to **server-reported usage if the response carries it** (operator maps the usage field), releasing the unused hold; **if unmeasurable, charge the reserved cap.** `max_units_per_call` is the operator's tuning knob.
- **Trusting server-reported usage for *billing* ≠ trusting the manifest for *policy*.** Billing is an accounting-accuracy tradeoff on your own ledger, not a security boundary — identical to using provider `UsageTotalTokens` for LLMs.

### D7 — Failure-cost: charge for work the provider did

- **Never reached the server** (transport/auth/admission-denied): **charge 0, release the hold.**
- **Reached the server, then failed:** reconcile to usage if the error response reports it; **else charge 0 by default, per-server `charge_on_failure: none | cap` override** (some providers bill partial work).
- The asymmetry with D6 is intentional: **success ⇒ work happened ⇒ cap-if-unmeasurable; failure ⇒ usually no work ⇒ 0 unless reported.** The ADR-0041 recurrence gate already prevents retry-storms, so failure-cost need not be punitive; the audit records every failure.

### D8 — Connection lifecycle: eager discovery, health-gated menu, graceful degradation

- **Eager discovery at boot** (an agent can only pick a tool already in its menu) + a **warm persistent connection per server**.
- **Graceful degradation — a down server is not a down kernel.** Unreachable at boot or dropped mid-session ⇒ log, **drop that server's tools from the menu** (health-gate `AvailableTools`), in-flight calls fail structurally. Background **backoff reconnect** (reuse ADR-0042 circuit-breaker + the backfill exponential backoff); reconnect re-syncs `tools/list` (D3). Unreachable-at-boot is **eventual**, not fail-fast.
- **Per-server call timeout** (operator config) — the kernel is the timeout authority (cf. `ProcessHandler.DefaultTimeout`); a timeout is a server-reached failure (D7).

### D9 — Auth: static credentials in v1, OAuth deferred (additive)

- **v1 = static credentials** — bearer token / API key / custom headers, **per server**, sourced from operator config by **env-var reference** (`auth: {type: bearer, token_env: FIRECRAWL_TOKEN}`), never an inline secret.
- **Secrets live kernel-side**, read by the in-process `MCPHandler` — *not* routed through `ProcessHandler`'s subprocess env-scrub. Each credential is bound to its one server's connection and never logged.
- **OAuth 2.1 is deferred** to a follow-up. It is **additive**: `auth.type` already names the method, so `oauth2` slots in behind the same config shape plus a **writable, per-server token store behind a pluggable port** (OAuth 2.1 rotates refresh tokens, so env-reference can't hold them). Consequence accepted: **OAuth-required remote servers are not served by v1.**

### D10 — Phase out native `tools/` in favor of MCP-sourced tools

Once D1 makes MCP tools indistinguishable from native ones to everything above the `ToolHandler` (grants, scope, approval, budget, retrieval), Cambrian no longer needs to **own** most tool development. The direction is to **consume ecosystem MCP servers** (Hermes, Firecrawl, etc.) and **retire the equivalent native `tools/` modules**, shrinking the surface Cambrian maintains.

- **This is a sourcing decision, not new mechanism.** The operator configures the ecosystem server (`cfg.MCP.servers`) and deletes the native equivalent; `LoadRegistry("tools")` just discovers fewer files. No code beyond config + deletion. Commodity tools — web search, scrape, fetch, file-read — move first; they are pure wins (Firecrawl already does it).
- **Not the rejected Option C.** This is **consume external MCP servers + retire native tools**, *not* "wrap our own `tools/` as an MCP server." Wrapping our own tools removes **no** development burden (we still wrote them) and reintroduces the confinement loss; consuming others' tools is what offloads maintenance to the ecosystem.
- **Keep native what confinement protects.** `execute_command` / `execute_python` (and anything running arbitrary code locally) stay on the `ProcessHandler` confined-subprocess path. Offloading these to an external MCP server forfeits the **per-call tempdir jail + scrubbed env + kill** *and* hands code execution + argument egress to a third party. So `tools/` shrinks toward **"only the tools where local confinement is the point,"** not to zero — unless the operator deliberately accepts that trade for a trusted execution server.
- **Migration is incremental and reversible.** Replace one native tool at a time with its MCP equivalent; `ProcessHandler` + native discovery remain until the last confined tool is gone (likely never, given execution tools). The two `ToolHandler`s coexist by design (D1).

---

## Consequences

**Positive**
- The whole MCP ecosystem becomes available **behind the one reference monitor** — grant/policy/scope/approval/injection-scan/budget all apply, no side door.
- Cambrian gains real **resource metering for tools**, unifying tool + LLM spend in one per-step/session budget — the first concrete step of the "OS distributes resources" thesis.
- The SDK dependency is **quarantined** to one infrastructure adapter; the hexagon and the separability rule hold.
- Reuses existing machinery throughout (Regime-1 scope, `SessionTokenID`, `MaxEnergy`, Pass-1/Pass-2, ADR-0042 health) — the only genuinely new code is the `MCPHandler` transport + the budget regime.
- **Offloads commodity tool development/maintenance to the MCP ecosystem (D10):** the native `tools/` surface shrinks toward only confined-execution tools; new capabilities arrive by configuring a server, not writing a tool.

**Negative / risks**
- v1 does **not** serve OAuth-required remotes (the primary target) — a fast-follow gap, mitigated by the additive auth design.
- Softer egress (D4) means a misconfigured remote tool can exfiltrate data with only an audit trail, not a block — accepted as the operator's responsibility.
- Cap-on-unmeasurable (D6) makes poorly-instrumented paid tools "expensive by default."
- Remote tools add a network failure surface; mitigated by graceful degradation (D8).

---

## Testing decisions

- **D1/D4 routing & authorization:** an MCP `tool_call` routes through `ToolExecutor` → `MCPHandler`; grants/availability are enforced; an ungranted agent is denied; responses are injection-scanned.
- **D3 discovery & identity:** `tools/list` populates the registry as `mcp:<server>/<tool>`; reconnect re-syncs; an unpriced tool is callable + audited `unpriced`.
- **D4 egress audit:** a remote call with unset `DataWriteKinds` proceeds and emits the egress event; with `DataWriteKinds` set, Regime-1 denies an out-of-scope caller.
- **D5/D6 budget:** a priced call reserves against step+session budget; over-budget ⇒ `budget_exhausted`; reconcile to server-reported usage when present, else the cap; the hold is released.
- **D7 failure-cost:** never-reached ⇒ 0 + hold released; server-reached failure ⇒ 0 by default, `cap` under the per-server override.
- **D8 lifecycle:** unreachable-at-boot ⇒ kernel boots, tools absent from menu, appear after background reconnect; a dropped server's tools leave the menu.
- All tests use a **stub MCP server** (hermetic, like the Firecrawl httptest stub) — no live external dependency.

## Falsification

v1 is accepted when, against a local stub + one real static-token remote: (1) MCP tools are callable by a granted agent and denied otherwise, entirely under `ToolExecutor`; (2) a priced tool's spend is reserved, reconciled, and bounded by step+session budget (a budget-exhausted call is blocked); (3) a server going down degrades its tools out of the menu without crashing the kernel. Until then the status stays **Proposed**.

## Out of scope (deferred follow-ups)

- **OAuth 2.1** auth (token store + rotation + refresh worker; Dynamic Client Registration).
- **Per-tenant/caller budget pools** (top-down OS quota allocation).
- **Tool cost as an EFE/auction signal** (closed by D1 — tools aren't auction participants).
- **Cambrian as an MCP *server*** (exposing curated native tools to external clients) — a separate provider/product decision, and explicitly *not* the path to offloading tool dev (see D10).
- **Fully removing `ProcessHandler` / native tool discovery** — gated on offloading the confined-execution tools (D10); the confined-subprocess path stays until then (likely indefinitely).
- **MCP `resources` and `prompts`** primitives — this ADR covers `tools` only.
