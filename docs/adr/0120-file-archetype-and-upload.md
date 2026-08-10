# ADR-0120: The file archetype — loading a history from an export

**Status:** Implemented (D1–D6 shipped 2026-08-09)
**Date:** 2026-08-09
**Extends:** ADR-0119 (the history lane — this is the second route into it, and the one D6
was written to accommodate). Amends ADR-0112 (a fifth transport archetype; a seventh
verifier profile).
**Origin:** requirement R3 of `cambrian-premium/docs/known-gaps/customer-history-onboarding.md`.

## Context

ADR-0119 gave an ingress a second transport so a source that pushes its present can be asked
for its past. For a source with a usable history API that is the whole answer. For the sources
customers actually name it is half of one.

Slack is the case that decides this. Since May 2025 a commercially distributed non-Marketplace
app is limited to **one request per minute and fifteen messages per request** on
`conversations.history` — nine hundred messages an hour, against a workspace that has years of
them. The API-paging lane ADR-0119 shipped makes Slack *fillable*; it does not make it
*practical*. Slack's admin export is a zip of per-channel, per-day JSON files carrying the
identical message objects, and it moves in minutes. And for a regulated customer who will not
grant historical scopes at all, an export is not the faster route, it is the only one.

So the export is not a fallback. It is the expected route for the flagship source, and the
requirement said so: *"Accept an export — CSV, NDJSON, JSON, a zip of them — through the same
mapping, evidence and projection path as any other delivery. This is what 'upload our history'
means literally."*

**What was actually missing is smaller than "support files" and more specific.** Everything
downstream of a transport — the mapping's JSON Pointers, `delivery_ref_path`, the content
digest that gives a record its identity — assumes something has already turned bytes into
ITEMS. For a poller that something is `items_path`. For a file there was nobody at all:

```go
// transport_poller.go, splitItems
dec := json.NewDecoder(strings.NewReader(string(body)))
if err := dec.Decode(&doc); err != nil {
    return nil, "", fmt.Errorf("response is not JSON: %v", err)
}
```

A CSV export dies on that line. So does an NDJSON dump, and so does a gzipped anything.

## Decision

### D1. One format seam, shared with the poller

Turning bytes into records is `SplitStream(format, io.Reader, RecordShape, cap, sink)` in
`ingressstudio/bodyformat.go` — not a method on either transport. The poller adopts it by
declaring a `body_format` and calling it; the file archetype is built on it from the start.

Two dispatchers over the same format names would be two vocabularies that drift. Formats are a
**closed set** — `json`, `ndjson`, `csv`, `tsv` — with `gzip` as an orthogonal wrapper, and an
unknown one is refused by name rather than falling back to JSON.

**The poller is wired to it (2026-08-09, same day).** `PollerConfig` gained `body_format`,
`compression` and `shape`, and `splitItems` dispatches: the JSON path runs the original code
**unchanged** — every poller in production runs it, and a rewrite that happens to be equivalent
is a claim rather than a fact — while every other format goes through `SplitStream`. So the
seam has two real consumers rather than one and an intention, which is the only version of
"shared" that means anything. A gzipped JSON feed became readable as a side effect; it never
was before.

One honest limit came with it: `cursor_path` is a JSON Pointer into a response body, and a CSV
has none. A non-JSON poller is therefore **stateless** — validation refuses a cursor on those
formats and names `cursor_source` (Axis 2 of `/todo.md` item 1, unbuilt) as the missing piece,
rather than letting a spec look paginated while re-reading page one for ever. A consequence
worth stating plainly: **a non-JSON poller cannot be backfilled**, because `CanFill` requires a
cursor.

**Declared, never sniffed.** A source that changes its `Content-Type`, or an export whose first
line happens to look like JSON, must not silently change how it is parsed: that failure surfaces
as a mapping that stopped matching rather than as a transport that changed.

**Records come out as JSON objects, whatever went in.** A CSV row leaves as
`{"order_id":"A-1","total":1234.56}`. That is what "through the same mapping path as any other
delivery" means literally — the mapping language is JSON Pointers over an item, so a row that
has become an object needs no second mapping language, no second identity rule and no second
coercion vocabulary. Keys are marshalled by `encoding/json`, which sorts them, so the same row
always produces the same bytes — and item bytes ARE the identity, so a format that emitted map
order would re-archive a source on every read.

### D2. The normalisation spec is transport-side, and it is the actual work

An upload is not a clean rectangle. `RecordShape` declares what a real export does:

| Field | The thing in the file |
|---|---|
| `header_row` | a title and a date range above the header |
| `delimiter` | the semicolon a European ERP calls CSV |
| `drop_trailing_rows` | the `TOTAL` line at the foot |
| `number_columns` | `1,234.56`, `1.234,56`, `(890.00)` |
| `items_path` | the array inside a nested document |

Every field is inert by default, so a well-formed file declares none of them.

This lives on the TRANSPORT and not in the mapping, and that placement is forced rather than
chosen: ADR-0119 D3 makes one mapping project **both** lanes, so teaching it that `1,234.56` is a
number would change how the live lane reads its own JSON, where `"1,234.56"` is a string and
means it. The grouping is a fact about this file.

Two details are worth recording because both were found by tests rather than by reasoning:

- **`header_row` counts physical lines; `drop_trailing_rows` counts records.** They disagree,
  and they have to. An operator counts lines in an editor, blank ones included — but
  `encoding/csv` does not consider a blank line to be a record, so a record-counted `header_row`
  would be wrong by however many blank lines a preamble contains. The lines above the header are
  therefore consumed as lines, before parsing begins, which is safe precisely because they are
  not CSV.
- **"Stop at the first blank row" was designed, built and CUT.** It is the natural way to
  describe a totals block, and it cannot work: `encoding/csv` skips blank lines entirely, so the
  setting would have been silently inert on exactly the files it was written for. Detecting one
  means finding record boundaries ourselves, and a quote-aware boundary scanner is the CSV
  parsing this deliberately does not write. `drop_trailing_rows` does the same job with the
  library intact.

**No hand-rolled parsers** (owner directive, 2026-08-09). For every format here the library is
the standard library — `encoding/csv`, `encoding/json`, `compress/gzip`, `archive/zip` — so this
adds no dependency at all. Premium still has two.

### D3. A file is a transport archetype, not a special case

`{archetype: "file", role: "history"}` is a transport revision like any other: pinned by a
release, filled by the runner registered for its archetype. Nothing in ADR-0119's lane machinery
learned that files exist — which is what D6 of that ADR was for.

A file transport is legal in the **live** role too, and that is not an oversight. An upload-only
ingress — "here is ten years of orders, we have no API" — arms with a file as its live transport
and is filled through it by ADR-0119 D5's fallback. No live manager picks it up, because all
three filter on their own archetype, so a file transport delivers nothing until it is asked to.

`FileBackfill` differs from the poller's runner in exactly the three ways ADR-0119 D6 predicted a
runner would: **how it walks** (an artifact end to end, not a cursor), **how it terminates** (the
artifact is exhausted — there is no catching up to a live edge, because an export has a last
record), and **what `from` means** (a record ordinal to resume at). Everything after "here is a
record" is shared: the same `DeliveryPreserver`, the same evidence idempotency, the same armed
pipeline on the backfill lane.

Zip entries are read in **name order**, which is load-bearing rather than tidy: a fill records
progress as a record ordinal, and a count is only a position if the same artifact always yields
the same sequence.

### D4. The artifact travels over HTTP; only the reference travels on the admin plane

A real Slack export is measured in gigabytes. Chunking that over a client-streaming RPC is a
couple of thousand round trips plus a resume story of its own, to move bytes that HTTP already
moves in one streaming request from curl, a browser, or a console. So:

```
POST /ingress/<id>/artifacts        →  {"artifact_ref": "art_8f2c1a…", "bytes": 2411533021}
```

mounted on the studio's existing receiver, streamed straight to disk, never held in memory,
written to a temporary file and renamed only once the whole body is durable — so an interrupted
upload leaves nothing a spec could later name and a fill could read as a truncated history.

**References are minted, never accepted.** The name comes from the endpoint, the reader rejects
anything that is not a minted name, and the ingress id is pattern-bounded. Path traversal is
therefore not merely blocked but inexpressible: there is no reference that names a location.
`artifact_ref` in a stored spec is data, and data that could name a path would be a spec that
reads whatever the process can.

### D5. The upload is authenticated by an ingress capability, not the operator token

The endpoint compares a bearer, in constant time, against the secret `ingress:<id>:upload` —
set once through the admin plane's existing write-only `SetCredential`, which builds the name
itself and can never read a value back.

Deliberately **not** the kernel's operator token. This is a data-plane intake, the same kind of
thing a webhook route is, and handing a bulk-upload URL the credential that controls the whole
kernel would widen a data path into a control path. A per-ingress capability leaks, at worst,
the ability to upload to one ingress — which is also the only thing it is for.

Closed by default: no capability set means uploads are refused, so an ingress created and
forgotten is not an open write endpoint. The refusal is uniform across "no credential",
"wrong token" and "no resolver", because those three answers together are a map of the
deployment. And there is no download route: an artifact is never read back over HTTP, or one
ingress's upload token would hand out a customer's raw history.

The whole surface is opt-in on `INGRESS_STUDIO_ARTIFACT_DIR`. Unset, the receiver mounts no
upload route at all and the runner refuses with the missing directory as its reason — a
deployment setting is a better answer than "no runner for a file transport".

### D6. `operator_upload_v1` is a verifier profile that refuses everything

A file has no source to authenticate to. But "who vouches for these bytes" still has an answer —
the operator who uploaded them through an authenticated plane — so this is a profile rather than
an exemption, and the archetype and the profile imply each other in both directions.

Its `Verify` refuses every input, and that is the profile working rather than the profile being
unimplemented: there is exactly one way bytes may enter under it, and a delivery arriving at a
network transport that claims it is either a misconfigured spec or an attempt to borrow an
upload's provenance for something that came over the wire.

## What this ADR does NOT decide

- **XLSX, XML, Atom/RSS, fixed-width.** Named in `/todo.md` item 1 with libraries already chosen
  by the owner (`excelize`, `gofeed`, `xmlquery`). None is needed for a Slack, Jira or Zendesk
  export, and each adds dependencies to a module that has two.
- **Multi-node deployments.** `ArtifactVault` is local disk, so a fill reads the artifact from
  the node that stored it. The `ArtifactStore` port is what makes an object-store implementation
  a second implementation rather than a redesign.
- **Drafting the normalisation spec.** The gap document settled its shape — the existing
  `Drafter` proposes it from a redacted sample, a human confirms, the armed path stays
  deterministic and LLM-free — and it is not built. A spec is written by hand today.
- **Deleting or listing artifacts.** An uploaded export stays until an operator removes it from
  disk. Retention for artifacts is not modelled.
- **Resuming an interrupted upload.** A failed upload is re-sent whole.

## Consequences

- Slack, Jira and Zendesk are loadable by the route their vendors actually make practical, and a
  customer who will not grant historical scopes has a lane at all. Both were open after
  ADR-0119.
- An ingress can be upload-ONLY: a file as its live transport, filled through D5's fallback,
  with no API anywhere in the picture.
- The poller reads CSV, TSV, NDJSON and gzip as of the same day, on the shared seam — so every
  "download report" endpoint behind an ERP, WMS or finance system is now pollable, and the JSON
  path it always ran is untouched. What remains of `/todo.md` item 1 is Axis 2 (`cursor_source`),
  `total_path`, and making the whole-body fallback opt-in.
- Re-uploading an overlapping export does not double a customer's history: records carry the
  source's own id through `delivery_ref_path`, so the overlap collapses on the evidence
  idempotency triple exactly as a re-fetched poller page does.
- A new reserved path (`/ingress/`) on the receiver. A webhook claiming it is refused at parse
  time, because a shadowed route would answer deliveries from the upload endpoint while every
  surface reported a healthy ingress.
- No core contract bump and no new dependency. The upload surface is premium HTTP; the reference
  travels on the premium ingress proto, which does not change.
