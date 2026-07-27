---
id: 0091
title: Closed Tags and the Crippled Grant — Adding Deny-by-Default Without Breaking Narrowing
status: Proposed
date: 2026-07-27
supersedes: []
superseded_by: []
amends:
  - 0085-access-policy-port-and-extraction
  - 0087-policy-composition-groups-links
depends_on:
  - 0034-scope-and-classification
  - 0085-access-policy-port-and-extraction
  - 0086-tool-effect-classes
  - 0087-policy-composition-groups-links
  - 0090-ingress-and-surface-identity
---

# ADR-0091: Closed Tags and the Crippled Grant

## Status

**Proposed — implemented and live-validated.** Both directions of the airline isolation are
enforced against a running kernel.

## Context

### What the model could not say

ADR-0087 made every fold an **intersection**. A policy can only ever take access away, which
buys a genuinely valuable property: order affects the explanation and never the outcome, so
there is no precedence puzzle and no "which rule won" debugging.

It also means the model cannot express *"only this path may reach this data"*. There is no
baseline to carve an exception out of:

- An unprofiled principal is **unrestricted** (ADR-0034 D8) and OSS fails open by design.
- A `ForbiddenTags` broad enough to cover everyone also covers the one principal you meant to
  allow, because forbidden is **absolute** and deliberately has no exception mechanism.

The concrete request that exposed this: *make the airline tools reachable only from the airline
ingress.* The reverse direction — from the airline surface, reach nothing but airline — was one
policy (`RequiredTags: ["airline"]` at `surface:airline`) and worked immediately. The forward
direction had no expression at all.

The workaround inside the shipped model is a `ForbiddenTags` linked to a group containing every
*other* principal. It works, and it rots: every new agent must be added to that group or it is
unrestricted. Fail-open, maintained by hand, degrading as the roster grows.

### The naming was already telling us

The field is called `classification_tags`, borrowed from the MLS tradition. But in that
tradition a *classification* is a **clearance you must hold**, not a label you avoid. Cambrian
had implemented only the avoid-half. This ADR makes the name honest.

### Prior art — the narrowing-only model is the unusual one

Default-deny with explicit grants is the dominant practice, not an exotic choice:

| System | Shape |
|---|---|
| **AWS IAM** | Implicit deny → `Allow` grants → explicit `Deny` overrides everything. |
| **AWS SCPs** | A ceiling that only narrows and never grants — **this is ADR-0087's model**. AWS ships both layers and keeps them separate, with effective access as the intersection. |
| **Kubernetes NetworkPolicy** | A pod is open until a policy *selects* it, then deny-by-default for that direction. Structurally identical to "a tag is open until declared closed". |
| **Kubernetes RBAC** | Default-deny, grant-only; deny rules deliberately rejected to keep evaluation predictable. |
| **Zanzibar / SpiceDB / OpenFGA** | Purely grant-based — a check is false unless a relationship path exists. |
| **Bell-LaPadula / MLS** | A classification level is inherently closed; you need clearance ≥ label. |
| Firewalls, CSP, OAuth scopes | `-P DROP`, `default-src 'none'`, empty-scope tokens. |

AWS is the instructive one: they needed **both** directions and kept them as separate layers.
That is a decade-old confirmation that a ceiling alone cannot express "only X may reach Y".

## Decision

### D1 — A tag may be declared CLOSED

A closed tag is deny-by-default: no principal reaches it unless a policy grants it. Declared
via `CAMBRIAN_CLOSED_TAGS` in the premium environment (the OSS config schema must not name a
premium feature — ADR-0057 D5), and every closed tag must be in the controlled vocabulary, so
closing a typo cannot deny-by-default a tag nothing carries.

### D2 — There is NO global default-deny switch

Closure is opt-in per tag, forever. ADR-0085 names SELinux as the cautionary tale — a correct
policy system that operators switch off — and the mitigation is that a deployment which closes
nothing behaves **exactly** as it did before. No migration, no surprise, nothing to disable.

### D3 — Grants are deliberately crippled

`ScopeConfig.GrantedTags` reopens a closed tag for principals in the container the policy is
linked to. It is the only term in the model that adds access rather than removing it, and it is
restricted on purpose:

- **A grant may name nothing but a closed tag.** A grant on an open tag confers nothing, so it
  is refused at write time rather than stored as a policy that reads like it gives access and
  does not.
- **A grant can never override a bound.** If a tag is both granted and forbidden, the forbid
  survives.
- **Grants union; everything else intersects.** Reaching a closed tag through any applicable
  container is enough. Union is also order-independent, so ADR-0087's "order affects the
  explanation, never the outcome" property survives intact.

The crippling is the whole safety argument. General-purpose grants are where these models rot:
somebody writes a broad allow and the narrowing guarantees quietly stop meaning anything. Here
**the blast radius of introducing grants is exactly the set of tags someone deliberately
closed.** Nothing that was not closed can be widened by any grant, ever.

### D4 — Closure enforces through the existing absolute forbid

No new concept enters the kernel. At resolve time, every closed tag NOT granted to this
principal is added to `ForbiddenTags`:

```
effective.ForbiddenTags ∪= closedTags − grantedTags(principal)
```

`domain.TagPredicate` is unchanged. The invariant that `ForbiddenTags` is absolute is *literally
the mechanism* that makes closure hold, and it is why D3's "a grant cannot override a bound"
falls out for free rather than needing enforcement: closure only ever adds to the forbidden set.

### D5 — A closed-tag denial must explain itself

Because enforcement runs through `ForbiddenTags`, the raw answer is `forbidden_tag` for a tag
**nobody forbade** — true and useless, since the operator wrote no such policy. The decision
detail instead reads *"tag `airline` is closed (deny-by-default) and no policy grants it to this
principal"*, and `AccessDecision.GrantedTags` records what policy did reopen.

This is ADR-0085 D8's rule applied to a new denial kind: a fail-closed model that cannot say
*why* turns a misconfiguration into an unexplained empty result.

## Consequences

**The airline case is now expressible in both directions**, verified live:

| | |
|---|---|
| from `chat:airline` → another MCP tool | denied, `missing_required_tag airline` (D6 clamp, ADR-0087) |
| from `chat:airline` → untagged tool / internal memory | denied |
| `scout_agent` on the agent surface → `[airline]` | denied, *closed and no grant reaches you* |
| `scout_agent` → `[web]` | allowed — closure touches only the closed tag |

**The enumeration burden disappears.** A newly registered agent nobody profiled reaches a closed
tag **never**, instead of by default. That is the difference between a boundary that is
maintained and one that holds.

**MCP tools became taggable** as a prerequisite (ADR-0085 D2 extension): a remote server's tools
are discovered dynamically, so without operator-set `classification_tags` they arrive untagged —
and an untagged resource has no tags for any predicate to act on. The boundary was inexpressible
regardless of the policy written.

**Cost is one env var, one field, one resolve-time step.** No kernel change, no contract change
on the OSS plane; `granted_tags` is additive on the premium admin plane.

## Risks and known gaps

**Operators are NOT exempt from closure, deliberately.** A human at the console asking for a
closed tag is denied like anything else, and the way to change that is to author a grant linked
at `surface: operator` — which an operator can do, because they hold the administration plane.
Verified end to end: denied, grant linked, allowed, grant unlinked, denied again, with an agent
on another surface unaffected throughout.

The alternative — exempting the operator surface in the resolver — was rejected. It would be an
implicit bypass in a design whose rule is that the only bypass is the single greppable
`ScopeSystem` one, and it is **role-blind**: the authorizer cannot see roles, so exempting the
surface would hand a Viewer (who cannot mutate anything) read access to closed data. Explicit
and auditable beats convenient and invisible.

Residual: a newly closed tag is not automatically granted anywhere, so closing a tag can lock
operators out of it until a grant is authored. That is the intended direction of failure, but it
should be visible rather than discovered.

**A granting direction now exists.** It is crippled (D3) and cannot touch anything unclosed, but
the mechanism is there and future work must keep it that way. The review question for any change
here: *can this grant affect a tag nobody closed?* If yes, reject it.

**Closure is per-tag, so granularity is the tag.** Sub-tag scoping (this document, this person)
is Zanzibar's problem, and ADR-0085 already rejected relationship tuples as an explicit v1
non-goal. Unchanged.

**Two implementation traps, recorded because both were silent:**

- `ScopeConfig.IsZero()` predated grants, so a grant-only policy read as empty and was dropped —
  stored, listed in the console, never applied. Any new term must be added to `IsZero`.
- The plugin's `Build` phase reconstructed the `Vocabulary`, discarding the closed set from
  `New`. The boot log announced `closed=[airline]` while the decision point allowed everything.
  **A security bug that reports success is the worst kind**, and the only reason it was caught is
  that behaviour was checked rather than the log believed. The regression test for it needs a
  real database, because `Build` returns early without one — the first version of that test
  passed against the bug it was written to catch, and mutation-checking is what exposed it.

**Destructive integration tests now refuse a live database.** Separately but from the same
session: the Postgres integration tests `TRUNCATE` the authz tables, were pointed at the live
database, and destroyed a real enforcing policy — twice. The guard is structural rather than
procedural: the DSN must name an obviously disposable database, or the test skips with the
reason. Same principle as the standing rule that a benchmark must never reset the shared store.
