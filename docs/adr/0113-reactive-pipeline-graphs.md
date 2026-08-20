# ADR-0113: Reactive pipeline graphs — composable signal → memory ingestion

**Status:** Implemented
**Date:** 2026-08-02
**Status corrected 2026-08-20.** This record read *"Proposed (design only; no code written)"*
while the graph engine was running. The model shipped: `pipeline/graph.go`, `compile.go`,
`loop.go` and `dryrunner.go` in cambrian-premium (node graph, branching, aggregation and
bounded loops as specified), `app.ReactiveWatchStore` with `WriteWatchConfig`/`DeleteWatchConfig`
in cambrian-core, the visual authoring canvas at `ui/src/screens/plan/DagBuilder.tsx`, and
`ingress_studio_projections` in `ingressstudio/pg_store.go`. Residual items from the RP series
and `cambrian-premium/docs/defects/` are not closed by this correction — the status flip records
that the model exists, not that every slice landed. See ADR-0128 §8.
**Supersedes:** two earlier drafts of this ADR, both wrong in the same direction — they shrank the
model to what today's engine could nearly do (first: per-ingress watches over the existing arms
model; second: a linear step list). The owner's direction is a **node graph with an item stream**,
authored visually, with branching, aggregation and bounded loops.
**Related:** ADR-0090 (ingress and surface identity), ADR-0032 (reactive rule engine), ADR-0061
(durable reactive execution), ADR-0062 (backpressure), ADR-0071 (watch observability), ADR-0104
(reactive-first), ADR-0105 (evidence), ADR-0108 (event-shaped knowledge, evidence outbox),
ADR-0110 (kind registry), ADR-0112 (ingress studio foundations), contract 0076 (multi-arm watches).

## Context

The owner's picture: build an ingestion pipeline and **view and create it as a diagram**, n8n-shaped.
Do operations on a signal *anywhere* — before the memory save, after it, or not at all. Aggregate.
Branch with if/else. And the memory save itself must be a **reactive-engine-native operation**, not
an ingress-studio private action.

### Vocabulary, corrected

An **ingress** is *every point where the outside world enters* — ADR-0090's subject: chat and
messaging surfaces, webhooks, pollers, websockets, API clients and servers, inbound HTTP. The kernel
already models this: a Postgres-backed ingress registry with namespaces, the `domain.IngressResolver`
port, and `Session.Surface`.

**Ingress Studio (ADR-0112) is a builder for the declaratively-configurable subset** of ingresses
(webhook / poll / websocket, with verification and a versioned mapping). It is one producer of
ingresses, not the definition of one.

This matters because it generalises the whole design: **every ingress can have a pipeline**, and the
same canvas serves a Telegram message and a Stripe webhook.

### The finding that decides the "native memory save"

A native memory-save action already exists and is structurally unfit. The built-in `ingest` action
(ADR-0032) is, in full:

```go
func (e *IngestExecutor) Execute(ctx context.Context, action domain.WatchAction, signal domain.Signal) (*domain.Handoff, error) {
	meta := map[string]string{}
	if action.Target != "" {
		meta["source_tag"] = action.Target
	}
	e.mem.ProcessAndStoreAsync(ctx, signal.RawText, meta)
	return nil, nil
}
```

Fire-and-forget, text-only (`signal.RawText`), into the pre-substrate LTM path, returning `nil, nil`
unconditionally. It cannot report whether the save happened, so **nothing can meaningfully run after
it**, and it does not write the ADR-0108 substrate. The primitive exists in name only.

### Four blockers in the current execution model

1. **Arms are independent, not connected nodes.** `runAction` (`reactive_engine.go:775-808`) runs
   every arm regardless of what earlier arms did — its own comment: *"a failing arm does not cancel
   the ones after it."* "Notify after the memory save" is inexpressible.
2. **There is one gate, at the front.** `Condition`/`ConditionType` are header fields evaluated once
   against the raw signal. A branch *inside* the flow has nowhere to live, so it could only ever see
   the raw payload, never transformed values.
3. **Nothing flows between arms.** `Execute(ctx, action, signal)` receives the *original* signal, and
   `runAction` discards every arm's `*domain.Handoff` except the first.
4. **The idempotency claim wraps all arms** — one key per (watch, signal). A failure at the fourth
   arm dead-letters everything with the key consumed, so replay re-runs *nothing*: neither retrying
   the save that failed nor suppressing the notification already sent.

## Part A — The pipeline graph (engine)

### A1. A pipeline is a directed graph bound to an ingress

```
        ┌──────────────────┐
        │ TRIGGER          │  ingress: acme-webhook  (ADR-0090 registry)
        └────────┬─────────┘
                 │ 1 delivery
        ┌────────▼─────────┐
        │ PRESERVE RAW     │  pinned for evidence-bearing ingresses (B3)
        └────────┬─────────┘
        ┌────────▼─────────┐
        │ TRANSFORM        │  mapping revision 7  → fan-out
        └────────┬─────────┘
                 │ 50 items
        ┌────────▼─────────┐
        │ IF  amount > 1k  │
        └───┬──────────┬───┘
       true │ 47    3  │ false → (unwired: items end here, recorded)
   ┌────────▼───┐
   │ NOTIFY     │
   └────────┬───┘
   ┌────────▼─────────┐
   │ SAVE TO MEMORY   │  ENGINE NATIVE
   └────────┬─────────┘
   ┌────────▼─────────┐
   │ AGGREGATE count  │  47 → 1
   └────────┬─────────┘
   ┌────────▼─────────┐
   │ NOTIFY summary   │  "47 saved, 3 skipped"
   └──────────────────┘
```

Sketch of the model:

```go
type Pipeline struct {
    ID       string
    Name     string
    Trigger  TriggerSpec        // binds an ADR-0090 ingress (or a raw stream)
    Nodes    []PipelineNode
    Edges    []PipelineEdge     // from (node, port) → to (node)
    Active   bool
    MaxLoopIterations int       // global cap; per-loop caps may be lower
}

type PipelineNode struct {
    ID     string               // stable; a journal key component
    Type   string               // see the catalogue below
    Name   string
    Params map[string]any
    Arity  string               // "per_item" (default) | "per_signal"
    Pinned bool                 // immovable, non-deletable (B3)
    Layout struct{ X, Y float64 }  // canvas position; semantically inert
}

type PipelineEdge struct {
    FromNode string
    FromPort string             // "out" | "true" | "false" | "exhausted" | "case:<n>"
    ToNode   string
}
```

`Layout` lives in the pipeline record deliberately: a diagram whose positions are not durable is
re-laid-out on every load, and an operator's mental map of their own flow is part of the artifact.

### A2. Node catalogue, v1

| category | nodes | shape |
|---|---|---|
| trigger | `trigger` | binds an ingress / stream; exactly one per pipeline, no inputs |
| routing | `if` (ports `true`/`false`), `switch` (ports `case:<n>`, `default`) | routes items; never mutates |
| shaping | `transform` (1→N, the fan-out point), `map`, `aggregate` (N→1 or N→G by group key), `merge` (join branches) | |
| effect | `save_to_memory`, `emit_event`, `dispatch_agent`, `start_plan`, `notify` | changes the world; journaled (A6) |
| control | `loop` (ports `body`/`done`/`exhausted`) | bounded iteration (A5) |

There is deliberately **no `filter` node**: an `if` with nothing wired to `false` *is* a filter, and
one concept is better than two that overlap (A4).

### A3. The item stream

A signal enters as one item and becomes an item set. Every node is `f(items) → items per output port`.

```go
type Item struct {
    Key     string            // deterministic identity; a journal key component
    Value   map[string]any    // current value
    Outputs map[string]any    // node id → that node's output for this item
    Iter    int               // loop iteration (A5); 0 outside loops
}
```

`Arity: "per_signal"` on notification-shaped nodes is what stops a 50-item fan-out becoming fifty
notifications. It is a **node** field rather than a property of the node type, because the same
`notify` is wanted per-item in one place and per-signal in another — and it doubles as storm control.

**Fan-out must be deterministic.** Same signal + same immutable revision ⇒ same item keys. For
ingress items the key already exists: the DW-1R revision-qualified source_ref
`ingress:<id>:<delivery_ref>[:<child>]`. An `aggregate` node's output key derives deterministically
from its group key and input keys. Any node type that mints keys non-deterministically is refused at
registration — without this, per-item journaling means nothing.

### A4. Routing, not filtering — and dangling ports are load-bearing

An `if` node routes; items sent to a port with no outgoing edge simply **end there**. That is the
owner's "don't save what got filtered", expressed as not wiring the `false` branch.

Items terminating at a dangling port are **durably recorded** as *deliberately terminated*, with the
node id and port. This is not bookkeeping — see B5: an unrecorded termination is indistinguishable
from a failure, and a reconciler will "repair" it by saving exactly the item the operator routed
away.

### A5. Loops: bounded, with legality checked at save time

Per owner decision, cycles are allowed **with a hard iteration cap**.

- Item keys carry the iteration: `<base>#iter=N`. Journal claims are per
  (pipeline, signal, node, item, **iter**).
- A `loop` node declares `max_iterations`; the pipeline declares a global ceiling. Absent or
  above-ceiling caps are refused at save time.
- **Legality rule:** a cycle is legal **only if every cycle in the graph passes through at least one
  `loop` node with a finite cap.** Any other cycle is refused at save time, with the cycle named.
  This keeps termination a property of the graph rather than a hope.
- Cap exhaustion is a **first-class terminal outcome**, not an error. The `loop` node's `exhausted`
  port lets the operator decide: unwired ⇒ items end there (recorded per A4); wired ⇒ handled.
- Loop continuation frequently depends on an effect node's output ("paginate until the API returns
  empty"). Determinism therefore comes from **journaled effect outputs** (A6), not from purity.

### A6. The journal records outputs, not just claims

- **Pure nodes** (`if`, `switch`, `transform`, `map`, `aggregate`, `merge`) are deterministic
  functions of their inputs and an immutable revision. Replay **recomputes** them. No claim.
- **Effect nodes** claim per (pipeline, signal, node, item, iter) **and record what they produced.**

Recording outputs — not merely claiming — is what makes a graph replayable. With aggregation
downstream of an effect, "recompute the pure prefix" is not enough: the aggregate's input is an
effect's output, so replay must read the recorded value rather than re-run the effect. Replay
becomes: walk from the trigger; recompute pure nodes; for effect nodes, read recorded output if
claimed, else execute.

This is precisely the failure the current whole-signal claim cannot express: *don't re-send the
notification that already went out, but do retry the save that failed.*

Journal volume is the cost, paid by retention — `reactive/journal_gc.go` already exists.

### A7. Retry, then per-item dead-letter

Effect nodes get bounded retry with backoff. On exhaustion the **item** dead-letters and the pipeline
continues with the rest — one bad record must not block the other 49. A signal's outcome is a tally,
not a boolean: *saved / routed-away / dead-lettered / loop-exhausted*.

`save_to_memory` reuses the existing permanent-vs-transient split (`ErrKindRefused`, ADR-0110): a
kind refusal can never be fixed by retry, so the item completes as **refused** with the constraint
named — not retried, not dead-lettered. A transient error retries.

### A8. `save_to_memory` — the native node

Engine-native, not plugin-contributed:

- **Synchronous**, returning what it wrote — item ids, kinds, refusals — so downstream nodes can read
  it and a summary notification can state what happened.
- Writes the **ADR-0108 substrate** through a new consumer-side port on the engine (`MemorySaver`),
  wired at Build from kernel services; the substrate-shaped sibling of the existing `MemoryWriter`.
- The legacy `ingest` action is **kept unchanged and documented as deprecated**. Persisted watches
  use it and the LTM lane still exists; it is a different operation and should not pretend otherwise.

### A9. Backward compatibility

Persisted watches must behave **identically**. Following the `EffectiveActions()` precedent, an
`EffectiveGraph()` accessor returns `Nodes`/`Edges` when present, and otherwise **synthesises** a
graph from the legacy shape: trigger → front `Condition` as an `if` → one effect node per arm, all
wired from the `true` port in parallel (independent-arm semantics, preserved exactly). Nothing on
disk changes; nothing behaves differently until an operator authors a graph.

### A10. Budget accounting across the whole graph

`reactive_engine.go:679` draws a plan token only when `cfg.Action.Type == "start_plan"` — the **first
arm only**. A `start_plan` anywhere else bypasses REACT-02's budget entirely. Under operator-authored
graphs this is trivially reachable. The budget must be evaluated across every node, and fan-out makes
it sharper: a `start_plan` downstream of a 50-item fan-out draws **50** tokens, not one. In scope.

## Part B — Ingresses

### B1. Every ingress may have a pipeline

A pipeline's `trigger` binds an ingress from the ADR-0090 registry. Telegram, HTTP chat, webhook,
poller, websocket — all the same. This replaces the studio-specific framing entirely.

The single wildcard watch `ingress-studio-raw` (`ingress.raw.*`, one arm `ingest_raw`) is retired.
Per-ingress streams already exist in every transport (`transport_webhook.go:270`,
`transport_poller.go:232`, `transport_websocket.go:273`) and are simply unused by the watch layer.

Migration at Build: seed a pipeline for every non-`draft`/`retired` ingress; **only if every seed
succeeded**, delete the wildcard. A delivery landing between the two is handled twice and absorbed by
idempotency — duplicate work, never a dropped delivery. `app.ReactiveWatchStore`
(`app/options.go:443-449`) already exposes `WriteWatchConfig`/`DeleteWatchConfig`; no new seam.

### B2. The studio builds doors and maps them into knowledge; the canvas defines the flow

Ingress Studio has exactly two responsibilities (owner's framing):

1. **Create ingresses** — transport spec, verification profile, credentials, capture samples, schema
   profile. The door.
2. **Map their data into the knowledgebase data structures** — the LLM-drafted, human-confirmed,
   immutable mapping revision.

It therefore contributes exactly two nodes to a pipeline — `preserve_raw` and `transform` — and stops
there. Everything downstream is drawn on the canvas. Two surfaces, one seam, no rewrite of ADR-0112's
staged human gates.

**This pins down the item contract at the seam.** Because the mapping's output is already
knowledge-shaped (ADR-0108 events/observations, conforming to the ADR-0110 kind registry), the items
flowing out of `transform` are exactly what `save_to_memory` accepts. Every node between them —
`if`, `aggregate`, `notify` — operates on knowledge-shaped items, which is why an operator can filter
on *semantic* values rather than raw payload fields.

It also explains why only `preserve_raw` is pinned (B3) and `transform` is not: deleting `transform`
does not corrupt anything, it makes items reach `save_to_memory` unshaped, where the kind registry
**refuses them loudly and names the constraint** (A7). A self-announcing failure needs no pin; a
silently-skipped evidence preservation does.

### B3. The pinned node — the one carve-out from full editability

ADR-0112's gate sentence:

> Cambrian authenticates and durably preserves the original delivery before any generated or
> human-authored semantic mapping runs.

For evidence-bearing ingresses the `preserve_raw` node is `Pinned: true` — immovable, non-deletable,
and no edge may bypass it — while **everything downstream is completely free**. This is the minimum
carve-out that keeps ADR-0112 structural rather than aspirational.

Consequences of operator ownership, handled rather than hidden: ingress-state enforcement moves into
the nodes (where the transformer already half-implements it); a deleted pipeline silently disables
the ingress, so ingress status must report pipeline binding; seed-once means an explicit **re-seed**
affordance rather than silent rewriting.

### B4. The reconciling floor

Per owner decision the ADR-0108 outbox is **kept** while per-node retry proves itself, but its role
changes from "project on every tick" to **reconcile**. For evidence whose pipeline run is terminal
(or stale beyond a grace window), it compares what the ARMED mapping revision would produce against
`CurrentProjections(evidenceID)` and projects only what is genuinely missing.
`ingress_studio_projections` is already keyed per typed row (`projection_key`, DW-1R).

### B5. The hazard the graph creates

**Routing and reconciliation will fight.** If 3 items were deliberately routed to an unwired `false`
port, a floor that only knows "50 expected, 47 present" will project those 3 — silently undoing the
operator's branch. This is the most dangerous interaction in the design. Two mechanisms, both
required:

1. **Terminations are durable and typed** (A4): the floor distinguishes *routed away* from *failed*,
   and skips the former for the current mapping revision.
2. **The floor is lagged and run-state aware:** a `pipeline_run` record per (pipeline, signal) carries
   a terminal status, and the floor considers only terminal-or-stale runs. Otherwise the 2s tick races
   the pipeline and projects items the branch had not yet routed away.

A termination is scoped to the revision that produced the item, so a **new** mapping revision
legitimately re-produces and re-evaluates it. That is repair, not resurrection.

## Part C — The canvas

`@xyflow/react` v12 is already a dependency; `PlanGraph.tsx` renders DAGs and `DagBuilder.tsx` (734
lines) is already a DAG **authoring** surface with dependency edges and fan-out modelled. The pipeline
canvas is a new editor over the same renderer: typed ports, a node palette, and live per-edge item
counts — *"47 down the true branch, 3 down the false"* — which the per-node journal makes real rather
than decorative.

**A naming trap to avoid:** plan DAGs are LLM-generated *agent task* graphs; pipeline graphs are
operator-authored *deterministic data flows*. Same renderer, different concepts. They must not share
vocabulary in the UI or the contract.

## Slices

| slice | content |
|---|---|
| **RP-1** | Graph data model + `EffectiveGraph()` compatibility + topological execution with pure/effect split (A1, A2 minus loops, A9). Legacy watches byte-identical. |
| **RP-2** | Item stream, deterministic keys, fan-out, per-item claims **and recorded outputs**, per-item dead-letter (A3, A6, A7). |
| **RP-3** | Routing nodes (`if`, `switch`), durable termination records (A4). |
| **RP-4** | `save_to_memory` native node, synchronous result, kind-refusal split (A8). **The `MemorySaver` port named in A8 is withdrawn by ADR-0114 D15** — the node is an adapter over ADR-0108's existing `domain.EventStore`, which is already reachable and already idempotent on `source_ref`. |
| **RP-5** | `aggregate` / `merge` (A2). |
| **RP-6** | `loop`: cycle legality check, iteration keys, `exhausted` port (A5). |
| **RP-7** | Ingress pipelines: trigger binds the ADR-0090 registry, seed-on-arm, pinned `preserve_raw`, wildcard retired, floor becomes a lagged reconciler (B1–B5). |
| **RP-8** | Contract bump (graph on the wire, per-node/per-edge metrics), the canvas, budget fix (A10), dead-letter replay RPC. |

RP-1 is a prerequisite, not a parallel track. Expressing ingresses on today's arms model and
retrofitting a graph later means migrating every stored pipeline twice — the same mistake in a new
costume.

## Gates

1. **Compatibility.** Every pre-existing watch runs byte-identically under `EffectiveGraph()`; the
   arms-model regression suite passes untouched.
2. **Ordering.** `save → notify` sends no notification when the save fails, exactly one when it
   succeeds.
3. **Routing.** With an `if` sending 3 of 50 items to an unwired port, the substrate holds 47 — and
   **still holds 47 after the reconciler runs.** The B5 gate, and the one most likely to fail.
4. **Replay.** Kill the kernel between `save` and `notify`; on restart the save is not repeated and
   the notification is sent exactly once.
5. **Aggregation across replay.** An `aggregate` downstream of `save_to_memory` produces the same
   result after a mid-run crash — proving recorded outputs, not recomputation, back it.
6. **Loops.** A cycle without a capped `loop` node is refused at save time with the cycle named; a
   capped loop terminates at the cap and routes to `exhausted`.
7. **Partial failure.** One item failing permanently leaves 49 saved and reports
   `49 saved · 0 routed away · 1 dead-lettered`.
8. **Floor.** With `save_to_memory` forced to fail, deliveries still land via the reconciler and the
   console shows the dead letters.
9. **Independence.** Two ingresses ⇒ two pipelines on two streams; deliveries to one move only its
   counters.
10. **Budget.** A `start_plan` downstream of a 50-item fan-out draws 50 plan tokens (A10).

## Non-goals

- Parallel/concurrent node execution as an authored concept. Execution order is a topological walk;
  concurrency is an engine detail, not a node type.
- Removing the ADR-0108 outbox (explicitly retained, B4).
- Replacing the legacy `ingest` action or the LTM lane (A8).
- Sub-pipelines / reusable fragments. Natural next step once the vocabulary settles; not v1.
- Per-fire history for console sparklines — still aggregate-only.
