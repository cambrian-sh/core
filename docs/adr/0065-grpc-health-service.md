---
id: 0065
title: gRPC Health Service (grpc.health.v1, DB-gated readiness, drain semantics)
status: Accepted
date: 2026-07-15
supersedes: []
superseded_by: []
depends_on:
  - 0047-operator-transport-plane
  - 0033-daemon-agent-lifecycle
---

# ADR-0065: gRPC Health Service

## Status

Accepted

## Context

This is PLAT-03 (`distribution-production-readiness.md` §6 gap 2). The kernel exposes no
health surface (only the pagerank worker has one), which blocks Kubernetes
liveness/readiness probes, load balancers, installer smoke tests, and a robust
`cambrian status`. Benchmark runs also fall back to sleep-based boot waits — a flake
source — for lack of a readiness signal.

## Decision

Serve the **standard `grpc.health.v1.Health`** service on the kernel's main gRPC
listener (the same server that carries `Orchestrator` and `OperatorConsole`). The proto
is grpc-go's own (`google.golang.org/grpc/health` + `.../health/grpc_health_v1`, already
a transitive dependency), so nothing is vendored and `make proto-check` stays clean by
construction. SDK agents already use grpc-health-checking, so the pattern is in-house.

### Readiness = process up + database reachable; NOT agents

A small `internal/health.Checker` wraps grpc's `health.Server` and drives its status from
a **readiness probe**. Readiness is:

- the process is up (bbolt open, operator service registered — implied by the server
  serving requests at all), **and**
- **the database is reachable** — a periodic `pgxpool.Ping` (default every 10s,
  `server.health_check_interval_seconds`).

Readiness is deliberately **not** gated on agents: agents degrade independently and a
single crashed agent must not take the whole kernel out of the load-balancer rotation.
Per-agent health is exposed through the existing status surfaces, not this probe.

The initial status is `NOT_SERVING` until the first probe runs, so nothing reports ready
before the DB has actually been reached. Both the overall service (`""`, what
`grpc_health_probe` checks by default) and a named `cambrian.OperatorConsole` key are
set, so a probe can target either.

### Drain: NOT_SERVING before GracefulStop

`Kernel.Shutdown` calls `Checker.Shutdown()` **first** — flipping the status to
`NOT_SERVING` and making it sticky (a late probe cannot resurrect `SERVING`) — *before*
`GracefulStop`. Load balancers and probes therefore stop routing to a draining kernel
before it drops in-flight work.

### Optional `/healthz` HTTP shim (off by default)

For dumb probes that cannot speak gRPC, `server.healthz_port > 0` starts a tiny HTTP
server returning `200` when ready and `503` otherwise. Off by default (the gRPC health
service is always on); no new HTTP surface unless an operator opts in.

## Consequences

**Positive.**
- K8s liveness/readiness probes, load balancers, installer/benchmark smoke tests, and
  `cambrian status` have a real, standard signal; the benchmark harness can replace
  sleep-based boot waits.
- DB-gated readiness means a kernel whose Postgres is down is correctly pulled from
  rotation without being killed (it can recover when the DB returns).
- Drain-before-stop avoids dropping requests a balancer would otherwise still route.
- No vendored proto, no new dependency, no CGO.

**Negative / costs.**
- A periodic DB ping adds a trivial, bounded query every interval.
- The probe is coarse (a ping) — it does not exercise the full query path; a deeper
  readiness check (e.g. a canonical retrieval) is a possible future refinement.

**Neutral.**
- `cambrian status` and the installer smoke test consuming the probe live in the CLI/
  installer repos; this ADR delivers the kernel-side service they call.

## References

- PLAT-03 (`docs/backlog/PLAT-03-grpc-health-service.md`);
  `distribution-production-readiness.md` §6 gap 2, §7 topology probes.
- ADR-0047 (operator transport plane — the listener this shares), ADR-0033 (daemon
  lifecycle — per-agent health lives there, not in this probe).
