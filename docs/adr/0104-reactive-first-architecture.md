---
id: 0104
title: Reactive-First — the Watch Pipeline as Cambrian's Primary Shape
status: Proposed
date: 2026-08-01
supersedes: []
superseded_by: []
amends:
  - 0102-drift-plugin-record-lane
depends_on:
  - 0031-universal-input-router
  - 0032-reactive-rule-engine
  - 0033-daemon-agent-architecture
  - 0061-durable-reactive-execution
  - 0063-reactive-llm-condition-injection-hardening
  - 0072-schedule-watch-source
  - 0081-answer-memory-grounded-cited
  - 0090-ingress-and-surface-identity
  - 0095-experiences-as-first-class-entities
  - 0102-drift-plugin-record-lane
---

# ADR-0104 — Reactive-First: the watch pipeline as Cambrian's primary shape

## Context

Cambrian has a reactive engine with everything a general event system needs:
five source types (`daemon`, `filesystem`, `webhook`, `signal_stream`,
`schedule`), four condition types (`deterministic`, `pattern`, `llm`, `always`),
four action types (`dispatch_agent`, `emit_event`, `ingest`, `start_plan`), a
durable journal with exactly-once execution, dead-letter, replay-on-start,
backpressure, daemon supervision with flap quarantine, and a cron scheduler.

**Almost nothing uses it.**

The drift product — the flagship, the thing the whole company-brain campaign was
built to prove — registers **no watch at all**. `IngestMessages` is a synchronous
gRPC call that runs detector → extractor → reconciler → alert store in-process and
returns. Verified 2026-08-01: no `RegisterConfig`, no `SignalReceiver`, no
`OnSignal`, no `WatchConfig` anywhere in `plugins/driftplugin/` or `drift/`.

### This contradicts a decision already on the record

The company-brain backlog's **D10** says, in full:

> **Drift runs on the reactive engine, in-process** — watch condition + watch
> action inside the plugin. Not bespoke machinery: the engine already gives
> journaling, idempotency, dead-letter, replay, backoff/flap quarantine (REACT-04)
> and cron (REACT-06).

None of it was built, and **no rationale for the divergence was ever recorded.**
DRIFT-01's title still reads *"deterministic pre-filter → `llm` watch condition"*;
the pre-filter shipped and the watch condition did not.

### Why it happened, which is the part worth learning from

The benchmark harness scores by asking *"did this message produce an alert?"* —
which needs an answer **in-band**. So `IngestMessages` became synchronous, and its
own doc comment says why: *"a fire-and-forget ingest would make 'did that produce
an alert' unanswerable without polling, and it is the question every caller has."*

That is a good reason for a **benchmark affordance** and a bad reason for a
**product architecture**. The measurement instrument shaped the thing it measures,
and nobody noticed because the pipeline works: it is correct, fast (4 ms p50) and
well-tested. It is simply the wrong shape.

### What it costs today

1. **Drift has no durability.** None of REACT-01's journal, exactly-once,
   dead-letter or replay. If `IngestMessages` errors, the caller retries or the
   message is gone. There is nothing to replay from.

2. **A record change triggers nothing.** `IngestRecords` writes versions and
   returns; the only path into `Reconciler.Reconcile` is from message ingest. In
   the canonical arc — rep promises day 3, supplier slips day 12, rep repeats the
   stale date day 14 — **the alert fires on day 14's message, not on day 12's
   slip.** If the rep never speaks again, the contradiction sits in the data and
   nothing surfaces it. That is a structural silent false negative, and no
   benchmark measures it because every planted arc ends with a message.

3. **The record-change signal already exists and nothing consumes it.** DEMO-02's
   ingress contract emits `ingress.records.<kind>` on a *changed* tracked value,
   specifically so something could re-examine commitments when a supplier slips.
   The emitter shipped; the consumer never did. This is the sixth instance in this
   codebase of correct code wired to nothing — here the unbuilt half is the
   consumer rather than the caller.

## Decision

### D1 — The reactive pipeline is the primary shape for receiving work

Every external input reaches Cambrian as:

```
ingress daemon → signal(stream) → watch{condition} → action
```

An ingress registers under ADR-0090 so the kernel stamps its surface; it lands
content and then announces it per the DEMO-02 ingress contract (content first,
references only). A watch decides; an action acts.

This is not new machinery. It is a commitment to **use** the machinery that
exists, and to treat any feature that bypasses it as owing an explicit reason.

Slack, WhatsApp, email, ERP exports, sensors, camera feeds and schedules are all
the same shape under this decision. *"If the temperature exceeds 30, dispatch an
agent"* is already expressible today with `source: signal_stream`,
`condition_type: deterministic`, `condition: "temperature > 30"`,
`action: dispatch_agent` — no new concepts required.

### D1.1 — Stream matching supports prefixes (added during implementation)

D1 could not be built as written. The engine's fan-out is an exact map lookup
(`e.registry[signal.StreamID]`), which is right for a watch on one named thing and
wrong for the shape this ADR makes primary — a source that produces a FAMILY of
streams nobody can enumerate up front:

```
ingress.comms.sales-internal      a workspace grows channels without a deploy
ingress.records.purchase_order    a second record kind is a config change
ingress.cameras.gate-3            "watch my cameras" is not "watch camera 3"
```

Without it, every new channel needs its own registered watch, and a channel with no
watch is a detection gap **that reports nothing**: the signal arrives, matches no
key, and is dropped. Silent by construction.

A trailing `*` makes a stream id a prefix — `ingress.comms.*`. Explicit rather than
implicit, so no existing watch changes meaning: a bare `ingress.comms` stays exact.
Exact matches remain an O(1) map hit and prefix watches are a separate small slice,
so the cost is O(1) + O(prefix watches) rather than scanning every registration.

Delete and disarm walk both structures. A prefix watch that lived only outside the
map would keep firing after an operator was told it was gone.

### D2 — Two lanes, and the split is about EVIDENCE, not data shape

The reactive lane acts on events as they arrive. The **memory lane** answers
questions about what already arrived, via retrieval and ADR-0081's grounded,
`[n]`-cited synthesis.

Both are required, and conflating them produces watches that poll a database.

The default is **memory**. The retrieval agent retrieves, ranks and answers, and
it has the property no schema can match: **it answers questions nobody predicted.**
*"Where was plate XXX last seen?"* needs no plate table and no new RPC.

A **typed store** is an additional index, and is justified in exactly one
circumstance:

> **When a model's judgement about a fact would be unacceptable.**

For drift that is the entire product claim, for four measured reasons:

- **Currency.** The record store knows which version is current
  (`superseded_at IS NULL`). Memory holds chunks from every version ever ingested
  with no supersession concept. A retrieval for "PO-4471 promised date" returns
  both the old and new dates, and the ranker has no principled reason to prefer the
  newer one — recency is not relevance. Synthesis would then *decide*, which is a
  model guessing at something the record store knows exactly.
- **The measured ceiling.** TechQA recall@10 0.65, @30 0.74, embedder-bound at
  0.84. Retrieval misses roughly one in four at k=30. A reconciliation that misses
  an existing record reports `record_not_found` — a false "we have no data" about
  data we hold.
- **Cost and determinism.** Reconciliation runs per commitment. Routing it through
  synthesis puts an LLM at message granularity (hard rule #2) and destroys the
  4 ms p50, reproducible-from-seed property every published drift number rests on.
- **Evidence.** An alert cites a record *version* — a row with an id and an
  `observed_at`, from which "the date moved twice" is recomputable. From chunks it
  is an inference, and an inference is not evidence in an audit product.

This carve-out is deliberately narrow. Retrieval is the answer for the long tail;
a typed store is for claims that must be definitely true.

### D3 — One write path. A daemon reports once; the kernel fans out.

An ingress does **not** write to memory and to a typed store as two independent
calls. It reports once, and the kernel routes: **memory always**, plus a typed
store when a registered schema claims the data.

The simulator currently does the two-call version, and that is the bug this
prevents: two independent writes can diverge, and the failure mode is an alert
citing a record the memory lane never saw — evidence that does not reconcile with
the corpus it came from.

### D4 — Ingested content carries classification on every path

Everything landing in memory inherits the source's tags (ADR-0095 D9 / REC-04). An
ingress that writes untagged into shared memory is the most efficient available
mechanism for making restricted material retrievable by everyone.

This is stated as a decision rather than assumed because high-volume ingress makes
it load-bearing: a camera feed is thousands of writes an hour, and a classification
bug there is not recoverable by deleting one document.

### D5 — Per-source retention is a precondition for high-volume ingress

Frames-per-second into memory is unbounded growth. The machinery exists (REC-03
record retention, GOV-02 journal GC) but is **not wired per ingress source**. A
high-volume source must not be enabled before its retention policy is, and the
policy is configuration rather than code (the REC-03 precedent: unknown kind is a
startup error, not a silent default).

### D6 — Drift is re-expressed as the first instance, in three steps

1. **Record-change watch.** `ingress.records.<kind>` → condition "a tracked value
   changed" → action: re-examine live commitments for that record. This closes the
   silent false negative in §Context and touches no measured path.
2. **Message watch** replaces the synchronous call on the production path, so
   drift inherits journal, exactly-once, dead-letter and replay.
3. **The benchmark waits for outcomes** rather than receiving them in-band, and the
   paired arms are re-run to confirm the numbers survive the move.

`IngestMessages` remains, explicitly as a **benchmark affordance and a
synchronous-caller convenience** — not as the product's shape. That distinction is
recorded here so the next reader does not infer architecture from its existence.

### D6.3 outcome (measured 2026-08-01)

The reactive arm was built and run against the synchronous control on the same
corpus. **Every headline metric is identical**: recall 0.7273, precision 0.6154,
deceptive-defeated 4/10, delta exact 8/8. Detection quality is unchanged by the
move (`runs/d63-sync` / `runs/d63-reactive`).

Two limitations the run exposed, both recorded in DECISIONS.md rather than smoothed
over:

- **The reactive arm is a partial instrument.** A watch returns nothing to its
  caller, so per-message `outcome`/`unresolved_reason`/`committed` are
  unobservable; only alert-derived metrics can be scored. Eleven rows whose correct
  behaviour is to produce NO alert therefore score as failures. Closing this needs a
  per-message outcome read surface, which does not exist.
- **The latency prediction in Consequences below is still untested.** This ADR said
  latency would rise. `detect_latency_ms` is stamped inside the pipeline, so it is
  blind to the journal append and queue hop the reactive path adds — the two arms
  report identical p50/p95 because the metric cannot see the cost. End-to-end
  reactive latency is unmeasured, and nothing here may be quoted as showing the
  reactive path is as fast.

## Consequences

**Positive.**
- Drift gains durability it should always have had, and the record-side detection
  hole closes.
- If drift is expressible as ingress + watch + action, **every case in D1 is** —
  drift stops being a special product and becomes the first proof the platform
  generalises. That is a materially stronger claim than "we built a drift detector".
- The reactive engine stops being expensively-built infrastructure with one
  benchmark suite and no product on it.

**Negative, and stated plainly.**
- **The measured numbers are at risk.** An async path means the harness must wait
  for an outcome instead of receiving one; that is real harness work, and any
  change in precision/recall across the move must be published with the
  architectural change named, exactly as the deceptive-corpus change was.
- Latency will rise. The 4 ms p50 is an in-process synchronous number; a journal
  append and a queue hop are not free. The correct comparison after the move is
  end-to-end, and it must be re-measured rather than assumed.
- More moving parts on the critical path. A watch that is not armed, a quarantined
  daemon, or a shed signal are all new ways for detection to silently not happen —
  which is why REACT-05's per-watch metrics and dry-run become load-bearing rather
  than nice to have.

**Neutral.**
- D2's carve-out will be argued about again the first time someone wants a second
  typed store. The test is the one sentence in D2, not the data's shape.

## References

- Company-brain backlog **D10** (the decision this ADR is finally implementing).
- `docs/ingress-signal-contract.md` — the emission contract, whose record-side
  consumer this ADR supplies.
- ADR-0061 (durable reactive execution), ADR-0090 (ingress + surface identity),
  ADR-0095 (classification inheritance), ADR-0081 (AnswerMemory).
- `docs/research/company-brain/DECISIONS.md` — the drift numbers this must not
  silently change.
