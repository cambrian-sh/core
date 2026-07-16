---
id: 0073
title: Premium Transport-Plane Extension — app.Options.ExtraServices
status: Accepted
date: 2026-07-16
supersedes: []
superseded_by: []
depends_on:
  - 0057-open-core-boundary
  - 0047-operator-transport-plane
  - 0032-symbiotic-reactive-rule-engine
---

# ADR-0073: Premium Transport-Plane Extension

## Status

Accepted (OSS `ExtraServices` seam + premium `ReactiveControl` plane delivered and unit-
tested; a live full-kernel E2E of the benchmark `reactive` suite is the residual — it
needs Postgres/pgvector, absent in the unit environment).

## Context

The benchmark harness needed to exercise the reactive lane's *guarantees* — exactly-once
(REACT-01), debounce (REACT-02), dry-run (REACT-05), schedule fire (REACT-06) — not just
capture events. Every one of those needs the harness to **inject a signal** onto a
reactive stream. But signals only enter the kernel from external producers (daemon
agents, the filesystem watcher); the operator plane has no signal-injection RPC, and
`schedule` is the only source the kernel drives itself.

The obvious move — add an `EmitSignal` RPC to the OSS `operator.proto` as a nil-in-OSS,
premium-backed handler (the established `RegisterWatch`/`GetWatchMetrics` pattern) —
works, but puts a benchmark/control affordance into the **pinned public UI contract**
(invariant #2) and spends a contract-version bump on it. The reactive engine, its control
surface, and its metrics all live in the premium module; the affordance should too.

## Decision

Add a single, inert-by-default OSS seam and let the **premium module own the service**.

### 1. OSS seam: `app.Options.ExtraServices func(*grpc.Server)`

Invoked in the composition root after the core services (`Orchestrator`, `Health`,
`OperatorConsole`) are registered on the kernel `grpc.Server` and **before** `Serve`.
OSS default: nil (no extra services). It names nothing premium — it is a generic
"register additional gRPC services" hook, consistent with ADR-0057's downstream-library
model (Model C). It is carried from `Options` to the serve site via a `Kernel.ExtraServices`
field.

**Auth is inherited, not bypassed.** The operator auth interceptors are installed at the
`grpc.NewServer(...)` level (`ChainUnaryInterceptor`/`ChainStreamInterceptor`), so *any*
service mounted on the kernel server — including a premium one — sits behind the same
operator authentication as `OperatorConsole`. A service added here is an **extension of
the authenticated operator transport plane**, not a second unauthenticated door.

### 2. Premium service: `ReactiveControl` (premium-owned proto)

The premium module gains its first proto: `cambrian-premium/api/proto/reactive_control.proto`,
defining `service ReactiveControl { rpc EmitSignal(...) }`. `EmitSignal` delivers a
synthetic `domain.Signal` onto a stream via the engine's existing `OnSignal`, so it flows
through the **unchanged** condition/action pipeline. The handler (`reactive/control.go`)
best-effort JSON-decodes string payload values so numeric/boolean condition inputs arrive
typed. It is wired through a `wiring.ReactivePlane` that captures the concrete engine when
the `NewSignalReceiver` hook runs (before `ExtraServices`, both inside `bootstrapKernel`)
and mounts the service in `RegisterControlService`.

The OSS `operator.proto` contract is **untouched** — no new RPC, no contract-version bump.
The benchmark harness vendors the premium proto stub *separately* and only reaches the
plane against a premium kernel (which the reactive suite already requires, since
metrics/backtest are premium-only).

### Reconciliation with invariant #2 (`OperatorConsole` is the only UI→kernel API)

Invariant #2 governs the **UI/human** path: no second service lets a human impersonate an
agent or slip past scope/audit. `ReactiveControl` is (a) premium-only — absent from every
OSS build, (b) mounted through an explicit OSS seam, (c) behind the *same* operator auth
interceptors, and (d) leaves the pinned OSS contract unchanged. The invariant's spirit —
no un-audited human-as-agent bypass — holds. The UI still speaks only `OperatorConsole`;
`ReactiveControl` is a premium/benchmark control surface, not a UI API.

## Consequences

**Positive.**
- The reactive lane becomes benchmark-drivable end to end: the harness injects signals and
  asserts exactly-once / debounce / dry-run / dead-letter / schedule-fire deterministically.
- Zero pollution of the pinned OSS operator contract; no contract bump for a control/test
  affordance. Premium owns and versions its own proto.
- The seam is generic and reusable — any future premium-only service mounts the same way,
  authenticated for free.
- Byte-identical OSS behavior when unused (nil hook).

**Negative / costs.**
- A *second* gRPC service now runs on the kernel in premium builds (reconciled above).
- The premium module gains its first proto + codegen step (protoc; no buf dependency).
- The benchmark harness vendors an additional stub; its regen tool (`transport/stubs.py`)
  is now premium-aware (auto-globs a sibling `cambrian-premium/api/proto`).
- Live full-kernel E2E of the suite needs Postgres/pgvector — deferred as a residual; the
  injection path itself is unit-proven in `reactive/control_test.go`.

**Neutral.**
- `Options` grows from three hooks to four (`TraceWrapper`, `AgentCallLogger`,
  `NewSignalReceiver`, `ExtraServices`).

## References

- ADR-0057 (open-core boundary / Options seam), ADR-0047 (operator transport plane —
  invariant #2), ADR-0032 (reactive engine), ADR-0061/0062/0071/0072 (the REACT guarantees
  the control plane makes benchmark-drivable). Code: `app/options.go`, `app/app.go`
  (serve site), `cambrian-premium/api/proto/reactive_control.proto`,
  `cambrian-premium/reactive/control.go`, `cambrian-premium/wiring/control.go`,
  `cambrian-benchmarks/src/cambrian_bench/suites/reactive/`.
