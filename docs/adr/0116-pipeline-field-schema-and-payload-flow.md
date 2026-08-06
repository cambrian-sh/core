# ADR-0116: Pipeline field schema and payload flow

**Status:** Implemented
**Date:** 2026-08-05
**Amends:** ADR-0114 (pipeline runtime semantics) — extends D25–D28's provenance/payload split with
a rule for WHEN the payload flows through the graph, and D36's field-reference extraction with a
per-node schema projection.
**Related:** ADR-0112 (ingress studio, capture profiles), ADR-0113 (graph model), contract 0089
(canvas reads), contract 0090 (draft saves).

## Context

The graph language could always express a decision on the flowing data — `choice` gates on a CEL
expression over `item`, `map` rewrites its fields, `split` fans out over one — but two facts kept
that expressiveness theoretical on a live ingress:

1. **The item flowing through an ingress run was empty.** RP-6 moved provenance off the payload and
   onto the run, and the routing seam started every run with an empty root item; `save_to_memory`
   read the archive instead. Correct for the generated `trigger → save` shape — and it meant a gate
   inserted between them evaluated `item.magnitude` against `{}`. Measured on the live deployment
   (2026-08-05): the armed `ingress:usgs-earthquakes` pipeline's tasks all carry `input: {}`. The
   dry run, meanwhile, replays captured deliveries **with** their values — so a graph with any
   discard gate behaved differently live than in its own rehearsal.

2. **Nothing answered "which fields exist at this node".** D36 reports what an expression READS;
   nothing reported what is READABLE — which is the question an operator authoring a condition is
   actually asking, and its answer changes at every transform: a `map` renames and drops, inside a
   `split` the item is one member, downstream of an effect the item is the effect's receipt.

## Decision 1 — the payload flows when, and only when, the plan consumes it

`Compile` derives **`NeedsPayload`**: true when any node's expressions read `item.*`, or any
`map` / `split` / `aggregate` / `merge` node exists. The ingress routing seam parses the just-
preserved delivery (the same bytes, already in hand — no second read, no second source) and OFFERS
it to the router; the router keeps it only when the armed plan asks.

- **Derived, not authored.** The graph itself says whether it operates on the flowed data. There is
  no toggle to forget, and the vanilla generated `trigger → save` pipeline keeps its empty-item
  economy (the save still reads the archive; a payload nobody reads would put a copy of every
  delivery in every task row for nothing).
- **Refusal, not emptiness.** A plan that reads item fields fed a delivery with no readable JSON
  payload is REFUSED at routing — visibly, into the error path — because a filter gating an empty
  item routes every delivery the same way forever, silently. Silence reading as success is this
  project's most-repeated defect; this closes one more instance.
- **Wrap rule shared with `applySplit`:** a JSON object is the item; an array or scalar becomes
  `{value: it}`; `null`/unparseable is no payload.
- Provenance is untouched: it still rides the run and the item envelope, never the payload (RP-6
  holds).
- `NeedsPayload` is excluded from the plan checksum — it is a restatement of the nodes, never an
  independent fact.

This also closes the live/dry-run divergence in the one direction that is safe: both now agree the
payload is present exactly when the graph uses it.

## Decision 2 — a per-node field schema, projected by the compiler, served on the operator plane

New pure projection beside the compiler (`pipeline/schemaproj.go`): starting from the trigger's
capture profile (the ingress studio's `SchemaProfile`, converted to dotted paths), walk each scope
in topological order and report per node: **available fields (path, types, origin node)**, **reads**
(from D36), **writes/drops** (a map's own config), or — where a shape is not statically knowable —
**a named reason**.

The walk uses the runtime's own rules and nothing else: `applyMap`'s set-then-drop order (with the
CEL type-checker supplying the static type of each `set`), `applySplit`'s member and scalar-wrap
shapes, `applyAggregate`'s fixed output. Honesty boundaries are runtime facts, stated per node
rather than guessed around:

- downstream of an effect the item is the node's **receipt** (the scheduler hands successors the
  receipt, never the item);
- a computed fan-out (`over` anything but a bare `item.path` select) has no claimable member shape;
- loop outcomes are authored by the body;
- a non-ingress trigger has no schema source, and an ingress that has captured nothing yet is
  "not profiled", which is not the same claim as "no fields".

**Wire (contract `0090 → 0091`):** `field_schema_json` on `GetPipelineOpResponse` and
`ValidatePipelineOpResponse` — the validate copy is what lets the console's picker track the DRAFT
as it is edited. JSON for graph_json's reason: a per-node map of open, recursive facts. Empty means
**unchecked**, never "no fields". Capability `pipeline-field-schema`, advertised with the authors.
`GraphAuthor` gains an optional `TriggerSchema` source; the ingress-studio plugin wires it from its
capture store, the reactive plugin's author degrades to named unknowns.

## What the console builds on it (recorded here because the boundary is the decision)

The canvas' node inspector becomes an editor when the graph is being edited: per-kind forms from a
closed registry (`nodeEditors.ts`, same mirror discipline as `nodeVocabulary.ts` — it names keys and
input shapes, decides nothing), a field picker fed by the projection (member `*` paths shown dimmed
with the fan-out that reaches them, instead of inserting CEL that cannot compile), and field
lineage — click a field anywhere, and every node that reads / writes / drops / routes on it stays
lit while the rest recede, joined client-side over kernel facts only. Validation stays exactly
where it was: every keystroke lands in the draft, the compiler is asked (debounced), refusals
render keyed by constraint. The console still evaluates nothing.

Deliberately NOT offered as form fields: `save_to_memory`'s `ingress_id` and `mapping_revision` —
the identity of the transform a save applies, pinned at arm time. A typo there would silently apply
a different transform to live data; they remain visible, and change through the studio lifecycle.

## Alternatives rejected

- **A `materialize` flag on the trigger** (config or TriggerSpec): a toggle the operator must
  discover exactly when they are doing something else (inserting their first gate), and a second
  fact that can disagree with the graph. The graph already says it.
- **Client-side schema propagation** (the UI walking map/split rules itself): a second reader of
  the language; it would have gotten the receipt boundary wrong — nothing in the document says an
  effect's `out` carries the receipt; only the runtime does.
- **A structured condition-builder that round-trips CEL**: parsing CEL in the console is the same
  second reader. A one-way row builder that GENERATES CEL remains open as sugar, deliberately
  unbuilt until raw-CEL authoring proves insufficient.

## Decision 3 (added same day, contract `0091 → 0092`) — the dry run carries the data, from the archive

Operator feedback on D1/D2 as shipped: the schema was visible but the DATA still was not — and the
dry run was replaying **redacted capture samples**, where every value is a type token, so any
numeric or text comparison failed on every item and the report blamed the graph.

- **`pipeline.DeliverySource`** (`RecentDeliveryBodies`): the dry run replays the newest REAL
  bodies, read back from the evidence archive via the deliveries sidecar
  (`ingressstudio.DeliveryReplay` = sidecar hashes + the same evidence-fetch seam the save
  dispatcher reads through). Redacted captures remain the fallback, and the report **names its
  source** (`PipelineDryRun.Source`) — over redacted samples the console warns that failures are a
  fact about the source, not the pipeline.
- **`PipelineDryRun.NodeExamples`**: per node, up to three real items — the value as the node
  received it (each dry-run task already stores its input envelope; the examples are read back
  from the sample stores, no new execution hook) plus the node's own receipt where one was
  recorded. This is "the actual data flowing", per node, bounded because it is a panel a person
  reads.
- Console: a "data through this step" panel on the inspector (operator-terms summary line first,
  raw envelopes behind a disclosure), and the field picker's chips show the value each field held
  in a real delivery (`= 5.3`) beside its type.

Also shipped with this bump, UI-only (no contract impact): the canvas became directly
manipulable — palette steps drag onto the canvas (drop on a pipe splices in, auto-wired both
sides), pipes are grabbable (drag an end to re-route, drop on nothing to disconnect — loudly),
port dots drag to wire, steps drag and their positions persist into the graph's own
`Node.Layout` (which the model always had and the console never wrote), stored positions win over
auto-layout, and auto-placed nodes are nudged so nothing overlaps by default.

## Consequences

- Editing a live ingress pipeline into a filter is now real end to end: insert a `choice` on
  `item.magnitude > 5.0 && item.place.contains("Turkey")`, terminate `false` with a reason, dry-run
  to see the counts, save/publish/arm through the existing lifecycle. Nothing about the lifecycle
  gates changed.
- Task rows for payload-consuming plans now carry the payload in `input` (bounded by the existing
  budgets). Plans that read nothing are unaffected.
- Known residual, out of scope here: pipeline RUNS never reach a terminal state (7,288 `running`,
  0 succeeded on the live deployment while tasks drain normally) — run closure is missing in the
  engine and tracked with the RP-7 shipped-engine defects.
