---
id: 0085
title: Access Policy — the Authorizer Port, and Moving the Decision Out of the Kernel
status: Proposed
date: 2026-07-26
supersedes: []
superseded_by: []
amended_by:
  - 0091-closed-tags-and-crippled-grants
depends_on:
  - 0034-tag-based-isolation
  - 0035-write-classification
  - 0039-tool-execution
  - 0046-skills
  - 0047-operator-transport-plane
  - 0057-open-core-boundary
  - 0073-premium-transport-plane-extension
  - 0074-plugin-architecture
  - 0082-additive-licensed-plugins
---

# ADR-0085: Access Policy — the Authorizer Port, and Moving the Decision Out of the Kernel

> **Amended-by: [ADR-0091](0091-closed-tags-and-crippled-grants.md)** — adds **closed tags** (deny-by-default per tag) and a deliberately crippled **grant** that reopens them. Every decision here stands: composition is still intersection, `ForbiddenTags` is still absolute, and closure enforces THROUGH that absoluteness rather than around it. What changes is that the model can now express *"only this path may reach this data"*, which a narrowing-only fold cannot.

## Status

Proposed — **implemented**. Extends ADR-0034 (tag-based isolation) from memory to all four
securable kinds, and relocates the whole decision mechanism out of the OSS kernel into a
premium plugin. ADR-0086 (tool effect classes) and ADR-0087 (policy composition) build on it.

Derived from `SCOPE-POLICY-SPEC.md` at the monorepo root, which remains the design rationale
of record. Where this ADR and that spec differ, the differences are called out in
"Deviations from the spec" below and this ADR wins.

## Context

ADR-0034 gave memory a three-set opaque-tag boundary that is correct, fail-closed, and
battle-tested. It also left four problems.

**One: only two of the four resource kinds were properly governed.** Memory was tag-governed;
skills reused the memory read path (ADR-0046 D9). Agents carried a `ScopeProfile` field that
nothing read. Tools had `PolicyConfig.ToolAllowList`/`ToolDenyList` — flat name lists,
structurally separate from scope, and dead code by the time this work started.

**Two: the mechanism lived in the OSS kernel.** `domain/scope.go`, `internal/scope/`, and
`internal/centralexec/scope_gate.go` shipped in `cambrian-core`, even though multi-tenant
isolation is explicitly a premium concern and the OSS product is single-tenant and unscoped.

**Three: the neutralisation was in the wrong place.** `execution.scope_enforcement_enabled`
(default false) turned the CHOKEPOINT off, not the policy. An unscoped deployment therefore
had no gate at all rather than a permissive one — and the flag had to be off, because the
query path additionally applied a hardcoded ownership ACL (`source_agent_id == callerID`)
that made agents unable to read operator-ingested documents.

**Four, and worst: silence.** `ScopedVectorStore` failed closed and silently. An unrecognised
principal got zero results and no error. Ingest succeeded, queries returned nothing, and
nothing anywhere reported a problem. ADR-0083 D6 already named this the single most silent
failure mode in the system.

## Decision

### D1 — One algebra, four resource kinds

The three-set opaque-tag model is the universal predicate for memory, skills, agents, and
tools. `TagPredicate.Allows(tags)` is the authoritative row-level test for every kind. What
differs per kind is only which tags the resource presents, expressed as `domain.Taggable`.

### D2 — What "the resource's tags" means per kind

| Kind | Tags come from |
|---|---|
| Memory | `metadata.tags` on the document (unchanged) |
| Skill | `Skill.ScopeTags` (unchanged, ADR-0046 D9) |
| Agent | **new** `AgentDefinition.ClassificationTags` — what the agent IS, so policy can permit or deny INVOKING it |
| Tool | **new** `SystemTool.ClassificationTags` — what domain it touches (`crm`, `filesystem`, `payments`) |

`Agent.ScopeProfile` and `Agent.DefaultWriteTags` are **deleted**. They were read by nothing:
their authoritative home was always the `agent_scopes` table, and carrying a second copy on
the record invited exactly the drift the ADR-0034 R1 fix was about.

### D3 — The kernel APPLIES a predicate; it never COMPOSES one

Two data types, in OSS, with no policy semantics:

- `domain.TagSet` — an authored three-set term, carried on a session or an agent record. The
  kernel stores and transports it; only the decision point gives it meaning.
- `domain.TagPredicate` — the computed, ready-to-apply form (required / CNF any-of /
  forbidden / bypass), which both an in-memory store and the pgvector SQL builder apply.

Applying is enforcement, and enforcement stays in the kernel. Composing — intersection,
precedence, inheritance, vocabulary validation, principal resolution — is the decision and
lives in the plugin. This is the Windows split made concrete: `AccessCheck` is in the NT
kernel and cannot be replaced; the DACL is data that arrives from outside.

`ScopeSystem` keeps its name and stays in OSS as `&TagPredicate{Bypass: true}`, so INV-7's
"one grep enumerates every kernel-internal bypass" survives.

### D4 — Skills are privilege bundles, and must intersect

A skill is gated on retrieval by its tags, and loading it activates `ToolGrants`. Unintersected,
that makes skill VISIBILITY a privilege-granting operation and silently turns the skill tag
vocabulary into a tool-permission vocabulary.

`ToolExecutor.ConferSkillGrants` now intersects, per tool, with what the decision point
permits. A denied tool is CLIPPED: the rest of the skill activates, a `skill_grant_clipped`
decision is emitted, and the agent is told the effective grant list rather than what the skill
wished for. Denying the whole skill makes the system feel broken; granting the tool is a hole.

The intersection is against POLICY, not against the agent's static grants. ADR-0046 D6 —
an operator-authored system skill MAY confer a tool the agent has no standing grant for —
survives intact. Policy is the ceiling; the static grant list is not. The ADR-0051 D6
`RestrictedTools` ceiling (the Scout's confinement) outranks any skill.

### D5–D6 — Policy composition

See ADR-0087.

### D7 — The surface clamp (loopback)

`EffectiveScope = caller ∩ agent ∩ surface`. The surface is the entry point a request arrived
through, and it can clamp what may be done regardless of who is asking — the Windows loopback
analogue. An outsider-facing chat ingress carrying `ForbiddenTags: [internal_only, secrets,
PII]` cannot reach internal knowledge even if identity resolution is wrong and even if group
membership is misconfigured. That is what makes a customer-facing surface shippable before
multi-tenancy is solved.

**The surface is established by the kernel from the transport**, by a gRPC interceptor keyed
on the service being invoked, and is never read from a request payload. A session's RECORDED
surface (`domain.Session.Surface`, written once at creation) overrides the transport-derived
one, because it is the narrower fact: a conversation opened on an outsider ingress stays an
outsider conversation even when a later turn arrives over an internal path.

### D8 — Explainability ships with the mechanism

`domain.AccessDecision` carries `Allowed`, `Reason` (a ten-value controlled vocabulary),
`Detail` (the SPECIFIC tag/clause/effect responsible), `DecidedBy` (which policy, linked
where, contributed which term), and `PolicyVersion`. Two surfaces consume it:

- `ExplainAccess` on the operator plane — "why can/can't this principal see this?", answered
  without performing the access. The `gpresult` analogue.
- **Empty-result annotation** — `QueryMemoryResponse.policy_note` and the agent plane's
  `MemoryResponse.policy_note`. A policy-caused zero-row result carries its reason, so an
  agent can say "I am not permitted to see that" instead of "I found nothing". The note is
  emitted only when policy actually shaped the outcome; annotating every response trains
  callers to ignore the field.

### D9–D10 — Report-only and What-If

See ADR-0087.

### D11 — Controlled vocabulary, enforced at write and authoring time

Tags are validated against the deployment's vocabulary on the write path and at policy save
time, and listed by `ListClassificationTags` so an admin UI offers SELECTION. A free-text tag
field is a defect: a typo is the primary route to a scope that silently matches nothing.

### D12 — Time-bound grants: the seam now, the feature later

`PolicyObject` and `Link` carry `ExpiresAt` and `GrantedBy` from v1. Retrofitting
time-bounding into a grant model that assumes permanence is expensive; carrying two fields is
free. Expiry IS honoured at resolution — an expired policy contributes nothing at all,
including its denies, because an expired deny is not a deny that was removed, it is a policy
that stopped existing.

### D13–D14 — Audit and authoring-time unsatisfiability

See ADR-0087.

## The OSS / premium split

In XACML terms: the **PEP** (enforcement points) is in the OSS kernel and is **not
pluggable**; the **PDP/PAP/PIP** (decision, administration, policy store) are the premium
plugin.

| Component | Lives in |
|---|---|
| `domain.Authorizer` port + `AllowAllAuthorizer` | OSS |
| Every enforcement call site | OSS |
| `domain.Taggable`, `TagSet`, `TagPredicate`, `ScopeSystem` | OSS |
| `internal/authz/` — read/write chokepoints, artifact filters, surface interceptor | OSS |
| Tag algebra, `ScopeResolver`, vocabulary, write classification, promotion, `agent_scopes` | premium `authz` |
| Groups, policy objects, links, precedence, journal, simulation | premium `authz` |
| Policy administration RPCs | premium proto plane (ADR-0073) |

**This is consistent with ADR-0082's "the security kernel is never pluggable", not an
exception to it.** The enforcement points are in the kernel and cannot be replaced. What is
pluggable is the DECISION — which is data, not enforcement.

**The direction of failure inverts at the seam, deliberately.** The OSS kernel fails **open**:
unrestricted is the correct and only semantics for a single-tenant open-source deployment,
and `AllowAllAuthorizer` is the right answer rather than a stub. The plugin fails **closed**:
an unresolvable principal is denied. Getting this backwards makes OSS unusable or premium
insecure, so both halves are tested explicitly and named as such.

## Consequences

### `execution.scope_enforcement_enabled` is gone

The chokepoints now run unconditionally and only the answer varies. The old flag disabled the
gate itself, which meant an unscoped deployment had no enforcement point to reason about at
all — the opposite of what a security design wants from its off switch.

### The hardcoded ownership ACL is deleted

`aclAllows` (`source_agent_id == callerID`, three call sites in `internal/memory/query.go`) is
removed from OSS. It was already inert on the default path. Ownership-based visibility is now
expressible declaratively as `RequiredTags: ["provenance:source=<agent>"]` — the write path
already stamps that tag — which is both the Zero-Hardcode-correct form and something an
operator can see and change.

**This is a behaviour change for a deployment that previously ran with scope enforcement on.**
Such a deployment must add the equivalent provenance policy if it relied on per-agent
ownership isolation.

### The unclassified-write warning moved to the decision point

Only the PDP can tell a classification tag from a provenance stamp, so it emits the "you
narrowed yourself to nothing" warning. The kernel keeps the coarser "this write carries no
classification at all".

### Contract

Operator contract **0065 → 0066**: `ExplainAccess`, `ListClassificationTags`,
`AccessDecisionOp`, `PolicyContributionOp`, `QueryMemoryResponse.policy_note`, and
`ToolOp.{classification_tags,effects,effects_inferred}`. Capability string `access-policy`,
advertised by the plugin manifest. The agent plane gained `MemoryResponse.policy_note`.

**Known skew:** `ui/proto/operator.proto` (pinned at 0047) and `cli/proto/operator.proto` were
not re-vendored — they were already many revisions behind before this change, and re-vendoring
them is its own piece of work. The SDK's generated `cambrian_pb2` was not regenerated either;
`policy_note` is an additive field that old clients ignore.

## Deviations from the spec

**`ScopedVectorStore` stays in the kernel.** `SCOPE-POLICY-SPEC.md` §5.2 lists it among the
files moving to the plugin, while §4.1 says the enforcement points must be in the kernel
because "if enforcement points were pluggable, a missing plugin would mean unguarded resource
access — the exact failure the design exists to prevent". Those two statements conflict.
§4.1 wins: the decorator IS the call site that asks. It is renamed
`internal/authz.EnforcingVectorStore` and consults the `Authorizer`; the POLICY it used to
carry is what moved.

**Question 1 (§6.3) — the agent's own scope stays on the agent, as a floor.** Resolved per the
spec's own recommendation: the resolver's per-agent scope is folded last and no policy can
widen it.

**Question 2 — an unclassified tool.** See ADR-0086; the answer is "registration fails", but
gated behind a migration flag because the shipped tool manifests live outside this repo.

**Question 3 — report-only is per-policy.** As recommended. A global lever exists too, but as
the operator's coarse "stop enforcing now", not as the rollout mechanism.

**Question 4 — the vocabulary is a deployment input**, not a shipped list: the plugin reads
`CAMBRIAN_CLASSIFICATION_VOCABULARY` from the premium environment (ADR-0057 D5 forbids the OSS
config schema naming a premium feature). An empty vocabulary disables the coinage check and
leaves every other rule in force. Curating a starter vocabulary is a product decision left
open.

## Invariants

- **INV-1 — Intersection only.** No code path widens an effective scope. Property-tested over
  3000 random scope sets in `authz/scope_test.go`, and again over the container hierarchy.
- **INV-2 — Deny is absolute.** A forbidden tag cannot be removed by any policy, link,
  ordering, or flag. It even survives Block Inheritance.
- **INV-3 — No silent empties.** Any policy-caused zero-result carries a reason.
- **INV-4 — No tattooing.** Policy is evaluated at decision time and never written into
  resource state. Removing a policy fully restores prior behaviour.
- **INV-5 — Kernel decides identity.** Principal and surface come from the authenticated
  session or the transport, never from a request payload or a daemon's claim.
- **INV-6 — OSS has no policy.** `grep ScopeConfig cambrian-core` returns nothing;
  `check-no-premium.sh` passes.
- **INV-7 — `ScopeSystem` stays greppable.**

## Risks and known gaps

**The surface clamp is only as trustworthy as the transport.** Deriving the surface from the
gRPC service is sound on localhost, where the transport is the process boundary. On a
remotely-reachable deployment it holds only once **SEC-03** (TLS + client authentication on
the operator plane) lands — until then an attacker who can reach the port can present on
whichever plane they like. SEC-03 is open; `internal/authz/surface.go` says so in the code,
and the clamp must not be treated as a security boundary on a remote deployment before it.

**Persistence.** Groups, policy objects, and links live in an in-process store. Agent scopes
and write tags are durable (Postgres `agent_scopes`); policy composition is not yet. A restart
loses authored policy. The store is behind an interface, so a Postgres-backed implementation
is additive.

**No benchmark.** Access control has no benchmark suite and the DDD mandate's step 6 applies:
the closest proxy is the retrieval suites, which this change must not move (it is behaviour-
preserving on the OSS default path). A `policy` suite — deny/allow fixtures, an escalation
corpus, and an explanation-completeness check — is the honest follow-up.

## Alternatives considered

**Keep the algebra in OSS, gate it with a flag.** This was the status quo and it produced the
exact failure this ADR exists to remove: an off switch that removed the chokepoint rather than
the policy.

**A `RowFilter` interface instead of a `TagPredicate` struct.** Purer — the kernel would hold
no tag semantics at all — but the pgvector adapter needs to push a predicate into SQL, and an
interface returning goqu expressions would leak infrastructure into the domain port. The
struct is data the kernel applies without interpreting, which is the same relationship the NT
kernel has with a DACL.

**Zanzibar-style relationship tuples.** The right architecture for per-object sharing ("this
document, with this person"). That is an explicit non-goal for v1 and a much larger piece of
work; the container model covers the org-chart shape the product actually needs.
