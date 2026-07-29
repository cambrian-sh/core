---
id: 0099
title: Identity Is Not a Classification — Splitting the Overloaded Ingest Tag List
status: Proposed
date: 2026-07-29
supersedes: []
superseded_by: []
amends:
  - 0093-document-as-entity
depends_on:
  - 0085-access-policy-port-and-extraction
  - 0093-document-as-entity
---

# ADR-0099: Identity Is Not a Classification

## Status

Proposed — implemented 2026-07-29 (`memory.classificationTags`, applied in
`persistChunks`). Not yet benchmarked; see *Consequences*.

## Context

`domain.ExternalDocument.Tags` carries two different things at once.

The first is a **classification**: what the document *is*, drawn from the controlled
vocabulary (`CAMBRIAN_CLASSIFICATION_VOCABULARY`), and the only thing access policy
can act on. ADR-0085 is explicit that policy acts on **labels and never on a document
by name** — restricting one named document is a stated non-goal.

The second is an **identity**. `externalDocumentID` rule 1 reads the tag immediately
following the literal `source_document` and makes it the document id:

```go
for i, tag := range doc.Tags {
    if tag == sourceDocumentMarker && i+1 < len(doc.Tags) && doc.Tags[i+1] != "" {
        return doc.Tags[i+1]
    }
}
```

That convention exists for a real reason. Rule 1b — the fallback — keys on the first
non-reserved tag plus a content digest, and before the digest was added it *silently
destroyed data*: N documents sharing one tag all collapsed onto `<tag>-chunk-K`, each
ingest overwriting the last while every call returned an id and reported success. Six
callers therefore pass `source_document` + a unique id deliberately: `document_qa`,
`agentic_retrieval`, `orchestration`, `musique`, `task_accomplishment`, and
`tau2_knowledge`.

The cost of the overload is measurable. On the live store, 2026-07-28:

```
distinct tags in use on documents : 726
configured vocabulary size        :  12
documents with zero labels        : 422  (of 1163)
```

710 of those 726 tags are a document's own id. A tag that names exactly one document
is a term no rule can usefully match — it is the "restrict one named document"
non-goal expressed as data — and one per document makes the operator console's
vocabulary picker unusable.

### The rejected fix, and why it is wrong

The obvious correction is to stop sending the id: drop `doc.id` from the suite's
ingest call. A handoff proposed exactly this, reasoning that the tag was only there
so the scorer could recover document ids, and that the scorer now reads
`metadata["document_id"]` instead.

That reasoning is circular. `metadata["document_id"]` **is** `externalDocumentID(doc)`,
which is derived from that very tag. Removing it does not remove a label; it renames
every document — `doc_savings_accounts_gold_account_010` becomes
`tau2-knowledge:<content-digest>` via rule 1b. The scorer matches document ids against
the dataset's `required_documents`, so recall goes to a flat 0.0 across every task,
and the suite reports a catastrophic retrieval regression that is really a renaming.
Verified against the live store: `documents.id` for a τ²-bench row **is** the tag.

## Decision

**Identity stays on the wire. It is stripped from the classification at the write
chokepoint.**

`persistChunks` resolves `documentID` first, then narrows the tag list before anything
is stored:

```go
documentID := externalDocumentID(doc)
doc.Tags = classificationTags(doc.Tags, documentID)
```

`classificationTags` removes exactly two things and preserves everything else in
order:

1. the `source_document` marker, which is wire protocol and was never a label; and
2. a tag equal to the document id it produced.

The id is matched against the **resolved** `documentID` rather than blindly dropping
whatever follows the marker. A caller whose id came from elsewhere — a thread, a
source URI, a content digest — keeps every tag it sent; a caller that never used the
convention is untouched.

### Why the chokepoint and not the callers

- It is the only place that knows **both** the full request and which tag actually
  became the id. A caller knows its own convention; it does not know what the kernel
  resolved.
- One change fixes all six suites, and every future caller, without any of them
  changing — and without any of them being able to reintroduce the defect.
- It is narrowing at a write chokepoint, which this path already does: ADR-0093 D4
  has `SaveDocument` return what was *actually* stored, with the chunk cache derived
  from that rather than from the request. This is the same shape one step earlier.

## Consequences

**Good.** The label space collapses to real classifications: 726 distinct tags → 15 on
the current corpus. Document ids, chunk ids (`{docID}-chunk-{n}`),
`metadata["document_id"]` and every scorer that reads it are unchanged, so no
benchmark moves. The vocabulary picker becomes usable, and — the point of ADR-0085 —
every remaining tag is a term a rule could actually be written about.

**Cost.** The kernel now silently discards two caller-supplied tags. That is a
behaviour change a caller cannot observe from the response. It is bounded to terms
that could never function as labels, and it is the same authority `SaveDocument`
already exercises, but it is real: a caller that genuinely wanted `source_document` as
a classification cannot have it. Nobody does — the source-document *kind* is already
recorded separately in the entity metadata (`"kind": "source_document"`).

**Historical data is not migrated by this change.** Documents ingested before it keep
their junk tags. `cmd/tag-repair` applies the same rule offline, through
`RetagDocument` so the document row and its derived chunk copies move in one
transaction; it is dry-run by default.

**Not benchmarked.** No code is added to any retrieval, routing or ranking path — the
change removes two strings from a label list at ingest and cannot move a recall or
routing score. Recorded here rather than silently skipped, per change-control §8
("If a change seems to need an exemption from a gate, that is itself a decision").
The claim that scores are unaffected is testable and untested: the cheap confirmation
is a `tau2-knowledge` run, whose recall must be unchanged because document ids are
unchanged.

## Alternatives considered

**An explicit `DocumentID` field on `ExternalDocument`.** The cleanest expression —
identity would never travel in the tag list at all, and the overload would be
impossible rather than merely corrected. Rejected *for now* as disproportionate: it
changes the agent-plane ingest contract and every caller, to fix a problem the
chokepoint already fixes without a contract change. Worth revisiting if a second
identity convention ever appears; this ADR does not preclude it, and
`classificationTags` would remain correct as a compatibility shim.

**Strip in each caller.** Six edits instead of one, no enforcement against the
seventh, and — as shown above — the naive version of this edit silently breaks
document identity. Rejected.

**Do nothing; clean historical data only.** The vocabulary re-pollutes on the next
benchmark run and `tag-repair` becomes a recurring chore. Rejected: ADR-0093 made the
document row authoritative precisely so classification would be correct *by
construction* rather than by operator diligence.
