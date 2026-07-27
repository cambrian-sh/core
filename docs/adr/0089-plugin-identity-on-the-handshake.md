---
id: 0089
title: Plugin Identity on the Handshake — Per-Plugin Version Skew
status: Proposed
date: 2026-07-26
supersedes: []
superseded_by: []
amends:
  - 0047-operator-transport-plane
  - 0082-additive-licensed-plugins
depends_on:
  - 0047-operator-transport-plane
  - 0074-plugin-architecture
  - 0082-additive-licensed-plugins
  - 0088-ui-premium-plane-consumption
---

# ADR-0089: Plugin Identity on the Handshake

## Status

Proposed — **implemented**. Contract `0066 → 0067`. Applied to both plugin-backed console
surfaces (access policy, watches).

## Context

ADR-0047 D14 gave the console a version handshake: the kernel reports `contract_version`, the
console pins what it was built against, and a mismatch raises a skew banner. That has worked
well enough that the failure it prevents is easy to forget — a client and a server that
disagree about a wire format, each behaving as though they agree.

ADR-0082 D2 then made plugins additive: a plugin declares capability strings, the kernel folds
them into the same handshake **without interpreting them**, and the console renders a panel
when its string is present. That is a good rendering contract and it is the reason premium
vocabulary stays out of the OSS core.

But it leaves a gap that the contract version cannot cover. `contract_version` describes the
**OSS proto surface**. It says nothing about a plugin. A kernel can serve contract `0067`
faithfully while running an access-policy plugin two major versions ahead of the panels
compiled into the console — and every RPC will answer normally, because the RPCs belong to the
plugin's own plane (ADR-0073/0088), which the OSS contract version does not describe. The
console would render a policy editor whose fields quietly mean something else, and nothing
anywhere would say so.

The capability list cannot close this either. A capability string is a boolean: the surface
exists or it does not. It carries no version, and it cannot distinguish the three situations
an operator most needs told apart:

1. this deployment does not have the plugin (correct, nothing to do);
2. the deployment has it, but it declined to register — entitlement, or an unmet dependency
   (someone paid for a surface they cannot see);
3. it is running, but built against a different version than this console (everything appears
   to work).

Today all three render as the same absence, and (3) does not render as anything at all.

There is a second, smaller problem. The skew banner that does exist lives on one settings page.
A warning an operator has to go looking for is a warning that arrives after the mistake.

## Decision

**D1. Plugin identity rides the existing handshake.** `SnapshotResponse` gains
`repeated PluginInfoOp plugins` — id, display name, **its own version line**, state,
capabilities, panels, reason, missing dependencies, entitlement expiry. Contract `0067`.

**D2. Every DECLARED plugin is reported, registered or not.** A plugin that failed entitlement
or has unmet dependencies appears with its state and the reason. This is the whole point of
distinguishing case (2) above: silence about a plugin the operator paid for is a support ticket
that starts with "the button isn't there."

**D3. The kernel still does not interpret any of it.** It reports what the plugin's manifest
declared, exactly as it already forwards capability strings (ADR-0082 D2). No plugin vocabulary
enters the OSS core. The mapping from `app.PluginStatus` into the operator plane's own
`PluginInfo` type lives in the composition root, so the operator package never learns how
plugins are composed.

**D4. The console pins per plugin, and compares on the major line.** `PINNED_PLUGIN_VERSIONS`
maps plugin id → the version this build's panels were written against. The verdict is one of
four values, not a boolean:

| Verdict | Meaning | Treatment |
|---|---|---|
| `aligned` | same version | no chrome at all |
| `minor` | differs below the major line | one quiet line |
| `major` | major differs | loud warning, surface still rendered |
| `unknown` | nothing pinned, or an unparseable version | one quiet line |

Collapsing these into a boolean either cries wolf on every point release or stays silent
through a breaking one. An unparseable version reports `unknown` rather than `aligned`: a
version the console cannot read is precisely when it must not claim compatibility.

**D5. The verdict is computed in the client's state of record, not the webview.** The pinned
table is a property of the compiled client; a projection that re-derived it could disagree with
the client it belongs to.

**D6. The banner is structural — every plugin surface gets it by construction.** Plugin panels
render inside a `PluginSurface` shell that owns both the skew banner and the absent state.
Wrapping is the entire opt-in: a new plugin panel gets the banner by existing, rather than by
its author remembering. This is the direct answer to "we want the skew banner to be there by
default for every plugin" — default-on is only true if it is impossible to forget.

**D7. Skew informs; it does not withhold.** A major-version banner still renders the panel. The
console tells the operator what it knows and lets them decide; hiding a surface because of a
version guess would be a worse failure than showing it with a warning.

## Consequences

**A whole class of silent wrongness becomes visible.** The specific case — kernel plugin ahead
of console panels, every RPC answering normally — previously had no detection at all.

**The absent state stops lying.** "This kernel does not run the reactive engine" and "the
reactive engine declined to register because its licence expired" are now different sentences.
The watch console in particular used to show an empty list with "watch configs will appear here
when the kernel advertises the watch capability", which described neither situation.

**A behaviour change: the watch console is now capability-gated.** It previously rendered
regardless and simply showed nothing. It now renders the absent explanation when
`watches-read` is not advertised. This is a correction — the console should not present a
plugin's surface against a kernel that has no such plugin — but it is a visible change, not
purely additive.

**Pinning is a maintenance obligation.** `PINNED_PLUGIN_VERSIONS` must be bumped when a console
surface is rebuilt against a new plugin major. A stale pin produces a false warning; a missing
entry produces `unknown`, which is honest but noisy if left. Both are visible failures, which is
the intended direction — the alternative is an invisible one.

**Contract 0067 is additive.** An older console ignores the new field and keeps its
contract-level skew banner. A newer console against an older kernel sees no plugins and says
exactly that, rather than assuming alignment.

## Risks and known gaps

**Versions are only as honest as the manifests.** All four shipped plugins currently declare
`1.0.0`, so the mechanism is untested against a real divergence in production; the comparison
logic is unit-tested against major, minor, unknown and unparseable inputs.

**The pinned table is compiled in, so a console cannot learn about a plugin it has never heard
of.** That is deliberate — the point is comparison against a known expectation — but it means a
third-party plugin's surface reports `unknown` forever. If plugin surfaces ever become
descriptor-driven (the `Panels` metadata already carried here is the seam), this should be
revisited.

**No aggregate view.** There is no single page listing every plugin and its state; the banners
are per surface. An operator whose plugin declares no panels has nowhere to see its state at
all. The data is on the wire, so this is additive when wanted.
