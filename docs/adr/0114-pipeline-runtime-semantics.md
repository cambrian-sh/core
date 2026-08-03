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
the twenty-two decisions that must be settled before any code is written.

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

#### D14a. The fencing token is per-task, not global

Each task row carries its own monotonically increasing `lease_token`, incremented on every claim.

Fencing only has to order *attempts on the same task*: the question a commit asks is "am I still the
owner of this task", never "am I newer than some other task". A global sequence would answer a
question nobody asks and would become a write-contention point on exactly the hot path this store
exists to make fast — every claim, from every worker, forever. A per-row counter is sufficient,
contention-free, and local to the row already being locked.

#### D14b. Lease expiry does not invalidate a commit — reassignment does

The fence check verifies the token and deliberately **does not** check whether the lease has expired.

If a lease lapsed but no other worker reclaimed the task, the token is unchanged and the slow
worker's result is the only result that exists. Rejecting it would throw away completed work — and
often externally-visible work, already performed — because a timer elapsed, gaining nothing: there is
no competing result to protect. Since reclaiming a task *always* bumps the token, a genuinely
superseded worker is already refused by the token check alone.

So the rule is: **it is reassignment, not the clock, that makes a result stale.** Expiry only decides
when a task becomes *claimable* again; it never decides whether finished work counts.

The interaction to keep in mind: this means a very slow worker can commit long after its lease
lapsed, provided nobody took the task. That is intended. The lease TTL is therefore a
recovery-latency knob, not a correctness boundary — shortening it makes crashed work recoverable
sooner and makes duplicate *attempts* more likely, which is precisely what the effect protocol's
idempotency keys (D1) exist to absorb.

**The live defect (`MarkExecutedOnce` loss window) is fixed by this state machine** — a claim becomes
a lease with an attempt record, not a permanent one-way flag. Whether to also patch the shipped engine
before the runtime lands is a separate call, tracked outside this ADR.

### D15. `save_to_memory` is an adapter over ADR-0108's existing contract, not a new one

**Owner directive, 2026-08-02.** The node reuses ADR-0108's existing **synchronous** substrate write
contract directly. It does not introduce a parallel request model, and the reactive engine never
touches substrate tables.

`save_to_memory` is **only a graph-node adapter**. Its entire job is to convert its typed item into
the substrate's existing command and supply a stable effect/idempotency key. Validation,
kind-registry enforcement, provenance, isolation, transactionality, refusal handling and result IDs
all remain owned by ADR-0108.

**Verified: no new OSS surface is required.** The contract is already reachable —
`app.KernelServices.Events` is `domain.EventStore`, "the substrate's typed event/observation
boundary (ADR-0108 D2)", and it is synchronous and result-returning:

```go
RecordEvent(ctx, ev Event) (id EventID, inserted bool, err error)
// Idempotent on (namespace, source_ref) when SourceRef is set: a replayed
// delivery returns the existing id with inserted=false and writes nothing.
```

Two consequences worth stating, because they simplify the design rather than complicate it:

- **The stable effect key is `SourceRef`.** The idempotency mechanism D1 asks for already exists in
  the store, so this node's exactly-once claim is *structural* rather than something the pipeline
  runtime implements or could get wrong.
- **ADR-0113's "`MemorySaver` port" is withdrawn.** It would have been exactly the parallel model this
  decision forbids. RP-3's work is the adapter and its key derivation, nothing more.

The ADR-0112 "zero new OSS surface" directive is therefore honoured here with no deviation to
ratify — unlike the three seams the ingress studio needed.

### D16. Segment rollover: loop bodies are child runs

**Decided 2026-08-02.** Of the two industry remedies D14's context names, we take AWS's: each loop
iteration executes as its own **child `LogicalRun`**, linked to a parent, rather than rolling one run
into successive journal segments (Temporal's Continue-As-New).

Why this one, for us specifically:

- **D5 already made loops own nested scopes.** A nested scope executing as a nested run is the same
  shape twice, not a second concept.
- **Everything already built keeps working unchanged** — tasks, leases, fencing, redrive, cancel and
  the dead-letter view are all keyed by run, so a child run inherits them for free. Journal
  segmentation would need a segment table, continuation-state encoding, atomic rollover and
  cross-segment queries, none of which have a counterpart in the store.
- **It bounds the thing that actually grows.** The unbounded quantity is tasks-per-run, and a child
  run per iteration bounds it structurally rather than by watching a counter.

**The rule that stops this becoming a loophole:** budgets roll up to the **root** run. ADR-0114's
budgets section already says a child run must never be a way to hide unbounded work, and this is how
that is enforced — every run carries a `root_run_id`, and work is charged against the root's counter
no matter which child created it. A per-child budget bounds one iteration; the root budget bounds the
whole tree.

**Refinement (owner, 2026-08-02): the child-run boundary is the EXECUTION SCOPE, not a storage
trick.** A child run is not "the same run's rows in another bucket". It is a first-class execution
scope, and that has consequences the storage-only reading would miss:

- **A run executes a scope, not always the root scope.** Every run records which compiled scope it
  runs — the root scope for a parent, the loop's body scope for a child — so it has its own entry
  node and its own task graph. This is what makes D5's nested scopes and D16's nested runs the same
  structure rather than two that must be kept in sync.
- **A child has its own lifecycle, and its terminal state is an observable fact.** The parent's loop
  node consumes the child's completion to decide `done` / `exhausted` / `error`. It does not inspect
  the child's tasks; it reads the child's outcome. Iterations are therefore composable in the same
  way nodes are.
- **Isolation is real.** A failure inside an iteration is contained to that child run, and what it
  *means* is the parent's decision — which is precisely the "failure is local" invariant applied one
  level up.
- **Every store operation applies to a child unchanged**: it is independently claimable, resumable,
  cancellable, redrivable and dead-letterable, because it is a run.

Cancelling a parent cancels its descendants, and a child's failure surfaces on the parent's loop node
through its `error` port. Both follow from the parent link and the scope boundary rather than needing
separate machinery.

### D17. Delivery mode is declared per node; there is no implicit default

A sink that supports neither idempotency keys nor status lookup cannot be given a safe default by us,
because the right answer is a business judgement. Such a node must declare:

```
delivery_mode: at_least_once | at_most_once
```

The **compiler refuses to publish or arm** a pipeline containing an uncapable-sink node that has not
chosen. It is a **node** property, not a connector or deployment one: two uses of the same connector
may have opposite consequences — a duplicate notification is noise, a duplicate payment is not.

**No implicit deployment default exists.** Legacy configurations keep their historical behaviour
under the D13 compatibility mode; every new node chooses.

### D18. Retry policy: bounded inheritance, two budgets, classified errors

Policy resolves through a fixed hierarchy, where an outer level can only *tighten*:

```
deployment hard ceiling
    └── pipeline default
            └── node override
```

Backoff is **exponential with full jitter**, shared with REACT-04's daemon backoff so the product has
one curve rather than two that drift.

**Two budgets, not one.** Retries are counted **per item**, so one poisonous record cannot consume
the allowance of the other forty-nine — but there is also a **cumulative per-run attempt budget**,
because per-item limits alone let 1,024 poisonous items each exhaust their own retries and become a
retry storm. The per-item limit protects items from each other; the per-run limit protects the
deployment from the run.

Errors are classified, and the classification decides retryability rather than the call site:

| Class | Behaviour |
|---|---|
| `refused` | Never retry — a constraint that rejected it will reject it again |
| `permanent` | Never retry |
| `transient` | Retry under policy |
| `ambiguous` | The connector's delivery policy (D17) decides |

**The default for an UNCLASSIFIED error is phase-aware, not fixed.** "Default to transient" is only
sound while the runtime knows the request never left the process. Once it may have, a timeout, a
connection reset or a worker crash is not evidence of failure — it is absence of evidence:

| Phase | Unclassified error defaults to |
|---|---|
| Before dispatch, or a **confirmed** rejection | `transient` |
| After possible dispatch, no receipt | `ambiguous` |

An explicit classification always wins, which is what keeps a confirmed HTTP 400 *after* dispatch
`permanent` rather than ambiguous.

For `save_to_memory` the ambiguous case still resolves by retrying, because the same projection key
(D23) makes Postgres return the original receipt rather than writing twice. That is the difference
between an internal transactional sink and an external one, and it is why D1 grants exactly-once to
one and not the other.

### D19. `ProjectionIntent` is a distinct logical record in the execution store

The execution store is the right home, but **the task must not be the only representation** — an
earlier draft of this decision proposed exactly that and was wrong.

**A `NodeTask` is an execution attempt. A `ProjectionIntent` is the stable logical mutation** that
must survive attempts, leases, worker replacement and replay. Collapsing them makes the durable
intent inherit the lifecycle of whichever attempt happened to carry it.

```
NodeTask
  └── ProjectionIntent
        ├── stable effect key
        ├── substrate command / input digest
        ├── evidence and item lineage
        ├── prepared | dispatched | confirmed | ambiguous | failed
        └── result receipt
```

They may share a transaction and a storage backend; they have **distinct identities and distinct
lifecycle states**.

**For `save_to_memory`, ambiguity normally resolves automatically.** Retrying the ADR-0108
synchronous commit with the same effect key is safe: if the first commit succeeded, Postgres returns
the existing receipt. Operator intervention is therefore mainly for *uncapable external sinks*, not
for the substrate — which is another consequence of D15's reuse.

**It does not go in `ingress_studio_projections`.** That would create a second execution authority,
which is the split-brain D11 exists to remove.

### D20. Join policy: strict on failure, successful closure on emptiness

"Fail fast on everything" is too broad — producing no items is frequently the correct result of
filtering or routing, and treating it as an error would make declared termination unusable.

| Branch outcome | Default join behaviour |
|---|---|
| Succeeded with items | Include |
| Closed empty | Continue, with an empty contribution |
| Explicitly terminated | Continue, recording the termination |
| Refused | Fail the join |
| Permanently failed | Fail the join |
| Timed out | Fail the join |
| Cancelled | Fail the join |

Overridable per join node.

**Sibling cancellation is a separate policy**, `on_terminal_failure: cancel_pending_siblings`,
**enabled by default** once the join can no longer succeed. The limit is stated rather than assumed
away: already-running external effects cannot be presumed cancellable or reversible, so their
eventual outcomes are still recorded even when their join is already lost.

### D21. Canonical ordering at joins

Ordering is specified precisely, because "deterministic" is not self-evident:

- **normalised** item keys;
- **bytewise lexicographic ascending**;
- **duplicate keys within one join scope are rejected** rather than merged;
- grouped joins sort by canonical group key, then item key;
- the iteration path is part of identity where it applies.

**Never worker completion order, and never enqueue order** — both make a replay's result depend on
timing.

### D22. Loop iteration state is explicit and typed

`repeat_until` declares: `state_schema`, an `initial_state` expression, the child-body input schema,
the `next_state` output schema, the termination condition, and maxima for iterations, state bytes and
wall time.

Each child run receives an **immutable envelope**:

```
{ iteration, state, item, loop_scope }
```

The body returns `next_state` through a **dedicated typed output**. That value becomes the next child
run's input and is recorded in the child's outcome. There is no ambient mutation and no
general-purpose `loop.acc` — which is what makes an iteration's contribution an observable fact of
its run rather than a side effect, consistent with D16's execution-scope boundary.

**`foreach` has no shared mutable state at all.** Each child receives its item plus read-only loop
context. Shared iteration state would make the future move to parallel iteration nondeterministic,
and that door should not be closed by an accident of the sequential implementation.

#### D22a. Batching runs over a canonical order, and a batch's identity carries its contents

**Owner invariant, 2026-08-02.** Two properties, and neither is optional:

1. **Batching operates on a canonically ordered collection.** Partitioning is a function of order, so
   an unstable order is an unstable partition. Elements are ordered by their canonical JSON encoding,
   which is total and deterministic for anything the durable store can hold. `preserve_order: true`
   opts out for sources where position both carries information and is trustworthy — opt-in, because
   the failure it permits is silent while the failure the default permits (surprising reordering) is
   visible immediately.
2. **A batch's child-run identity is loop-node id + parent lineage + batch index + batch-content
   digest.**

The second is what makes the first enforceable rather than merely intended. A body task's key carries
the batch index, and effect keys derive from the task key — so *which* items sit in batch 3 decides
what effect key their work is recorded under. Index-only identity means a repartition silently hands
batch 3's identity to different items, and a resume adopts work it never did. With the content digest,
a repartition produces a *different* child run: the mismatch surfaces instead of corrupting.

The same reasoning applies to `repeat_until` for a different reason and needs no digest: its identity
is the iteration index and its content is the explicit `next_state`, which is itself recorded as the
child's outcome.

### D23. The projection key identifies the projection, not the evidence

D15's reuse of `domain.EventStore` is only safe if the uniqueness key identifies **the logical
projection**. Today's `source_ref` does not, and the gap was verified in code:
`ingressstudio/projection.go` composes `ingress:<id>:<delivery_ref>[:<child>]@r<mappingRevision>`.

That encodes the delivery, the *mapping's* child ordinal, and the *mapping* revision. It does **not**
encode:

| Distinction | Consequence if omitted |
|---|---|
| Save-node UUID | Two `save_to_memory` nodes consuming the same item collide; the second is silently suppressed |
| Pipeline revision | Distinct from the mapping revision; a re-authored graph looks like a replay |
| Pipeline fan-out lineage | A `split` node's children are not the mapping's children |
| Iteration path | Loop iterations suppress each other |
| Reprocess intent | Deliberate re-projection is indistinguishable from a resume |

So the adapter derives a **projection key** and supplies *that* as the event's `SourceRef` — which is
exactly what D15 means by "supplies a stable effect/idempotency key". The store's existing uniqueness
constraint and returned receipt then operate on logical projection identity:

```
proj:<pipelineID>@r<pipelineRevision>:<saveNodeUUID>:<itemKey>[#iter=<path>][~e<epoch>]:<evidenceSourceRef>
```

**Resume versus reprocess falls out of the key rather than needing a flag.** A resume reuses the same
run, revision and lineage, so it produces the same key and the store returns the original receipt —
which is why ambiguity resolves automatically here. Deliberate re-projection changes the key, and
follows the model DW-1R already established for the studio: **a new revision legitimately
re-produces**. The optional `~e<epoch>` is the explicit escape hatch for re-projecting under an
unchanged revision; without it, re-running the same revision is by definition the same projection and
suppression is correct.

**The failure this prevents is bidirectional**, which is why both halves matter: too coarse a key
suppresses legitimate remapping, and too coarse a key also lets two knowledge items from one delivery
collide. Neither is detectable after the fact.

### D24. The duplicated backoff curve is pinned by shared golden vectors

D18 requires REACT-04's full-jitter curve. The implementation cannot be shared — REACT-04's lives in
core's `internal/` tree, unreachable from premium — so the formula is duplicated, and a "must mirror"
comment is not a control. Comments drift; the drift is silent; and the symptom is a retry storm in
production.

A **canonical golden-vector file** therefore pins the curve: fixed attempt numbers, base and max
delays, and injected random values, with the expected delay for each. Both implementations assert
against the same file. A change to either curve fails that package's test with a concrete number,
which is what "must mirror" cannot do.

### D25. Provenance is a run-pinned field and an item field, never payload

RP-6's first implementation read the originating evidence out of `item.Value["evidence_ref"]`. That
is a convention, not a guarantee. `Value` is the data plane — the thing a `map` node exists to
rewrite — so a `map` that projects three fields **drops** provenance and a `map` that writes that key
**forges** it. Both are silent, and both change the projection key (D23), which is the idempotency key
of every save the run performs. The pipeline canvas was therefore an interface for editing
idempotency keys.

So:

- **`LogicalRun.Provenance`** is pinned at creation, immutable afterwards, and inherited unchanged by
  loop-body child runs. A non-zero value is the durable proof that D10's step 2 preceded step 3.
- **`Item.Evidence`** travels *beside* `Item.Value`. The runtime propagates it through every
  transform; only a fetch node may replace it (D26).
- CEL sees provenance read-only through the `evidence` variable. Forging it is not discouraged, it is
  **unexpressible** — the language has no assignment.
- Zero provenance means "this run has none" (a preview run, or one predating RP-6), which is
  deliberately distinguishable from "this run has wrong provenance". The first is migrated; the second
  is a defect.

`StartRun` does not accept provenance as an argument. `IngressGateway.Admit` is the only supported way
in, because an optional provenance parameter is an invitation to create runs without it.

### D26. A fetch node archives before it releases, and a missing preserver is permanent

D10's corollary — "any node that fetches new external data creates new evidence" — needs a mechanism,
and D6 already names `external fetch` as an external action. `fetch` is therefore a node kind with the
ordinary external-action contract (D17 delivery mode included) plus one extra obligation: its response
is archived under **derived provenance** before its successors are enqueued.

The ordering is enforced structurally rather than by convention. The successor list is built by a
function that **can fail**, and a failure there prevents the commit that would have released the data
downstream. There is no path where fetched bytes reach a save node unarchived.

Derived identity is `parent source key + /fetch:<node>:<item>[#iter]`, at the **pipeline revision**, so
page 2 is a distinct evidence row from page 1 and a re-authored graph re-fetches rather than being
suppressed as a replay. `DerivedFrom` points back at the causing evidence, making a pagination chain
walkable in both directions.

Two refusals, both fail-closed: arming a plan containing a fetch node with no preserver wired is
refused at `StartRun` in D12's shape, and a missing preserver at runtime is classified **permanent**
rather than transient — no number of retries wires a dependency into a running scheduler, so retrying
only delays the operator reading the error.

### D27. Two guarded exceptions to the intent/task coupling

D19 separates the intent from the attempt. RP-6 found the two places where that separation must be
expressible in the store, and each is a *narrow, precondition-checked* exception rather than a general
loophole:

- **`ConfirmIntentOnly`** records a confirmed effect **without** settling its task. Between a call
  returning and its successors being ready there is post-effect work that can fail — archiving what a
  fetch fetched — and when it does, two things are true at once: the effect happened and must never
  repeat, and the task has not finished and must retry. `ConfirmIntent` cannot express that, because it
  settles both in one transaction. Fenced like every other in-flight transition.
- **`ResolveOrphanIntent`** settles an intent **without a fencing token**, and is the only fenceless
  intent transition in the store. It is safe solely because the store verifies the orphan precondition
  *inside the same transaction as the write*: if the task exists and is not terminal, the call is
  refused with `ErrIntentNotOrphaned`. Checking ownership in the reconciler instead would leave a
  window where a task is revived between the check and the write — exactly what fencing prevents
  everywhere else. An **absent** task is orphaned by the strongest definition and is allowed through.

### D28. What the reconciler may repair, and what it can never invent

D11 says the reconciler "completes unresolved intents and nothing else". Implementation makes the
boundary concrete.

**Ownership is decided by the task, not by a lag.** While a task is live the scheduler resolves its own
intents through the normal retry path. The intents that need anyone else are the **orphans** — unsettled
intents whose task has reached a terminal state and will never be claimed again: a run cancelled
mid-effect, a retry budget exhausted after the call left the process, a worker that died between
dispatching and confirming. ADR-0113's deliberate lag is not needed and is not used; task state answers
the ownership question exactly.

Per orphan, by connector capability: `prepared` → recorded as never performed (the request demonstrably
never left the process); status lookup → asked, and a definite *no* is recorded while an **unanswerable**
lookup **escalates rather than guessing**, because "I could not tell you" is not "it did not happen";
idempotent or transactional sink → re-sent under the same effect key; at-most-once with no confirmation
→ recorded ambiguous, never re-sent. A re-send whose recorded input no longer matches the intent's digest
escalates instead, because it would be a different command than the one intended.

**And the property that dissolves the split-brain:** an item the graph deliberately routed away has no
intent, so there is nothing for the reconciler to act on. It cannot resurrect a discard — not because a
lag window makes it unlikely, but because the record it would need does not exist.

### D29. Two checksums, because pinning and comparison are different questions

D3 requires a run to pin its plan checksum, so that checksum embeds the revision number: r4 and r5
must be different by construction even when their content is identical, or a run could execute
against a revision it did not start under.

That makes it structurally unable to answer the other question an operator has — *"does this edit
change what the pipeline does?"* — because every revision differs from every other. So the compiler
emits **two** fingerprints over the same canonical form:

- **`Checksum`** includes the revision. Used for pinning, and for nothing else.
- **`SemanticsChecksum`** omits it. Two revisions that differ only cosmetically compare **equal**.

Both are built from **per-node semantic hashes** (D3's "per-node semantic/config hashes", previously
unimplemented), which is what lets a revision diff say *which* nodes changed rather than only *that*
something did. Display name and layout are excluded by construction, not by filtering — renaming a
node or dragging it across the canvas changes no hash, so a cosmetic edit can never invalidate a run.

### D30. The lifecycle enforces immutability in the store, not only in the API

D3's states are unchanged: `draft → validated → published → armed → retired`. What RP-7 settles is
where the rules live.

- **Editing forks.** `Draft` is the only way to change a pipeline, and it always produces a NEW
  revision at `n+1`. Nothing returns to `draft`, because reopening a published revision is precisely
  how it would become mutable.
- **The store refuses content changes to a frozen revision**, independently of the registry. A rule
  enforced only by the layer above is a rule the next caller bypasses, and D3's guarantee is about
  what can exist in storage. State transitions on a frozen revision remain legal — publishing, arming
  and retiring all rewrite the row — but content changes are refused with `ErrRevisionImmutabe`.
- **`validated` is the compile gate.** D4 says the runtime executes only compiled plans, so a
  revision cannot be published without compiling, and cannot be armed without being published. An
  uncompilable graph is therefore unarmable at every step rather than at the last one.
- **Arming is exclusive, and disarming returns to `published`** — not to retired, because
  `published` is exactly the state rollback needs the previous revision to be in. Rollback is
  therefore `Arm` pointed backwards: it introduces no state the system was not already in, which is
  what makes it trustworthy under pressure.
- **Arming never disturbs runs in flight.** They pinned their checksum at creation and the scheduler
  verifies that pin per drain. Arming changes what the next delivery starts, and nothing else.

### D31. A dry run is a real run with the sink replaced

REACT-05's dry run evaluates the condition and stops. For one condition and one action that is the
whole question; for a graph it is barely the beginning — the operator needs to know which branch each
item took, what the aggregate came to, how many items the discard port swallowed, and above all what
*would* have been written and sent.

So a dry run here executes the real compiled plan through the real scheduler over a real execution
store. Every deterministic node genuinely runs. The single substitution is at the **dispatcher**, and
the position matters: short-circuiting any earlier would skip exactly the machinery most worth
testing — effect-key derivation, projection identity, barrier obligations, retry classification. Those
all run, which is why a dry run can surface a **colliding effect key** or a deadlocked join rather
than only a routing mistake.

Consequences that follow from the position of the substitution:

- **Fetched material is shadowed too.** Archiving a synthetic body would put fiction in the evidence
  archive, which is worse than a dry run that cannot exercise a fetch for real.
- **Pinned receipts make a dry run repeatable.** Given the same pinned data, the same routing and the
  same derived keys — so a diff between two dry runs means the *pipeline* changed.
- **D12 is relaxed only under shadow, and the flag is unexported.** The gate asks whether an effect
  could reach the world from a store that cannot remember it did; under shadow nothing reaches the
  world, so the question does not arise. The relaxation is set only by `DryRun`, together with
  substituting every dispatcher — because a flag that could be set alone would be a way around D12
  rather than a consequence of it.

### D32. Node tests refuse effect nodes rather than faking them

A single-node test — pin an item, pick a node, see which port it takes and what comes out — is
restricted to deterministic nodes. An effect node cannot be tested in isolation without either
touching the world or claiming to have done something it did not, and a node test that silently faked
a save would be the most misleading result the product could produce. The refusal names the dry run
as the honest alternative.

### D33. Ingress pipelines run side by side, and an unarmed ingress has no pipeline

**Owner decision, 2026-08-02.** The new runtime reaches production through **ingress pipelines only**.
Existing watches keep running on the shipped engine, untouched. Nothing is migrated by this; a
pipeline is *added* where one is armed.

**The attachment point is already correct, so nothing is rebuilt.** The ingress studio owns a working
delivery lane: a transport stages bytes and emits a reference-only signal, and the studio's own
kernel-side action preserves the delivery as evidence under ADR-0105's ordering. Evidence is durable
before anything downstream runs — which is exactly D10's precondition, already met. The pipeline
runtime therefore does not re-preserve, re-fetch, or take over the transport. It attaches where
evidence becomes durable and starts the ingress's armed pipeline from it.

D10 still holds here without the router preserving anything, and by construction rather than by
convention: routing requires valid `Provenance`, and `Validate` refuses provenance without an evidence
id. **A caller that has not preserved cannot construct an argument that would start a run.**

**An ingress with no armed pipeline is not a reactive pipeline at all.** `ErrNoPipelineForIngress` is
a sentinel, not an error string, because during side-by-side operation it is the *normal* answer and
callers must be able to branch on it. An ingress that has not been armed must not appear in the
reactive panel.

**The default graph is generated, not blank.** An ingress already knows how to save what it receives,
so a new one arrives with a pipeline that works, and the operator edits from there. The generator
lifts the mapping's **control flow** into visible nodes and leaves its **value transform** alone:

| Mapping construct | Becomes |
|---|---|
| discard rule | a `choice` whose matched port is declared terminated, named with the rule's own name |
| fan-out | a `split` carrying the mapping's cardinality cap onto the pipeline budget |
| roles, observations, event type, coercions, timestamps | unchanged, inside the mapping, reached through the save node |

Re-implementing the mapping as a pile of `map` nodes is the obvious mistake and is rejected: it would
fork the transform into two implementations that must agree forever, and it would put the closed
mapping language's guarantees — missing ≠ null ≠ empty, named destructive filters, deterministic
refusal — behind a graph that cannot express them. The save node stays an adapter (D15); the mapping
stays the authority on what an envelope contains.

Two ordering rules fall out and are enforced: **discards precede fan-out**, because a discard rule
tests the delivery and evaluating a document-level predicate against a member would silently stop
matching; and **generation is deterministic** for an unchanged mapping, or every regeneration would
look like an edit and fork a pointless revision.

The generated pipeline is a **draft**. Arming follows the ingress's own lifecycle — the studio's
`DRAFT → CAPTURING → MAPPED → DRY_RUN → ARMED ⇄ PAUSED → RETIRED` maps onto the pipeline's
`draft → validated → published → armed → retired`, so the two states track rather than drift.

### D34. The ingress lifecycle drives the pipeline lifecycle

D33 established that an ingress owns a pipeline. What makes that usable is that the operator moves
**one** lifecycle and the other follows — two state machines that can drift are worse than one that is
merely limited, because the reactive panel would show a live pipeline for a paused source.

The binding is called from the studio's own transitions, not discovered afterwards by a reconciler: a
reconciler noticing the drift later would be a second authority over which pipeline is live, and D11's
whole point is that there is exactly one.

| Ingress transition | Pipeline |
|---|---|
| `CAPTURING → MAPPED` | generate, validate, **publish** — inspectable and dry-runnable, not live |
| `DRY_RUN → ARMED` | arm the revision built for **the release's mapping revision** |
| `ARMED → PAUSED` | disarm, back to `published` |
| `PAUSED → ARMED` | re-arm the revision for the **pinned** release (after a rollback, deliberately not the newest) |
| `* → RETIRED` | retire every revision |

Four consequences worth stating, because each is a way this could have been subtly wrong:

- **Publishing is not arming.** Confirming a mapping produces something the operator can inspect and
  dry-run. Only their explicit Arm makes it live, which is what keeps "an unarmed ingress is not a
  reactive pipeline" true (D33).
- **Arming resolves by mapping revision, not by recency.** A release pins a specific mapping; arming
  the newest graph regardless would silently arm one built for a different transform. The
  correspondence is read off the generated save node's `mapping_revision`, which already had to be
  there — a second copy would be a second thing to keep in step.
- **Regeneration never discards edits.** Re-confirming a mapping that already has a pipeline generates
  nothing. And because an edited graph for mapping revision 3 is still *the pipeline for mapping
  revision 3*, arming picks the operator's newest edit rather than reverting to the generated original.
- **A generation or arming failure does not roll the ingress back.** The mapping really was confirmed,
  and reversing the ingress would misreport that; the operator is told what failed instead.

**The revision store must be durable.** An in-memory registry loses the operator's live pipeline on
restart while the ingress still reports itself `ARMED` — precisely the drift this decision exists to
prevent. `PgPipelineStore` persists revisions, enforces content-immutability in the store rather than
only in the registry, and still permits the state transitions that publishing, arming and retiring
legitimately make.

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
| **RP-3** | Effect protocol: intents, stable idempotency keys, dispatcher, `ambiguous`, retry/catch policy, typed error outputs. `save_to_memory` first (strongest idempotency) — as an **adapter over `domain.EventStore`** per D15, not a new port; connectors only once their capability contracts are explicit. |
| **RP-4** | Barriers and aggregation: fork obligations, branch closure, explicit merge/join, deterministic ordering, empty/failed/timeout policy. |
| **RP-5** | Structured loops: `foreach`, `repeat_until`, iteration paths, continuation state, child/segment summaries. |
| **RP-6** | Ingress integration and provenance: preserve-before-run, provenance propagation, external-fetch preservation, projection-intent reconciliation replacing independent repair. **Implemented — see D25–D28.** |
| **RP-7** | Authoring and operations: canvas, pinning isolated from production, node tests, shadow-effect dry runs, backtests, per-run/node/edge inspection, cancel/redrive, draft→publish→arm→rollback and revision diff. **Runtime half implemented — see D29–D32. Canvas, contract bump and backtest-over-journal outstanding.** |

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
