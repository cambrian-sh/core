# ADR-0119: The history lane — a second transport per ingress

**Status:** Implemented (D1–D6 shipped 2026-08-09; validated against live PostgreSQL)
**Date:** 2026-08-09
**Amends:** ADR-0112 (ingress studio resources — a transport revision gains a role, and a release
pins two of them). Consumes the backfill machinery built against
`cambrian-premium/docs/known-gaps/customer-history-onboarding.md` §11.
**Origin:** owner direction, 2026-08-09, after the customer-history backfill shipped for poller
sources and left every push source unfillable. Requirement R2 of that gap document.

## Context

Every connection has two halves — the live tail and the history — and we ship one and a half. The
backfill that landed on 2026-08-09 loads a poller source's past end to end, but for most real
sources **the thing that delivers new records is not the thing that can be asked for old ones**:
Slack pushes events and serves history from `conversations.history`; Jira and Zendesk push webhooks
and serve history from a search or export API. Those are the sources customers actually name.

Which lane is *practical* is a per-vendor fact, and Slack is the cautionary case. Its history API
is not one stream but a nested walk — `conversations.history` pages one channel's parent messages,
and every thread needs its own `conversations.replies` call — and since May 2025 a commercially
distributed non-Marketplace app is limited to **1 request per minute, 15 messages per request** on
exactly those methods (a customer's own internal Slack app keeps the older ~50/min tier). Nine
hundred messages an hour against two years of a busy workspace is not a fill, it is a season.
Slack's admin **export** — a zip of per-channel, per-day JSON files, each an array of the same
message objects the API serves — carries the identical item shape with none of the walking. So for
Slack the expected route is an **upload** (R3's file archetype, first-class in D6), and the
API-paging lane is the fallback for deployments whose own internal app can still page at the old
tier. This ADR builds the lane both routes run through; it does not assume the API route is the
practical one.

The blockage is one field. A `Release` — the immutable pairing an armed ingress pins — carries a
single `TransportRevision`, and `StartBackfill` therefore resolves exactly one transport and refuses
anything that is not a poller:

```go
// ingressstudio/service_backfill.go:74
if spec.Archetype != ArchetypePoller {
    return nack(fmt.Errorf(
        "ingress %q has a %s transport, and a push transport cannot be asked for history … "+
        "Loading this source's past needs either a second, history-only transport bound to this "+
        "ingress or an upload of an export — neither is built yet", id, spec.Archetype)), nil
}
```

That refusal already names this ADR's job. Four things checked in the code shape the answer, and
three of them mean this is smaller than it looks:

- **The fill's cursor is already its own.** A fill walks the cursor on its `ingress_studio_backfills`
  row exclusively — the history transport never touches `ingress_studio_cursors`, which only the live
  fetch reads. That separation is deliberate — "sharing one would make asking for history rewind the
  live poller" — and it exists today, so the two-lane design inherits it rather than building it.
  (`ingress_studio_cursors` is keyed `(ingress_id, transport_revision)`, so if a replay-capable
  history lane ever *does* poll, its cursor row separates for free too.)
- **The fill already records its transport.** `BackfillRecord.TransportRevision` exists and is
  populated; it simply has only one candidate today.
- **`items_path` is transport-side, not mapping-side.** It lives in `PollerConfig`, and a mapping's
  pointers are relative to the *item*. So the envelope difference between two APIs is absorbed by the
  transport, and the two lanes usually yield the same item: Slack's `messages[]` and its Events
  `event` both produce a message object; Jira's webhook `issue` and its search `issues[]` both
  produce an issue.
- **Credentials are named `ingress:<id>:<name>`** with no lane dimension, and `SetCredential` builds
  that name itself.

## Decision

### D1. A transport revision has a ROLE; the revision counter stays per ingress

`TransportRevisionRecord` gains `Role` (`live` | `history`) as an ordinary column. **The primary key
does not change:**

```
ingress_studio_transport_revisions
  + role TEXT NOT NULL DEFAULT 'live'
  PK (ingress_id, revision)        ← unchanged
```

**Why a role and not simply another revision.** Revisions are a *timeline* — `n+1` supersedes `n`,
rollback arms an earlier one. Without a role, "the history one" would be a fact nobody records, and
the first rollback would prove it: rolling the live transport back one revision would land on the
history spec. The role is what lets each lane have its own ordered history of edits, selected with a
filter — `latest revision WHERE role = 'history' AND revision < current` — rather than with a
separate counter.

**Why the counter is NOT per role**, though that is the tidier-looking option. Four existing
structures key on a bare revision integer, and per-role numbering would give both lanes a revision 1:

- `ingress_studio_deliveries.transport_revision` records, on **every preserved delivery**, which
  transport accepted it — a bare integer beside the ingress id, and it is what retroactive repair
  reads. Per-role numbering would make every backfilled sidecar ambiguous between lanes: "which spec
  fetched this record?" — a provenance question — would stop having an answer.
- `ingress_studio_cursors` is `PRIMARY KEY (ingress_id, transport_revision)`. Two lanes numbered from
  1 would share one cursor row the day a replay-capable history lane polls; with one counter the two
  lanes hold distinct integers and can never collide there.
- `ingress_studio_releases` carries
  `FOREIGN KEY (ingress_id, transport_revision) REFERENCES ingress_studio_transport_revisions(ingress_id, revision)`.
  Widening the referenced key makes that pair non-unique, and Postgres cannot keep a composite FK
  against a non-unique target — so the FK would have to be dropped and rebuilt with a role column
  beside it.
- `GetTransportRevision(ingressID, revision)`, `GetPollCursor`/`SetPollCursor`, `RecordGap` and
  `BackfillRecord.TransportRevision` all name a revision by bare integer and would every one of them
  become ambiguous.

The cost of one counter is cosmetic: the two lanes interleave, so a live transport's revisions may
read 1, 3, 4 while its history lane reads 2, 5. Each lane's own order is still total, which is all
rollback needs.

This keeps this table's migration to what it should be — **one column with a default** (D2 adds one
more, nullable, to `ingress_studio_releases`). Every existing row is `live`, every existing FK is
untouched, an ingress with no history lane has `live` revisions only, and a fill behaves as D5
says. That matters particularly here because the studio owns its own DDL through
`CREATE TABLE IF NOT EXISTS`, which does not alter an existing table's primary key: a PK change
would have needed migration machinery this component does not have.

**One accessor must learn the role, or the role breaks the live lane.** Every live transport
manager resolves pre-ARMED states through "the latest revision" — `transportFor` in the poller and
websocket managers and the webhook router's CAPTURING/DRY_RUN branch all call
`LatestTransportRevision`. Once a history revision is saved, it *is* the latest. The failure is
concrete: an ARMED Slack ingress gains a history lane (now the newest revision), and later walks
`PAUSED → MAPPED → DRY_RUN` to change its mapping — during which the poller manager would run the
live lane on the **history** spec, polling `conversations.history` as if it were the feed. So from
this ADR on, `LatestTransportRevision` means **latest `live`-role revision**; a role-aware sibling
(`LatestHistoryTransportRevision`) serves the history lane; and `IngressStatus`'s
`latest_transport_revision` keeps meaning the live lane on the wire. `GetTransportRevision`,
`GetPollCursor`/`SetPollCursor`, `RecordGap` and every other bare-revision accessor is genuinely
untouched — a bare revision still names exactly one spec.

The filter belongs to the **read accessors only**, and the store's internals make the wrong
implementation tempting enough to name. Revision *allocation* is its own inline query —
`COALESCE(MAX(revision), 0) + 1` inside `saveRevision`'s transaction — and it must stay role-blind:
that is precisely what interleaves the counter, and filtering it would hand both lanes the same
next integer and collide the primary key. And `latestRevision` is a *shared* helper —
`LatestMappingRevision` runs through it too, and mappings have no role — so "add a role filter to
`latestRevision`" breaks mappings. The role predicate lives in the transport-table read paths and
nowhere else.

**How a history transport is authored.** The role is declared **in the `TransportSpec` itself**, not
in the RPC that saves it:

```json
{ "archetype": "poller", "role": "history", "poller": { … }, "credentials": [ … ] }
```

`SaveSpecRequest` is `{ingress_id, spec_json}` and stays exactly that — no wire change to the
premium ingress proto — because the strict parser already reads the spec and can default `role` to
`live` and validate it (a `history` role on a `webhook` is refusable at parse time, before it ever
becomes a revision, which is where this system prefers to refuse). `SaveTransportRevision` reads
`spec.Role` and stores it. The division of labour with D6 is deliberate and narrow: parse time
refuses only the *categorically* impossible — a receive-only archetype in a history role, which is
a fact about the source — while whether *this build* can walk a given history transport is D6's
question, answered by a runner at `StartBackfill` time. The two checks never encode the same rule
twice.

`GetTransportRevision(RevisionRef{ingress_id, revision})` also needs no change — and that is the
second payoff of keeping one revision counter per ingress: a bare revision number still identifies
exactly one spec, whichever lane it belongs to.

### D2. One release pins both lanes, and they move together

```
Release
  TransportRevision         int    // live
  HistoryTransportRevision  *int   // nil ⇒ no history lane
  MappingRevision           int    // ONE mapping — it projects both lanes (D3)
```

Rollback reverts both lanes (it re-pins a release, and a release carries both); editing either lane
leads to a new release. The release stays the single immutable answer to "what is live?", which is
what ADR-0114 D34 requires of one lifecycle driving another — two independently-armed lanes would be
two state machines that can disagree, and a fill running a spec nobody armed is a fill whose output
was never dry-run.

**How the history transport gets into a release.** "Editing mints" needs verbs, and today only
`Arm` mints — so the mechanics are these, all additive:

- **`ArmRequest` gains `history_transport_revision`**, optional, beside the explicit live pair it
  already carries. Proto3's zero value is a safe "none" — revisions start at 1 — so the wire change
  is additive, the same class of change as the backfill verbs themselves. Implicitly pinning
  "whatever history revision is newest at arm time" was considered and rejected: it would be the
  one place this system pins newest rather than named, and the very next paragraph forbids exactly
  that for fills.
- **A new verb, `SetHistoryLane`, serves the ingress that is already ARMED** — the flagship case:
  Slack is live, and the history lane arrives later. Without it, the only route to a new release is
  `Pause → MAPPED → DRY_RUN → Arm`: a full re-rehearsal of a live lane that is not changing.
  `SetHistoryLane` mints a release copying the pinned live pair verbatim and adding (or clearing)
  the history transport, then re-pins. It is legal on an ARMED ingress precisely because the live
  half is bit-identical — which also means a fill already running is unaffected by it.

  *As built:* it is a **store verb**, one transaction under the ingress row lock, not three calls
  composed on the plane. Two reasons, and the first is a hard constraint rather than a
  preference. `PinRelease` refuses outside `DRY_RUN`/`PAUSED` — "arming always follows a dry
  run" — and that guard is right: pinning an *arbitrary* release while armed would arm a live
  half nobody rehearsed. What makes this call legal is the narrower fact that the live half is
  copied verbatim, and the only place that can be *checked* rather than promised is inside the
  transaction that copies it. Second, read-then-mint-then-pin would race an operator arming
  concurrently, and could leave a release minted but unpinned. PAUSED is admitted alongside ARMED
  for the same reason ARMED is: the live half does not move either way. Every earlier state is
  refused with `Arm`'s new field named, because before the first arm there is no live pair to
  copy.
- **`MintRelease` validates role agreement**: the history slot must name a `history`-role revision
  and the live slot a `live`-role one, refused otherwise. This is the one place the role column
  binds — without it, D1's rollback story is discipline rather than construction.
- **`IngressStatus` gains `armed_history_transport_revision` and
  `latest_history_transport_revision`**, both `0` when absent — the same sentinel `ArmRequest`
  uses. The console can only show what the wire carries: without these, "the console gains a second
  transport to show per ingress" (Consequences) would be a promise the proto cannot express — the
  exact shape of defect that has bitten before, a DTO short of the field its feature needs. The
  armed field says which history spec the pinned release runs; the latest field is what lets a
  console show "a history spec exists but is not pinned yet", which is precisely the state
  `SetHistoryLane` exists to consume.

A fill therefore always runs a **pinned** revision — `release.HistoryTransportRevision` when set,
else the live `release.TransportRevision` (D5) — never "the latest history spec". This is the same
rule arming already applies to mappings: pinned, not newest.

**An arm pins exactly what it names, so a re-arm that omits the history lane DROPS it.**
(Owner decision, 2026-08-09, taken during implementation.) The alternative — carrying the
currently pinned history revision forward whenever the field is absent — was considered and
rejected as the same implicitness this decision rules out everywhere else. The cost is real and is
paid deliberately: a caller that re-arms after a mapping change without re-naming the lane loses
it, which is the one way a deployment can stop being able to read a source's past without asking
to. Two things make that survivable, and both are construction rather than convention. The
**spec is not lost** — only the pin is — so `SetHistoryLane` restores it without re-authoring
anything; and `Arm`'s ack **says the lane was dropped and names the revision**, rather than
leaving it to be discovered by a backfill that refuses months later.

The schema delta is one nullable column and one more foreign key, which is valid precisely
because D1 left the referenced key alone:

```sql
ALTER TABLE ingress_studio_releases
  ADD COLUMN IF NOT EXISTS history_transport_revision INT NULL;
DO $$ BEGIN
  ALTER TABLE ingress_studio_releases
    ADD CONSTRAINT ingress_studio_releases_history_transport_fk
    FOREIGN KEY (ingress_id, history_transport_revision)
      REFERENCES ingress_studio_transport_revisions(ingress_id, revision);
EXCEPTION WHEN duplicate_object THEN NULL; END $$;
```

The `DO` guard is not decoration. Postgres has no `ADD CONSTRAINT IF NOT EXISTS`, and this DDL
block runs at **every boot** — the table's existing composite FKs are idempotent only because they
live inside `CREATE TABLE`, a shelter a column added later cannot use. Without the guard, the
second boot errors; with it, the constraint is enforced by the database, which is where this
store puts every rule it can (the one-running-fill index made the same choice). If guarded
constraints are ever added beside this one, keep **one `DO` block per constraint**: a PL/pgSQL
`EXCEPTION` clause rolls back its whole block, so a combined block can never converge from a
half-state — the constraint that exists raises, the handler swallows it, and the one that is
missing is silently never created, on every boot, forever. A composite FK with a NULL member
passes under `MATCH SIMPLE`, which is exactly what lets the column be nullable.

Nullable rather than defaulted: `NULL` means "this ingress has no history lane", which is a fact
about the ingress, not a missing value to be filled in. Every existing release is correct as it
stands.

This column and D1's `role` follow a pattern the studio already uses — its DDL block
already contains `ALTER TABLE … ADD COLUMN IF NOT EXISTS` statements (the `egress` column, the
DW-1R delivery columns) beside the `CREATE TABLE IF NOT EXISTS` statements. The guarded-constraint
`DO` block above is the one new shape, and it stays inside the same boot-time DDL discipline. So
this needs no migration machinery the component lacks, which is only true because D1 changes no
primary key.

### D3. The history lane shares the mapping — by construction, not by option

The release pins **one** mapping, and it projects both lanes. This is first a fact about the
records: `items_path` is transport-side (see Context), so both lanes hand the mapping the same
kind of item, and the same record produces the same knowledge whichever lane delivered it — the
property the whole two-lane design exists to protect.

It is also a fact about the machine, and the machine settles what a gate could only guard. A fill
routes every preserved record through the **same armed pipeline** the live lane runs —
`armedPlanFor(ingressID)` resolves the single pipeline `ingress:<id>`, whose `apply_mapping` and
`save` nodes carry the release's mapping revision baked into their config. The routing code says
it itself: *"Same router, same registry, same armed revision — so the past and the present"*
project identically. A per-lane mapping override was considered and **rejected**: making it real
would need lane-qualified armed pipelines (`ingress:<id>:history`) — two armed pipelines per
ingress, two answers to "what is live?", against everything D2 stands on — and making it a mere
release field without that machinery would be worse: a setting every gate approves and the runtime
silently ignores.

The source whose history endpoint returns a *thinner or different* record than its live event is
not stranded — it is served by the route that already owns reshaping: the **upload**. R3's file
archetype carries its own declared normalisation spec, so an awkward history shape is normalised
into the same items at the transport, where `items_path` already does that job for pollers — the
mapping stays single and shared. If a real source ever appears whose API history is genuinely
unmappable by the shared mapping *and* whose data cannot arrive as an export, a per-lane override
gets its own ADR, with the pipeline work priced in.

There is consequently **no rehearsal machinery for the history lane** — no `PreviewBackfill`
verb, no preview gate, no second lifecycle (`CAPTURING → MAPPED → DRY_RUN` stays a live-lane
story; a second state machine per ingress is the drift D34 and §8 both steered away from). The
mapping that projects the history *is* the mapping the operator already rehearsed through DRY_RUN
and armed — sharing it is what makes rehearsing it twice redundant.

### D4. Credentials keep the flat namespace; the lanes bind different names

No change to the secret store or to `SetCredential`. The history transport's spec declares its own
`CredentialBinding`, pointing at a differently-named secret:

```
ingress:acme-slack:bot_token      ← live events
ingress:acme-slack:history_token  ← conversations.history (needs channels:history)
```

Two lanes share a credential only when they name the same one, so sharing is a decision rather than
an accident. Adding a lane segment to the name would isolate them by construction, but it changes a
scheme deployments already hold secrets under — a migration for something convention solves.

### D5. The refusal becomes a route

`service_backfill.go:74` stops being a dead end. `StartBackfill` resolves the fill's transport from
the pinned release — **`HistoryTransportRevision` when set, else the live `TransportRevision`** —
and asks a runner to accept it (D6 — not "be a poller"; that test is what D6 replaces). The live
transport's archetype stops being decisive on its own — a webhook ingress with a history lane is
fillable, which is the entire point.

The fallback to the live transport is not a convenience; it is what keeps this ADR from shipping a
regression. A plain poller ingress — the sources the backfill shipped for on 2026-08-09 — fills
today through its live transport with no further setup. "History lane required" would take that
away and hand it back as paperwork: a second, identical copy of the transport the ingress already
runs, re-authored with `role: history`. With the fallback, a poller fills exactly as it does now
and a push source without a history lane is refused by its runner's own answer.

The refusals that remain, and their new wording, matter as much as the route: a push ingress with
no history lane must be told that it needs one and which verb adds it (`SetHistoryLane`, D2), not
that "a push transport cannot be asked for history", which will have stopped being the reason.

### D6. Fillability is a runner's answer, not an archetype test

A history transport is **not** required to be a poller. What it must be is *askable for the
past*, and the archetype is a poor proxy for that — which the current code already shows,
because the archetype test cannot stand alone:

```go
if spec.Archetype != ArchetypePoller { … }              // proxy
if spec.Poller == nil || spec.Poller.CursorParam == "" { … }   // the real condition, partly
```

The proxy is wrong in both directions. It excludes a file upload, which does not merely
support history but *is* history. It admits a cursorless poller, which is why the second
check had to be written.

So `BackfillRunner` gains `CanFill(spec) error`, and runners register per archetype behind
the port that already exists:

```go
type BackfillRunner interface {
    CanFill(spec TransportSpec) error   // nil ⇒ this runner can fill it
    StartFill(ctx context.Context, rec BackfillRecord, spec TransportSpec) error
    CancelFill(ctx context.Context, ingressID, backfillID string)
}

registry: poller → PollerBackfill      (built)
          file   → FileBackfill        (R3, when it lands)
```

*As built:* the registry is a type (`BackfillRegistry`), and `AdminService.Backfills` holds one
rather than a single runner. `Empty()` is what the plane asks instead of the old `== nil`, so a
build with nothing wired refuses with the same character it always had. The plugin registers
`ArchetypePoller → BackfillManager` and nothing else, so every other archetype is refused by name.

`StartBackfill` looks the runner up and asks it, instead of testing the archetype itself.
The refusal then comes from the component that would have had to do the work and names its
own reason: a cursorless poller and a webhook are both refused, differently and correctly,
and the existing fail-closed behaviour for an unwired runner becomes "no runner for this
archetype" without changing character. `CancelBackfill` needs no equivalent lookup logic
invented: `BackfillRecord.TransportRevision` resolves to a spec and therefore an archetype,
so the registry finds the right runner from the record alone.

Sorted by capability rather than by name:

| Archetype | Fillable | Why |
|---|---|---|
| `poller` with a cursor | yes | pages from `from` — the runner that exists |
| `file` (R3) | yes | the artifact *is* the past; `from` may be meaningless |
| `websocket` | only if it declares replay-from-cursor | the WS manager already keeps sequenced streams with a last-durable-seq and records gaps, so replay is plausible — but declared, never assumed |
| `webhook`, `inbound` | never | they receive; there is nothing to ask |

**An upload rides this same lane, for any source.** A bulk export — Slack's zip, a Jira or
Zendesk export, a CSV dump from a database nobody will grant API access to — is authored as
a `{archetype: "file", role: "history"}` revision (its normalisation spec is R3's subject),
pinned like any other history transport, and filled by `FileBackfill`. The lane is
deliberately route-agnostic: one source may page an API, another may take an upload, a
third may do both over its life — each is just a history revision with a different
archetype, and nothing in the lane machinery knows or cares which. Bulk data does not need
a second mechanism, and no vendor's constraints are baked into the design.

**Not a declared field on the spec.** `history_capable` would be a property of our *code*,
not of the source, so a spec could claim a capability no runner implements — a field that
lies about the system rather than about the data. Every other declared field on a transport
describes the source; this one would not.

Three things differ per archetype, which is why this is a registry and not a widened
allowlist: **how a fill walks** (page a cursor / read an artifact / replay a sequence),
**how it terminates** (caught up to live / EOF / reached the live seq), and **what `from`
means** (a cursor value / a filter or nothing / a sequence number). Those belong inside a
runner, not in a service handler's `if`.

## What this ADR does NOT decide

Named so they are not assumed settled:

- **Which archetypes actually get a runner, and when.** D6 says fillability is a runner's answer,
  but only `PollerBackfill` exists. `FileBackfill` arrives with R3 — whose own shape was settled on
  2026-08-09 (a declared normalisation spec drafted by the existing `Drafter`, so a file produces
  items the way `items_path` does and the armed path stays deterministic) and which gets its own
  ADR when scheduled. Whether a websocket replay runner is ever worth building is undecided; the
  capability is expressible, nobody has asked for it.
- **Source-side rate limiting.** The execution lane built in §11 of the gap document keeps a fill
  from starving live
  *pipeline* work, but Slack's history API is throttled per channel, so a large workspace is bounded
  by the vendor. That is a transport-level budget this ADR does not add.
- **Backfilling more than one lane.** One running fill per ingress is a partial unique index today.
  Whether that stays per-ingress or becomes per-lane is a question for when a second fillable lane
  exists.
- **A fill that outlives a re-arm.** `SetHistoryLane` copies the live pair verbatim, so a running
  fill is unaffected by it — a point in its favour. The narrow exposure is `Arm` with a *new
  mapping revision* while a fill is running: the remainder of that history projects through the new
  mapping. Whether to refuse arming during a fill, cancel the fill, or accept the seam is not
  decided here.

## Consequences

- Slack, Jira and Zendesk become fillable — the sources §1 of the gap document is about — and each
  by whichever route its vendor makes practical: an API-paging history transport, an export upload
  (once R3's `FileBackfill` lands), or both. The lane does not pick for them.
- Nothing fills by itself. A history lane is authored by an operator, pinned by name, and walked
  only when `StartBackfill` is called with a reason; the lane is declared capacity, not behaviour.
- Every existing ingress keeps its current behaviour — and D5's fallback is what makes that
  sentence true: a poller ingress with no history lane fills through its live transport exactly as
  it does today, and a push ingress with no history lane is still refused, now with the verb that
  would fix it named in the refusal.
- The console gains a second transport to show per ingress, and "is this source's history loaded?"
  stays a question about one ingress rather than two.
- The two lanes cannot produce different knowledge from the same record — not by gate but by
  construction: one pinned mapping projects both the past and the present, and the fill runs the
  very pipeline the live lane runs (D3). There is no override to guard.
- `BackfillStatus.transport_revision` needs no wire change, but the integer it carries names a
  history-lane revision whenever one is pinned — consoles should label it as the lane's revision,
  not as "the transport revision" bare.
- A deployment can DECLARE its history lane: `cambrian-apply`'s `IngressSpec` gains
  `history_transport`, converged against `latest_history_transport_revision` and named on every
  arm. That last part is a requirement rather than a nicety, and it follows directly from the
  decision above that an arm pins exactly what it names: an installer that did not know about the
  lane would unpin a hand-pinned one on the first re-arm after a mapping change. There is
  deliberately no `history_mapping` key — D3 shares the mapping by construction.
- No core contract bump: the studio's admin plane is the premium `ingress` proto, as with the
  backfill verbs themselves. The premium proto changes are additive — one optional field on
  `ArmRequest` (`history_transport_revision`), two on `IngressStatus`
  (`armed_history_transport_revision`, `latest_history_transport_revision`), and the
  `SetHistoryLane` verb.
