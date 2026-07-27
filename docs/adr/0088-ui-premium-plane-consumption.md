---
id: 0088
title: The UI May Consume Premium Planes — Capability-Gated Plugin Surfaces
status: Proposed
date: 2026-07-26
supersedes: []
superseded_by: []
amends:
  - 0047-operator-transport-plane
depends_on:
  - 0047-operator-transport-plane
  - 0057-open-core-boundary
  - 0073-premium-transport-plane-extension
  - 0074-plugin-architecture
  - 0082-additive-licensed-plugins
  - 0085-access-policy-port-and-extraction
---

# ADR-0088: The UI May Consume Premium Planes

## Status

Proposed — **implemented** for the first case (the access-policy console).
**Amends cross-repo invariant #2**, which said `OperatorConsole` is the only UI→kernel API.

## Context

Cross-repo invariant #2 has said, since ADR-0047: *"`OperatorConsole` is the only UI→kernel
API."* It exists for a good reason — a human operator must not reach the kernel through the
agent plane, because that smuggles a person past scope and audit as an agent principal.
`CORE-OPS-1` existed precisely because the CLI was doing that.

ADR-0073 then established a second kind of plane: a **premium-owned gRPC service**, defined in
premium's own proto, mounted on the same kernel server, behind the same operator auth
interceptors. It was introduced for the benchmark harness (`ReactiveControl`), and the UI was
never in scope.

ADR-0085 makes that gap load-bearing. Access policy splits cleanly in two:

- What the **kernel** must answer for itself — `ExplainAccess`, `ListClassificationTags` —
  belongs in the pinned OSS contract, because a kernel that cannot explain its own denials is
  the failure mode the whole design exists to prevent.
- What the **product** offers — groups, policy objects, links, What-If, audit export — is
  premium, and putting it in the OSS contract would drag premium vocabulary into the
  open-source proto, which ADR-0082 D2 explicitly avoids.

So either the UI never administers policy (leaving the feature CLI-only, which is not a
product), or invariant #2 changes. Writing this down is the change.

## Decision

### D1 — The UI may consume premium-owned planes, under three conditions

1. **Mounted on the kernel's server, behind the same operator auth interceptors** (ADR-0073).
   A premium plane is an *extension* of the operator transport plane, not a second door. It
   authenticates identically, and a Viewer is refused identically.
2. **Over the same connection and the same bearer token.** A second connection would be a
   second thing to authenticate, reconnect, and reason about, and it would make the premium
   plane look like a side channel. `Transport::authed_channel` hands the premium client the
   channel the operator console already holds.
3. **Capability-gated, never probed.** The UI renders a plugin's surface only when the
   handshake carries that plugin's capability string.

### D2 — What is still forbidden, and why

The original prohibition stands where it matters: **the UI never speaks the agent-facing
`Orchestrator` service, never carries an `x-agent-id` principal, and never touches the
kernel's databases or files.** Invariant #2's purpose was to stop a human being smuggled in as
an agent principal — nothing here weakens that. What changes is only the assumption that
"operator plane" and "the `OperatorConsole` service" are the same thing. They are not: the
operator plane is a set of services sharing one authenticated connection and one auth policy,
and `OperatorConsole` is the pinned OSS member of it.

### D3 — Capability gating is the rendering contract

The kernel folds a plugin's declared capability strings into the handshake WITHOUT
interpreting them (ADR-0082 D2). The UI keys panels off those strings, exactly as it already
does for `chat` and `memory-answer`.

This makes the OSS/premium difference a *rendering* difference rather than an error-handling
one. There is no probing, no try/catch as flow control, and no half-drawn surface: an OSS
kernel simply does not grow the panel. Every premium RPC still answers `Unimplemented` without
the plugin, so skipping the gate degrades to a clean error rather than bad data — the gate is
what turns that error into a good empty state.

### D4 — An absent plugin is a correct deployment, not a broken one

The empty state says what the deployment **is**, not what is missing. An OSS kernel running
unscoped is the correct and only behaviour for single-tenant open source (ADR-0085 §4.2); it is
not a premium kernel with a fault. Copy that nags an operator to buy something is how a
capability gate becomes an upsell, and an upsell in a security console is worse than no
message at all.

### D5 — The vendoring rule extends unchanged

Premium protos are vendored into `ui/proto/<plugin>/` the same way `operator.proto` is
vendored into `ui/proto/`: copied from the owning repo, never hand-edited, and re-vendored when
the owning contract bumps. The OSS contract keeps its pinned-version handshake check; a premium
plane is versioned by its plugin, not by the operator contract.

## Consequences

- `ui/proto/authz/access_policy.proto` is vendored from `cambrian-premium`; `build.rs`
  compiles both planes; `pb.rs` exposes the premium client under `pb::authz`.
- `PINNED_CONTRACT_VERSION` 0065 → **0066**.
- Root `AGENTS.md`, root `CONTEXT.md`, `ui/CLAUDE.md`, and `ui/CONTEXT.md` are updated. The
  rule files matter as much as the ADR here: `ui/CLAUDE.md` listed "Only `OperatorConsole`" as
  non-negotiable, and leaving it stale would make the next session correctly "fix" this work
  back out.
- The pattern is now established for every future plugin surface: declare a `PanelSpec` on the
  plugin manifest, advertise the capability, vendor the proto, gate the panel.

## Risks

**Plane sprawl.** Every plugin gaining a UI surface means another vendored proto and another
generated client compiled into the binary. That is acceptable while plugins are compile-time
and few; it stops being acceptable if plugin count grows or plugins become dynamically loaded.
The check to apply at that point is ADR-0082 D9's descriptor-driven rendering — a UI that
renders panels from the manifest rather than compiling a client per plugin. This ADR does not
build that, and should not be read as a licence to keep adding planes indefinitely.

**The pinned-contract discipline is now weaker by one.** `OperatorConsole` has a
version handshake and a skew banner; a premium plane has neither. A UI built against an older
plugin will fail at the RPC rather than at a banner. Acceptable while premium and the UI ship
together; it needs its own handshake the moment they do not.

**Capability strings are untyped.** `access-policy` is a string agreed by convention between a
plugin manifest and a UI constant, with nothing checking they match. A typo silently hides the
panel — which is at least the safe direction, but it is invisible. The manifest's `PanelSpec`
is the eventual fix.

## Alternatives considered

**Put policy administration in the OSS operator contract.** Simplest, and it keeps invariant #2
untouched. Rejected because it drags groups, policy objects, and links — the premium product —
into the open-source proto that ADR-0082 D2 keeps clean, and every OSS consumer would carry a
contract it can never serve.

**Leave policy administration CLI-only.** No invariant change, no new plane. Rejected because
the spec's whole argument is that this model is only usable if an administrator can see the
resultant policy and the blast radius before enforcing — and a console is where that happens.
An unusable safety feature gets switched off, which is the SELinux failure the design set out
to avoid.

**A separate connection for the premium plane.** Rejected: two connections to authenticate,
two to reconnect, two recovery state machines, and a premium plane that looks like a side door
instead of the extension it is.
