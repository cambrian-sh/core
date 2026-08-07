# ADR-0118: Substrate Read-Scope and the Premium Agent Retrieval Surface

**Status:** Implemented (gate PASSED 2026-08-06: `substrate-retrieval` suite
16/16 live, up from the pre-change baseline 12/16 — all four programme rows
flipped, zero regressions on the eleven baseline-green rows; runs
`substrate-retrieval-baseline3` → `substrate-retrieval-after3`)
**Date:** 2026-08-06
**Relates to:** ADR-0111 (the query plane this scopes and exposes), ADR-0085/0086
(the Authorizer port and policy PDP this reuses), ADR-0047 (the plane separation
this preserves), ADR-0057/0074 (the seam it crosses), ADR-0054 (the retrieval
pipeline it composes with)

## Context

The substrate programme closed (phases 0–7) with a typed query plane and exactly
one consumer: the operator-side `SubstrateLane.Query`. Three facts make that an
unfinished product rather than a finished feature, all verified live by the
`substrate-retrieval` suite's baseline run (12/16, arm `baseline-pre-programme`):

1. **The lane is unauthenticated.** The operator auth interceptor gates only
   `/cambrian.OperatorConsole/*`; `/cambrian.premium.*` methods pass through, so
   anyone reaching the kernel port can run the full AST (`auth_hole` row, served
   1 row without a bearer). Three separate comments claim otherwise.
2. **Agents cannot reach the substrate at all** — no agent-plane RPC, no scoped
   seam (`surface_missing` rows). The programme ledger already records
   substrate read-scope as "the successor programme's opener", deliberately not
   half-built.
3. **No fact-lane answer cites substrate evidence** (`no_citation` row): a
   `domain.SearchResult` structurally cannot carry provenance, so even a
   perfectly modelled fact is served as just another ranked chunk.

The settled two-lane positioning binds the shape: substrate answers MODELLED
questions exactly (guarantee labels, refusal-only failure), RAG answers
UNMODELLED questions approximately, and composition happens at the ANSWER level
— citations and verification — never by fusing typed rows into a ranked list.
The boundary decision (owner, 2026-08-06): agent-facing substrate retrieval is a
**premium feature**. The OSS kernel keeps the port, the enforcement point, and
inert seams; the premium module supplies the PDP, the surfaces, and the
retrieval hook.

## Decisions

### D1 — Scope enforcement is a kernel PEP wrapping the query plane; principal in, never predicate in

A `ScopedQueryPlane` in `internal/authz/` (the package whose charter is
enforcement points, deliberately NOT pluggable) wraps `domain.QueryPlane`. It
resolves the caller's predicate via `domain.Authorizer.ReadFilter(principal,
surface)` and enforces it on every row. Following the `KernelServices.Documents`
precedent, the seam handed to plugins takes a **principal, never a predicate** —
a seam accepting a `*TagPredicate` would let the plugin choose its own access
scope, which is a bypass with extra steps.

`KernelServices` gains `QueryKnowledge(ctx, principal domain.PrincipalRef, q
domain.KnowledgeQuery) (domain.QueryResult, error)`, built in `app.go` from the
raw plane + the effective Authorizer. The raw `KernelServices.QueryPlane` seam
is REMOVED — it was the single unguarded read path onto the substrate, and its
only consumer (the substrate plugin) moves to the scoped seam in this same
change. OSS behaviour is unchanged by construction: the OSS default
`AllowAllAuthorizer` yields the bypass predicate, so an unscoped deployment
reads exactly as before; the premium authz plugin makes the same chokepoint
fail closed (nil predicate ⇒ deny), preserving invariant 5's deliberate
inversion (memory fails open, scope fails closed).

### D2 — Two-tier enforcement matched to what rows actually carry

Evidence and knowledge items carry `classification TEXT[]`; events,
observations and resolutions do not — they reach classification only through
`evidence_id` / `item_id`. Enforcement therefore differs by shape, and every
shape is written out and labelled (the `EnforcingVectorStore` discipline —
interface embedding is how KG expansion once shipped an unguarded by-ID path):

- **evidence** — filter on the row's own classification. An unauthorized
  evidence read returns zero rows, and never the classification list itself.
- **current / as_of / contradictions** — filter on the backing
  `KnowledgeItem.Classification` BEFORE grouping; a contradiction row must not
  leak one side's value because the other side was readable.
- **point / history / events** — batch-resolve the rows' `evidence_id`s to
  classifications and filter. A row whose evidence cannot be resolved is
  DROPPED for non-bypass principals (fail closed; `observations.evidence_id`
  has no FK, so absence is possible and must not become visibility).
- **traverse** — an edge is visible only if its via-event's evidence passes the
  predicate, and traversal DOES NOT CONTINUE through unauthorized events: a
  forbidden edge is not a bridge, otherwise reachability leaks what the rows
  would not.
- **aggregate** — cannot be post-filtered (the rows are already summed), so
  under a non-bypass predicate the SQL itself gains an `EXISTS` join to
  `evidence` using native `TEXT[]` operators (required `@>`, forbidden
  `NOT &&`, each any-of clause `&&`). Under bypass the query is unchanged.

The predicate lives in Go or in SQL per shape, never both half-way; the
records-lane audit (`nil` predicate DENIES, required positional scope) is the
model for the plane's internals.

### D3 — The auth hole closes by plane declaration, not by prefix accident

`Registry.AddGRPCService` keeps its meaning — an operator-plane mount — and the
operator auth interceptor now enforces exactly that: every mounted premium
service's methods require the operator bearer unless the service was declared
agent-facing. A new **`Registry.AddAgentGRPCService`** (add-many) mounts a
premium service on the AGENT plane: no operator bearer, surface stamped
`SurfaceAgent`, and the caller principal seeded from `x-agent-id` metadata by
the kernel's interceptor chain (the first principal-seeding interceptor;
previously only individual handlers stamped principals). The kernel collects
the service-name sets from the two buckets after registration and hands them to
the interceptors — no premium service name is ever hardcoded in OSS.

Trust honesty: `x-agent-id` remains unauthenticated transport metadata in this
deployment class (the SEC-03 residual). This ADR makes the agent surface
exactly as trustworthy as the rest of the agent plane — no more, no less — and
scope enforcement composes with whatever transport authentication SEC-03 adds.

### D4 — The agent surface is a separate premium service mirroring the AST

`cambrian.premium.substrate.SubstrateRetrieval/Query` — same `QueryRequest` /
`QueryResponse` messages, mounted via `AddAgentGRPCService`. One implementation
serves both planes; only the principal source differs (operator identity vs
`x-agent-id`). It is a distinct service rather than a dual-mode `SubstrateLane`
because ADR-0047's lesson is precisely that overloading one plane for two
audiences smuggles one past the other's auth. Handler flow: principal from
context (absent ⇒ `PermissionDenied`, fail closed at the premium boundary) →
`svc.QueryKnowledge` → rows-as-JSON v1. Guarantee labels and
`ErrCannotExpress → InvalidArgument` refusal semantics are untouched — the
agent gets the same warranty an operator gets.

### D5 — The retrieval loop consults the substrate through a nil-in-OSS port; composition at the answer level only

`domain.SubstrateConsultant` (new port, ~20 lines, next to
`DecisionObserver`): `Consult(ctx, callerID, query string) (*SubstrateCitations,
error)`. `QueryService` gains one nil-default field + setter
(`SetSubstrateConsultant`, the `SetDecisionObserver` idiom — no constructor
change, no MemoryStack change), one nil-checked call site after final lane
assembly, and a plugin registry point (`SetSubstrateConsultant`, replace-one).
The premium substrate plugin injects an implementation that:

- detects substrate-known entity anchors in the query DETERMINISTICALLY (exact
  entity-id match in v1 — the anchor-tier philosophy: same normalization at
  ingest and query, no LLM at this stage; an LLM AST-former is a later arm,
  per-query and therefore permitted, but it must earn its place by
  measurement);
- runs `point`/`current` through the SCOPED seam as the calling agent's
  principal — the hook cannot read what the agent could not;
- returns citations: guarantee label, evidence ids, and the typed rows,
  bounded.

The call site ships citations as ONE synthetic result row (reserved id
`_substrate_citations`, metadata keys `_substrate_guarantee` /
`_substrate_rows` / `_substrate_evidence_ids` — the `_agentic_control`
convention), appended AFTER top-k truncation. Synthetic rather than stamped
onto matching hits because the citation must not depend on RAG recall: on a
large noisy store the corroborating chunk may not be fetched at all, and the
substrate's exact answer to a modelled question is worth carrying even then —
the baseline run demonstrated exactly this failure shape. **Composition rules,
all three invariant-shaped:** typed rows never enter the ranked list and the
synthetic row occupies no top-k slot (non-displacement preserved); any
consultant error or refusal fails open to the ordinary answer (invariant 5);
the consultant's answer is grounded in stored rows by construction (rule 1). `MemoryResult.metadata` is already a
free-form JSON string on the agent plane, so citations ship with ZERO proto
change; the operator plane's typed messages are untouched (a typed citation
field there is a later contract bump when the UI wants it). `WorkspaceStage`
bypasses `QueryService` and is explicitly out of scope.

### D6 — What this ADR deliberately does not do

- **No namespace plumbing** — the plane stays namespace-hardcoded `default`
  (pre-existing, recorded); scope enforcement is orthogonal and composes when
  namespaces arrive.
- **No projection-currentness filter** — the accepted debt that the plane can
  return superseded rows is unchanged here.
- **No per-kind visibility in `KindSpec`** — attractive (deployment-declared,
  boot-validated) but monotonic adoption means undeclared kinds pass, which
  cuts the wrong way for security; deferred until the registry gains a strict
  mode.
- **No OSS config flag** — OSS `ExecutionConfig` names no premium feature
  (ADR-0057 D5); the hook is gated by plugin presence and premium config.

## Gate

The `substrate-retrieval` suite, one arm per slice against the recorded
baseline (`baseline-pre-programme`, 12/16): the access rows must flip
(`auth_hole` → refused, `surface_missing` → served-and-scoped with
`leak_scope`/`over_denial` both absent), `cited_answer` must flip to a
`_substrate_*`-cited answer, and every baseline-green row must STAY green —
in particular the eleven exactness/belief/refusal rows, which price the shipped
query plane and would make any regression here a product regression, not a
programme miss. Plus `make per-pr` in core and the premium test suite.

## Addendum (2026-08-06): the ask/explore surfaces — D5's deferred arm, built and measured

The owner's UI direction ("definitely a natural-language box, no structured
query builder") brought the deferred LLM AST-former forward. Decisions:

### D7 — AskKnowledge: the model forms the query, never the answer

`SubstrateLane.AskKnowledge(question)` compiles ONE natural-language question
into the closed AST with a temp-0 model and executes it through the same
validated, scoped plane. Three deterministic guards keep the no-guessing
property: (1) the model sees a bounded, deterministic CATALOG (entities and
predicates discovered from the question's own tokens, item kinds, actors) and
(2) whatever it emits is name-verified against that catalog in Go — a compiled
query naming anything the store does not hold is REFUSED, never executed —
then (3) `Validate()` refuses anything outside the AST. The compiled query
returns WITH the answer (transparency: the caller always sees exactly what was
asked); open-ended questions refuse with `lane_hint: memory`. Discovery reads
NAMES only; value disclosure happens exclusively through the scoped seam.

### D8 — Catalog reads for the explore surface

`Overview` (per-lane counts, per-KIND counts, per-SOURCE-SURFACE holdings with
their classification tags — the visible form of "ingress data is scoped by its
surface" — outbox backlog, cross-actor contradiction count computed THROUGH
the query plane so it can never disagree with the contradictions view) and
`ListEntities` (names/predicates/counts by substring). Plain deterministic
SQL; introspection is not question-answering, so these are closed RPCs, not
AST extensions. The plugin now reports the `substrate-query` live capability
post-Build (the record-lane precedent) so consoles gate instead of probing.

### Gate

Suite extended to 21 rows (+`ask` ×3, +`explore` ×2; new failure kind
`over_refusal`). Baseline vs live kernel without the surfaces: 16/21 (all five
new rows `surface_missing`; the sixteen prior rows unchanged). After: **21/21**
— including the model correctly compiling the point query, refusing the
open-ended question with the memory lane named, and compiling the
contradiction question (which required one measured prompt iteration:
few-shot examples fixed an over-refusal on entity-filtered contradictions).
Runs: `substrate-retrieval-ask-baseline` → `substrate-retrieval-ask-after2`.

The UI consumes all of this as the Knowledge console (`ui` repo: `/knowledge`,
PRD story 54) — ask box with the compiled-query chip and first-class refusal
rendering, holdings per source surface, entity browser + co-participation
graph, contradictions side by side, and the evidence receipt one click from
any value.

### D9 (addendum 2026-08-06, later the same day) — catalog discovery anchors on predicates and event types, not only ids

The first live user question ("When did the last earthquake happen and what
was its magnitude") was refused: discovery anchored only on IDENTIFIER-shaped
tokens, and a real feed's entity ids are opaque codes — the word "earthquake"
appears nowhere in a USGS quake id. The productive anchor was in the question
all along: **"magnitude" is a predicate name**. Discovery now has three
anchors — id tokens against entity ids; content words against PREDICATE names
(consultant normalization: every name part present in the question); content
words against EVENT TYPES — each pulling the most recent carrying entities.
The catalog lists entities NEWEST FIRST with their latest record time and the
prompt says so, which is what makes "the last / latest X" compilable: point
on the first carrying entity, `occurred_at` answers the "when". Name
verification is unchanged — looser discovery cannot widen disclosure, because
values still flow only through the scoped plane.

Gate: suite row `ask_latest_class` (a class question with no entity id
anywhere); full suite **22/22** (`substrate-retrieval-class-after`), and the
original earthquake question answers live: `point · tx2026pizbxl · magnitude`
→ 2.2 at 2026-08-06T11:17:44+03:00, receipt attached.

### D10 (addendum 2026-08-06) — union-pruned rows, multi-predicate points, threshold refusal, word-anchored discovery

Four fixes from the first hours of real use, gated together (suite 23/23,
`substrate-retrieval-d10-after`):

- **Rows emit only the value member `value_type` selects** (`obsRow`): an
  unset Go `time.Time` serialized as year 1 and read as data. Unset union
  members and empty `location` are now absent, not zero.
- **Multi-predicate point compilation**: a question asking several attributes
  of ONE entity ("when and where and how strong") compiles to one bounded
  point per predicate (≤4, each name-verified and validated), rows merged
  under one guarantee; the reported compiled query carries the predicate
  list. Answers must never silently drop an asked-for attribute.
- **Value thresholds refuse by rule**: "significant", "major", "above 5" are
  not expressible as filters in the closed AST; the prompt instructs refusal
  naming the filter rather than silently answering the unfiltered question.
- **Discovery anchors on plain words too**: content words now match entity
  ids as substrings ("erzincan" → `station/erzincan`) and predicates/event
  types by substring overlap ("weather" → `weathercode`). Verified live:
  "When and where was the last earthquake, what was its magnitude?" →
  place + magnitude in one answer; "How is the weather right now in
  erzincan" → temperature/weathercode/windspeed/winddirection at the latest
  observation. Name verification unchanged — looser discovery cannot widen
  disclosure.

Recorded, not fixed here (mapping-lane items): the openmeteo mapping
materializes current weather only — no forecast rows exist, so forecast
questions cannot be answered until the mapping extracts them.
