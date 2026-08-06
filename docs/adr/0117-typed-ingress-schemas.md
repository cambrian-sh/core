# ADR-0117: Typed ingress schemas, and the mapping as a pipeline element

**Status:** Accepted — Part A implemented 2026-08-05; Part B designed here, next slice
**Date:** 2026-08-05
**Amends:** ADR-0112 (ingress studio resources — adds a third versioned resource), ADR-0116 (field
schema projection — changes its source of truth). Part B, when it ships, amends ADR-0114 D15.
**Origin:** owner direction, 2026-08-05: "we definitely want typed schemas, also the mapping itself
should be a pipeline element … I prefer declaring the schema of incoming data, and handling the
transformation/mapping on the pipeline with actual types."

## Context

Field availability on the canvas was INFERRED at read time: capture profile → dotted-path
conversion → per-node projection → author routing. Every hop in that chain failed silently at
least once in one day (redacted values read as shapes; the schema-less author outrunning the
schema-holding one — twice). An inference chain that long is not a foundation; a stored fact is.

Separately, the transformation itself is invisible: the generated pipeline's `save_to_memory`
node *applies* the mapping inside its dispatcher (ADR-0114 D15 — "the save names WHICH mapping").
The graph an operator inspects therefore hides the one step that decides what their data becomes.

## Part A (implemented) — the ingress declares its schema

A third versioned, insert-only studio resource, beside transport and mapping revisions:

- `ingress_studio_schema_revisions` — `SchemaDecl{Fields: []DeclaredField{Path, Type, Format},
  DerivedFrom}`. Paths in the projection's own dotted notation; Type is ONE of the profiler's five
  words, because a declaration picks.
- **Snapshotted at `ConfirmMapping`** — the moment the deployment commits to the source's shape —
  from the capture profile (`DeclareFromProfile`: dominant observed type per path; null-only paths
  declare nothing). Minted ONCE: a re-confirm never silently re-declares over an edited revision
  (the mapping-regeneration lesson, applied). Best-effort: a failure decorates the ack, never
  blocks the transition the mapping earned.
- **`TriggerSchemaSource` reads DECLARED first**, observed profile as fallback — so every ingress
  predating this ADR keeps its fields, and every new confirm gets deterministic, typed ones.
  Store capability is asserted (`schemaDeclStore`), not added to the `Store` interface, so fakes
  only grow it where a test exercises it.

What A deliberately does not do yet: validate deliveries against the declaration (schema-on-read
stays; refusing a whole feed on an upstream field rename is an outage, not a safety property).
Drift between declaration and arriving data is a REPORT to build on this, not a gate.

## Part A′ (implemented 2026-08-06) — registrations declare, too

The studio's resource covers studio-authored sources; every OTHER entry organ — the Telegram
bridge, SDK chat ingresses — is an ADR-0090 registration. Those now declare the same way:

- `domain.IngressRegistration.Schema` (`IngressSchemaField{path, type, format}`), persisted on the
  registry row (`authz_ingress.item_schema`), declared through a third narrow power beside list
  and deregister: `domain.IngressSchemaDeclarer` / `svc.DeclareIngressSchema`. Declaring onto a
  MISSING registration is refused by name — registering still mints a surface and stays the
  operator's act (ADR-0090 D2).
- The telegram plugin declares at Build and at runtime bot-add — and declares what the bridge
  TRANSPORTS, because the item now carries it (owner follow-up, same day): the turn envelope was
  widened end to end. `domain.TurnMessage` replaces the bare text through `TurnRouter.RouteTurn`;
  the admitted sender facts the kernel already held (`external_id`, `speaker_id`, `username`,
  `display_name` — relayed claims, namespace-checked where checkable, never matched on for
  authorization) ride the item as a `sender` block, with absent claims staying ABSENT rather than
  empty (`has(item.sender.username)` must answer honestly). Declaration and item construction are
  two statements of one contract (`telegramItemSchema` ↔ `pipeline.turnItemValue`), each naming
  the other. Still narrower than the raw Telegram Update: declared = what arrives, always.
- `pipeline.RegistrationSchemaSource` feeds the reactive plugin's `GraphAuthor.TriggerSchema`, so
  chat-pipeline canvases get their field picker from declarations with zero captures.
- Found and fixed while wiring: `KernelServices.RegisterIngressDeregistrar` was declared and
  consumed but NEVER wired in the composition root — the authz plugin's nil-guard skipped it and
  `svc.DeregisterIngress` silently no-opped. Both halves now wire on adjacent lines.

## Part B (designed, next slice) — the mapping is a node on the canvas

The insight that makes this cheap: the mapping's `Evaluate(spec, body)` is already a **pure
function** from delivery bytes to typed envelopes, and the mapping language already declares
`value_type` per extracted field. The transformation is typed today — it is just performed in the
wrong place (inside the save's dispatcher) and invisible.

Decisions:

1. **New node kind `apply_mapping`** (closed-language addition): config names `ingress_id` +
   `mapping_revision` — the identity discipline of D15 preserved, moved one node upstream. It
   executes through the dispatcher seam (the studio registers its dispatcher, as it does for
   `save_to_memory`), and its RECEIPT is the evaluated envelopes — which the existing
   receipt-forwarding semantics hand to the next node. Deterministic in substance, journaled in
   mechanism: replay reuses the recorded outcome (D6-safe) and the pure function makes the
   at-least-once retry a no-op.
2. **`save_to_memory` grows a writer-only mode**: an item carrying envelopes is written
   (`planProjections` → `writeProjections`), never re-evaluated. `source_event_ref` rides inside
   the envelopes, so write idempotency and the side-by-side outbox transformer are untouched.
3. **The generator emits `trigger → apply_mapping → save`** for new confirms. Existing revisions
   are immutable and keep running as generated; nothing is migrated in place.
4. **The projection types the mapping's output EXACTLY**: downstream of `apply_mapping`,
   available fields are the mapping's own declared `value_type`s — declaration end to end, zero
   inference. Gates the operator adds between the mapping and the save filter TYPED, extracted
   fields rather than raw payload paths.
5. D15's text gains a banner pointing here when B ships; its principle (config names WHICH
   transform, never what it does) survives intact on the new node.

## Consequences

- "Which fields exist" is now a versioned, auditable fact with named provenance
  (`capture profile · N samples`, or an operator edit), pinned like every other studio resource.
- The B slice turns the canvas into the actual data path: declared input schema → visible typed
  transform → typed gates → writer. The mapping stops being the one step the graph lies about.
- Residual for B: arm-time pinning should extend the release to reference the schema revision, so
  "what did we believe the shape was when this went live" stays answerable.
