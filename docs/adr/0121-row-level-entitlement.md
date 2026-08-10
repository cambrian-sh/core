# ADR-0121: Row-level entitlement — "the records you are a party to"

**Status:** Proposed (design only; no code written)
**Date:** 2026-08-10
**Amends:** ADR-0085 (access policy — the scope algebra gains a qualifier, not a new grant),
ADR-0087 (composition — a new term that follows the restriction rules, D1a),
ADR-0091 (closed tags — this answers its named residual: *"Closure is per-tag, so granularity is
the tag. Sub-tag scoping (this document, this person) is Zanzibar's problem"*).
**Builds on:** ADR-0108 (event-shaped knowledge — `event_roles` is already the party relation),
ADR-0099 (identity is not classification), ADR-0111 (the query plane that enforces scope).
**Origin:** the resident worldsim deployment, 143 graded asks that the access model cannot
express. The largest measured limitation in the policy plane.

## Context

### What the model can say today

Access is decided by TAGS. A record carries a `classification TEXT[]`; a principal resolves to a
`domain.TagPredicate{RequiredTags, AnyOfClauses, ForbiddenTags}`; and the query plane renders
that as three SQL conditions against the evidence row (`query_plane.go`, `evidenceScopeSQL`):

```sql
e.classification @> $required          -- has all of these
NOT (e.classification && $forbidden)   -- has none of these
e.classification && $anyOf             -- has at least one of these
```

Every term is a property of the RECORD ALONE, and the whole algebra is an intersection.
`ScopeConfig.GrantedTags` says so in the code: *"This is the only term in the model that adds
access rather than removing it, and it is crippled on purpose."*

### What it cannot say, and why that is structural

It cannot express **"the orders that are yours."**

"Yours" is not a property of the order. It is a relationship BETWEEN the order and whoever is
asking, and the same record must answer differently for two different readers. A predicate over
`e.classification` cannot do that at any level of cleverness, because the reader does not appear
in it.

So for a sales rep, an operator has exactly two options today, and both are wrong:

| Option | Result |
|---|---|
| Grant the `orders` tag | The rep reads **every** customer's orders |
| Do not grant it | The rep reads **none of their own** |

That is the shape of all 143 asks: "the tickets I am assigned", "my team's incidents", "the
supplier records for the supplier I am", "my own HR record". A deployment that answers them by
granting the broad tag has not implemented entitlement; it has disabled it and written a note.

### The one workaround, and why it does not survive contact

A tag per relationship — `account:A-1042` on the record, granted to the rep — does work, and it
is what a determined operator will do. It fails for reasons of arithmetic rather than
correctness: every grant list grows with each account a person covers, and every reassignment is
a policy edit.

It also fights ADR-0091 specifically. For the scheme to deny by default, each `account:*` tag
must be CLOSED, and D1 there requires that **every closed tag be in the controlled vocabulary**
— so the vocabulary grows by one term per customer, and the thing that makes closure safe (a
small, deliberate, reviewable set) becomes a mirror of the customer table. ADR-0091 anticipated
this and left it open in as many words: *"Closure is per-tag, so granularity is the tag. Sub-tag
scoping (this document, this person) is Zanzibar's problem."* This ADR is the answer to that
sentence, and it takes the same position on Zanzibar (D2) that ADR-0085 took: relationship
tuples remain a non-goal.

### Prior art, including one attempt that was deleted

This is not the first row-level filter in this system, and the ADR would be dishonest without
saying so.

**`aclAllows` (deleted, ADR-0085 extraction).** The pre-0085 scope system carried a SECOND read
filter beside the tag predicate: a fact was readable only if `meta["source_agent_id"] ==
callerID` — an ownership ACL on the AUTHOR. It was removed with the rest of `internal/scope/`,
and the incident it caused is the most useful thing about it. Because the filter ran in Go
*after* retrieval and applied unconditionally, an agent could retrieve documents and then read
none of them: the symptom was `SpreadingEngine source_count=42`, every sub-query answering `""`,
and synthesis abstaining with "corpus does not answer". Retrieval looked healthy the whole way.

Party-scoping differs from it in the four ways that matter, and each is a decision below rather
than a coincidence: it is **opt-in per grant** (D1) rather than unconditional; it is about the
record's SUBJECT rather than its author; it is **rendered into the same SQL predicate** (D5)
rather than applied as a second pass; and it is **authored** by a policy rather than implicit in
the store. What it inherits from `aclAllows` is the failure signature, which is why D6 exists.

**The replacement ADR-0085 already shipped, and what it does not cover.** Deleting `aclAllows`
was not a loss of function: *"Ownership-based visibility is now expressible declaratively as
`RequiredTags: ["provenance:source=<agent>"]` — the write path already stamps that tag."* That
is the correct answer to the question `aclAllows` was asking, and it is genuinely sufficient
for it.

It does not reach this ADR's question, for two reasons that are easy to miss because the shapes
look alike. It is about **who wrote the record**, not who the record is **about** — an order
written by the CRM ingest is not thereby the rep's order. And the tag names a specific principal,
so it is one policy per person: "reps see their own" cannot be written once. Provenance
ownership is a static tag and works within the existing algebra; party-ness is a relation and
does not.

**`Conversation.OwnerID` (live).** `domain.Conversation` carries an owner and
`ListConversations(ctx, ownerID, …)` filters on it — "ownership is load-bearing security",
because employees and end customers share one store. This is real row-level entitlement, working
today, and this ADR **does not touch it**: conversations are not substrate rows, do not reach
the query plane, and are filtered by a parameter rather than a policy. Unifying the two is a
plausible future and an explicit non-goal here (see "What this ADR does NOT decide").

### What already exists, which makes this cheaper than it looks

Three of the four pieces are built:

- **The party relation, for events.** `event_roles(event_id, role, entity_id)` (ADR-0108,
  migration 0014) is one row per participant with `role` naming HOW they participated, indexed
  `(entity_id, role)`. `EventsForEntity` already runs the exact join this needs:
  `EXISTS (SELECT 1 FROM event_roles r WHERE r.event_id = e.id AND r.entity_id = $4)`. It is
  used for RETRIEVAL and never for authorization.
- **The party relation, for observations.** An observation carries `entity_id` — the subject it
  is about. Degenerate (one party, one role) but real and indexed.
- **A place to put the decision.** The scope reaches the query plane as one struct and is
  rendered in one function.

The missing piece is the fourth: **nothing says which entity a READER is.** The kernel knows a
principal (`domain.PrincipalRef`); it has no notion that principal `u:rita` is entity
`employee:E-42`, or that she covers accounts `A-1042` and `A-1043`.

## Decision

### D1. Party-scoping is a QUALIFIER on a grant, never a new way in

A policy does not gain "let this person read rows they are a party to". It gains the ability to
mark a tag it already grants as **party-scoped**:

```
ScopeConfig.PartyScopedTags []string   // authored, premium
TagPredicate.PartyScopedTags []string  // compiled, OSS
TagPredicate.PartyIdentities []string  // who the reader is, resolved per request
```

Read as: *for rows carrying this tag, additionally require that the reader is a party.* In SQL,
a material implication:

```sql
AND (NOT (e.classification && $partyScoped) OR <reader is a party>)
```

**This can only ever remove rows.** A row the tag rules did not already admit is not admitted by
being party-scoped; a row they did admit may now be filtered out. That is the entire reason for
this shape. The model is an intersection with exactly one crippled additive term
(`GrantedTags`), and a second additive term is a second thing that can widen access by accident.
Party-scoping stays on the narrowing side of that line, so it composes with `Compose` without
amending the algebra.

The consequence to state plainly: **entitlement is expressed by granting broadly and narrowing
by party**, not by granting narrowly. The operator's mental model is "reps may read orders —
their own", and the two halves are authored in that order.

### D1-check. ADR-0091's own review question, applied

ADR-0091 leaves a standing test for anything that touches this area: *"A granting direction now
exists. It is crippled (D3) and cannot touch anything unclosed... The review question for any
change here: **can this grant affect a tag nobody closed?** If yes, reject it."*

Party-scoping passes trivially, because it is not a grant: it confers nothing, on any tag, ever.
The granting direction remains exactly `GrantedTags`, exactly as crippled, and this ADR adds no
second way to widen. That is the same reasoning as D1 and it is worth stating in ADR-0091's own
terms, because the next person to review this area will apply that question and should find the
answer already written down.

The corollary is a difference from grants worth being explicit about: **party-scoping applies to
open and closed tags alike.** A grant may name nothing but a closed tag, because a grant on an
open tag confers nothing and would be a policy that reads like access and is not. The crippling
exists to bound the blast radius of *adding* access. Party-scoping only removes, so there is no
radius to bound — "for rows tagged `orders`, require party-ness" is meaningful whether or not
anybody closed `orders`, and refusing it on open tags would forbid the common case for a reason
that does not apply to it.

### D1a. Party-scoping is a RESTRICTION, and takes ADR-0087's restriction rules exactly

This is the decision the first draft of this ADR got wrong, and the error is worth recording
because it is invisible until someone uses a legitimate feature to escalate.

ADR-0087 D2 rule 3: *Block Inheritance stops accumulation from above — except for
`ForbiddenTags` and except for links marked Enforced. A blocked policy therefore contributes
only its denies: **restriction survives a block, permission does not.*** If `PartyScopedTags`
folded like `RequiredTags`, a downstream container could set `BlockInheritance` and the party
restriction would simply vanish, leaving the broad tag grant behind — **Block Inheritance as a
privilege escalation**, reachable by an ordinary org-chart exception and silent.

So party-scoping is classed with `ForbiddenTags`, on all three counts:

1. **Accumulates by union**, never intersection — reaching a tag through any container that
   party-scopes it is enough to party-scope it.
2. **Never removable** — no link, no ordering, no Enforced flag un-scopes a tag that some
   applicable container scoped.
3. **Survives Block Inheritance** — it is a deny in the sense rule 3 means.

Rule 5 (ties impossible) survives unchanged: union is commutative, so order still affects only
the explanation.

**The wrinkle this creates, stated rather than hidden.** A person in two containers — "reps may
read their own orders" and "managers may read all orders" — gets the NARROWER answer, because
restrictions union. That is consistent with every other term in the model (more containers means
less access, except for `GrantedTags`), and it will still surprise the administrator who built
those two groups. The remedy is the one the model already offers: managers are granted a
different tag, or the rep policy is not linked to them. The alternative — letting an unscoped
grant of the same tag defeat a party scope — reintroduces "later wins" widening, which rule 1
exists to forbid.

### D1b. The admin surfaces must show it, or the preview lies

`AdminService.ResultantPolicy` answers "what can this principal actually see", and
`authz/simulate.go` answers "what would this policy change". A party-scoped grant that appears
in neither renders as a full grant, and an administrator checking their work would be told the
rep reads every order.

Both must report the party term AND the fact that the answer is per-row rather than a set of
tags — `ResultantPolicy` cannot enumerate which rows without running the query, and it must say
that rather than imply completeness.

### D2. The party relation is the data already in the substrate, not a new store

No relationship-tuple store, no Zanzibar. Party-ness resolves per lane against what exists:

| Lane | A reader is a party when |
|---|---|
| events | a row exists in `event_roles (event_id, entity_id)` for one of their identities |
| observations | `observations.entity_id` is one of their identities |
| evidence-backed reads | the evidence carries the identity in a `parties TEXT[]`, derived at the ingress |

The third is the only new column, and it exists because the observation/event lanes reach scope
*through* evidence and some evidence backs neither. It is derived exactly as classification is
(ADR-0120's `ClassificationFrom` shape): the source already states who the record is about —
`customer_id`, `assignee`, `supplier_id` — and a declared rule reads that field. Deriving it at
the ingress is not a convenience; it is the only place upstream of everything that could later
be asked to filter.

A relation store was considered and rejected for v1. It is the right answer to a question nobody
here has yet asked — arbitrary-depth relationships authored independently of the data — and it
brings a consistency problem (the tuples and the records drift) that the derive-at-ingress
approach does not have, because the record and its parties are written in one transaction.

### D3. The PDP resolves who the reader is — OSS gains no port

Who a principal IS — `u:rita` is `employee:E-42` and covers `A-1042`, `A-1043` — is **principal
resolution**, and ADR-0085 D3 puts that on the decision side of the line: *"Composing —
intersection, precedence, inheritance, vocabulary validation, principal resolution — is the
decision and lives in the plugin."*

So there is no `PartyResolver` port in OSS. The premium PDP already produces the
`TagPredicate`; it fills `PartyIdentities` while composing, from whatever the deployment
supplies — an HR feed, a directory, a CRM ownership table. The kernel receives identities the
way it receives every other term: as data it applies without interpreting. An earlier draft of
this ADR added an OSS port and had the kernel call it; that would have put principal resolution
in the kernel and inverted D3 for no gain.

**Whatever feeds it must NOT be the knowledge substrate being queried**, and this is the
load-bearing security decision here. Authorization that reads from the store it authorizes is a
loop with a poisoning path: an ingested record asserting `assignee: u:mallory` would make
Mallory a party to it, so any source that can write a record could widen its own audience. The
resolver's inputs are policy-grade infrastructure and inherit the protection the policy store
has.

Resolution happens once per request and travels in the compiled predicate, so the SQL stays a
single array-overlap and the filtered-vector-search plan (SUB-00: EXACT, GIN) is unchanged.

**Party identities are deliberately NOT vocabulary-checked**, and that is a departure from D11
worth naming rather than leaving to be noticed. D11 requires classification tags to come from a
controlled vocabulary because *"a typo is the primary route to a scope that silently matches
nothing"*. Identities cannot: a deployment cannot enumerate every customer in a vocabulary, and
requiring it to would be the tag-per-customer scheme this ADR exists to avoid. They are
identity, not classification, which ADR-0099 already established as a distinction the write path
must respect. The typo risk is real and is answered by D6's reporting instead — a reader who
resolves to identities that match nothing sees nothing and is told so.

### D4. Depth one. No transitive walk in v1

"The accounts I manage" is depth one — the resolver returns those account ids. "My team's
records", where team membership itself confers party-ness, is depth two, and is where a
userset-rewrite engine begins.

v1 flattens at resolution time: the resolver may expand a person into a set as large as it
likes, but the query plane receives a flat list and tests membership. A deployment that needs
"my team's" expands the team at resolution. What v1 does NOT do is let a POLICY express a walk,
because that is the feature whose evaluation cost and cycle detection are the hard part of every
system that has one.

### D5. One filter, in SQL — never a second read filter

The predicate is rendered in `evidenceScopeSQL` and the per-lane equivalents, never applied
after the rows come back. `aggregate` already carries the reason in a comment — *"summed rows
cannot be post-filtered"* — and it applies identically here: a `count` over party-scoped rows
computed without the party term is a leak with no row in it.

The stronger form of this rule is a scar, not a preference. `aclAllows` was a second read filter
running in Go beside the SQL predicate, and "the scope system has TWO read filters, not one" is
how the incident was eventually diagnosed — after the first filter had been disabled and the
symptom did not go away. **Party-scoping adds no second place where a row can be refused.** If
it cannot be expressed in the predicate for some lane, that lane does not get party-scoping
until it can.

### D5a. The parties column is not tattooing (INV-4)

INV-4 forbids writing policy into resource state: *"Policy is evaluated at decision time and
never written into resource state. Removing a policy fully restores prior behaviour."* A new
column populated at ingest invites the objection, so: the parties of a record are a **fact about
the record** — this order is for customer C-9 — in exactly the sense `classification` already
is, and neither is policy. Nothing about who may read it is stored anywhere.

The invariant's own test settles it. Remove every party-scoped policy and the column remains,
nothing consults it, and access reverts precisely to what the tag rules alone allow. No policy
was tattooed, because none was written down.

### D6. Fail closed, and visibly

A principal with a party-scoped grant and NO resolved identities reads **nothing** of that kind
— not everything. A resolver that is unavailable is an error, not an empty set: an empty set is
indistinguishable from "this person is party to nothing", and the two must not be confused when
one of them is an outage.

A log line is not enough here, because **INV-3 is a hard invariant: "No silent empties. Any
policy-caused zero-result carries a reason."** A party-scoped empty result is a policy-caused
zero-result, so it must set `policy_note` through the same channel ADR-0085 D8 built —
`QueryMemoryResponse.policy_note` and the agent plane's `MemoryResponse.policy_note` — with a
detail that distinguishes the two cases an operator will confuse:

- *"you are party to none of the records that match"* — the entitlement working
- *"no party identities resolved for this principal"* — the entitlement misconfigured

D8's rule that the note is emitted **only** when policy actually shaped the outcome still holds;
annotating every empty response trains callers to ignore the field. The decision journal
additionally records the identity count, because the second case above is the one where the
number zero is the whole diagnosis.

### D6a. Two implementation traps ADR-0091 already paid for

Both are recorded there as silent failures, and both apply here unchanged. Neither is a design
question; both are things this ADR would otherwise let someone rediscover.

- **`ScopeConfig.IsZero()` must learn the new term.** It reads
  `len(RequiredTags)==0 && len(AnyOfTags)==0 && len(ForbiddenTags)==0 && len(GrantedTags)==0`,
  and its comment records exactly this trap for grants: a rule the check reads as empty is
  *"stored, listed in the console, and silently never applied."* A policy whose only term is
  `PartyScopedTags` is precisely that shape — an operator would author "reps see only their own",
  see it listed, and get no narrowing at all. Add the term or the feature ships inert.
- **INV-1's property test must learn the term.** *"Intersection only. No code path widens an
  effective scope"* is property-tested over 3000 random scope sets in `authz/scope_test.go`, and
  again over the container hierarchy. A generator that never emits `PartyScopedTags` proves
  nothing about it, and the invariant this ADR most needs held is exactly the one that test
  covers.
- **The resolver must not be rebuilt without the party state.** ADR-0091's other trap was a
  `Build` phase that reconstructed the `Vocabulary` and discarded the closed set, so *"the boot
  log announced `closed=[airline]` while the decision point allowed everything"* — a security bug
  that reports success. Anything holding resolved party configuration is in the same position,
  and the regression test needs a real database because `Build` returns early without one.

### D7. OSS carries the mechanism; premium decides

The ADR-0085 split, unchanged, and **OSS gains no port** (D3). Its whole share is two fields on
`domain.TagPredicate` — data it applies without interpreting, exactly the relationship D3
describes between the NT kernel and a DACL — plus the branch in the query plane that renders
them, plus the `parties` column the ingress fills.

Everything that decides stays premium: `PartyScopedTags` on a `PolicyObject`, the composition
rules of D1a, and the identity resolution that fills `PartyIdentities`. INV-6 holds unchanged —
`grep ScopeConfig cambrian-core` still returns nothing.

An OSS kernel receives a predicate with both fields empty and behaves exactly as it does today:
no party-scoped tags means no implication clause is rendered, and `AllowAllAuthorizer` continues
to fail open, which remains the correct semantics for a single-tenant deployment.

### D8. What measures it — the policy suite ADR-0085 called for

ADR-0085 closed with an admission: *"No benchmark. Access control has no benchmark suite and the
DDD mandate's step 6 applies... A `policy` suite — deny/allow fixtures, an escalation corpus,
and an explanation-completeness check — is the honest follow-up."* That follow-up is now due,
because this is the first change to the model where "behaviour-preserving on the OSS default
path" stops being the whole safety argument.

The three parts already have their content:

- **Deny/allow fixtures** — the 143 graded asks from the resident deployment, which are the
  reason this ADR exists and are already labelled with the answer each should give.
- **An escalation corpus** — and D1a supplies its first entry, because "a downstream container
  sets `BlockInheritance` and the party restriction disappears" is a real escalation that a
  reasonable implementation reaches by accident. Beside it: an unscoped grant of the same tag
  from a second container, and a principal resolving to another principal's identities.
- **Explanation completeness** — every denial and every party-filtered empty carries the reason
  D6 requires, checked rather than assumed.

The suite gates the change; a party-scoping implementation that cannot pass the escalation
corpus is not shippable regardless of how the 143 fare.

## What this ADR does NOT decide

- **Write-side entitlement.** This is a read filter. Whether being a party lets you WRITE to a
  record is a separate question and a different chokepoint (`ClassifyWrite`).
- **Effect grants.** `EffectGrant` composes by the same shape (denies union, allows intersect)
  and is untouched here: whether a tool effect may be party-scoped is a real question and a
  different one.
- **Unifying with `Conversation.OwnerID`.** The conversation store's owner filter does the same
  job in a different lane by a different mechanism. Making them one thing is plausible and is
  not attempted; anyone who tries should start by asking whether a conversation is substrate.
- **Revocation latency.** Resolution is per request, so a revoked relationship takes effect on
  the next query — but whether the resolver may cache, and for how long, is a deployment
  decision this ADR does not bound. Monotonic security argues for no caching; a directory that
  charges per lookup argues otherwise.
- **Aggregate inference.** Party-scoping makes counts honest per reader, but a reader who can
  see their own row count across many queries can still infer things about rows they cannot see.
  Differential-privacy-style bounds on aggregates are out of scope and unaddressed.
- **How parties are declared for the file and poller archetypes.** D2 says "a declared rule like
  `ClassificationFrom`"; the exact spec field is left to implementation, and it should reuse that
  machinery rather than parallel it.
- **Trust in the source's own claim.** A record states its own parties, so a party-scoped grant
  is exactly as trustworthy as the ingress that admitted the record — which is what the verifier
  profile establishes and why `operator_upload_v1` and a signed webhook are different things.
  This ADR inherits that trust boundary; it does not strengthen it.

## Consequences

- The 143 asks become expressible: "reps may read orders, their own" is a tag grant plus one
  party-scoped mark, and the same policy serves every rep without naming any of them.
- The tag vocabulary stops growing with the data. `account:A-1042` per customer was the only
  workaround and it was ADR-0091's closure abandoned in practice.
- `event_roles` gains a second consumer and becomes load-bearing for authorization, not only
  retrieval. Its index `(entity_id, role)` is already the right one.
- One new evidence column, two predicate fields, and **no new port** — the PDP already returns
  the predicate, so resolution needs no seam of its own. No new store, no new service, no
  dependency.
- **`domain.TagPredicate` changes, and this is the first term that could not avoid it.** ADR-0091
  closed its cost section with "no kernel change, no contract change on the OSS plane", because
  closure compiles down into the existing `ForbiddenTags` (its D4). Party-scoping cannot: the
  condition is per row AND per reader, and there is no static tag set that expresses "…unless you
  are a party". So the predicate gains two fields and the query plane gains a rendering branch.
  Worth noticing rather than glossing — the run of changes that cost nothing structurally ends
  here, and if a later reviewer finds a way to compile this into the existing three conditions,
  that version is better than this one.
- A new class of operational failure — the correct-looking empty result — which D6 exists to
  make diagnosable, and which has a measured precedent: `aclAllows` produced exactly it, and the
  diagnosis took a database session and a raw vector search to reach because retrieval reported
  success at every layer.
- `ResultantPolicy` and What-If gain a term they must render, and an answer they must qualify:
  with party-scoping, "what can this principal see" stops being answerable as a set of tags.
- The resolver becomes policy-grade infrastructure. Whatever feeds it inherits the protection
  the policy store has, and a deployment that wires it to something casual has moved its access
  control there without noticing. This is the sentence to re-read before implementation.
