---
id: 0082
title: Additive Licensed Plugins — Manifest, Entitlement, and Plugin-Owned Surfaces
status: Proposed
date: 2026-07-24
supersedes: []
superseded_by: []
depends_on:
  - 0074-plugin-architecture
  - 0057-open-core-boundary
  - 0073-premium-transport-plane-extension
  - 0047-operator-transport-plane
---

# ADR-0082: Additive Licensed Plugins

## Status

Proposed — **Phase 1 implemented** (see Migration). ADR-0074 delivered the compile-time
registry and made the reactive lane the first plugin; this ADR makes plugins
**self-describing, independently entitled, and fully excisable from the OSS repo**, and is
the precondition for selling them as subscriptions.

**Phase 1 landed:** the `PluginManifest` (D1), plugin-owned capability advertisement (D2),
dependency ordering with non-fatal unmet dependencies (D10), the `Build` phase (D12), and
the premium `plugins/<name>/` reorganization with its isolation guard (D11). The seven
premium `watch-*` capability strings no longer appear in the OSS kernel.

**Phase 2 landed:** the `EntitlementProvider` seam and its chokepoint in `applyPlugins`
(D3) — evaluated *before* dependency resolution, so an unentitled plugin's dependents
correctly report `deps_unmet`; a nil provider allows everything (OSS default) and a provider
error fails closed. `KernelServices` is now the real type with `ReactiveServices` a
deprecated alias (D7, rename half). **Chat is now its own plugin** — the Build phase gave it
direct access to `Manager`/`AcquireLLMToken`, so it no longer rides inside reactive — and
`execution.chat_manager_addr` was removed from the OSS config, closing the standing ADR-0057
D5 violation.

**Not yet built:** the licence verifier and issuance (D5 — deliberately deferred until
billing launches), the package layer (ADR-0083), `ListPlugins` (D9), plugin-owned operator
protos (D8), and `PluginStore` (D7's persistence half).

**Known blocker, recorded honestly:** `PluginStore` cannot replace `WatchStore`/`Journal`
until `domain.WatchConfig` leaves the OSS domain package, and that is blocked by the ~11
operator-plane references to it — i.e. by D8. So D7's persistence half is *sequenced after*
D8, not merely pending. `KernelServices` documents those two fields as debt rather than
pretending it is already generic.

One refinement was made during implementation: `Manifest()` **replaces** `Plugin.Name()`
rather than sitting beside it, so a plugin has exactly one source of identity — the
manifest `ID`, which is also the entitlement key. Two names would drift.

## Context

ADR-0074 turned "premium" into a set of compile-time plugins over a curated registry. The
premium binary today composes two (`LangfusePlugin`, `ReactivePlugin`). Three gaps block
the next step.

### Gap 1 — plugins are not self-describing

A `Plugin` is `{ Name() string; Register(*Registry) error }`. It carries no version, no
declared dependencies, and no statement of what it contributes. Nothing can enumerate the
active plugin set, so the operator UI cannot render per-plugin surfaces; it can only guess
from a flat capability array assembled by hand in the OSS composition root.

### Gap 2 — premium vocabulary has leaked into the OSS repo

The reactive plugin's *implementation* is correctly in `cambrian-premium`, but its
*vocabulary and surface area* are not. Verified 2026-07-24:

| Leak | Location |
|---|---|
| 7 `watch-*` capability strings hardcoded | `app/app.go` (~1559–1581) |
| `execution.chat_manager_addr` | `internal/config/config.go:481` — an explicit **ADR-0057 D5 violation** ("OSS `ExecutionConfig` names no premium feature") |
| 7 watch RPCs + ~17 messages (41 `Watch` mentions) | `api/proto/operator.proto` |
| `ReactiveServices`, `ReactiveJournal`, `ReactiveWatchStore`, `ReactivePlanner`, `ReactiveAgentDispatcher`, `ReactiveMemoryWriter`, `ChatManagerAddr` | `app/options.go` |
| `WatchConfig`, `WatchConfigHandler`, `ReactiveDeadLetter`, `JournaledSignal` | `domain/signal.go`, `domain/reactive_journal.go`, `domain/event.go` |

Worse, the boundary is **inverted for persistence**: `ReactiveJournal` is backed by an OSS
bbolt decorator and `ReactiveWatchStore` by the OSS `AgentRepoDecorator` — the open-source
kernel implements durability for, and serializes the private types of, a premium feature.

A second symptom of the same defect: the ADR-0080 Chat Manager is a **passenger inside the
reactive plugin**, because the only way to reach `Manager` / `AcquireLLMToken` is the
`ReactiveServices` bundle handed exclusively to reactive's `NewSignalReceiver`. Chat is not
independently additive today, despite being conceptually separate.

### Gap 3 — there is no notion of entitlement

Plugins are to become **subscription products**. The kernel has no concept of a plugin being
present-but-not-purchased, and therefore no way to distinguish *not sold*, *lapsed*, and
*not built* — a distinction the business model depends on. Product constraints, confirmed:

- **Kernels must run offline / air-gapped.** No network call may sit on the boot path.
- **No third-party marketplace yet**, but possible later.
- The operator UI should list plugins and render their panels conditionally (no reactive
  plugin ⇒ no watch panels).

## Decision

### D1 — Plugins are self-describing via a data manifest

`Plugin` becomes `{ Manifest() PluginManifest; Register(*Registry) error }` — `Manifest()`
**replaces** the former `Name()`, so identity lives in exactly one place:

```go
type PluginManifest struct {
    ID           string      // "reactive" — stable, the entitlement + panel key
    DisplayName  string      // "Reactive Engine"
    Version      string      // plugin's own version line, NOT the operator contract version
    Requires     []string    // plugin IDs this one is built on. reactive: none
    Capabilities []string    // capability strings this plugin advertises
    Panels       []PanelSpec // operator surfaces this plugin contributes
}
```

The manifest is **data, not Go type identity** — deliberately, so a future out-of-process
plugin host can reuse this layer verbatim and only change how `Register` is delivered.

### D2 — Plugins advertise their own capabilities

`Registry` collects `Capabilities` from each entitled plugin's manifest; `SetHandshake`
becomes `baseCaps + registry-collected`. The 7 `watch-*` strings move out of `app/app.go`
into the reactive plugin. **The OSS core stops knowing what `watch-deadletter` means.**

### D3 — Entitlement gates at the registry chokepoint: "not entitled" ≡ "not installed"

`applyPlugins` consults an `EntitlementProvider` **before** calling `p.Register(reg)`. A
non-entitled plugin contributes *nothing*: no capabilities, no gRPC services, no lifecycle,
no goroutines. Consequences that fall out for free:

- The existing "absent plugin ⇒ no capability ⇒ UI hides the panel" path (ADR-0047 D14) is
  reused unchanged. Unpaid and unbuilt are the **same code path**, already tested — an
  unentitled reactive plugin leaves `NewSignalReceiver == nil`, so the kernel falls back to
  the OSS `Watcher` exactly as an OSS build does.
- Enforcement is **kernel-side**, never UI-side. UI gating is cosmetic; anyone can call the
  gRPC directly. Because nothing was mounted, a non-entitled plugin's RPCs return
  `Unimplemented` by construction.
- One chokepoint to audit. Per-plugin self-checks are rejected: a plugin can forget, a
  chokepoint cannot.

```go
type EntitlementProvider interface {
    // Entitled reports whether this deployment may activate the plugin, plus the state
    // to surface to operators (ACTIVE / NOT_ENTITLED / EXPIRED / DEPS_UNMET).
    Entitled(manifest PluginManifest) (EntitlementState, error)
}
```

OSS default: **allow-all** (the OSS build ships no paid plugins, so it gates nothing).

### D4 — One binary, all plugins compiled in, gated at runtime

The premium binary contains **every** plugin; the license decides which activate. The
alternatives were rejected against this project's own constraints:

| Rejected | Why |
|---|---|
| Dynamic loading (Go `plugin`, `.so`) | CGO-only, **no Windows support**, host/plugin version-locked. Already rejected by ADR-0074. |
| Per-customer / per-tier builds | Collides with the offline requirement: buying a plugin would require shipping a **new binary to an air-gapped site** instead of a license file. Plus a combinatorial build+signing matrix and a support surface where no two customers run the same artifact. |
| Out-of-process gRPC plugins | `KernelServices` hands reactive `Manager`/`Auctioneer`/`Memory`/`Planner`/`LLM`/`EventBus`/`PluginStore`. Remoting all of it means designing and versioning a full gRPC surface, plus process supervision and serialization cost on every signal. Reserved for genuinely untrusted extensions (ADR-0074). |

Properties gained: trials are a 14-day license file (no special build); upgrades work
**offline** (drop in a license, restart); one artifact to build, test, sign, and ship (one
container image, not a matrix); every customer runs identical bytes, so support is
reproducible. Artifact granularity stays exactly where it is — **two binaries, OSS and
premium**; a premium binary with no license is behaviourally the OSS kernel.

Accepted cost: all proprietary code lands on every customer's disk and a determined actor
can patch the gate out. **The protection is legal (BSL 1.1), not technical.** This is the
standard open-core bargain (GitLab EE ships all paid code in one artifact and gates by
license); we do not design as though the gate were tamper-proof.

### D5 — Offline-first licensing: build the seam now, the verifier later

License format: an **Ed25519-signed** blob (pure-Go `crypto/ed25519`, stdlib — no CGO,
consistent with the SEC-01 discipline), public key embedded in the premium binary, read
from `~/.cambrian/license`, carrying `customer_id, plan, entitled_plugins[], issued_at,
expires_at, grace_days`.

Three offline-specific rules:

- **Grace period is mandatory.** An air-gapped kernel must not lose a plugin the moment
  `expires_at` passes. Degrade to `EXPIRED` + `grace_until`, keep running, hard-stop only
  after grace.
- **Monotonic clock guard.** Clock rollback is the offline attack. Persist a high-water
  mark of the newest observed time in the `PluginStore` and refuse a wall clock behind it.
  Cheap; stops casual date-setting; we deliberately go no further.
- **Online refresh is an optimization, never a requirement.** No network call on the boot
  path, ever.

**Scoping decision: build the `EntitlementProvider` seam + chokepoint now; defer the
verifier and license issuance.** The seam is cheap and retrofitting it later would touch
every plugin; the verifier needs business decisions not yet made (plan definitions, key
management, renewal, downgrade behaviour). Premium passes allow-all today and drops in
`NewLicenseVerifier(pubkey)` at launch, with zero architectural churn.

### D6 — Entitlement granularity: the *package* is the SKU, resolved to plugins

Plugins are **not** sold individually. The revenue unit is a **business package** (Law,
Company Brain, ERP, Customer Chatbots, Employee Chatbots, Coding), each composing plugins +
agent packs + memory packs — see **ADR-0083**, which owns that layer.

Entitlement therefore resolves in two steps:

```
license grants packages[]  →  union of those packages' plugins[]  →  chokepoint gate
```

`EntitlementProvider.Entitled(manifest)` asks "is this plugin ID in the union of the
entitled packages' plugin sets?" The D3 chokepoint is unchanged — only the *resolution*
in front of it is new. Plugins become shared infrastructure that packages compose: the
reactive engine underpins Customer Chatbots and Company Brain without either owning it,
and the same plugin activates once regardless of how many entitled packages require it.

Per-*capability* entitlement is explicitly rejected: it would push the gate inside each
plugin, defeating the single chokepoint. The manifest still enumerates `Capabilities`, so
a future provider could filter them without a redesign, but there is no product need —
packages, not capabilities, are what customers buy.

### D7 — Generic kernel seams replace reactive-named ones

Two generalizations remove the bulk of the Gap-2 leakage:

**`ReactiveServices` → `KernelServices`.** `Manager`, `Auctioneer`, `Memory`, `Planner`,
`LLM`, `EventBus`, `AcquireLLMToken` are *kernel* capabilities, not reactive ones. Any
plugin takes what it needs. This also dissolves the chat coupling: the chat plugin takes
`Manager` + `AcquireLLMToken` from the same generic bundle and stops being reactive's
passenger.

**`PluginStore` replaces `ReactiveJournal` + `ReactiveWatchStore`.** The kernel offers every
plugin a durable, **namespaced** key-value store; the plugin serializes whatever it wants
into it. Reactive builds its journal semantics (REACT-01/ADR-0061) and watch persistence on
top, in premium. The OSS kernel stops knowing what a `WatchConfig` or a dead-letter *is* —
it stores opaque bytes for namespace `reactive`. This corrects the inverted persistence
boundary and removes two premium concepts from `domain/` and `options.go` at once.

Domain types split on genuine ownership: **`Signal` and `SignalReceiver` stay** (the kernel
produces signals; the OSS `Watcher` consumes them — genuine kernel concepts).
**`WatchConfig`, `WatchConfigHandler`, `ReactiveDeadLetter`, `JournaledSignal` move** to
premium (rule-engine concepts). `execution.chat_manager_addr` moves to premium config,
closing the ADR-0057 D5 violation.

### D8 — Plugins own their operator surfaces

The watch CRUD / dead-letter / metrics / backtest RPCs move **out** of the OSS
`operator.proto` into a premium-owned `reactive_operator.proto`, mounted via
`AddGRPCService` on the kernel's gRPC server — inheriting the server-level operator auth
interceptors, exactly as ADR-0073 already established for `reactive_control.proto`. The
precedent is proven; this extends it from a control plane to an operator plane.

This closes the loop on the UI model: **plugin entitled → its service is mounted → the UI
learns from `ListPlugins` → the UI renders that plugin's panels against that plugin's own
proto.** No watch vocabulary anywhere in the OSS contract.

### D9 — `ListPlugins` operator RPC

A first-class enumeration returning `[{id, display_name, version, state, capabilities,
requires, expires_at}]`. The flat `capabilities` array **remains** as the mechanical
"what's live right now" signal; `ListPlugins` adds what a flat array structurally cannot
express:

| State | UI renders |
|---|---|
| `ACTIVE` | the plugin's panels |
| `NOT_ENTITLED` | locked panel + upsell |
| `EXPIRED` | "subscription lapsed" + renew CTA, within grace |
| `DEPS_UNMET` | "requires Reactive Engine" |

**Panels are capability-gated, not descriptor-driven.** The UI ships panel code for the
known first-party catalog and renders it based on plugin state. Descriptor-driven generic
rendering was rejected for now: it only pays off for third-party plugins the UI cannot know
at build time, and it actively fights the upsell requirement (it can only render what
exists, never what you want to sell). `PanelSpec` metadata rides in the manifest anyway, so
descriptor-driven rendering remains an **additive** evolution.

### D10 — Plugin dependencies: declared, validated, non-fatal

`Requires` is topologically validated at boot; `Register` runs in dependency order. An unmet
dependency yields a **non-fatal `DEPS_UNMET`** state surfaced via `ListPlugins`, not a
startup error. Rationale: with subscriptions, an unmet dependency is a *billing*
combination. A paying customer must never get a kernel that refuses to boot because their
plan includes Conversation Engine but not Reactive Engine.

### D11 — Repo topology: one premium module now; per-plugin repos when earned

Plugins live as isolated packages in the single `cambrian-premium` module
(`plugins/reactive/`, `plugins/langfuse/`, `plugins/chat/`), **not** one repo per plugin.
Separate repos solve independent release cadence, independent authorship, and per-repo
access control — none of which apply to one team shipping one binary. Their cost is
immediate: a version matrix across core × N plugins, and cross-repo seam propagation at a
time when `cambrian-premium/go.mod` has no `require` on core at all (it resolves solely
through the uncommitted `go.work`), core has no committed CI, and the ADR-0057 D13 two-lane
CI is designed-not-built.

What makes plugins additive is **not** repo boundaries but the rule that *no plugin imports
a sibling plugin*. That is enforced by `scripts/check-plugin-isolation.sh` (modelled on the
existing `check-no-premium.sh`) in `make per-pr`: a `plugins/X` importing `plugins/Y` fails
the build. Each plugin package may import only `core/app`, `core/domain`, and its own
implementation package.

Because the contract is an interface and the manifest is data, **extraction is cheap and
reversible** — moving a plugin to its own repo is "add a `go.mod`, add a `require`." Split
one out when it earns it: a third party authors it, it needs its own cadence, or it ships to
customers who must not see the rest of the source. Note that a genuine third-party
marketplace also forces the out-of-process model (you cannot compile a stranger's code into
your binary, and they would reach `AddSystemAgent`), so the separate-repo future is coupled
to that transition, not to this decision.

### D12 — Explicit three-phase plugin lifecycle: Register → Build → Start/Stop

Today a plugin's runtime objects are constructed *lazily inside a hook* and captured by
pointer: `ReactivePlane.NewSignalReceiver` builds the engine and stores it on the plane so
`RegisterControlService` can later mount a control plane over the same instance. That works
only because `NewSignalReceiver` happens to run before `ExtraServices` inside
`bootstrapKernel` — an **implicit, undocumented ordering dependency**. It is also the direct
cause of the chat coupling: the only way to reach `Manager` / `AcquireLLMToken` was to
hijack reactive's hook, so the chat manager became a passenger rather than a plugin.

A plugin therefore has three explicit phases:

| Phase | Signature | Kernel state | Purpose |
|---|---|---|---|
| **Register** | `Register(*Registry) error` | Nothing built | *Declare* contributions. No kernel objects exist. |
| **Build** | `Build(KernelServices) error` | Stacks built, nothing running | *Construct* runtime objects from the capability bundle. |
| **Run** | `Lifecycle.Start(ctx)` / `Stop()` | Serving | Start and drain background work. |

`Build` runs in dependency order (D10), **after** the kernel stacks exist and **before**
gRPC service registration and the handshake — so a plugin's gRPC service, capabilities, and
lifecycle may all depend on objects `Build` created. It is an optional phase: the composition
root type-asserts for it, so a plugin with nothing to construct implements only `Register`.

This removes the pointer-capture pattern entirely, makes ordering explicit and documented
rather than incidental, and gives any plugin needing only `Manager` (chat) a sanctioned
construction point without touching the reactive lane.

### Tiering (extends ADR-0074)

The `EntitlementProvider` is **Tier-3, never-pluggable** — a plugin must never be able to
install its own entitlement provider. `PluginStore` namespaces are assigned by the
composition root from the manifest `ID`, never chosen by the plugin, so one plugin cannot
read or corrupt another's state.

## Migration

Staged to avoid a flag-day. Phases 1 and 2 are independently landable.

| Phase | Scope | Gates |
|---|---|---|
| **1. Quick wins** | Capability strings out of `app.go` into plugin manifests (D1/D2); `chat_manager_addr` out of OSS config (D7); premium reorg into `plugins/<name>/` + isolation script (D11); delete dead `wiring.NewSignalReceiver` | `make per-pr`; CONTEXT.md sync (core + premium). No proto change. |
| **2. Seams** | `KernelServices`, `PluginStore`, `EntitlementProvider` + chokepoint, `Requires` topo-validation (D3/D7/D10); chat becomes its own plugin | `make per-pr`; ADR-0074 amended; CONTEXT.md sync. No proto change. |
| **3. Contract** | `ListPlugins` RPC (D9); premium `reactive_operator.proto` added; OSS watch RPCs marked **deprecated** but retained (D8) | `make proto-breaking` / `proto` / `proto-check`; contract bump + capability strings; re-vendor ui/cli. |
| **4. Cutover** | UI consumes `ListPlugins`, renders state-gated panels against the premium plane; then **delete** the deprecated OSS watch RPCs | Contract bump; UI re-pins. Breaking — one release of overlap. |
| **5. Later** | Ed25519 verifier + license issuance (D5), when billing launches | — |

No benchmark gate: this is a structural/boundary change with no kernel-behavior delta, and
the OSS default path stays inert (allow-all entitlement, unentitled ⇒ existing `Watcher`
fallback). The reactive suite should be re-run as a **regression check**, not a gate.

## Consequences

**Positive.**
- Reactive becomes fully excisable: no premium vocabulary in OSS `domain/`, `config`,
  `options.go`, `app.go`, or `operator.proto`. ADR-0057's boundary is finally honest, and
  the ADR-0057 D5 violation is closed.
- Subscriptions get a single, auditable enforcement point that reuses already-tested paths.
- The UI can render, lock, and upsell plugins from one RPC.
- Chat stops being reactive's passenger; ADR-0080 becomes independently additive.
- `PluginStore` gives every future plugin durability without OSS learning its types.
- The manifest/entitlement layer is transport-agnostic, so the out-of-process future reuses
  it rather than replacing it.

**Negative / costs.**
- Phase 4 is a **breaking operator-contract change**; UI and CLI must re-vendor. (The UI is
  pinned at 0047 against a kernel serving 0060, so it is re-vendoring regardless — this
  makes it mandatory rather than optional.)
- `KernelServices` + `PluginStore` are new semi-stable public seams to maintain.
- Moving `WatchConfig` out of `domain/` touches every watch call site.
- All proprietary code ships to every customer (D4, accepted).

**Neutral.**
- `Options` keeps its direct fields; plugins fold into the same effective configuration.
- Per-plugin SKU granularity (D6) is revisitable without redesign.

## References

- **ADR-0083 (business packages — the SKU layer above this one; owns D6's package resolution
  and the agent/memory artifact classes).**
- ADR-0074 (compile-time plugin registry — the mechanism this extends), ADR-0057 (open-core
  boundary; D5 config rule, D8 stable-contract scope, D13 CI topology), ADR-0073
  (`ExtraServices` / premium-owned proto plane — the precedent for D8), ADR-0047 (operator
  transport plane; D14 capability handshake), ADR-0080 (chat daemon ownership — the
  passenger this frees), ADR-0061 (reactive journal — moves onto `PluginStore`), ADR-0034/
  0038 (the un-pluggable security gates).
- Code: `app/plugin.go`, `app/options.go`, `app/app.go` (~1528–1582 capability assembly),
  `internal/config/config.go:481`, `api/proto/operator.proto`, `domain/signal.go`,
  `domain/reactive_journal.go`; `cambrian-premium/wiring/`, `cambrian-premium/cmd/orchestrator/main.go`.
