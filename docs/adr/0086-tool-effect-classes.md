---
id: 0086
title: Tool Effect Classes — What an Invocation Does, Not Just What It Is About
status: Proposed
date: 2026-07-26
supersedes: []
superseded_by: []
depends_on:
  - 0039-tool-execution
  - 0043-mcp-integration
  - 0051-scout-discovery
  - 0085-access-policy-port-and-extraction
---

# ADR-0086: Tool Effect Classes

## Status

Proposed — **implemented**, with the strict-registration half behind a migration flag (see
Migration).

## Context

A classification tag answers *what is this about*. It cannot answer *what am I doing to it*.
Reading a CRM contact and deleting one carry the same tag and vastly different risk, so a
tag-only model either over-restricts (deny `crm` entirely) or under-restricts (permit `crm`
and hope).

Tools previously had two partial answers to this. `SystemTool.Dangerous` gated approval, but
it is one bit and it is about human confirmation, not about policy. `DataReadKinds` /
`DataWriteKinds` (ADR-0039 D8 Regime 1) distinguished read from write, but only for tools that
touch the tagged stores — so a tool that touches no store at all was ungoverned, and
"no tool may transmit outside this network" was not expressible.

That last sentence is the one a sovereign-deployment customer cares most about, and it is the
one worth putting in front of a security reviewer.

## Decision

### D1 — A closed set of effect classes

```go
type ToolEffect string

const (
    EffectRead   ToolEffect = "read"   // observes state, no mutation
    EffectWrite  ToolEffect = "write"  // mutates internal state
    EffectEgress ToolEffect = "egress" // transmits data outside the deployment
    EffectSpend  ToolEffect = "spend"  // incurs cost or moves money
    EffectAdmin  ToolEffect = "admin"  // alters the system's own configuration
)
```

**Closed, not an open namespace, and no wildcards.** Azure RBAC's `Actions` strings are
powerful and are how people accidentally grant far more than they intended. Five classes fit
on one line and can be reasoned about exhaustively.

### D2 — Both checks must pass

A tool invocation is permitted only if the tag predicate admits it AND every effect it declares
is granted. The effect gate applies to EVERY tool, not only the tagged-store ones — otherwise
"no tool may transmit outside this network" would silently exempt tools that touch no store,
which is most of the interesting ones.

An operator `ScopeSystem` execution (ADR-0047 A2.2) is not effect-gated, the same carve-out
the grant and data-store regimes already make. The resource-arg policy and process confinement
still apply.

### D3 — Grant broadly, subtract narrowly

`EffectGrant{Allow, Deny}`: an empty `Allow` permits everything, `Deny` always wins. This is
Azure's `NotActions` shape, and it is what makes an action-based policy usable at scale —
enumerating every permitted action is not something anyone maintains. Deny-wins is also
consistent with `ForbiddenTags` being absolute (INV-2).

Composition follows the tag rule: denies UNION, allows INTERSECT, so adding a policy can only
narrow (INV-1).

### D4 — A tool that declares no effects is a registration error

Fail closed. An unclassified tool is not an unrestricted tool.

### D5 — …but absence is a MIGRATION state, and invalidity is a bug

An effect outside the closed set is **always** fatal: silently dropping an unrecognised effect
would let a policy that denies it permit the very call it named.

Absence is different. The shipped `tools/*tool.py` manifests are distributed outside this
repository, so making D4 unconditional would refuse every tool in every existing install on
upgrade — strictly worse than the status quo it replaces. So:

- Effects absent ⇒ **inferred deterministically** from what the manifest already states:
  `data_write_kinds` / `command_args` / `dangerous` ⇒ `write`; `url_args` ⇒ `egress`; always
  `read`. Inference never invents `spend` or `admin` — those are claims only a manifest can
  make, and guessing them would be worse than omitting them.
- The result is marked `EffectsInferred` and surfaced on `ToolOp.effects_inferred`, so the
  set of un-migrated tools is enumerable rather than a matter of belief.
- `execution.tool_effects_strict` (default **false**) makes absence fatal. An operator flips
  it once the catalog reports no inferred tools, after which an unclassified tool cannot ship.

A declared set always beats inference and is never augmented by it: a manifest saying
`["read","spend"]` on a `dangerous` tool means exactly that.

### D6 — The registry is the normalization chokepoint

`InMemoryToolRegistry.Register` validates and normalizes, so no path — discovery, MCP
re-sync, a plugin's `AddMCPServer`, a hand-written test — can put an unclassified tool in
front of the executor. Discovery validates FIRST (that is where the deployment's strictness is
known) and the registry validates again.

Because validation runs twice, it must be **idempotent**: `EffectsInferred` is preserved on a
second pass, never cleared. An earlier revision cleared it, which quietly emptied the
migration checklist while appearing to work.

## Consequences

- `SystemTool` gains `ClassificationTags`, `Effects`, `EffectsInferred`; `TOOL_MANIFEST` gains
  `classification_tags` and `effects`.
- `PolicyConfig.ToolAllowList` / `ToolDenyList` are **deleted** rather than shimmed. They were
  dead code — declared in `domain/scope.go` and referenced by nothing — so the compatibility
  shim `SCOPE-POLICY-SPEC.md` §5.1 asks for would have been a shim over an empty set.
- `ToolOp` gains the three fields; contract 0066 (ADR-0085).
- A tool with an unrecognisable effect is refused registration and is then simply "unknown
  tool" at call time — an honest downstream answer.

## Migration

1. Ship with `tool_effects_strict: false` (the default). Everything works; inferred tools are
   logged once at boot with their names and flagged in the catalog.
2. Add `"effects": [...]` to each manifest, checking the inferred value first — it is usually
   right, and where it is wrong it is wrong by being too permissive, which is the direction
   that shows up as a working tool rather than a broken one.
3. When `ListTools` reports no `effects_inferred`, set `tool_effects_strict: true`.

## Risks

**Inference can under-classify.** A tool that transmits without declaring a `url_args` field —
one that reads an endpoint from its own config, say — infers `read` and escapes an egress
denial. This is the reason strict mode exists and the reason the inferred set is surfaced
rather than silently applied. A deployment that actually needs the egress guarantee should run
strict.

**No `spend` or `admin` without a declaration.** Deliberate: inference does not guess the two
classes whose consequences are financial or configurational.
