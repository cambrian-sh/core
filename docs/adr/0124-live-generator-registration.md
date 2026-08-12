---
id: 0124
title: Live LLM Generator Registration (SaveGenerator applies without restart)
status: Accepted
date: 2026-08-12
supersedes: []
superseded_by: []
depends_on:
  - 0042-llm-provider-broker
  - 0101-config-and-secret-store
  - 0123-config-surface-to-store
---

# ADR-0124: Live LLM Generator Registration

## Status

Accepted (owner directive 2026-08-12: "LLMs should be registered dynamically,
not at start. I should be able to register a new LLM and directly assign it to
things without restarting.")

## Context

Contract 0083 gave the console SaveGenerator/RemoveGenerator, but the writes
only persisted to the ADR-0101 store: the broker's routing table, the streaming
gateway's model clients, and the auction's TraitModel agents were all built
once at boot, so every write answered `restart_required` — and the console's
own ListGenerators/TestGenerator read a boot snapshot, so a saved generator was
invisible and unprobeable until the restart. Roles (contract 0096) and MCP
servers (contract 0097, live attach) had already crossed this bridge; the
generator surface was the straggler. A second, sharper defect shipped in the
same area: a console-added generator with no `llm_provider.default` REFUSED THE
NEXT BOOT (hard validation), the exact chicken-egg ADR-0123 constraint 2 warns
about — it bricked a real deployment on 2026-08-11.

## Decision

### One atomically-swapped table in the broker

`llm.Provider` now holds its generator state — registry, id order, capability
index, AND the global default — as one `atomic.Pointer[generatorTable]`.
`ReloadGenerators(gens, default)` builds the NEW registry first (a bad spec
fails without touching the serving table) and publishes it as a unit: a
resolve either sees the whole old table or the whole new one, lock-free on the
Acquire hot path. In-flight calls finish on old clients ("nothing in flight
moves", the SetRole tolerance). The circuit breaker is deliberately untouched:
unknown ids are optimistically healthy, so a new generator starts closed and a
replaced id keeps its history.

### Four tables, one apply

`SaveGenerator`/`RemoveGenerator` now hot-apply through a single closure that
updates everything that would otherwise drift: the broker table (routing), the
`generatorRuntime` (the mcpRuntime-analogue the console's reads, TestGenerator
and the write guards consult — so a re-read after save shows the generator just
made), the streaming gateway's model clients + default (agent-plane
`GenerateViaModelStream`), and the auction's TraitModel agents (register +
reconcile, so a new model can win steps and a removed one stops winning — the
qwen-after-removal orphan rule, applied live). Outcome: `live` when applied,
`restart_required` only when no provider is running.

### First-generator auto-default

When no default is configured anywhere, SaveGenerator also stores
`llm_provider.default = <saved id>`. Every store-reachable state stays
bootable; the 2026-08-11 brick class is closed at the write chokepoint. A
second generator never steals an existing default. RemoveGenerator's guard now
judges the EFFECTIVE default (store-first), not the boot snapshot.

### Ladder membership gate (latent bug fixed)

`resolveModel` rungs 1–3 now require the candidate id to EXIST in the table.
The breaker calls unknown ids healthy, so a suggestion/role/default naming an
unknown generator used to resolve and then hard-error in Acquire's registry
lookup — contradicting `GeneratorForModel`'s documented "unknown → ladder"
promise and the orphaned-role warning. Live removal made the path reachable;
the fix makes the documented fallback true everywhere.

## Consequences

- The operator flow is now: save generator → (optionally set its key) → test
  it → assign roles / let the auction use it — all in one console session, no
  restart. This satisfies the interviews-need-a-healthy-LLM direction's
  prerequisite: a provider can be added to a running kernel.
- Removed-id streaming clients stay registered in the gateway (no Unregister)
  but stop resolving via the ladder; cosmetic residual.
- Metabolism's default cost seeding remains boot-only (cosmetic, recorded).
- `llm_provider.default` remains a hard boot error when generators exist
  without it; the auto-default makes that state unreachable through the
  console. Softening the boot validation itself stays open with the owner.
- Verified: table-swap unit tests incl. a 200-reload × 4-resolver race test
  under `-race`; write-path tests for auto-default, live effect, guards; full
  llm + app suites green.
