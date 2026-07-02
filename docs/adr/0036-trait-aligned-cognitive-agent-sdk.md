# ADR-0036: Trait-Aligned Cognitive Agent SDK (v2)

**Status:** Accepted (2026-06-03) — *records the architectural decisions behind the SDK v2 redesign.
The full requirement is `docs/requirements/REQ-SDK-cognitive-agent-sdk.md`; this ADR captures the
load-bearing choices, their alternatives, and the "why" — not the exhaustive API surface.*
**Date:** 2026-06-03
**Author:** Afsin
**Depends on:** ADR-0058 (Agent Trait Classification), ADR-0033 (Daemon Agent Architecture), ADR-0034
(Tag-Based Data Access Scoping), ADR-0035 (Kernel-Derived Write Classification), ADR-0018 (Managed LLM
Gateway), ADR-0022 (Global Workspace Context)
**Requirement:** REQ-SDK-cognitive-agent-sdk (the "what"); this ADR is the "why"

---

## Context

The current Python SDK is a single flat `Agent` class that conflates three biologically distinct agent
roles (cognitive / deterministic / daemon) and **leaks the kernel's gRPC protocol** to the agent
author — `Handoff`, `Payload`, `ProposalRequest`, `RequestProposal`, `_dispatch_execute_safe`, etc. An
author writing a Spotify controller should not have to understand auction mechanics or proto envelopes.
It also lacks an **intra-agent tool registry** (a way for a cognitive agent's own LLM to choose which
local Python function to call) — the existing `@capability` decorator is for *inter-agent* auction
routing, a different concern.

The SDK is a **third-party, untrusted surface**. Whatever discipline it asks authors to follow, some
will violate. The design must make the safe path the default *structurally*, not by documentation.

## Decisions

### D1 — Three trait-aligned base classes (not one flat class, not mixins)

`CognitiveAgent`, `DeterministicAgent`, `DaemonAgent`, sharing an abstract `Agent`, map 1:1 to the
ADR-0058/0033 trait taxonomy.

- **Alternative considered — keep the flat `Agent` class** with optional features. Rejected: it lets a
  deterministic tool agent call `think()` or a daemon respond to tasks — trait contracts become
  documentation, not structure.
- **Alternative considered — mixins** (`MemoryMixin`, `ToolMixin`). Rejected: `DaemonAgent` serves a
  *different gRPC contract* (`SignalStream`, not `AgentService`); it cannot be a task-responder and a
  signal-producer in one lifecycle. The protocol reality forces a sibling, not a mix-in.
- **Trade-off accepted:** larger API surface (three classes) in exchange for **trait contracts enforced
  by class structure** — a `DeterministicAgent` literally has no `think()` method.

### D2 — Single-threaded request handling; scale by process, not thread (the keystone)

The SDK gRPC server runs `ThreadPoolExecutor(max_workers=1)`. The contract delivered to the author:
**`run()` is never invoked concurrently on the same instance.**

- **Alternative considered — thread pool (`max_workers=N`) + statelessness discipline / locks.**
  Rejected: authors *will* store per-turn state on `self` (it is the natural way to write stateful
  handlers), and the SDK cannot prevent it. Policing concurrency on an untrusted surface is a losing
  game. Removing the concurrency removes the hazard.
- **Why this is safe for throughput:** Cambrian already scales at the **process** level — JIT spawn per
  task, pool mode, and (ADR-0033) **one daemon process per `stream_id`**. A chatbot gets the correct
  isolation shape *for free*: `conv:acme` and `conv:globex` are different processes (OS-level isolation,
  no shared `self`), while turns *within* a conversation serialize in arrival order — which is the
  required semantics anyway.
- **Trade-off accepted:** no intra-process request parallelism. Throughput is recovered by process
  count, the kernel's existing scaling axis. The "never concurrent on the same instance" contract is a
  *one-request-at-a-time-per-process* invariant, so it survives a future async/await model (one event
  loop per process, one `await run()` at a time).

### D3 — The Handoff protocol is invisible to the author

Agents return plain Python (`AgentResult` / `dict` / `str`); the SDK wraps them into `Handoff`/`Payload`
at the boundary. Inbound tasks arrive as a protocol-free `AgentTask`.

- **Alternative considered — expose the protocol** (status quo). Rejected: it couples every third-party
  agent to the kernel's proto schema, so the Substrate cannot evolve the wire format without breaking
  agent code.
- **Trade-off accepted:** the SDK owns a coercion layer (`_coerce_agent_result`) and must preserve the
  semantically-significant `Payload.type` field (e.g. `code` routes to an executor, `budget_signal`
  trips the circuit breaker). Typed `AgentResult.type` is the mechanism.

### D4 — Two distinct levels of capability routing

- `@capability` (inter-agent) — what the Gatekeeper/Auctioneer use to discover and score the agent.
- `@tool` (intra-agent) — a closed menu of local Python functions the agent's *own* LLM may call,
  with JSON-Schema validation, no `exec`/`eval`, structured error returns, and schema auto-derivation
  from type hints.

- **Alternative considered — one decorator for both.** Rejected: they answer different questions
  (*which agent wins the task* vs *which function the agent calls*) and have different security
  properties. Conflating them invites the auction layer and the reasoning loop to leak into each other.
- **Trade-off accepted:** authors must learn two decorators; in exchange, intra-agent tool use is a
  security-bounded, schema-validated closed menu rather than free code execution.

### D5 — Scope/classification is server-side; the SDK carries only hints (defers to ADR-0034/0035)

The SDK exposes the three-set `ScopeConfig` but makes **no client-side isolation promise**:
- **Reads** (`memory.recall`, `artifacts.get`) carry no scope parameters — scope is server-derived
  from the authenticated agent (+ session `caller_scope` in Phase 2). (ADR-0034)
- **Writes** (`memory.remember`, `artifacts.save`) carry a **narrow-only hint**; the kernel derives the
  authoritative classification from operator-configured `DefaultWriteTags`. (ADR-0035)

This is not a new decision here — it is the SDK *honoring* ADR-0034/0035. Recorded so SDK builders do
not re-introduce client-trusted tags. See REQ-SDK §0b (re-grill log).

## Consequences

### Good
- Trait contracts are enforced by class structure, not convention.
- The cross-conversation isolation a chatbot needs falls out of the process model "for free" — no
  per-request threading, no `threading.local`, no race-documentation burden on the author.
- The Handoff protocol is an implementation detail the Substrate can evolve.
- Third-party integrations (`spotipy`, `boto3`, `slack_sdk`) expose their libraries as `@tool` methods
  with zero Cambrian code changes.

### Bad / Cost
- **Breaking change:** all existing cognitive agents and the example daemon must be rewritten. A
  deprecated `Agent` alias is shimmed during the migration window and removed in v2.1.
- **No intra-process parallelism:** a single slow `run()` blocks that process's queue. Mitigated by
  scaling process count, but a misconfigured pool size can bottleneck.
- **Larger SDK surface** to document and test (three base classes + `@tool` + memory/artifact clients).

### Neutral
- **No change to the kernel protocol.** `AgentService`, `SignalStream`, `Handoff` are unchanged; this is
  a client-side abstraction. The single-threaded choice is an SDK-server setting, not a kernel mandate.
- Async/await is deferred to v2.1; the concurrency *invariant* is model-independent, so the API contract
  survives that change.

## Related
- REQ-SDK-cognitive-agent-sdk (full requirements; re-grilled 2026-06-03)
- ADR-0058 (traits), ADR-0033 (daemon-per-stream process model — the scaling axis D2 relies on)
- ADR-0034 / ADR-0035 (scope reads / write classification — D5 honors these)
- ADR-0018 (`substrate.generate()` token accounting), ADR-0022 (`working_memory`)
