---
id: 0128
title: The Identity and Lineage Planes — Entities, Links, and the Confirm/Propose Boundary
status: Implemented
date: 2026-08-20
supersedes: []
superseded_by: []
depends_on:
  - 0093-document-entity-and-table-split
  - 0105-evidence-foundation
  - 0108-event-shaped-knowledge
  - 0110-knowledge-kind-registry
  - 0111-typed-query-plane
  - 0112-ingress-studio-foundations
  - 0118-substrate-read-scope-and-agent-retrieval
  - 0121-row-level-entitlement
---

# ADR-0128: The Identity and Lineage Planes — Entities, Links, and the Confirm/Propose Boundary

## Status

Implemented — built 2026-08-19/20 in five supervised agent waves, gated by the
`link_building` benchmark (§7). The build record with pinned interfaces, seam facts S1–S10
and supervisor decisions D-W2-1…5 / D-W5-1…6 is `FIVE-PLANES-BUILD.md` at the workspace
root; this ADR is the canonical decision unit and supersedes that file as the record of
*why*.

**Date:** 2026-08-20 (written after the fact — see §8)
**Relates to:** ADR-0105 (evidence, the plane every link cites), ADR-0108 (event-shaped
knowledge, whose deferred `Relation`/`Derivation` pair this finally settles), ADR-0110 (the
kind registry, whose immutability rule constrains the verb vocabulary), ADR-0111 (the typed
query plane this adds two ops to — "entity and alias lookup" was the one op the substrate
review demanded that 0111 did not implement), ADR-0112 (ingress studio, which authors the
declarations), ADR-0118 (the scoped read seam links must not widen), ADR-0121 (party
identity, consumed by one producer and endangered by closure — see D10), ADR-0093 D6 (the
additive-only constraint every migration here obeys)

---

## 1. Context

`entity_id` was bare, unconstrained `TEXT` in `knowledge_items`, `event_roles`,
`observations` and `statement_values.value_entity_id`. There was no entities table, no
aliases, no `same_as` record, and no item-to-item edge of any kind — the only self-reference
in the substrate was `evidence.revises_id`. Cross-source joins therefore worked by exact
string equality *by accident*: three disjoint graphs (chunk triplets, document edges, event
roles) shared no entity space, and a Slack message, an ERP event and a decision about the
same customer had nothing connecting them.

`Relation` and `Derivation` were explicitly deferred by ADR-0108 and never built. The
industry name for the resulting failure is well known — an opaque key and a company name
carry **no similarity signal at all**, so the link between them is a *fact* requiring
co-occurrence evidence, a declared rule, or a human. Entity resolution is a solved industry
(Fellegi–Sunter, Senzing, Splink, MDM crosswalks), but no comparable product had solved it
without a destructive merge step, and a wrong identity merge is the one failure mode that
produces a *confident wrong answer* rather than a visible gap.

Two further constraints shaped everything below. First, this deployment carries row-level
entitlement (ADR-0121), which no rival carries — so an identity link is not merely a
correctness question but a security one. Second, the Zero-Hardcode Rule: kinds and verbs are
data, and the kernel never branches on a specific vocabulary word.

## 2. Decision — the ontology

Adopt **Option A, "five planes"**, from the design document of that name:

| Plane | Holds |
|---|---|
| **Evidence** | what arrived — raw bytes, archived first, cited by everything downstream |
| **Entities** | what exists — a thin scoped registry row: id, kind, no attributes |
| **Events** | what happened — the OCEL-shaped event/role model from ADR-0108 |
| **Claims + links** | what was said, and what relates to what |
| **Beliefs** | what we currently think |

An **Entity** is deliberately attribute-free. A name is an *alias claim* on the entity —
a fact with a source — never a column in a golden record. This is what makes the model
non-destructive by construction: there is no merged record to un-merge, because there was
never a merged record.

**D1 · One `links` table, three families.** Every relationship in the system is one row in
one table, in family `identity` ("same thing"), `relation` ("subsidiary of", "placed by") or
`lineage` ("led to"). Every row carries evidence, author, mechanism, state, confidence and
two clocks. A single table is what makes the trust rule (D2) enforceable in one place and
the audit story uniform.

**D2 · Trust ceilings, enforced in the store.** This is the load-bearing decision of the
whole ADR:

> Mechanisms `{declared, record, reference, shared_object, witnessed, human}` may write
> `confirmed`. Mechanisms `{derived, scored, correlation}` may write at most `candidate`.

The ceiling is enforced in the store, not in the producers, and it is the one place
mechanism names appear in logic. A guessing producer cannot write a confirmed link at *any*
confidence, however certain it is, because certainty is not the currency — provenance is.
Producers rely on the ceiling rather than policing themselves, and the tests assert it.

**D3 · Admissibility.** A link with no `evidence_id` and a non-`human` mechanism is refused
by the store. Machines must cite; only a person may assert without one, because a person is
the evidence.

**D4 · Append-only lifecycle.** `candidate → confirmed → retracted` are appended state
transitions, never a `DELETE` and never an in-place edit. `ConfirmLink` writes a *new*
human-mechanism row and leaves the original untouched. A rejected candidate stays queryable
precisely so producers do not re-propose it — "never ask me this again" is a stored fact.

**D5 · Corroboration is information.** Two producers reaching the same conclusion write two
rows. Deduplicating at write time would destroy the fact that two independent mechanisms
agreed. Read paths deduplicate; the store does not.

**D6 · Scoped identifiers.** Entities are minted under a declared prefix
(`customer/C-0003`, `crm_account/acct-498`, `thread/th-015`), and the prefix comes from the
ingress declaration, never from the source record. This is what makes a bare token in a
ticket body incapable of silently colliding with an ERP customer number.

**D7 · Deferred crosswalks and the resolver lane.** A crosswalk whose far side does not yet
exist **does not mint it**. The link is written `candidate` with the unresolved target
preserved, and a background resolver upgrades it to `confirmed` if and when the target
genuinely arrives (producer `identity-resolver@1`, `source_ref` derived from the candidate id
so replays are no-ops; retracted candidates are never upgraded, because a human said no).
The asymmetry is deliberate: relation-rule far sides *keep* minting, because a
`parent_account_id` names an entity the same source owns and the record is the fact — only
crosswalks defer, because their far side belongs to another source. **The candidate in the
review queue is the quarantine surface; no new table was added.**

**D8 · Verbs are configuration, not inference.** Relation verbs are declared in the
deployment's config and folded into the registry at boot, which stays immutable thereafter.
An undeclared verb is refused at mapping-confirm time with a message naming the config path.
The drafter proposes a relation rule with the verb left *empty* for the operator to pick —
it never guesses one.

**D9 · Candidates are an operator surface.** Candidates are returned only to unscoped or
system callers. Agents never see them, so a suspicion cannot leak into an answer by being
read.

**D10 · Closure expands the subject, never the predicate.** Alias closure widens the
*subject* of a query — "everything about C-0003" becomes the confirmed alias set — but the
authorization predicate is still evaluated per row against that row's own stored parties,
never against the expanded set. Without this rule, one wrong `same_as` stops being a wrong
answer and becomes a breach, because ADR-0121 makes rows individually entitled. Guards:
bounded depth, a set cap of 8 that **refuses loudly rather than truncating silently**, and a
per-entity link cap that flags rather than grows. Proven non-widening against live
PostgreSQL during wave 2.

**D11 · Attribution keys revocation.** Ingest-path links carry
`AssertedBy = "ingress:<id>"` and `Producer = "ingress-mapping@1:<id>"` — deliberately
*without* the mapping revision, so a batch revocation catches every revision of a bad
mapping. The revision remains recoverable through the cited evidence row.

## 3. Consequences

Cambrian will not connect what nothing proves. Two records sharing only a similar name stay
separate until a person decides, permanently. That is the product's defining trade: an
honest gap costs a review click, whereas one wrong merge poisons every answer downstream and
is discovered late, if ever.

Correlation is labelled correlation forever. A co-occurrence hop is recorded under the verb
`preceded_and_shares_entities` and is never renamed to a causal verb at any confidence, in
any answer.

Arrival order stops mattering (D7), which is what makes the model usable against real
integrations where citations routinely precede the things they cite.

The review inbox becomes a load-bearing product surface rather than an afterthought: it is
where the system's honesty is spent, and its throughput is now a real operational quantity.

## 4. Alternatives rejected

- **Fuzzy or embedding-based identity merging** — rejected, consistent with ADR-0067's
  deterministic `NormalizeCapability` precedent. The normalizer is a rule table (case fold,
  punctuation strip, a legal-suffix table), not a similarity model.
- **LLM-judged identity** — never a producer. Measured failures across every comparable
  product; and an LLM cannot cite, which D3 requires.
- **Destructive merge / canonicalisation** — rejected. There is no golden record to merge
  into (§2), and unmerge-by-append is strictly better than unmerge-by-surgery.
- **Auto-declaring verbs seen in a mapping** — rejected by D8; the registry stays immutable
  after boot and a deployment's vocabulary is an authored decision.
- **Inventing a verb for bridge audit** (e.g. `duplicate_of`) — rejected. The audit writes
  nothing and surfaces through a read (`BridgeAudit`) instead. Do not invent verbs.
- **A separate quarantine table for dangling references** — rejected by D7; the candidate
  queue already is that surface.
- **Deduplicating agreeing producers at write time** — rejected by D5.
- **Silently truncating an oversized closure** — rejected by D10; refuse loudly.
- **Encoding the fired rule name in `producer`** — rejected; `producer` is the revocation
  key. Rule tier is carried by confidence, and the evidence id cites the observation.
- **`EntityKindSpec` in the immutable kind registry** — dropped by amendment during the
  build; `entities.kind` is the prefix stem, well-formedness is checked in the store, and
  vocabulary validation happens at mapping-confirm time in the studio.

## 5. What this ADR does not cover

Witnessed lineage from Cambrian's own actions is specified but is a bonus lane, not part of
the shipped gate: `Provenance.DerivedFrom` is the fetch/pagination chain, not an action
ledger, so the action-side hooks are `pipeline.EffectIntent` settlement plus receipt
correlation. Rollup closure over non-identity verbs (`expand: [verbs]`) was scoped out of v1.
The probabilistic entity-resolution plugin (Splink-style, candidates-only) remains a later
premium option, unblocked by D2 but unbuilt.

## 6. Implementation

Migration `0016_entities_links.sql` (additive, per ADR-0093 D6). Core: `domain/identity.go`
(Link, Entity, RelationRegistry, trust ceilings and admissibility in the store),
`postgres/entity_store.go`, `postgres/link_store.go`, the `entity` and `why` ops plus
`ExpandAliases` in `postgres/query_plane.go`, and the operator write surface in
`internal/substrate/operator/links_write.go`. Premium: mapping v2 with prefixes, crosswalks,
relations and refs; `ingressstudio/link_sink.go`; the deterministic background producers in
`identityproducers/` (normalizer with a confidence tier ladder, party agreement over ADR-0121
party identities, correlation bounded to a 72-hour window, bridge audit, and the D7
resolver); drafter link detections; and chat capture writing messages as evidence with author
and thread as first-class participants. Operator contract 0097 → **0098** (`ConfirmLink`,
`RetractLink`, `RetractLinksByProducer`, `ListLinkCandidates`, capability `links`), with the
links review screen in `ui/`.

## 7. The gate

Measured by the `link_building` suite (`cambrian-benchmarks/docs/LINK-BUILDING-SPEC.md`) on
the seeded "Meridian Supply" corpus — 40 companies, 200 orders, 300 tickets, 30 chat threads
across four feeds, 14 planted traps, and a gold sidecar the kernel never sees.

| | `lb-final2` (before) | `lb-w5-final` (after) |
|---|---|---|
| Verdict | **FAIL** 82/98 | **pass** 93/99 |
| Build F1 — identity / relation / lineage | 0.4082 — 0.9855 / 0.0 / 0.2390 | **1.0** — 1.0 / 1.0 / 1.0 |
| Confirmed-lane gate | FAIL, 1 violation | pass, **0 violations of 930 confirmed** |
| Traps | 13/14 (T6 0/1) | **14/14** |
| Why-chains | F1 0.2264, mechanism 1.0 | F1 0.9615, mechanism **1.0** |
| Entity-360 auto / oracle | 1.0 / 0.7400 | 1.0 / 0.8667 |
| As-of | 0.5000 | 1.0000 |
| Candidate recall | 0.0000 | 0.7727 |
| Leaks / idempotency | pass / pass | pass (0 rows, non-vacuous) / pass |
| Floor | `true` | `false` |

The failing run is the more informative one, and it is recorded here deliberately. It failed
on trap T6 — a CRM record citing a customer id that exists nowhere minted a garbage entity
and a *confirmed* link. D7 is the fix, and on the re-run the same trap produced exactly one
quarantined candidate and nothing else.

**Two honest caveats.** The before/after delta is part kernel and part answer-key repair —
the first run's gold key had its own defects and both were fixed in the same wave, so the
deltas should not be read as one number. And `declare_f1` reads 0.0 in both rows because both
were run in `build` mode; the drafter's unassisted score is 0.61 from a separate declare run,
which is a drafting aid's score and is all it needs to be, since an operator confirms every
proposal. The remaining candidate-recall gap is trade-descriptor names ("Industries",
"Group", "Holdings") that the deterministic normalizer correctly refuses to treat as
ignorable; closing it is a vocabulary decision (a descriptor table), not a defect.

**Wave record.** Wave 1: core foundations and premium mapping v2. Wave 2: query ops, closure
with guards, contract 0098, link sink. Wave 3: producers, drafter detections, chat capture,
review UI. Wave 4: the first full benchmark run (`lb-final2`), which produced the eight-item
kernel punch list and seven corpus gold defects. Wave 5: the full punch list plus gold
repairs, gated by `lb-w5-final`. `FIVE-PLANES-BUILD.md` records results for waves 1 and 2
only; waves 3–5 are recorded here, in this ADR, and that file should be read as superseded
for status purposes.

## 8. Record drift corrected alongside this ADR

Writing this record surfaced that it was not the only one missing. An audit on 2026-08-20
found four ADRs describing shipped systems as unbuilt, which matters because `docs/adr`
doubles as the roadmap view — `Proposed`/`Accepted` is read as *what is planned*, so a stale
`Proposed` invites the work to be done twice. Corrected in the same pass: **ADR-0107**
(embedding projection), **ADR-0113** and **ADR-0114** (pipeline graphs and runtime
semantics), and **ADR-0126** (published tool surface and MCP endpoint). ADR-0127 was
verified as genuinely unbuilt and left alone. A duplicate `0115-source-discovery.md` was
merged into ADR-0115 and removed, restoring the unique-number rule.

The general lesson is procedural, and it is the reason this section exists rather than a
commit message: **an architecture record written after the code has already drifted is
archaeology.** The five-planes work went through five supervised waves with a detailed build
document and still arrived here with no ADR, because the build document felt like the record
while the work was live. It was not; it was a work order. The ADR is the artifact that
survives the context that produced it.

## 9. D12 · Existence is protected content

**Decided by the owner, 2026-08-20: an unentitled asker must not learn that an entity
exists.**

The question was whether identity-closure membership is harmless metadata or protected
content. Until this ruling the entity op echoed, to any caller, the subject row (existence,
kind, created-at, evidence id) and the alias membership rows, withholding every substantive
row and every justification. The benchmark published those as
`gate.leaks.closure_membership_rows` — 6 in the gated run — rather than as leaks, and a
strict reading of the benchmark spec §4.3 would already have counted them as leaks.

Both readings were defensible. Membership arguably reveals no business fact: that
`crm_account/acct-498` and `customer/C-0003` are one company is not itself a price, a
quantity or a date. Against that, an identity closure is a disclosure in its own right — it
tells an outsider that two systems' records belong to one counterparty, and in a regulated
setting that relationship is frequently the sensitive part. The ruling takes the second
reading.

**The rule.** A principal who may reach no row of an entity must not be able to distinguish
that entity from one that does not exist. Existence, kind, creation time, evidence
references and closure membership are all protected content, not metadata. The entity op
answers such a caller exactly as it answers a caller asking about a fabricated id.

**Consequences to carry through:**

- The entity op must fail closed on the subject row, not merely on the substantive rows.
  Withholding the justification while echoing the handle was the previous behaviour and is
  now a defect.
- Alias-closure membership rows follow the subject: an alias the caller cannot reach is not
  listed, because listing it discloses the very relationship this rule protects.
- The benchmark's `closure_membership_rows` counter becomes a **leak count**, not a
  published-either-way statistic. The gate must fail on a non-zero value.
- Not-found and not-entitled must be indistinguishable **to the caller** while remaining
  distinguishable **in the audit record** — an operator diagnosing a complaint needs to know
  which of the two happened, and the decision journal is where that belongs.
- This is a read-path rule and does not change what is stored. Nothing about D2's trust
  ceilings, the link lifecycle or the producers is affected.

**Relationship to D10.** D10 governs closure versus the access predicate: expanding aliases
widens the query subject and never the authorization test. D12 governs what a refused caller
is told. They are separate doors into the same room — D10 stops a wrong link becoming a
disclosure, D12 stops the absence of a permitted row becoming one — and a solution to either
must not quietly reopen the other.
