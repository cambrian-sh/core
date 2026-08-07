# ADR-0112: Ingress Studio foundations — raw-delivery contract, versioned ingress resources, lifecycle

**Status:** Accepted (DW-0A + DW-0B + DW-1 + DW-1R implemented; later slices land against this record)
**Date:** 2026-08-01
**Design record:** `docs/research/daemon-writer-plan.md` (v2.0, monorepo root) — D1–D14 and the
independent pipeline review are binding context; this ADR records the decisions that shape code.
**Related:** ADR-0090 (ingress system), ADR-0092 (propose-don't-enforce), ADR-0101 (config/secret
store), ADR-0104 (reactive-first, payload-as-reference), ADR-0105 (evidence foundation),
ADR-0108 (event-shaped knowledge), ADR-0110 (kind registry), ADR-0061/0062 (journal, backpressure).

## Context

An operator describes a data source — webhook, polled API, websocket, inbound endpoint — and
Cambrian drafts, previews, and runs the ingress that feeds the knowledge substrate. The flow is
`describe → secure+capture → draft → confirm → dry-run → arm`, three of the six stages human
gates. v1 generates **configuration, not code**: human-written generic transports parameterized
by versioned specs.

The sentence the whole programme is gated on (adopted verbatim from the pipeline review):

> **Cambrian authenticates and durably preserves the original delivery before any generated or
> human-authored semantic mapping runs.**

The mapped envelope is never the evidence; the raw delivery is. Everything below exists to make
that ordering structural rather than aspirational.

## Decisions

### 0. Placement: a premium plugin; the plugin ID is the entitlement key (owner directive)

Recorded as binding from the earlier DW-0A draft of this ADR (owner directive): **the
studio is a premium product and lives in `cambrian-premium` as a plugin**, ID
`ingress-studio` — which is also its ADR-0082 entitlement key, so the feature is
independently SKU-able. The manifest declares no static capability; `ingress-studio`
surfaces post-Build via the CapabilityReporter pattern (the drift precedent), because
availability depends on Postgres + evidence capture, unknowable at Register.

**As-built deviation, flagged for owner ratification.** The draft directed "zero new OSS
surface", deferring one candidate gap (secret delivery) to DW-0B. DW-1 as built adds
THREE narrow OSS seams, for reasons the draft's own constraints force:
`StageEvidenceContent`/`FetchEvidenceContent` because ADR-0104 forbids bodies in the
journal — a durable-ack lane therefore needs content-first staging with a CID reference,
and the seams are just the evidence ingestor's own halves; `ResolveNamedSecret` because
SEC-01 strips credential-shaped env vars from daemon environments, so env injection —
the draft's "likely shape" — is exactly the delivery path the kernel already refuses,
and the telegram 0600-file workaround is the pattern this store exists to retire. All
three are nil-when-off, read-only/name-at-a-time, and carry no premium semantics. If the
owner prefers strict zero-OSS-surface, the seams can be re-homed behind a premium-owned
indirection at the cost of duplicating the ingestor's ordering contract.

### 1. Three versioned resources, not one spec

| Resource | Owns | Mutability |
|---|---|---|
| `TransportSpec` | endpoint/polling/stream config, credential BINDINGS (secret-store references only), verifier profile, method/content-type/size limits, quotas, cursor/retry/reconnect policy, capture retention, namespace + classification floor, source class (sheddability) | versioned; edits mint revisions |
| `MappingSpec` | the deterministic transform from raw evidence to substrate envelopes | versioned; **immutable per revision** — the unit of retroactive repair |
| `IngressRelease` | immutable pairing: transport revision + mapping revision + kind-registry revision + deployment policy | armed ingresses pin a release; edits mint a new draft, never mutate in place |

Revisions are integers minted by the store (`max+1` inside a transaction); there is no update
path for a revision row, mirroring the evidence store's structural immutability (ADR-0105).

### 2. The lifecycle is a server-owned state machine

`DRAFT → CAPTURING → MAPPED → DRY_RUN → ARMED ⇄ PAUSED → RETIRED` (RETIRED terminal).
Backward edges exist where revision work naturally reopens a stage
(`CAPTURING→DRAFT`, `MAPPED→CAPTURING`, `DRY_RUN→MAPPED`). Guards are enforced in code and by
optimistic `WHERE state = $from` updates in the store: entering `DRY_RUN` requires a mapping
revision; entering `ARMED` requires a pinned release. **Health (`ok/degraded/error`) is an
orthogonal field, not a state** — a degraded armed ingress is still ARMED; failures never
silently mutate the active revision or its state.

### 3. Delivery identity ≠ source-event identity (plan D7)

Every delivery carries two identifiers: `delivery_ref` (the transport attempt — GitHub's
`X-GitHub-Delivery`, a poll's fetch key; minted unique when the source offers none) and
`source_event_ref` (the underlying event — Stripe's `event.id`; empty when no trustworthy key
exists). Content hash is advisory metadata. Duplication is recoverable; silent deletion through
an unsafe dedup key is not — semantic dedup happens later and visibly.

### 4. Raw deliveries become evidence through the existing substrate contract

Mapping onto `domain.RawEvidence` (ADR-0105):

| RawEvidence field | Raw delivery value |
|---|---|
| `SourceID` | `ingress:<ingress_id>` — the mapping transformer's claim key |
| `SourceKey` | `delivery_ref` |
| `SourceRevision` | hex sha256 of the original body — identical redelivery dedups; a changed body under the same delivery_ref is preserved as a new revision, never lost |
| `Bytes` | the ORIGINAL delivery bytes, untouched (verifiers require raw-body preservation; repair replays original bytes) |
| `SourceTime` | the source's occurred hint, else received-at |
| `Classification` | the transport's classification floor (deployment vocabulary only, ADR-0091) |
| `Cursor` / `TraceID` | poll cursor / transport trace |

Evidence rows carry no free metadata, deliberately. Delivery metadata that is not part of the
raw bytes — transport revision, verifier + verification result, allowlisted headers,
content type, `source_event_ref` — lives in a premium **sidecar table
(`ingress_studio_deliveries`) keyed by `evidence_id` with a foreign key onto `evidence(id)`**.
Mixing metadata into `Bytes` would corrupt "raw"; widening the OSS evidence row for one
premium feature would violate the thin-envelope discipline (ADR-0105 §technical envelope).

### 5. Raw evidence precedes mapping — enforced in the schema

`ingress_studio_transform_runs` (`evidence_id, mapping_revision, registry_revision, timings,
outcome, output_count`) records mapping lineage, one row per `(evidence_id, mapping_revision)`.
Both sidecar tables **foreign-key `evidence(id)`**: a transform run structurally cannot exist
before its evidence row. This is the DW-0A gate made schema-shaped. The FK also means the
studio's store refuses to construct when the substrate migrations (0011+) have not run — an
honest boot failure naming `orchestrator migrate up`, not a silent degradation.
`projection_key = evidence_id + mapping_revision + output_ordinal` (shadow reprocessing, plan
D11) extends this in DW-1R.

### 6. Delivery rides signals; the journal is the durable queue (plan D5, mechanics)

Transports never call the substrate lane directly — a partly LLM-drafted transport
configuration must not hold an evidence write path. The armed flow is:

```
verify (profile) → stage original bytes content-first (ContentStore, CID) →
emit signal {ingress_id, transport_rev, delivery_ref, cid, meta} →
journal-durable ack → answer the source (2xx / advance cursor) →
ingest_raw watch action: resolve CID → EvidenceIngest + sidecar row →
mapping transformer consumes the evidence outbox → typed rows
```

Bytes ride as a **CID reference, never inline** — this keeps ADR-0104's payload-as-reference
rule intact for the journal and handles arbitrary body sizes. Crash windows are all safe:
orphaned staged content is GC'd and the unacked source retries; a journaled-but-unprocessed
signal replays into the idempotent evidence insert.

DW-1 built these seams (as designed here, not improvised):
- **`ReactiveEngine.OnSignalDurable`** (REACT-01/02 amendment): journal-first — before the
  availability, matching, and rate-limit checks — returning the seq, exposed to transports
  through the package-level `reactive.BindDurableEmitter`/`EmitDurable` seam (the
  action-registry pattern: transports and the reactive plugin share the library, never each
  other). Durable emits bypass the per-stream shed bucket: the transport's TransportSpec
  quotas are the intake governor, and a post-journal shed would drop what the source
  believes delivered. `Signal.Timestamp` is stamped at intake when zero (a zero timestamp
  collapses idempotency onto content forever).
- **`KernelServices.StageEvidenceContent` / `FetchEvidenceContent`**: the evidence
  ingestor's own Put/Get halves (`evidence.Ingestor.Stage`/`FetchStaged`), so the
  content-node shape can never drift from what `Ingest` writes.
- **`KernelServices.ResolveNamedSecret`**: name-at-a-time reads from the ADR-0101 store
  (no env indirection — the SEC-01 lesson; no list operation — a plugin can use a
  credential it knows the name of, never enumerate). Wired through a late-bound holder
  set in `Run` beside `llm.SetSecretResolver`, inside the same typed-nil guard.

Ack semantics are transport-specific and declared per archetype: webhooks answer 2xx only
after durability (retryable 5xx over success+shed); pollers advance cursors only after durable
evidence (duplicates allowed, gaps never; cursor scoped to source + transport revision);
streams declare their loss model and record unfillable gaps as findings.

### 7. Verifier profiles are human-owned code (plan D4)

`simulator_v1`, `github_v1`, `stripe_v1`, `twilio_v1`, `bearer_v1`, `capability_url_v1` — a
closed set; `TransportSpec` validation refuses names outside it. Profiles are not algorithm
enums, not a DSL, and never LLM-drafted. The name registry ships with DW-0A so specs are
validated against the closed set from day one; the implementations ship with DW-0B
(`ingressstudio/verifier.go`), each owning its provider's actual scheme — GitHub's raw-body
HMAC-SHA256, Stripe's signed-timestamp scheme with multi-`v1` rotation, Twilio's
URL+sorted-params HMAC-SHA1, bearer and capability-URL constant-time compares, and the
in-house simulator scheme whose signing half (`SimulatorSign`) lives beside its verifier so
the two cannot drift.

### 8. The mapping language is closed and exact (plan D12)

One path syntax (JSON Pointer, RFC 6901); missing ≠ null ≠ empty with declared per-field
behavior; explicit timestamp forms only (`rfc3339`, `unix_seconds`, `unix_milliseconds`,
`format(layout, timezone)`); scalar coercions, constants, defaults, enum maps; fan-out with
child identity, cardinality cap, and per-member failure behavior; every filter that DISCARDS
data is a named, visible rule. A `MappingSpec` parses deterministically (unknown fields
refused) or the parse refuses with the constraint named. The evaluator ships in DW-1; the
grammar is fixed here so drafted and hand-written mappings share one contract.

**v1.1 (2026-08-02, corpus-driven):** value sources gained `scope: "member"|"root"` —
under fan-out, a `root`-scoped path resolves against the document instead of the member
(an order's customer participating in every line event). Additive to the closed grammar;
the change exists because the DW-2 run's best output was a CORRECT abstention on a mapping
v1.0 could not express, and the restored `order_lines` gold now executes at 1.0 through
the stub harness where the old gold refused its own samples.

### 9. Storage is premium-owned Postgres DDL, not a core migration

The studio's tables (`ingress_studio_*`) are created by the premium store at construction,
the `authz_ingress` pattern — the OSS migration chain stays free of premium features
(ADR-0057). The two FK edges onto `evidence(id)` are the one deliberate coupling, and they
point premium→core, the allowed direction.

### 10. Capture security and model egress (DW-0B)

The pre-capture constraint layer (`CheckLimits`) refuses out-of-constraint deliveries —
method, content type, body cap, header cap — before the body is read past its cap and before
any verifier runs; `FilterHeaders` preserves ONLY allowlisted headers into the sidecar (an
empty allowlist preserves nothing). Verifier refusals log redacted: no secret material, no
signatures, no payload content in any Detail. Rotation is first-class: every profile accepts
several active secrets.

Model egress (plan D10) is typed from day one: `EgressPolicy{posture, raw_opt_in}` with
default `redacted` (structural schema profile + redacted samples) and zero raw fields; raw
values reach a drafting model only by explicit per-field JSON-Pointer opt-in. The
local-only-vs-hosted default posture stays an owner decision. Enforcement lands with the
readers (DW-2 drafting, DW-1R replay); the substrate read-scope integration for evidence
reads is on this programme's critical path before those slices and is tracked as open work,
not silently assumed.

### 11. Reprocessing is versioned shadow projection (plan D11/D14, DW-1R)

`projection_key = evidence_id + mapping_revision + output_ordinal` is a real registry
(`ingress_studio_projections`), and every typed row's wire identity is
**revision-qualified** (`…@r<revision>`). The typed stores are append-only and idempotent
on `source_ref`, so a repair CANNOT update a row — it writes beside the old one under the
new revision's identity, and the registry — not the event store — records which revision's
outputs are current. Supersession is a record, never an overwrite; the retired rows keep
answering "what did we once believe".

The repair flow: new immutable mapping revision → `Reprocessor.ShadowReprocess` (evaluates
the candidate over preserved deliveries, **writing nothing**; produces the review diff —
additions, removals, semantic changes by content digest, refusal/discard flips, each named)
→ human reads the diff → `Reprocessor.PromoteRepair` (idempotent end to end: typed stores
dedup on the qualified ref, the registry upserts by projection key, re-supersession matches
zero rows — a retried repair produces zero duplicate effects). A candidate that refuses or
discards a delivery retires the old outputs with no replacement, visibly, in lineage.

External effects during backfill: there is structurally no effect path in this lane —
promotion talks to the typed stores and studio tables, never to signals, watches, agents,
or models — and every promoted projection carries `backfill = true`, the marker any future
effect lane must consult before acting on repaired history (suppressed by default;
retroactive effects need separate approval). Reprocessing reads only studio-owned tables:
the delivery sidecar carries `content_hash`/`namespace`/`classification` so the repair loop
never grows SQL against the core evidence table. "Dependent resolutions re-evaluated" is
N/A today (no resolution consumes this lane's rows) and becomes real work when one does.

Known, accepted limitation: the substrate query plane does not yet consult the registry,
so both revisions' rows are visible to raw event queries until a current-ness filter is
surfaced — consumer-driven work, recorded rather than pretended away.

### 12. Drafting proposes; capture profiles; the corpus decides the model (plan D1/D9/D10/D13, DW-2)

**Capture (the CAPTURING stage, live).** The webhook door serves CAPTURING ingresses
through the SAME constrain+verify gate as the armed path (secure precedes capture,
structurally, on the latest transport revision — there is no release yet to pin). A
captured delivery becomes a schema-profile update plus a REDACTED sample; **v1 stores no
raw capture bodies at all** — stricter than the plan's quarantine, chosen because the raw
capture store's key management and promotion surface belong to DW-3's plane. The profile
accumulates per-path presence/null/missing counts (three different facts), type and format
hints (rfc3339/date/epoch-seconds/epoch-millis/numeric-string/identifier), bounded closed
value sets under a privacy heuristic (≤8 distinct, ≤32 chars, identifier charset — emails
and free text can never qualify), and array cardinalities: exactly the D13 coverage data.
Capture retention prunes samples at write time; coverage statistics survive pruning.

**Egress (plan D10).** `EgressPolicy` persists per ingress (default `redacted`, zero raw
fields); `RawOptIn` JSON Pointers are applied at REDACTION time, so an un-opted value is
simply never stored in promptable form. The drafting prompt is therefore non-sensitive by
construction; the harness enforces it with per-case canaries that fail the run — not the
score — if they ever reach a prompt.

**Drafting (plan D1).** `Drafter.DraftMapping` is propose-only through the read-only
`CaptureReader` port: no save, no transition, no arm is expressible. Its input is the
profile + redacted samples, nonce-fenced as DATA (ADR-0063); its output goes through the
same strict parser as hand-written mappings (verification is not even expressible —
`MappingSpec` has no verifier field). Refused parses get bounded repair rounds quoting the
named refusal; exhaustion abstains rather than shipping an invalid draft; zero coverage
abstains before any model call. Abstention is a first-class outcome.

**The corpus (plan D9).** `cmd/draft-eval` runs the SAME Drafter offline over held-out
real public schemas (GitHub push/PR, Stripe invoice with a named live-mode discard, USGS
GeoJSON, Open-Meteo layout timestamps, HN, Wikimedia, Telegram, an order-lines fan-out)
plus refusal traps (ambiguous timestamps, structurally inconsistent samples) and an
injection trap with canary enforcement. Metrics: field-level P/R, critical-field accuracy
(identity/time/type/actors, weighted separately), execution agreement (draft and gold
EXECUTED over the real samples, envelopes compared), cardinality match, silent-drop and
duplicate rates, refusal precision AND recall, first-draft acceptance, correction counts —
abstentions published beside accuracy. The report is the decision artifact for the
drafting-model choice, which remains the owner's (offline-before-online, as with every
learning phase).

### 13. The plane is the product's spine surfaced (plan D8, DW-3)

`IngressStudioAdmin` (premium proto `api/proto/ingress/ingress_studio.proto`, mounted via
`AddGRPCService` — no operator-contract bump) carries the FULL lifecycle: create, list,
status; `StartCapture → ConfirmMapping → StartDryRun → Arm → Pause/Resume → Retire`
as explicit verbs over the server-enforced state machine (handlers are thin — every guard
lives below the plane); `Rollback` = pinning an existing immutable release, never editing;
spec saves through the SAME strict parsers as hand-written specs; `DiffMappingRevisions`
over the shared field-set vocabulary (`MappingFieldSet`, also the DW-2 metric basis, defined
once); `DraftMapping` read-only; `GetPreview` returning the DRY_RUN stage's ephemeral
buffer (real deliveries evaluated in memory, capped, never preserved — plan D6);
`ShadowReprocess`/`PromoteRepair` (reason mandatory); `ListDeliveryFailures` from lineage.

Credentials are write-only end to end: `SetCredential` builds `ingress:<id>:<name>` itself
(callers never name store refs), the value crosses one function into the encrypted store,
and no RPC anywhere returns it — presence + last-four only. The core write seam
(`StoreNamedSecret`/`ClearNamedSecret`/`NamedSecretStatus`) extends the §0-recorded
deviation by the same late-bound-holder route; name-at-a-time, no enumeration.

DRY_RUN is now a live transport stage: constrain+verify as always, then the latest mapping
evaluates the REAL delivery into a bounded in-memory preview ring — nothing preserved,
nothing projected, gone on restart. The arm decision is made against real traffic.

The ADR-0089 panel (`ingress-studio`) is declared on the handshake, and the console
SCREEN shipped with the second half of DW-3: `ui/` vendors the plane (fifth premium
plane the console speaks), `screens/ingress-studio/` renders the full journey —
lifecycle verbs, immutable-revision spec editing with named refusals inline, the
propose-only drafter with the D13 coverage report as the confirm checklist, the
ephemeral dry-run preview, shadow-repair diff + reason-gated promotion, write-only
credentials — gated on the post-Build capability so absence reads as a complete
deployment. The DW-3 gate ("a non-CLI operator can create, pause, revise, diff,
rollback, retire") is closed; a live click-through against a running kernel is the
recorded residual.

### 14. The poller archetype (DW-4a, plan D5)

The generic poller makes the archetype's ack semantics control flow: fetch → split items →
stage each → `EmitDurable` each → **only then** advance the cursor. A crash or failed emit
anywhere before the cursor write re-fetches the same page next tick — duplicates, which the
evidence idempotency triple absorbs; never gaps, which nothing could recover. The cursor is
persisted in `ingress_studio_cursors` scoped to **(ingress, transport revision)**: a
revised transport never inherits a cursor whose meaning its predecessor defined. A failed
fetch is a skipped tick (the interval is the retry policy), and a caught-up page with a
stable cursor re-runs harmlessly.

`PollerConfig` declares the paging contract as data: `cursor_param`/`cursor_path` (both or
neither), `items_path` (each member re-serialized canonically — Go's sorted map keys make
the bytes deterministic — else the body verbatim), `delivery_ref_path` (source-native
identity per D7, minted when absent). Pollers authenticate THEMSELVES to the source, so
validation admits only the self-initiated profiles (`bearer_v1` header, `capability_url_v1`
query); signature profiles verify inbound deliveries, which a fetch is not. The manager
serves the same three lifecycle modes as the webhook door — CAPTURING fetches become
redacted samples (and advance the cursor, so a long capture never re-profiles one page
forever), DRY_RUN fetches evaluate into the SHARED preview buffer, ARMED fetches ride the
durable lane — and PAUSED pollers simply do not tick.

**Websocket (DW-4b).** A stream has no ack to give its source, so its honesty lives in
the DECLARED loss model: `lossy` (a message that cannot be made durable is counted and
dropped, loudly) or `sequenced` (the stream's own monotonic counter; the last durable
sequence persists in the SAME store as the poller's cursor — same position semantics,
same revision scoping). On a sequenced stream a failed emit CLOSES the connection instead
of skipping, and the reconnect's first message reveals what was missed — recorded as a
durable gap finding in `ingress_studio_gaps`, never only a log line. Ordering is the
cursor discipline transposed: emit durably → record gap → advance stored sequence; a
crash between steps re-detects (a duplicate FINDING), never loses. Replayed frames at or
below the stored sequence are skipped without minting false gaps. Reconnects back off
exponentially (declared base, capped), resetting on any delivered message.

**SSE: neither archetype nor poller mode — decided against the real stream.** Wikimedia
EventStreams observed live (2026-08-02): its `id:` field is a structured RESUMPTION token
(Kafka topic/partition/timestamp JSON) replayed via `Last-Event-ID` — the poller's
opaque-cursor semantics over a held-open connection with the websocket's reconnect
lifecycle. Both halves already exist (`ingress_studio_cursors` + the stream manager's
backoff loop), so SSE, when a deployment demands it, lands as a third small manager
reusing both with the same durable-before-advance ordering. Deferred until demanded;
v1 ships webhook + poller + websocket.

**Live validation (2026-08-02).** The poller ran the full PRODUCTION lane against a real
public source: USGS `all_hour.geojson` armed in `cambrian_test`, kernel booted — 10 real
earthquakes fetched, preserved as evidence via the real journal → `ingest_raw` watch →
outbox → transformer, 10/10 transform runs `projected`, 10 typed `earthquake` events with
revision-qualified identities and USGS's own unix-millis occurred-at clock.

## What DW-1 ships

The full raw-delivery lane, deterministic end to end: the `ingress-mapping/v1` evaluator
(missing ≠ null ≠ empty enforced at evaluation, explicit timestamp forms, bounded fan-out
with root-scoped identity + member-scoped fields, named discards); the generic webhook
transport (`constrain → verify → stage → EmitDurable → 2xx`, 5xx-over-shed, quota at the
door, per-release pinned specs — an armed door never runs "latest"); the `ingest_raw`
watch action (CID → bytes → `EvidenceIngest` + sidecar, idempotent under replay); the
mapping transformer (claims `ingress:`-prefixed evidence, projects through the ARMED
release's immutable mapping revision, per-revision lineage; mapping refusals and
kind-registry refusals complete-with-log as PERMANENT, transient store errors retry;
paused ingresses leave the backlog pending). Typed-row identity: event `source_ref` =
`ingress:<id>:<source_event_ref|delivery_ref>[:<child>]`, observation `source_ref`
additionally `:<entity>:<predicate>` so siblings never collapse. The armed path contains
no LLM at any granularity; drafting (DW-2) is per-source, once.

## What DW-0A ships

`cambrian-premium/ingressstudio/`: the three resource types + strict parsers, the verifier
profile name registry, the lifecycle state machine, the raw-delivery contract with its
`RawEvidence` mapping, the Postgres store (immutable revisions, releases, lifecycle row,
deliveries sidecar, transform runs), and tests including PG integration tests that prove
revision immutability and the evidence-before-mapping FK. No transport, no LLM, no RPC
surface yet — those are DW-0B/DW-1/DW-2/DW-3.

## Open items (owner decisions, deliberately not made here)

1. ~~SKU/entitlement key~~ — decided by owner directive (§0): plugin ID `ingress-studio`
   is the ADR-0082 key. The Ed25519 verifier remains deferred with ADR-0082 D5, so the
   plugin runs ungated until licensing ships.
2. Final naming ("Ingress Studio" adopted provisionally) — before the panel ships.
3. Drafting model/role + model-egress default posture (D10) — before DW-2.
4. Form-encoded (Twilio) support-or-refuse; SSE as archetype vs poller mode — decided
   against the real sources, in writing (DW-4).

## Addendum (2026-08-06): revise-by-prompt — the drafter gains a base

The mapping stage's own copy promised revision ("What is wrong with this
mapping?" / "Redraft it") but the wiring drafted from scratch off the profile,
discarding the current spec. Closed by giving the SAME drafter a base:

- `Drafter.DraftMappingFrom(ctx, ingressID, guidance, baseSpecJSON)` — when a
  base is present the prompt carries the current mapping inline (operator's
  own validated configuration — trusted material, not payload) plus the
  revision contract: change ONLY what the requested change requires, output
  the FULL revised object, abstain when the change is inexpressible. Same
  repair loop, same strict parser, same execution check, same propose-only
  contract (§12); one deterministic guard on top — a revision must keep the
  stream identity, refused through the repair loop like any parser refusal.
- Wire: `DraftRequest.base_spec_json` (empty = from-scratch, unchanged).
- UI: the mapping stage now passes the shown spec as the base, so "Redraft
  it" finally revises; live ingresses gain a "Revise the mapping" box
  (`ReviseMapping.tsx`) that authors a proposed revision and saves it as the
  fork the shadow-reprocess → promote flow (§11) then carries — authoring
  only, nothing applies until promote, viewers see nothing.

Verified live against the armed `usgs-earthquakes` ingress: the instruction
"add the event itself as a role named quake; rename station to network; add
depth_km from /geometry/coordinates/2" produced exactly that revision on the
first draft (zero corrections), stream and all prior observations preserved,
proposal left unsaved for the operator.

## Addendum (2026-08-06): poller item identity is content-derived when the spec declares none

The poller's cursor discipline has always leaned on the evidence idempotency
triple to absorb re-fetches ("duplicates, never gaps"). That absorption only
happens when a re-fetched item carries the SAME delivery ref — and the
fallback for specs without `delivery_ref_path` minted a fresh random ref per
item per tick, so the triple never matched. Measured on a live deployment:
282,522 evidence rows for 821 distinct bodies (99.7% byte-identical
re-archives) from one polling ingress, ~13k rows/hour — misread as a
transformer backlog until the Overview metric was corrected.

Decision: `splitItems` now derives the fallback ref from the item's bytes
(`itm_<sha256/32>`). A poll is a re-read of the source, not a new event: the
same bytes fetched again are the same item. Consequences, stated rather than
implied:

- An unchanged item re-polled dedups at the archive (same key, same
  revision) — no new evidence, no outbox item, no transformer work.
- A changed item gets a NEW key (a new lineage, not a `revises_id` link).
  Linking revisions of one item still requires `delivery_ref_path`, which
  remains authoritative when declared; the content-derived ref is the floor
  beneath it, not a replacement.
- Byte-identical members within one page collapse to one delivery. They
  carry no distinguishing information a minted ref could add.
- Webhook and websocket deliveries keep minted refs (`MintDeliveryRef`
  unchanged): for a push transport, two identical deliveries genuinely are
  two events.

Follow-up (same day): `splitItems` accepts an OBJECT-valued `items_path` as a
single item — `records_at` names what the item IS, and a single-record
endpoint's item is the object itself. This is what lets an envelope-volatile
whole-body poller (open-meteo's per-request `generationtime_ms` beside a
stable `/current_weather` payload) archive one row per new reading instead of
one per fetch. Discovery derives such roots automatically (ADR-0115
R-ITEM-IDENTITY addendum).
