---
id: 0095
title: Experiences as First-Class Entities — A Parent Row for the Memory Nobody Ingested
status: Proposed
date: 2026-07-27
supersedes: []
superseded_by: []
amends:
  - 0093-document-entity-and-table-split
  - 0049-experiential-memory-world-model
depends_on:
  - 0035-kernel-derived-write-classification
  - 0049-experiential-memory-world-model
  - 0064-embedded-db-migration-runner
  - 0085-access-policy-port-and-extraction
  - 0087-policy-composition-groups-links
  - 0090-ingress-and-surface-identity
  - 0091-closed-tags-and-crippled-grants
  - 0093-document-entity-and-table-split
  - 0094-procedural-memory-tier
---

# ADR-0095: Experiences as First-Class Entities

## Status

**Proposed — schema implemented (migration `0010_experiences`), writers not built.** D1/D2/D5/D8 are live: `experiences`, `chunks.experience_id` (nullable, `ON DELETE CASCADE`) and `experience_derivations` exist, verified against a scratch database through the real migration chain — table shape, GIN index on `tags`, and a **cascade proof** (deleting an experience removed both its chunk and its derivation row, which is D6's "forget an episode" working). Applied to the dev store 2026-07-27; a post-migration LoCoMo smoke showed no regression against the pre-change binary.

D4 (born-tagged stamp) is now **implemented and live-verified**: an episode written
under the arm carries `{internal, surface:agent, ingress:grpc}`, stamped by the kernel
from the ingress context, with its outcome record parented to it. D5's
`experience_derivations` link is implemented (`LinkDerivation`) and is what makes D9's
cross-boundary rule auditable rather than notional.

Still outstanding: D3's single-writer retag TRANSACTION (retagging an experience and its
chunks atomically) and D6's retention operations beyond the cascade itself. The table lands empty and no back-fill runs — see the note at the foot of the migration: reconstructing parents from `metadata->>'plan_id'` is correct in principle, but the write path is unwired and its record shape is about to change, so inventing parents now would put soon-to-be-wrong rows in the store the benchmarks measure against.

Sequenced with ADR-0049 §A2 Phase 1: the parent row must exist *before* the experiential write path is rewired, because it is what the writes attach to.

## Context

ADR-0093 split one overloaded table into six, and named the decision that mattered:

> "A document was not an entity. This is the one that matters. Nothing in the database represented the source document a chunk came from."

and recorded the consequence:

> "Access control can finally attach to a document. That was the request behind this work and it was previously **inexpressible** — not hard, *inexpressible*, because the thing to attach to did not exist."

**Every word of that is now true of experiences, and ADR-0093 said the opposite.** Its D3 reasoned:

> "A chunk carved from an ingested document has a parent. A memory an agent wrote about what it just did does not. Forcing the second kind under a synthetic document would be a lie told to satisfy a foreign key, so the column is nullable and unparented memories keep a NULL."

That was correct about `documents` and wrong about parentage in general. An agent-written memory **does** have a parent — not a document, but the **episode that produced it**. Under ADR-0049 every experiential record is minted by a plan execution, and `plan_id` is already stamped on those rows (`internal/memory/precedent.go` resolves an action path by `QueryByMetadata(ctx, {"plan_id": planID})`). The parent exists in the data and has no row.

### Three things that are impossible today

1. **You cannot explore what the system has done.** There is no row to list. An episode is a set of chunks discoverable only by knowing its `plan_id` in advance.
2. **You cannot delete an experience.** Its rows are scattered across `chunks` with no authoritative parent and no cascade. "Forget this customer's episodes" — tenant offboarding, retention, the active-forgetting GC that `docs/research/agent-memory/SUMMARY.md` flagged as an open gap — is not expressible.
3. **You cannot govern an experience.** This is the serious one. ADR-0091 shipped deny-by-default closed tags and verified the exact case live — *"from `chat:airline` → untagged tool / internal memory → denied"*. But it also recorded the prerequisite it had to fix for MCP first:

   > "without operator-set `classification_tags` they arrive untagged — and an untagged resource has no tags for any predicate to act on. **The boundary was inexpressible regardless of the policy written.**"

   Experiential memory is in exactly that state. There is no authoritative row to carry `classification_tags`, so ADR-0091's mechanism has nothing to bind to.

The exploration request and the access-control requirement are therefore **one requirement**. The table is the prerequisite for the boundary.

### Why this lane deserves the care

ADR-0049 D6/D8 store entity descriptors for `api:` / `db:` / `service:` / `repo:` kinds **including endpoints, auth and schema**, plus canonical filesystem paths and repo commit hashes. That is internal infrastructure detail, generated as a *byproduct* of doing work rather than by anyone deciding it should be retrievable. The corpus is opt-in by construction — an operator chose to ingest each document. Experience is not.

## Decision

### D1 — `experiences` is an entity table, one row per episode

An **experience** is one plan execution. Grain matches ADR-0049 D5's "one scene per plan", so a scene, its action path, and its outcome record share exactly one parent.

| Column | Purpose |
|---|---|
| `id` | Derived from the plan id, so mid-run writes can reference a parent that already exists (mirrors D5's pre-allocated scene id) |
| `session_id` | Plain grouping column, **not** a parent link — see D5 |
| `surface` | The ingress this episode's session was opened on (ADR-0090 `Session.Surface`) |
| `classification_tags` | **Authoritative.** The row policy attaches to |
| `outcome` | success / failure / partial |
| `started_at`, `completed_at` | Retention and exploration |

### D2 — `chunks.experience_id`, nullable, cascading

A second nullable parent column alongside `document_id`. Written **through a subselect**, so an id whose parent row does not exist resolves to NULL rather than violating the foreign key — ADR-0093 D5's rule stands: *a write must never fail over bookkeeping*, and losing a memory because its parentage could not be recorded is far worse than losing the link.

Deleting an experience **cascades to its chunks**, for ADR-0093 D4's reason: an orphaned chunk of a deleted parent is unreachable data that still answers searches.

This **amends ADR-0093 D3**. That decision's text is not rewritten; its claim that agent-written memories are parentless is narrowed to "parentless *with respect to `documents`*", which is what it was actually observing.

### D3 — Tags authoritative at the parent; the chunk copy is a derived cache with one writer

Identical to ADR-0093 D4 and for identical reasons. The per-chunk tag copy is **kept**, because the tag filter sits on the retrieval hot path behind a GIN index and replacing it with a join on every search would trade a correctness win for a latency regression on the most frequent query in the system.

What changes is that the cache has a single writer: retagging an experience updates the parent and rewrites all its chunks **in one transaction** — both move or neither does. Without this, the experiential lane inherits the exact half-classified failure ADR-0093 removed for documents, where a partial failure leaves some chunks reachable under new tags and some under old, with nothing able to report the disagreement.

### D4 — An experience is born tagged, and exactly one code path may write its classification

The decision that makes the boundary hold in practice rather than on paper. It has **two clauses, and the second is the one that rots**:

1. **An experience cannot be born untagged** — the default.
2. **There is exactly one code path that can write an experience's classification** — the chokepoint.

Clause 2 is not implied by clause 1, and stating only the default is the weaker half. **The incident, 2026-07-27:** `documents.tags` already had a sane default and the right semantics — and `SetDocumentStore(vec)` handed the ingest path the *raw adapter*, so the authoritative column was written outside the chokepoint entirely. An agent could have classified any document it ingested however it liked. The default was fine; the enforcement point was missing. Nothing in the semantics was wrong, and nothing in review caught it, because **wiring is invisible in review** — a setter taking the undecorated store looks identical to one taking the decorated store at every call site.

So the requirement is structural, not semantic: the classification writer is a single seam, the raw adapter is not reachable from any ingest or experience-write path, and a test asserts that the decorated store is what those paths actually hold. `EnableAuthorization` is the shape to copy — it replaces `q.vectorStore` with the enforcing decorator unconditionally at boot, so there is no configuration in which the unguarded store is live.

At creation the kernel stamps every experience with:
- the **surface** the producing session was opened on (`Session.Surface`, decided once by the ingress — ADR-0090 D3), and
- a default **`internal`** classification.

Properties, all required:

- **Unforgeable.** Kernel-derived, following ADR-0035: an agent may only *narrow* the classification, never broaden it, and never author it.
- **Total.** There is no code path that produces an untagged experience. This is what closes the hole ADR-0091 hit with MCP tools, and it is the same principle already applied to tools in `domain/tool_effects.go:153`, whose fallback exists so that "a tool that never grew `ClassificationTags` is still governed by something rather than nothing."
- **Free clamping.** An episode produced under `surface:airline` carries `airline`, so ADR-0087's existing `RequiredTags` clamp restricts it with no new mechanism. An operator who additionally declares `internal` a **closed tag** (ADR-0091 D1) gets deny-by-default for the whole lane from one premium env entry.

Note the direction of the guarantee: D4 does not *decide* who may read an experience. It guarantees there is always something for a policy to decide *about*. Governance stays in ADR-0085/0087/0091 where it belongs.

### D5 — Derived artifacts have no parent, and link instead

A procedure (ADR-0094) is induced from many experiences. A session narrative (ADR-0029) summarises many. Neither has one parent, and forcing one would be the lie D2 refuses.

Both keep `experience_id` NULL and record provenance in a link table:

```
experience_derivations(derived_chunk_id, experience_id)
```

One table for both, because they are the same shape: an artifact distilled from N episodes. This is also what makes ADR-0094's cross-boundary induction constraint checkable — the sources are enumerable rather than implied.

### D6 — Deletion and retention become first-class

With a parent row and a cascade, three operations exist that could not previously be written:

- **Forget an episode** — one delete.
- **Forget a tenant or a surface** — delete by `classification_tags` / `surface`.
- **Age out cold experience** — the `MemoryGC` / active-forgetting pass that `docs/research/agent-memory/SUMMARY.md` §2.11 identified as missing now has a unit to operate on, and ADR-0049 §A2's write gate keeps the volume bounded at the source.

Deletion is a **hard delete of the parent with cascade**, not a tombstone. An experience the operator asked to forget must stop answering searches.

### D7 — Threat-model boundary: OSS is operator-only (recorded assumption)

**Owner decision, 2026-07-27.** This ADR does **not** add a structural surface gate to the OSS kernel. The OSS deployment is an operator-only surface: OSS operators do not stand up external, customer-facing ingresses, and an operator who does so has accepted responsibility for what that exposes. Multi-surface and multi-tenant isolation is a premium concern, served by ADR-0087 clamps and ADR-0091 closed tags.

The assumption is recorded rather than assumed so it can be re-examined when it stops holding. **The condition to watch:** `HTTPChatIngress` ships in the *SDK* (`sdk/cambrian_agent_sdk/http_chat_ingress.py`), not in premium — only `AirlineChatIngress` is premium-side. A generic HTTP chat front door is therefore reachable by an OSS operator today. That does not change the decision, which deliberately places the consequence with the operator, but it does mean "OSS cannot create an external surface" is a **policy statement, not a technical guarantee**, and this ADR should not be read as claiming otherwise.

If that boundary is ever revisited, the change is small and known: a structural gate in OSS making experiential lanes unreachable from a non-internal surface, in the same class as the fail-closed `ScopedVectorStore` read (kernel invariant 5's deliberate inversion) and the Zero-Hardcode Rule's third exception for deterministic security gates.

### D8 — The migration is additive

Mirrors ADR-0093 D6, and for the same standing rule: **the corpus is the shared store the benchmarks measure against, and it is never destroyed to make a schema tidy.**

`experiences` is created empty; `chunks.experience_id` is added nullable. Existing experiential rows are back-filled by reconstructing parents from the only record that an episode ever existed — `metadata->>'plan_id'` on the chunks, with `DISTINCT` collapsing the N duplicates into the single row the schema should have had. Rows whose `plan_id` is absent keep NULL and are unaffected. Rollback is: drop the column and the table.

Migration `0010`, per ADR-0064's forward-only runner.

**The trap ADR-0093 recorded, checked again here.** `update_activation_strength` and `apply_ebbinghaus_decay` are plpgsql and resolve table names at call time; ADR-0093 had to redefine both against `chunks` after its rename. This ADR adds a column rather than renaming a table, so those functions are unaffected — stated explicitly because the failure mode is silent (every call succeeding, nothing decaying, no error anywhere) and the next person should not have to re-derive that it was considered.

### D9 — The derivation rule, stated once for all derived artifacts

**A derived artifact inherits its sources' restrictions. Derivation across a closed-tag boundary is refused, not unioned. For open tags, union is fine and cheap.**

Stated here rather than per-feature, because it is not a procedural-memory rule — it is a property of every distillation in the system, and writing it once is what stops the next one being built without it.

Refusal beats union for exactly the reason ADR-0091 D3 gives about grants: **unions are where these models rot.** A union quietly produces an artifact that is *slightly* restricted and reads as governed, and the blast radius grows with every source added. A refusal produces nothing, which is visible. The asymmetry is the argument — a missed abstraction costs an abstraction; a mis-derived one crosses a tenant boundary.

Compatibility is decided on the sources' `classification_tags`, never on the *content* of the artifact. Content-based judgement is precisely what no derivation pass should be making.

**Known derivations, and their status:**

| Derivation | Multi-source | Carries classification? |
|---|---|---|
| `chunk_triplets` (ADR-0053) | Yes — `sources TEXT[]` | **No classification column at all** |
| `CosineThemeClusterer.Cluster(docs []Document)` | Yes — clusters across a document set | No |
| Scene generation | Yes | No |
| Procedures (ADR-0094 D3) | Yes — many experiences | Constrained by ADR-0094 D6, enforceable via D5's link table |
| Session narratives (ADR-0029) | Yes | Same shape; same rule |

So **the laundering property is live today and does not wait for procedures.** `(h, r, t)` strings are extracted from chunk text — which, for an ADR-0049 entity descriptor, is endpoints, auth and schema detail — and distilled into an unclassified, GIN-indexed table that feeds `kgExpand`.

**Audit result (2026-07-27), recorded because the answer is narrower than the concern and the distinction matters:**

- Triplet *text* is never surfaced. `kgExpand` consumes only `t.H`/`t.T` as counting keys and `ChunksMentioningEntity` returns chunk **IDs**, not content. Relation labels are read nowhere at query time.
- Entity harvesting is scoped: `ForChunks` reads triplets only for frontier chunks the caller already retrieved through the enforcing store.
- **Chunk content was NOT scoped.** A first pass of this audit recorded that it was, on the reasoning that `mustGetDoc` fetches through `q.vectorStore` and `EnableAuthorization` swaps in the enforcing decorator at boot. That reasoning is wrong, and the correction is the finding: **`EnforcingVectorStore` embeds `domain.VectorStore` and overrides `Search` and nothing else** — its own type comment says *"Non-Search methods pass through unchanged (Search is the single SQL-building chokepoint)."* `GetByID` is an unguarded read of the raw adapter.
- `ChunksMentioningEntity` is likewise an unscoped lookup over the whole table.

So the exposure was **full content**, not identity: expansion supplied unscoped IDs and materialised the rows behind them, returning restricted chunk text — which for an ADR-0049 entity descriptor is endpoints, auth and schema. `kg2rag_enabled` defaults **true**. A separate, smaller leak sat on top: on any `GetByID` failure `mustGetDoc` returned `domain.Document{ID: id}`, a stub carrying the restricted chunk's ID (and `{docID}-chunk-{n}` ids encode their source document), which `expandedScore` floored at 0.5 and admitted.

**The generalisable lesson — "Search is the single chokepoint" was an assumption, not an invariant.** It holds only while every read goes through `Search`. KG expansion reaches rows *by ID*, so it was never covered, and nothing failed: the decorator was correctly wired and simply had no opinion about the method being called. This is the same species as D4's incident — the semantics were right and the enforcement point was absent — and it is why D4 is stated as two clauses.

The stub had its own root cause worth keeping: **the fallback predates authorization.** `return domain.Document{ID: id}` was correct when a missing row was the only way `GetByID` could fail; ADR-0085 added denial and the fallback treated them identically.

**Fixed 2026-07-27** (`internal/memory/kg_expand.go`). `mustGetDoc` is replaced by `authorizedDoc`, which applies the caller's predicate to the materialised document and returns `ok=false` — a drop, never a stub — for missing, unreadable *or forbidden* chunks. Because `GetByID` produces no denial signal, the predicate is applied to what comes back rather than to an error. `kgExpand` takes `scope *domain.TagPredicate` as a **required positional parameter** rather than an opts field, so a new call site cannot silently omit it, and a nil predicate denies everything (`TagPredicate.Check` fail-closes on nil), matching `readFilter`'s contract. Regression tests `TestKGExpand_DropsForbiddenChunk` and `TestKGExpand_NilPredicateFailsClosed` were mutation-checked — both fail when the predicate check is removed.

**Also fixed 2026-07-27 — the lookup itself is now scoped.** `ChunksMentioningEntity` takes the predicate and applies it by JOINing `chunks`, which carries the per-chunk tag copy that ADR-0093 D4 deliberately keeps on the hot path so a filter need not join `documents`. Restricted IDs are no longer returned at all, rather than returned and then dropped. The SQL reuses `scopeExpressions` via a new `scopeExpressionsOn(eff, alias)` qualifier variant, so the in-memory predicate and its SQL mirror stay in one place. **A nil predicate fails closed here**, deliberately departing from `scopeExpressions`' "nil ⇒ no filter" convention: that convention is safe only *behind* the authz chokepoint, and this lookup is reached directly from retrieval with nothing in front of it.

**Making `scope` a required parameter found two more unscoped call sites**, which is the argument for the discipline rather than a defence of it:

| Call site | Flag | Status |
|---|---|---|
| `kgExpand` | `kg2rag_enabled` — **default true** | fixed |
| `applyAnchorConstraint` (`anchor_query.go`) | `anchor_constraint_enabled` — **default true** | fixed |
| `injectQueryEntitySeeds` (`query.go`) | `query_entity_seeding_enabled` — default false | fixed |

Two of the three were production defaults. None would have been found by reading the code for the one bug already known.

**The class is now closed at the decorator (2026-07-27).** `EnforcingVectorStore` **no longer embeds `domain.VectorStore`.** The embed was the root cause: an embedded interface silently forwards every method nobody overrode, so "Search is the single chokepoint" held only while every read went through Search, and the compiler could not say otherwise. All eleven methods are now written out and labelled:

| Method | Disposition |
|---|---|
| `Search` | ENFORCED (unchanged — pushes `opts.Scope` into SQL) |
| `GetByID`, `GetBatch`, `QueryByMetadata` | **ENFORCED** — filter returned rows against the ctx predicate; no predicate ⇒ `ErrScopeMissing`, as Search already behaves |
| `GetStaleMemories` | PASS-THROUGH — kernel maintenance sweeps by activation age on nobody's behalf. Pinned by a test so making it principal-facing forces the question to be reopened |
| `Save`, `SaveBatch`, `Delete`, `DeleteBatch`, `IncrementAccess` | PASS-THROUGH — writes are the *other* chokepoint's job (`EnforcingStoreWriter`, ADR-0035). One decision, one place |

The labels are not the point. The point is that **adding a method to `domain.VectorStore` now breaks this file until someone decides which it is**, instead of defaulting to unguarded.

`QueryByMetadata` mattered most: it is the primitive `precedent.go` reads through, so without it this ADR's classification would have bound to nothing on the experiential lane — the governance would have shipped and done nothing.

**Two wiring changes were required, and both are the "state the intent" pattern rather than an exemption.** Principal-facing retrieval now seeds the resolved predicate onto the context (`ctx = domain.WithScope(ctx, eff)` in `searchByType`), because Search carries its predicate in `opts` and the by-id enrichment stages have no `opts` to carry one — ctx is their only channel, and seeding once means a by-id read added later is enforced by default. Kernel-internal readers that legitimately have no principal now seed the explicit, greppable `domain.ScopeSystem` bypass: `MemoryManager.GetByID`/`GetBatch` (which the Search directly above them already declared kernel-internal) and the `upsertEntity` cache read on the write path, which would otherwise have failed closed and silently re-minted entities instead of enriching them.

**Residual.** System components that hold the *raw* adapter deliberately (GraphStore assertions, spreading engine, profile store) are unaffected and remain outside the decorator by design — that is a separate question from this one. And enforcement here filters in Go after the row is fetched; pushing these predicates into SQL, as `Search` does, would be strictly better and is not done.

## Rejected alternatives

| Alternative | Why rejected |
|---|---|
| **Keep parentage in `metadata.plan_id`** | The status quo. It is exactly the "id string convention plus a metadata key" arrangement ADR-0093 removed for documents, and it is why the boundary is inexpressible. |
| **One `experiences` table with a `kind` column** (plan / session / procedure) | Reintroduces the sin ADR-0093 fixed — one table meaning several things, one index serving all of them. D5 links instead. |
| **Session as the parent grain** | A session contains many plans with different outcomes and possibly different surfaces. Governing at session grain would either over-restrict or under-restrict every plan inside it. |
| **Tombstone deletion** | An experience the operator asked to forget must stop answering searches; a soft-deleted row that is merely filtered is one missing predicate away from being returned. |
| **Put experiences in `documents` with a synthetic document per plan** | The lie ADR-0093 D3 refused, in the other direction. |
| **A structural OSS surface gate** | D7 — owner decision; the consequence sits with the operator. |

## Consequences

**The boundary becomes expressible.** Policy attaches to an experience the same way it attaches to a document, through the mechanism ADR-0091 already shipped and validated. Combined with D4, a newly produced experience is governed from the instant it exists rather than from whenever someone remembers to classify it.

**Exploration and forgetting become ordinary operations.** Both were previously impossible rather than merely unimplemented.

**What becomes harder.** A second nullable parent column on `chunks` means every write path must decide which parent applies, and D5's link table is a third relation to keep consistent. The back-fill in D8 is only as good as the `plan_id` stamps in existing metadata; rows predating that stamp stay parentless permanently, which is acceptable because they are already unreachable by the exploration surface this ADR adds.

**Residual.** Retention *policy* (how long an experience lives by default, per surface or per tenant) is not decided here — D6 provides the mechanism, not the schedule. Whether `internal` should be a *recommended* closed tag in the premium default config is left to ADR-0091's operator guidance.

## Related

- ADR-0093 — the model this extends, and whose D3 it narrows
- ADR-0049 §A2 — the write path that attaches to these rows; D4's stamp is the classification half of A2.2
- ADR-0091 — closed tags; the enforcement this ADR makes bindable
- ADR-0090 — `Session.Surface`, the origin stamp D4 reads
- ADR-0094 §D6 — cross-boundary induction, checkable via D5's link table
- `docs/research/experiential-memory/SUMMARY.md` — the trust-class argument behind treating this lane differently from the corpus
