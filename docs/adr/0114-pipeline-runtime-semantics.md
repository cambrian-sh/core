# ADR-0114: Pipeline runtime semantics — the Phase-0 contract

**Status:** Proposed (design only; no code written)
**Date:** 2026-08-02
**Amends:** ADR-0113 (reactive pipeline graphs). ADR-0113 remains the record for the graph *model*;
this ADR settles the *runtime semantics* it left open or got wrong, and must be approved before RP-1.
**Origin:** external architecture review of `REACTIVE-ENGINE-RESEARCH.md` and
`REACTIVE-ENGINE-SPEC-PLAN.md`, 2026-08-02, which declined to approve the specification unchanged.
**Related:** ADR-0061 (durable reactive execution), ADR-0062 (backpressure), ADR-0063 (condition
injection hardening), ADR-0071 (watch observability), ADR-0090 (ingress identity), ADR-0105
(evidence), ADR-0108 (event-shaped knowledge), ADR-0110 (kind registry), ADR-0112 (ingress studio).

## Context

ADR-0113 describes a node-graph reactive engine: visual authoring, branching, bounded loops,
aggregation, a native memory save, durable replay. An architecture review found the direction sound
but the runtime semantics under-specified in ways that would have shipped defects. This ADR records
the fourteen decisions that must be settled before any code is written.

### The finding that motivates the whole ADR

The review's central objection: **memoizing a completed node result does not make an external effect
execute once.** A worker can call an external service successfully and crash before recording the
response. On recovery the runtime must either call again (risking duplication) or not (risking loss).
There is no third option without the sink's participation. ADR-0113's invariant 3 ("effects happen
once") is therefore not implementable and must be replaced.

### A live defect found while verifying the review

The review claimed the *current* engine is not exactly-once either. **Confirmed in code.**
`MarkExecutedOnce` is called at `reactive_engine.go:697` and that is its only call site in the
package — there is no release, no unclaim, no pending state. The sequence is:

```
claim (MarkExecutedOnce) → runAction → advanceCursor
```

If the process dies between the claim and `advanceCursor`, the cursor was never advanced, so the
signal replays — and `MarkExecutedOnce` now returns `false`, so the engine **skips the work and
advances the cursor**. The action never runs and nothing records that it did not. This is
**at-most-once with a silent loss window**, under a code comment that reads "exactly-once claim".

This is a production defect independent of the redesign. It is recorded here as motivation; its fix
is tracked separately (see D14).

### Corrections to the source documents

| Where | Claim | Correction |
|---|---|---|
| research §1 | Temporal Activities "run once" | Activities are **at-least-once**; only a *recorded completed* result is not re-executed. Temporal recommends idempotency keys for exactly this reason |
| research §1 | Restate gives "exactly-once without idempotency keys" (third-party source) | Scope strictly to invocations inside Restate's own protocol, and cite the vendor |
| research §3 | n8n item linking is its "most-reported pain" | A demonstrated failure mode, not a market ranking |
| spec plan §II.2 | Temporal cap "50,176 events" | **51,200** (50Ki). The research doc had it right; the spec plan did not |
| research §landscape | Step Functions / Airflow classed as stream-dataflow | Step Functions is a durable orchestrator; Airflow is a batch scheduler. Reclassify by *lesson borrowed*, not by product category |

## Decisions

### D1. Delivery guarantees are a matrix, not an invariant

ADR-0113 invariant 3 is withdrawn. Guarantees are stated per operation class:

| Operation class | Honest guarantee | Mechanism |
|---|---|---|
| Deterministic node | One logical output per task key | Atomic result commit; replay reads the result |
| Internal knowledge save (same transactional store) | **Exactly-once logical mutation** | Unique effect key + transaction/upsert |
| External sink with idempotency-key support | At-least-once attempts, one logical sink effect | Stable key sent to the sink; response recorded |
| External sink with status lookup | At-least-once attempts, ambiguity auto-resolved | Stable key + status query |
| External sink with neither | Configurable at-least-once **or** at-most-once; duplicates or loss possible | Explicit policy, operator-visible warning |

**`ambiguous` is a first-class terminal state**: the request may have succeeded but we hold no durable
acknowledgement. It is never silently promoted to success or demoted to failure. It surfaces to the
operator and to the `error` path.

**Consequence:** `save_to_memory` — writing our own Postgres — is the *only* node that can claim
exactly-once, and it is the one that matters most. That is a genuine architectural advantage and
should be stated rather than diluted by claiming it everywhere.

### D2. Two keys, not one

- **Delivery-deduplication key** — recognises duplicate ingress deliveries within a policy window.
  This is what today's `DedupWindow` (default 5m) is for.
- **Logical execution/effect key** — stable for the entire resume/redrive lifetime of a run, derived
  from logical run + pipeline revision + node UUID + item lineage + iteration path.

A five-minute dedup bucket must never define durable execution identity. Conflating them is what
produces the loss window described above.

### D3. Immutable revisions; every run is pinned

Every run pins: pipeline ID + immutable revision ID, compiled-plan checksum, internal node UUIDs
(independent of display names), per-node semantic/config hashes, expression-compiler version, and the
kind-registry revision where relevant.

Lifecycle: `draft → validated → published → armed → retired`. Editing creates a new draft revision.
Rollback arms an older immutable revision. **An edited canvas can never change the topology of a
running or replayed run.**

### D4. Compile before execute

Canvas JSON is never interpreted directly. A compiler resolves the revision, validates typed ports and
cardinality, requires every output connected *or declared terminated*, compiles and cost-bounds
expressions, validates loop scopes and barriers, computes a theoretical work bound from configured
caps, classifies every node by runtime class (D6), and emits a canonical plan + checksum. **The
runtime executes only compiled plans.**

### D5. The outer graph is acyclic; loops are structured scopes

ADR-0113 allowed arbitrary cycles provided each passed a capped `loop` node. That makes static
validation, scheduling, joins, recovery and UI explanation harder than necessary. Instead:

- the outer execution graph is **always a DAG**;
- `foreach` owns a nested DAG body;
- `repeat_until` owns a nested sequential body with **explicit iteration state**;
- retry is a **policy** on a node or scope, never a dataflow loop;
- loop outputs are `done`, `exhausted`, `error`;
- each iteration has an immutable path and output.

**This preserves the owner's decision to have loops.** Batching, pagination and repeat-until are all
still expressible; only the stored and compiled form changes. The one reduction: ADR-0113's freely
mutable `loop.acc` becomes explicit, typed iteration state — which still carries a pagination cursor,
but cannot be a general mutable accumulator. The canvas may still draw a return arrow.

### D6. Node runtime classes replace the pure/effect binary

ADR-0113 split nodes into "pure" and "effectful". That is too coarse, and it produced a direct
contradiction: it called `if` nodes pure and recomputable on replay *while* leaving the `llm`
condition evaluator untouched. An LLM call is nondeterministic external work; recomputing it on
replay re-bills the model and can route differently, diverging the replay.

| Runtime class | Examples | Replay behaviour |
|---|---|---|
| Deterministic operator | CEL filter/choice, map, schema transform, split | Recompute |
| Stateful barrier | aggregate, merge/join, branch closure | Rebuild from durable barrier state |
| **Durable decision** | **LLM classify/route/extract** | **Read the recorded decision; never re-call** |
| External action | notify, emit, dispatch agent, start plan, external fetch | Effect protocol (D1) |
| Internal transactional action | `save_to_memory` | Exactly-once via unique effect key |
| Structured control | foreach, repeat_until, scoped retry/catch | Resume from iteration state |

A durable decision records: input digest, prompt/template version, model + provider + version, output,
and usage metadata.

### D7. Expressions: CEL

**Decision: adopt CEL (`cel-go`)** rather than growing our hand-written grammar.

Rationale: CEL is non-Turing-complete by design, type-checkable, compile-once/evaluate-many, and
cost-boundable — and the type checker is what makes D8's static reference validation actually
enforceable rather than aspirational. Our hand-written grammar cannot type-check.

**This is a deliberate deviation from the codebase's minimal-dependency posture** (we hand-wrote the
cron parser rather than take a dependency; ADR-0065 avoided a vendored proto). It is accepted by the
owner because expression *safety* and *static checking* are load-bearing here in a way that cron
parsing was not. Recorded so the deviation is legible rather than accidental.

Constraints, non-negotiable:

- **Deterministic function set only.** No I/O, no clock, no randomness, no global mutable state, no
  dynamic code loading. Any CEL extension that reaches outside the evaluation is refused.
- **Compile at pipeline-compile time**, cache programs, enforce a cost bound; refuse expressions above
  it at save time.
- **Payloads remain data.** The ADR-0063 payload-as-data discipline is unchanged and now applies to
  the deterministic path too.
- Existing stored conditions keep working through the legacy executor (D13); they are not rewritten.

The prohibition that stands regardless: **no general-purpose interpreter, no Code node, no `eval`.**
Operators needing arbitrary logic get `dispatch_agent` to a sandboxed agent.

### D8. References must be statically valid

`nodes.save.saved_count` is ambiguous after fan-out, loops or joins — which invocation, which item?
Only these are addressable:

- `item` — the current immutable item;
- `signal` / `evidence` — triggering evidence metadata;
- outputs of a **uniquely correlated ancestor** in the current item's lineage;
- collections **only** after an explicit aggregate/join has created them.

There is no global map of upstream node results. Ambiguous references are rejected at compile time by
type and cardinality checking.

### D9. Joins require a durable branch-closure barrier

An aggregate cannot know it has seen "all" items by watching its inputs: a branch may emit zero items,
terminate deliberately, refuse, fail, or fan out again. `wait-for-all` would deadlock.

At fork time the runtime creates a durable **scope/barrier token** carrying the expected branches or
producer count. Every branch must eventually report exactly one of: *items + closed*, *declared
termination + closed*, *refused + closed*, *failed/cancelled + closed*. Only then may the join
schedule. Join policy must declare behaviour for failed, refused, empty and timed-out branches.
**Inputs are canonically ordered before hashing or aggregating**, so worker completion order cannot
change results or item identities.

Multi-input nodes require an **explicit** `merge`; ADR-0113's implicit append is withdrawn.

### D10. Raw preservation is an ingress transaction, not a graph node

ADR-0113 made `preserve_raw` a pinned node. That is not strong enough: preservation is a
**precondition for a run existing**, not the first step inside it. The ingress transaction is:

1. accept delivery;
2. durably preserve raw evidence + provenance;
3. create/enqueue a run referencing that evidence;
4. acknowledge per the ingress protocol.

The graph cannot bypass, reorder, retry or version this. The canvas renders a locked boundary badge
*before* the trigger.

**Corollary, missed by ADR-0113:** any node that fetches new external data **creates new evidence**. A
pagination loop must preserve each fetched response — or a well-defined response bundle — before it can
become knowledge. Otherwise the ADR-0112 invariant holds at the front door and leaks everywhere else.

### D11. The graph is the only authority; the reconciler fulfils intents

ADR-0113 kept the outbox consumer as a "floor" that independently decided what knowledge should exist,
and tried to stop it undoing deliberate discards with termination records plus a lag. That is a
split-brain patched, not removed.

Instead:

- the **graph alone** decides whether knowledge is produced;
- reaching `save_to_memory` atomically writes a durable **`ProjectionIntent`**;
- the dispatcher/reconciler **completes unresolved intents** and nothing else;
- **no intent ⇒ no save**, permanently;
- the legacy floor survives only for legacy watches or an explicitly configured "save all evidence"
  compatibility policy.

The floor repairs *declared but incomplete* effects; it never infers missing ones. This dissolves
ADR-0113 §B5 — the hazard it called most likely to fail — by construction rather than by mitigation.

### D12. Fail closed when durability is unavailable

`Journal == nil` currently means no durability, no idempotency, no replay, no dead-letter — silently,
engine-wide. That is incompatible with advertising durable pipelines.

- Validation, preview and explicitly-marked best-effort/pure flows: allowed.
- Effectful durable pipelines with the execution store unavailable: **pause or refuse to arm**.
- A loud health state and metric.
- **Never silently downgrade an armed durable pipeline.**

### D13. Legacy watches get an executor, not a translation

ADR-0113 promised every stored watch would behave "byte-identically" through `EffectiveGraph()`. Too
strong: old arms are independent, while the new model has per-item failure, per-node claims and
dependency ordering. Translating arms into graph nodes changes failure and replay behaviour.

A `legacy_watch` compatibility executor preserves whole-watch semantics for existing configs. New
pipelines use new semantics. Migration is explicit and verified against **golden execution traces**
covering arm order, failures, dead letters, deduplication and replay. The goal is **behavioural
compatibility, not byte identity.**

### D14. Persistence target: PostgreSQL for execution state

The review left bbolt-vs-Postgres open, contingent on benchmarking. For Cambrian it is not open:
**Postgres is already a hard dependency** — the substrate, evidence and every ingress-studio table
live there, and the studio refuses to construct without migrations. So Postgres for the execution
store adds **zero new dependency**, while bbolt permits only one read-write transaction at a time,
which would make its single writer the throughput ceiling exactly where per-node/per-item journaling
multiplies write volume.

- Execution store: **PostgreSQL**, with `FOR UPDATE SKIP LOCKED` for worker claims, unique effect
  keys, durable task rows, leases and a transactional outbox.
- The bbolt watch/config store stays where it is; this is a new store, not a migration.
- No broker or in-memory channel is ever the authority; it may only wake workers.

**Store both** immutable execution events (audit, replay evidence) and materialised run/task/effect/
barrier records (efficient scheduling) — the scheduler must not rebuild state by replaying a journal
for every decision. Large payloads and effect responses are content-addressed blobs; the journal holds
digests, metadata and small values, or byte limits arrive before event-count limits.

Durable task state machine, with leases carrying owner, expiry, attempt number, heartbeat deadline and
**fencing token** so a late worker cannot commit after reassignment:

```
ready → leased → running → succeeded
                    ├── refused
                    ├── retry_wait → ready
                    ├── failed
                    ├── ambiguous
                    └── cancelled
```

One transaction per deterministic node completion: verify fencing token → store result and output
envelopes → close the consumed input obligation → enqueue ready successors or update the barrier →
append the audit event → mark terminal.

**The live defect (`MarkExecutedOnce` loss window) is fixed by this state machine** — a claim becomes
a lease with an attempt record, not a permanent one-way flag. Whether to also patch the shipped engine
before the runtime lands is a separate call, tracked outside this ADR.

## Budgets

ADR-0113's single "1024 items" ceiling, borrowed from Airflow's `max_map_length`, is withdrawn as a
universal default. A 1,024-item fan-out is trivial or catastrophic depending on item size, effect
count and downstream cost. Budgets are multidimensional and set from load tests:

max emitted items · max input/output bytes · max node executions · max journal records and bytes ·
max effect attempts · max loop iterations and nesting depth · max aggregate bytes · max run wall time ·
per-connector concurrency and rate limits.

Both a **per-segment** budget and a **root logical-run** budget apply; segmentation bounds history
size, not total work, and a child run must never be a way to hide unbounded work from root limits.
1024 may remain an absolute ceiling, but not as the justification.

**Retention is per-class**, and active runs are never pruned by the current fixed 1h TTL: raw evidence
· active execution state · completed execution/audit · effect-idempotency receipts · pinned test data ·
metrics. Effect keys must outlive any resume/redrive that could reuse them. Execution outputs can
contain sensitive ingress data, so redaction, encryption, access control and per-node retention apply.

## Revised slice order

| Slice | Content |
|---|---|
| **Phase 0** | This ADR, approved. |
| **RP-1** | Compiler + compatibility shell: revisions, node UUIDs, canonical plan + checksum, typed ports and cardinality, declared termination, legacy executor. Deterministic nodes only; no external actions. |
| **RP-2** | Durable execution core: logical runs, leases + fencing, attempts, state transitions, item lineage, atomic result + successor scheduling, events + materialised state, multidimensional budgets, segment rollover, **cancellation, deadlines, recovery, dead-letter/redrive API**. |
| **RP-3** | Effect protocol: intents, stable idempotency keys, dispatcher, `ambiguous`, retry/catch policy, typed error outputs. `save_to_memory` first (strongest idempotency); connectors only once their capability contracts are explicit. |
| **RP-4** | Barriers and aggregation: fork obligations, branch closure, explicit merge/join, deterministic ordering, empty/failed/timeout policy. |
| **RP-5** | Structured loops: `foreach`, `repeat_until`, iteration paths, continuation state, child/segment summaries. |
| **RP-6** | Ingress integration and provenance: preserve-before-run, provenance propagation, external-fetch preservation, projection-intent reconciliation replacing independent repair. |
| **RP-7** | Authoring and operations: canvas, pinning isolated from production, node tests, shadow-effect dry runs, backtests, per-run/node/edge inspection, cancel/redrive, draft→publish→arm→rollback and revision diff. |

Dead-letter and redrive move from ADR-0113's RP-8 into **RP-2**: they are needed the moment per-item
failure exists, not at the end.

**One sequencing risk, recorded rather than resolved.** This order puts the canvas last, which is
correct — the UI should follow settled semantics rather than discover them. But the owner's motivating
requirement was *seeing* pipelines, and a long semantics-only phase ships nothing visible. Mitigation:
a **read-only** canvas rendering compiled plans and live run state can land as early as RP-2, with
authoring deferred to RP-7.

## Acceptance gates

1. **Crash after external acceptance, before result commit** — the effect becomes `ambiguous`; an
   idempotent connector resolves it with no duplicate logical effect.
2. **Lease fencing** — a timed-out worker cannot commit after another worker completed the task.
3. **Atomic successor scheduling** — a crash at every write boundary yields neither lost nor duplicate
   successor tasks.
4. **Revision pinning** — editing and arming a new revision cannot change an active run or a replay.
5. **LLM replay** — recovery uses the recorded decision and does not call the model again.
6. **Barrier closure** — joins complete with empty, terminated, refused, failed and successful
   branches per declared policy; none deadlock.
7. **Deterministic merge** — different worker completion orders produce identical aggregate output and
   identical item IDs.
8. **Structured-loop recovery** — a crash at every iteration boundary resumes with the same iteration
   state and respects root and segment budgets.
9. **Budget exhaustion** — work is refused or routed to an explicit budget outcome; never truncated,
   never silently segmented into unlimited work.
10. **Durability unavailable** — effectful pipelines pause or refuse; they never silently run
    best-effort.
11. **Reconciler authority** — deliberately terminated items never become knowledge; incomplete
    projection intents are repaired.
12. **Compatibility** — legacy watches match golden traces for arm order, failures, dead letters,
    deduplication and replay.
13. **Retention** — active runs and required idempotency receipts survive GC; sensitive node outputs
    are redacted per policy.
14. **Load test** — realistic item sizes, fan-out, action mix, crash rate and fsync behaviour set the
    budget defaults, rather than importing another engine's limits.

## Identity

The system this describes is a **durable evidence-to-knowledge projection runtime**, not a general
workflow engine. That narrower identity is an asset: it is what justifies the closed expression
language, explicit provenance, typed knowledge saves, declared discards, bounded control flow, and an
execution surface far smaller than n8n's or NiFi's.

## Consequences

- ADR-0113's invariants 3 (effects once), 7 (arbitrary capped cycles), 8 (unwired implies terminate),
  9 (single 1024 cap) and 12 (byte-identical legacy) are superseded by D1, D5, D10, the budgets
  section and D13 respectively. The remaining invariants stand.
- ADR-0113 Part B5 (routing versus reconciliation) is dissolved by D11.
- The three companion reports (`REACTIVE-PIPELINES-REPORT.md`, `REACTIVE-ENGINE-RESEARCH.md`,
  `REACTIVE-ENGINE-SPEC-PLAN.md`) require revision to match this ADR before they are circulated
  further; the corrections table above lists the specific claims to fix.
- `cel-go` enters the dependency set — the first deliberate exception to the minimal-dependency
  posture, justified by static checking being load-bearing for D8.
