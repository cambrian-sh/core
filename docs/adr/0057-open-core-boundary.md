---
id: 0057
title: Open-Core Boundary, Licensing & OSS Release Model
status: Accepted
date: 2026-06-27
supersedes: []
superseded_by: []
---

# ADR-0057 — Open-Core Boundary, Licensing & OSS Release Model

**Status:** Accepted (2026-06-27) — resolved via a grilling session; **locked** (incl. the BSL
license, confirmed with a 10-year Change Date).

## Context

Cambrian is going open-core: a public OSS runtime plus a private premium layer. Publishing is
**irreversible** — anything in the public repo at tag time is permanently free, and the license on
the published snapshot is permanent for that code (future code can be relicensed; shipped code
cannot). This ADR fixes the boundary, the split mechanism, the license, the contribution terms, and
the stability contract *before* the first publish, because each is load-bearing and hard to undo.

Today premium lives at `internal/premium/` (reactive rule engine + langfuse tracing), tightly
coupled to `internal/` packages, behind `//go:build premium` tags, selected at compile time. The
composition root is ~1633 lines in `cmd/orchestrator/main.go` (`package main`, not importable).

## Decisions

### D1 — Premium feature set (minimal)
Premium = **{reactive rule engine, langfuse tracing}** only. Everything else is OSS, including the
**operator/control plane** (ADR-0047), verifier pool, daemon agents, memory, MCP, tool retrieval,
and the Python SDK. Premium may **extend** the operator plane later via downstream injection; that
is a non-breaking *addition* to OSS (safe to defer, since OSS is our own source of truth).

### D2 — Split model: downstream library (Model C), not a mirror
The OSS repo is the **published, single source of truth**. Premium lives in a **separate private
repo** (`cambrian-sh/cambrian-premium`) that `require`s the OSS module and ships its **own binary**
(its own `main()` wiring OSS + injecting premium). **No mirror, no private monorepo as an ongoing
artifact, no diverging history.** The mirror model was rejected: it creates two-headed bookkeeping
(diverging history, ambiguous home-of-record for tags/issues/PRs) for a project with no users yet.

### D3 — Thin extension seam; kernel stays internal
Premium imports a **narrow exported extension surface**, not the kernel internals. The reactive
seam is refactored so premium depends on a small set of exported interfaces (dispatcher, auction
dispatcher, daemon-lifecycle, memory-writer, watch-config store, planner, evaluator-factory) —
**not** the concrete `*MemoryStack`/`*MetabolismStack`/`*AwarenessStack`, which remain `internal/`.
langfuse needs only `domain.Generator`/`StreamChunk` (already trivial).

### D4 — Composition root extracted; build tags dropped
The boot logic moves from `package main` into an importable **`app` package** exposing
`app.Run(ctx, opts)`, where `opts` carries the injection hooks. OSS `cmd/orchestrator` becomes a
thin shell (`app.Run(ctx, app.DefaultOptions())`); the premium binary calls
`app.Run(ctx, opts.With(premiumReactive, premiumLangfuse))`. The `//go:build premium` tags and the
`provider_oss.go`/`provider_premium.go` pair are **removed** — physical (two-repo) separation
replaces compile-time separation.

### D5 — Three-tier configuration
(1) **User/deployment** config (gitignored `config.json`; dirs, db, mcp, endpoints, secrets via env).
(2) **OSS tuned hyperparameters** (`go:embed`-ed `configs/defaults.json`; `execution` + tuned
`embedder`). (3) **Premium hyperparameters** owned by the premium repo (its own embedded defaults),
fed into OSS via the injection `opts`. The `ReactiveEngine*` fields are **removed from OSS
`ExecutionConfig`** so the OSS config schema names no premium feature.

### D6 — License: BSL 1.1 (source-available)
The OSS repo and the Python SDK ship under **Business Source License 1.1**:
- **Change Date:** 10 years per release. **Change License:** Apache-2.0.
- **Additional Use Grant:** any use **except** offering the Software (or a substantially similar
  service) to third parties as a hosted/managed commercial service.
Rationale: license tightening is impossible for shipped code while loosening (BSL→Apache) is
trivial; BSL preserves both options, permissive throws one away. Messaged as **source-available**,
not "open source" (OSI). **Confirmed/locked (10-year Change Date).**

### D7 — Contributions: CLA (relicensing rights)
A **CLA** (Apache-ICLA-style: broad license grant **with** sublicense/relicense rights, **no**
copyright assignment), **bot-enforced** on PRs. Required so OSS-core contributions can flow into
premium and so the BSL→Apache conversion / future dual-licensing are legal. DCO alone is
insufficient (it would trap contributions in BSL-core).

### D8 — Stability contract: proto + config only
In `v0.x`, the held-stable contracts are the **proto/operator gRPC surface** (gated by
`buf breaking` / `WIRE_JSON`) and the **config schema**. The **Go package API is explicitly
unstable** — its only consumer during alpha is our own premium repo, so it is an internal
coordination concern, not an external promise. Premium pins **exact** OSS tags and runs a **canary
CI** build against OSS `main`.

### D9 — Clean-snapshot publish
The public OSS repo is published from a **fresh squashed snapshot** at the split point. The full
development history (with premium, secrets, and large benchmark blobs) **stays private** in the
monorepo, which also becomes the premium repo's ancestor. This eliminates premium-history leakage,
historical secret exposure, and blob bloat in one move — retiring the `git filter-repo` purge.

### D10 — Python SDK
OSS, **BSL-licensed**, published to **PyPI under `cambrian-sh`**, versioned on its **own line** with
a documented **proto-compatibility range** (the SDK↔runtime contract is the proto, not the version).

### D11 — Documentation split
Premium docs (ADR-0032 reactive, the langfuse ADR, premium PRDs/issues) **travel to the premium
repo**. Raw issue specs (425) and `CURRENT_CODEBASE_STATE.md` stay **private**. Only curated OSS
ADRs (post-reconciliation) + a scrubbed `docs/ARCHITECTURE.md` publish. Dangling cross-refs to
now-premium ADRs are **stubbed**, not left broken.

### D12 — Sequencing: langfuse as the tracer-bullet
**langfuse is built out as the full end-to-end Model C apparatus first** (premium repo, premium
binary via `app.Run`, CLA bot, canary CI, coordinated release) — not a code move. **reactive then
rides the proven pipeline** (seam-narrow → extract → carry tests → live-wire). The reactive
live-wiring is the **single long pole** that drives the alpha date.

### D13 — CI/CD topology & cross-repo dev ergonomics
- **Premium CI is two-lane:** a *blocking* lane builds/tests against the **pinned OSS tag** (green by
  construction); an *advisory canary* lane builds against OSS `main`, **never blocks**, and only
  notifies on drift. The canary's core check is **compile-only** (Go API breaks are deterministic
  compile errors — no flake); tests run in a separate retry-tolerant lane.
- **Reverse canary in OSS CI:** PRs touching the seam packages (`app` + exported extension
  interfaces) compile the private premium repo against the PR (credentialed, maintainer PRs;
  soft-skip for external contributors). Detection shifts left to OSS review.
- **`apidiff` gate on the seam package only.** Premium depends on the seam, not all of OSS — so only
  the seam's exported surface is held semi-stable and diffed in CI; non-seam churn is irrelevant to
  premium. Flagged seam changes are **batched** into minor releases (`v0.2.0`, …), not trickled.
- **Local dev = Go Workspaces (`go.work`)** loading `cambrian-runtime` + `cambrian-premium` together,
  so cross-repo refactors don't fight `go.mod replace`/pseudo-versions. Documented in `DEVELOPER_GUIDE.md`.
- **Premium-leak audit is automated** via a `go/ast` import-rule check in CI (no human review):
  forbidden proprietary import paths fail the build.

### D14 — Reactive-seam de-risking spike (precedes extraction)
The langfuse tracer-bullet validates the *apparatus* but **not** the reactive seam — auction-based
coordination resists thin interfaces. Before Phase 4b, run a **spike** that prototypes the reactive
extension interface set against the real adapters (`auctioneerDispatcher`, `daemonLifecycleAdapter`,
BBolt WatchConfig load) and proves it is thin enough. **Fallback if it isn't:** promote the kernel
stacks wholesale (larger public surface) rather than ship a leaky abstraction.

**Spike status (2026-06-28, COMPLETE — empirically validated; verdict GO):**
- *Empirical proof:* a throwaway `BuildSignalReceiverFromServices(SpikeServices)` built the **entire**
  reactive `SignalReceiver` + `WatchHandler` (engine, all 4 executors, condition-evaluator factory,
  WatchConfig load, daemon-aware handler) from an **interface-only bundle** — zero kernel types —
  and **compiled + vetted clean** under `-tags premium`. Baselines confirmed: OSS no-tag build and
  premium build both green. (Throwaway deleted.)

- *Forward* (premium→OSS): `internal/premium/reactive` imports **only `internal/domain`** and already
  declares 7 consumer-side interfaces (`ConditionEvaluator`, `ActionExecutor`, `DirectDispatcher`,
  `AuctionDispatcher`, `MemoryWriter`, `WatchConfigStore`, `LLMGenerator`). Kernel coupling lives in the
  `provider_premium.go` adapters, not the engine. OSS must expose ~8 capabilities (CallAgent, Auctioneer,
  MemoryWriter, WatchConfig CRUD, LLM, Planner, daemon spawn/stop, EventBus) + `domain` types.
- *Reverse* (OSS→premium): exactly **5 enumerated spots** (see plan Phase 2): `cmd/.../main.go`→langfuse,
  `kernel/provider_premium.go`→reactive, `substrate/network/watch_rpc.go` (premium-tagged in core),
  `substrate/network/watch_rpc_test.go`→reactive, `internal/benchmarks/e2e_quality_test.go`→langfuse.
  Invariant must be **`go test ./...`**, not just `go build` (items 4–5 are test-only).
- *domain granularity (resolved):* `internal/domain` is a **leaf** package (231 exported types, ~9.3k
  lines, no internal deps). Reactive's boundary touches ~22 of those 231 — but they are the *central*
  types whose transitive closure spans most of `domain`, so carving a `domain/contract` cascades or
  cycles. **Decision: export `domain` wholesale** (`internal/domain` → `domain/`, mandatory anyway since
  premium is a separate module). It lifts cleanly; the surface carries no SemVer promise (Go API
  unstable in v0.x; only the seam is `apidiff`-gated). Mark `domain` `UNSTABLE`; revisit a curated
  subset near v1.0.
- **Spike verdict: GO — seam is thin in both directions; the kernel-stacks-wholesale fallback is NOT
  needed.** OSS-exported surface = `domain` (wholesale) + an `app.ReactiveServices` capability bundle
  (~8 methods) + `app.Options` hooks + the server's nil-able `SignalReceiver`/`WatchHandler` injection.
  Kernel stacks stay `internal/`. Premium keeps the 7 consumer-side interfaces, the adapters, and `watch_rpc.go`.

## Consequences

- **Positive:** public repo is the genuine source of truth; premium fully external and minimal;
  clean public history; small, deliberate public API surface; license preserves optionality;
  contributions legally usable in premium.
- **Cost:** full Model C refactor (composition-root extraction + seam narrowing + reactive
  externalization) lands **before** the alpha — the reactive live-wiring gates the release date.
- **Irreversibility handled:** the free/paid line, the license, and the published history are all
  decided before the (one-way) publish.

## Status History
- 2026-06-27 — Accepted via grilling session (14 questions). Execution sequenced in
  `OSS_RELEASE_EXECUTION_PLAN.md`; analysis in `OSS_PREMIUM_SPLIT_OBSERVATIONS.md`.
- 2026-06-28 — **Locked.** BSL confirmed with a **10-year** Change Date (Change License Apache-2.0).
  No remaining open license question.
