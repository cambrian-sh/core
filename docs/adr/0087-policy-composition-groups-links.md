---
id: 0087
title: Policy Composition — Groups, Policy Objects, Links, and Deterministic Precedence
status: Proposed
date: 2026-07-26
supersedes: []
superseded_by: []
amended_by:
  - 0091-closed-tags-and-crippled-grants
depends_on:
  - 0034-tag-based-isolation
  - 0071-watch-observability
  - 0073-premium-transport-plane-extension
  - 0085-access-policy-port-and-extraction
  - 0086-tool-effect-classes
---

# ADR-0087: Policy Composition — Groups, Links, and Precedence

> **Amended-by: [ADR-0091](0091-closed-tags-and-crippled-grants.md)** — adds **closed tags** (deny-by-default per tag) and a deliberately crippled **grant** that reopens them. Every decision here stands: composition is still intersection, `ForbiddenTags` is still absolute, and closure enforces THROUGH that absoluteness rather than around it. What changes is that the model can now express *"only this path may reach this data"*, which a narrowing-only fold cannot.

## Status

Proposed — **implemented** in `cambrian-premium/authz`. Depends on ADR-0085 for the port and
the OSS/premium split; this ADR is only about how a decision is COMPOSED.

## Context

ADR-0085 established that the decision is data supplied from outside the kernel. It did not
say how an administrator authors that data.

The naive answer — assign a scope to each principal — does not survive contact with an
organisation. Administering 500 individuals is not a product; administering 12 groups is.

Microsoft solved this once, well, and the parts that survived forty years are the parts worth
copying. Group Policy objects are authored once and **linked to containers** (Site, Domain,
OU) rather than assigned to users; principals inherit every policy linked at or above their
container; processing order is named and predictable; **Block Inheritance** and **Enforced**
are two escape hatches that cover almost every real org-chart exception; and `gpresult` reports
the resultant set of policy *and which GPO won each setting*.

The parts not worth copying are equally clear: setting sprawl (thousands of knobs), tattooing
(settings that persist after the policy stops applying), and slow opaque convergence (a change
may take 90 minutes and you cannot tell whether it has landed).

## Decision

### D1 — Four concepts

```
Principal    — a user, an agent, or a daemon/surface identity (domain.PrincipalRef)
Group        — a named set of principals; nestable; a principal may be in many
PolicyObject — a named, versioned bundle of rules (the GPO analogue)
Link         — attaches a PolicyObject to a container
```

A `PolicyObject` knows nothing about who it applies to. That is the `Link`'s job, and keeping
them separate is the single most important structural idea in the model.

### D2 — Precedence, stated once

Containers, in application order, broadest first:

```
Organisation  →  Group (outermost to innermost)  →  Principal  →  Surface clamp
```

then intersected with the agent's own intrinsic scope, which is an irreducible floor no policy
can widen.

Five rules, and there is no second place where they are written down:

1. **Later links narrow further.** Every fold is an intersection, so "later wins" cannot
   widen. This is a deliberate simplification over GPO, where later genuinely overwrites: GPO
   buys flexibility and pays for it in precedence bugs that are silent and severe.
2. **`ForbiddenTags` accumulate by union and are never removable.** No link, no Enforced flag,
   no ordering removes a forbidden tag once contributed.
3. **Block Inheritance** on a container stops accumulation from above — *except* for
   `ForbiddenTags` and *except* for links marked Enforced. A blocked policy therefore
   contributes only its denies: restriction survives a block, permission does not.
4. **Enforced** on a link means a downstream Block Inheritance does not apply to it.
5. **Ties are impossible by construction.** The fold is commutative for Required and
   Forbidden; AnyOf clauses append in order and are AND-composed, so order affects only the
   EXPLANATION, never the outcome.

Effect grants follow the same shape: denies union, allows intersect, and a blocked link
contributes only its denies.

### D3 — One resolution pass

`PolicyStore.applicable(principal, surface)` resolves the ordered, block-decided link set
once; both the tag terms and the effect grants read it.

This matters more than it looks. An earlier revision derived effects from the tag
*contributions*, which silently dropped any policy carrying effects and no tag rule — and
"no tool may transmit outside this network" is exactly such a policy. Sharing the pass makes
it structurally impossible for a policy to contribute one half and not the other.

### D4 — Nested groups, cycle-safe at write AND at read

Membership is transitive: a member of a subgroup is a member of every ancestor. Cycles are
rejected at `SaveGroup` with the offending path in the error, because a cycle is an authoring
mistake and the administrator should hear about it while they still remember making it.

Resolution ALSO defends with a visited set. The store may be loaded from a database written by
an older version or edited by hand, and a hang is a worse failure than a rejected write.

### D5 — Authoring-time unsatisfiability (spec D14)

`SavePolicy` refuses a rule that can never match anything (`RequiredTags ∩ ForbiddenTags ≠ ∅`,
or a fully-forbidden AnyOf clause) and refuses a tag outside the controlled vocabulary. An
administrator who creates a policy that can never match must be told at save time, not
discover it through an empty result three days later.

Composition can still produce an impossible predicate from individually-valid policies — that
is undecidable to prevent and is exactly how the zombie boundary arrives in practice — so
`ResultantPolicy` reports `unsatisfiable_reason`, and every decision computed against one
carries `ReasonUnsatisfiablePolicy`. It is a SAFE state (zero rows) and therefore the easiest
one to mistake for "there is no data", which is why it is stated rather than implied.

### D6 — Report-only, and what makes it a rollout tool

`PolicyObject.Mode` is `enforced` or `report_only`. A report-only policy contributes nothing
to the enforced predicate — but the decision point ALSO resolves a **shadow** predicate that
includes it, and journals the difference as `would_have_denied`.

Without the shadow pass, report-only would be an inert setting rather than a rollout tool.
With it, an administrator turns policy on in report-only, watches what it would have blocked,
confirms the blast radius, then flips to enforced — the same trust story the reactive engine's
backtest already sells, and the product's core promise (*prove it before you switch it on*)
applied to policy.

A global `SetReportOnlyAll` lever exists as the operator's coarse "stop enforcing now". It is
deliberately separate from per-policy mode: global is too blunt to roll out with.

### D7 — What-If simulation

`Simulate(draft, limit)` replays journalled decisions through a **clone** of the live store
with the draft applied, and reports `NewlyDenied` / `NewlyAllowed` / `Unchanged` with a reason
per changed row.

Three properties make it trustworthy: it uses a real `Authorizer` rather than a
re-implementation of the fold (a simulator that disagrees with the enforcer is worse than no
simulator); the clone is deep, so a draft can never mutate production; and the simulation is
not itself journalled, because asking the question must never be the thing that changes the
answer.

`NewlyAllowed` is reported even though an intersection-only model can only reach it by
REMOVING a policy — a non-zero value means the draft deletes or relaxes something, which is
worth seeing explicitly rather than inferring.

### D8 — Audit

Every decision is journalled with principal, surface, resource, outcome, reason, contributing
policies, policy version, and timestamp. Allows may be sampled at volume; **denials are never
sampled** — and neither is a report-only `would_have_denied` allow, because those rows are the
entire output of a rollout.

### D9 — Administration lives in the premium proto plane

Groups, policies, and links are administered through `AccessPolicyAdmin` in
`cambrian-premium/api/proto/authz`, mounted via the ADR-0073 `ExtraServices` seam behind the
same operator auth interceptors. The pinned OSS operator contract carries only what the KERNEL
must be able to answer — `ExplainAccess` and the vocabulary listing.

Authoring errors (a group cycle, an unsatisfiable rule, a coined tag) come back as inline
response FIELDS rather than gRPC errors: "you created a loop, here is the path" is a normal
authoring outcome a UI renders next to the form, not an exceptional condition. A dangling
link IS a transport error, because there is nothing for the administrator to look at.

## Consequences

**No tattooing (INV-4).** Policy is evaluated at decision time and never written into resource
state. `DeletePolicy` removes the policy and every link to it, and prior behaviour is fully
restored — tested.

**Convergence is observable.** Every write bumps a snapshot version that every decision
carries, so "has my change landed?" is answerable, unlike GPO's 90-minute refresh.

**The vocabulary stays small.** If the tag vocabulary grows past what one page can list,
something has gone wrong — that is the setting-sprawl failure this design is explicitly
avoiding.

## Risks and known gaps

**Persistence — closed by amendment (D9).** Groups, policies and links are durable in
Postgres (`authz_groups` / `authz_policies` / `authz_links`, created on first use by the
plugin). The in-memory map remains the READ model — a decision fronts every retrieval and must
not cost a round-trip — and Postgres is the backing:

- **Write-through, durable first.** A mutation persists BEFORE the in-memory model moves. The
  inverse order would show an administrator a policy that is enforced now and gone after a
  restart, which is the worst possible failure for a subsystem whose product is trust. A failed
  write therefore surfaces as a failed write, not as a phantom policy.
- **Validation still precedes storage,** so an unsatisfiable rule never reaches the database.
- **Reload replaces, never merges.** Policy is small and changes are rare, so a wholesale swap
  has no partial-application failure mode; an incremental merge would reintroduce exactly the
  class of bug — a stale term nobody can see — that this ADR exists to eliminate.
- **Cross-replica propagation** rides `LISTEN/NOTIFY` on `authz_policy_changed`, so a policy
  authored on one node lands on the others without a restart. If the subscription cannot be
  established the replica logs and serves its loaded snapshot: stale-but-consistent, never
  half-applied. **Amended 2026-08-11 — the sentence above was true only of the FIRST attempt;
  see "The replication lane" below.**
- **A kernel without Postgres still runs.** A nil persistence is a no-op and the store behaves
  exactly as it did before, which keeps tests and single-node demos working.

Residual: there is no policy history table, so "who changed this and when" is answerable only
from `UpdatedAt` and the decision journal, not from a durable audit trail of authoring.

**Link identity.** A link is `(policy, container, target)`. Re-linking the same triple updates
in place rather than appending, so an edited link resolves once and is explained once.

**Journal is in-memory and bounded.** What-If replays at most the retained window, so a
simulation over a quiet period sees little history. A durable journal implements the same
interface.

**Delegated administration is out of scope.** One admin role, per the spec's v1 non-goals.

**Complexity is the enemy of adoption.** SELinux is the cautionary tale: a correct policy
system that operators switch off. The mitigations here are the small closed vocabulary, the
five-rule precedence that fits in a paragraph, and `ResultantPolicy`. If precedence ever needs
a diagram to explain, simplify the precedence.

---

## Amendment 2026-08-11 — the replication lane

This ADR owns the "in-memory read model kept fresh by LISTEN/NOTIFY" pattern, so the correction
is recorded here and cross-referenced from the ADRs that copied it: **ADR-0034** (agent scope
invalidation), **ADR-0090** (ingress registry), **ADR-0121** (party identities), and contract
**0077** (identity bindings).

**What was wrong.** The pattern was implemented five separate times, months apart, one per ADR.
None of them could reconnect. The bullet above says a replica that cannot subscribe logs and
serves its snapshot — true of the first attempt, and irrelevant to the failure that actually
occurred, which was a subscription successfully established and then **lost**. The five had
drifted into three different answers to "what happens when the connection dies":

| Shape | Who | Behaviour on a dropped connection |
|---|---|---|
| Give up silently | policy (`PgPolicyStore`), agent scopes (`PgAgentScopeStore`) | `WaitForNotification` errors → return → channel closes → the watch loop sees `!ok` and returns. **No log on that path.** Cross-replica updates end for the life of the process. |
| Retry the corpse | ingress, party identities | Sleep 2s and call `WaitForNotification` again on the connection that already died — forever. Never heals, and because the loop never exits, the deferred `Release` never runs, so a pooled connection is held permanently by a goroutine that looks alive. |
| Never listen | identity bindings | `IdentityPersistence` has no `Subscribe` at all. Loaded once at startup, never updated. |

Three answers to one question means nobody chose; each author wrote something plausible because
there was nothing to reuse and the previous copy was in another file under another noun.

**Measured, not reasoned.** Against live PostgreSQL: replica A watching, replica B writes, A sees
it. Then `pg_terminate_backend` on the listening connection — what a database restart or a pooler
recycle does routinely. B writes again; **A never caught up**, and the pool reported
`acquired=1`, the dead listener's connection held for good. The regression test
(`authz/replicated_test.go`) reproduces exactly this and now passes.

**The fix (`cambrian-premium/authz/replicated.go`).** One shared lane, chosen over a shared
registry base type: the five differ in ways that are load-bearing — identity bindings write
asynchronously because they are minted while a stranger's message is in flight, party identities
write durable-first because they are access control — and a base type that absorbed those
differences would be a place for one of them to be quietly flattened into the other. So only the
part that was uniformly wrong is shared.

- `listenOnce` opens ONE session and is single-shot by design: its channel closes when the session
  ends, and the connection is **always** released, so a broken one returns to the pool to be
  discarded instead of being held.
- `watchReplicated` owns resubscription, with context-aware backoff from 1s to 30s (the old
  `time.Sleep` ignored cancellation and could hold shutdown).
- **Every session begins with a resync**, and this is the point rather than a detail.
  Reconnecting is not enough: a notification sent while nobody was listening is *gone*, and no
  later notification mentions it. Reload-style consumers reload; agent scopes calls the new
  `InvalidateAll`, because it cannot know *which* agents were revoked during the gap and the only
  sound answer is to distrust the whole cache. It runs on the first session too — one redundant
  read at boot, in exchange for an invariant with no exception to remember.
- Failure logging is loud once, then every tenth attempt, and recovery is logged. Silence is how
  the old shape hid: a replica serving stale access control looked perfectly healthy.

**Blast radius, honestly.** On a single-process deployment the staleness could not bite, because
writes go through the same in-memory object; the leaked connection could, on any database blip.
Agent-scope revocation additionally degraded to the resolver's safety TTL rather than stopping —
slower, not broken. The staleness became real with a second replica or a direct SQL edit.

**Not fixed here:** identity bindings still have no `Subscribe`, so they remain load-once. Adding
one is a behaviour change (bindings would begin syncing across replicas) and is left as a named
gap rather than folded into a bug fix.

**The rule going forward:** a premium authz read model does not hand-roll a listener. It supplies
a single-shot `Subscribe` and a `Reload`, and `watchReplicated` owns everything between them.
