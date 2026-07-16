---
id: 0074
title: Compile-Time Plugin Architecture — Registry + Curated Extension Points
status: Accepted
date: 2026-07-16
supersedes: []
superseded_by: []
depends_on:
  - 0057-open-core-boundary
  - 0073-premium-transport-plane-extension
  - 0032-symbiotic-reactive-rule-engine
---

# ADR-0074: Compile-Time Plugin Architecture

## Status

Accepted (registry + `Plugin`/`Lifecycle` surface delivered; the reactive lane is the
first plugin, validated live end-to-end. Two Tier-1 replace-one points are live —
`SetSignalReceiver` (reactive) and `SetResourceSelector` (ADR-0037 routing selector) —
plus the Tier-2 add-many points. Widening further, e.g. the `Chunker` map, `Generator`,
is incremental follow-up).

## Context

`app.Options` (ADR-0057) grew a fixed set of hooks — `TraceWrapper`, `AgentCallLogger`,
`NewSignalReceiver`, and (ADR-0073) `ExtraServices`. Each new premium capability meant a
new named field on a struct the OSS core owns. We want "premium" to become *plugins*
rather than a parallel repo wired through an ever-growing hook list, and we want the door
open to more extension points over time — without turning the kernel into a free-for-all
where a plugin can rewrite anything.

Two non-options were ruled out first:
- **Go `plugin` (.so/.dll)**: CGO-only, no Windows support, host/plugin version-locked to
  identical toolchains + deps. Incompatible with the "no CGO" rule and the Windows dev
  environment. Rejected.
- **Out-of-process gRPC plugins (hashicorp/go-plugin)**: viable and isolated, but every
  cross-boundary call is a gRPC round-trip. The reactive engine calls Auctioneer / Memory
  / Planner on every signal/condition/action — a poor fit for process separation. Reserved
  for genuinely untrusted, latency-tolerant extensions (see Consequences).

## Decision

### Mechanism: a compile-time plugin registry

`app.Plugin` is `{ Name() string; Register(*Registry) error }`. A distribution binary
composes the plugin set it wants (`Options.Plugins`) and calls `Run`; `applyPlugins` runs
each `Register`, folds contributions into the effective Options, and returns the ordered
`Lifecycle` set (started at boot, drained in reverse on shutdown). Plugins are **compiled
in** — type-safe, cross-platform, no CGO, and (unlike out-of-process) they keep in-process
access to the kernel capability bundles they extend (e.g. `ReactiveServices`).

The `Registry` distinguishes **replace-one** points (at most one owner; a second
registration is a startup error — e.g. `SetSignalReceiver`) from **add-many** points
(composed — `AddTraceWrapper`, `AddGRPCService`, `AddLifecycle`). Directly-set `Options`
fields and plugin contributions coexist.

The **reactive lane is the first plugin** (`wiring.ReactivePlugin`): it registers the
signal receiver + watch CRUD, the ReactiveControl gRPC plane (ADR-0073), and the engine
lifecycle (start/stop) — replacing the ad-hoc hooks. This also fixed a latent bug the
live benchmark exposed: the reactive engine was never `Start()`ed in a running kernel
(the seam carried the signal path but not the lifecycle); the plugin's `Lifecycle` now
starts its worker pools + REACT-06 scheduler at boot.

### Policy: what is (and is NOT) a plugin extension point

A plugin system's value comes from an **invariant core with curated extension points**,
not "override anything." Extension points are tiered:

| Tier | Kind | Rule | Examples |
|---|---|---|---|
| **1. Replace-one** | Swap the single implementation of a versioned interface | At most one owner; conflict = startup error | `Generator`/LLM providers, `Chunker` (ADR-0060), retriever/embedder, `ResourceSelector` (auction↔EFE), `SignalReceiver` (reactive) |
| **2. Add-many** | *Add* capability without replacing core | Compose; never override | extra gRPC services (ADR-0073), agents, tools, watch action executors, event subscribers, benchmark suites, trace/telemetry wrappers |
| **3. Invariant core** | Load-bearing guarantees | **Never pluggable** | scope/approval/budget gates (ADR-0034/0038), auction merit-selection integrity, grounded-only retrieval, the audit trail, the open-core import boundary |

Three principles keep it sound:
- **Policy vs mechanism.** Plugins provide *mechanism* (a retriever, a chunker, an action
  executor); the core keeps *policy* (routing rules, security, budgets). The Zero-Hardcode
  rule is exactly this — routing intelligence stays in the awareness layer.
- **Conformance + fail-closed.** Each extension point ships a default impl and a
  conformance expectation; unknown or conflicting registration is a startup error, never a
  silent fallback (the `chunker_registry` discipline).
- **Isolation only where trust ends.** In-process compile-time plugins are
  trusted-by-construction (you compiled them in). Untrusted third-party plugins are the one
  case where out-of-process gRPC + a capability sandbox earns its cost — process isolation
  is the only real trust boundary.

**Explicitly NOT sanctioned:** making the security kernel (scope/approval/budget),
Gatekeeper merit scoring, or the audit log pluggable. Those are the guarantees the product
rests on; a plugin that can rewrite them is a privilege-escalation hole.

## Consequences

**Positive.**
- "Premium" becomes a *bundle of plugins over sanctioned extension points* — ADR-0057's
  spirit, generalized. Adding a capability no longer means a new OSS `Options` field.
- Replace-one vs add-many + fail-closed conflict handling gives predictable composition.
- The lifecycle surface fixed a real production gap (reactive engine never started).
- No CGO, cross-platform, zero marshalling, full in-process access — the reactive engine
  keeps its tight coupling to Auctioneer/Memory/Planner.

**Negative / costs.**
- Plugin set is chosen at **build time**, not runtime — adding one recompiles a
  distribution binary. Acceptable for a kernel; runtime pluggability is Model B territory.
- The curated extension-point set is a maintenance surface: each Tier-1/2 interface is now
  a semi-stable contract that needs a conformance expectation.
- Trust model: compiled-in plugins run with full kernel privilege. Untrusted plugins need
  the out-of-process path, not this one.

**Neutral.**
- `Options` keeps its direct fields (back-compat); plugins fold into the same effective
  configuration.

## References

- ADR-0057 (open-core boundary / Options seam), ADR-0073 (ExtraServices — the additive
  gRPC extension point), ADR-0060 (`chunker_registry` — the fail-closed data-driven
  precedent), ADR-0034/0038 (the un-pluggable security gates). Code: `app/plugin.go`,
  `app/options.go`, `app/app.go`, `cambrian-premium/wiring/control.go`
  (`ReactivePlugin`), `cambrian-premium/cmd/orchestrator/main.go`.
