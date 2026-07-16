---
id: 0070
title: Daemon Supervision — Restart with Backoff, Flap Quarantine, (Heartbeat Liveness)
status: Accepted
date: 2026-07-16
supersedes: []
superseded_by: []
depends_on:
  - 0033-daemon-agent-lifecycle
  - 0032-symbiotic-reactive-rule-engine
---

# ADR-0070: Daemon Supervision

## Status

Accepted (crash-restart + flap quarantine delivered; heartbeat liveness scoped as a
follow-up)

## Context

Gap **G4** from `docs/research/daemon-watches-readiness/REPORT.md`. Crash *detection*
exists — `agentmgr/crash_detection.go` distinguishes an expected stop (via `StopDaemon`)
from an unexpected exit and publishes `DaemonCrashedEvent`, which the ReactiveEngine uses
to mark the stream unavailable (ADR-0033). But crash *policy* is thin: on an unexpected
exit the daemon is simply marked unavailable and left down — **its watches go silently
dead**. There is no automatic restart, no flap guard against a crash-looping daemon, and
no liveness check for a daemon that *hangs* without exiting (it stops emitting signals and
its watches quietly stall).

## Decision

### 1. Auto-restart with exponential, full-jitter backoff

`DaemonRestartPolicy` (pure, concurrency-safe) tracks restart attempts per stream within a
sliding window. On an unexpected exit, `handleDaemonExit` calls `Register(streamID)`:

- It returns a **delay** — exponential backoff `Base·2ⁿ` (n = prior attempts in the
  window), capped at `Max`, with **full jitter** (uniform in `[0, ceiling]`) so a fleet of
  daemons that die together do not thundering-herd their restarts.
- After the delay, the daemon is re-spawned with its **original params** (captured from the
  registry before the entry is cleared). On success the stream status returns to `running`
  and a `DaemonRecoveredEvent` is published — the ReactiveEngine re-marks the stream
  available, so the watches resume.

Config: `daemon_restart_{max_attempts,window_seconds,base_backoff_ms,max_backoff_ms}`.
`max_attempts = 0` disables auto-restart entirely (the pre-REACT-04 behavior).

### 2. Flap quarantine

When the attempt count in the window reaches `MaxAttempts`, `Register` returns
`quarantine = true`: the daemon is **not** restarted, its status is set to `quarantined`,
and a `DaemonQuarantinedEvent` is published (operator-visible). A crash-loop therefore
lands in a stable quarantined state — spending bounded restart budget — rather than
spinning. The window lets a genuinely-transient failure resume later.

### 3. Heartbeat liveness (scoped follow-up)

The hung-daemon case (a process that lives but stops emitting) needs a heartbeat: the SDK
`daemon.py` emits a periodic heartbeat signal, the kernel tracks last-heartbeat per stream,
and missed-N-heartbeats routes into the same restart path (kill → the restart policy
above). An "expected signal rate" per watch is the alternative for daemons with a regular
cadence. This is a cross-repo change (SDK + a kernel heartbeat monitor + a signal-path
hook) and is documented as the REACT-04 residual; the crash-restart path it would feed is
delivered here.

## Consequences

**Positive.**
- A crashed daemon recovers automatically with backoff, and its watches resume — G4's
  core failure (silently-dead watches) is fixed for the crash case, on by default.
- A crash-looping daemon is quarantined with bounded restart cost, not an infinite spin,
  and the quarantine is an operator-visible event.
- Full-jitter backoff avoids restart thundering herds.
- The policy is pure and unit-tested (backoff schedule, flap threshold, window rollover,
  disabled/nil, reset); no live daemon needed to validate the decision logic.

**Negative / costs.**
- Auto-restart is now default-on (behavior change); `max_attempts = 0` restores the old
  leave-it-down behavior for anyone who wants it.
- The hung-daemon (no-exit) case is not yet covered — that is the heartbeat follow-up.
- Restart replays the original spawn params captured at spawn time; a daemon whose correct
  params changed since spawn would restart with the stale set (params are static per
  WatchConfig today, so this is not a live concern).
- Degraded-watch surfacing in `ListWatches` (operator plane) beyond the quarantine event
  is a smaller follow-up.

## References

- REACT-04 (`docs/backlog/REACT-04-daemon-supervision.md`);
  `daemon-watches-readiness/REPORT.md` G4. ADR-0033 (daemon lifecycle + crash detection),
  ADR-0032 (reactive engine — stream availability). Resource ceilings ride SEC-01.
