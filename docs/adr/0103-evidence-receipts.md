---
id: 0103
title: Evidence Receipts — Signed, Chained Decision Provenance
status: Proposed
date: 2026-08-01
supersedes: []
superseded_by: []
amends: []
depends_on:
  - 0057-open-core-boundary
  - 0074-plugin-architecture
  - 0082-licensed-plugins
  - 0085-access-policy
  - 0101-config-and-secret-store
---

# ADR-0103: Evidence Receipts

## Status

Proposed — 2026-08-01. **D1–D9 implemented** the same day, across both repos.

- **OSS (`cambrian-core`)**: `domain.DecisionObserver` / `RetrievalDecision` /
  `MultiDecisionObserver` (D3), `Registry.AddDecisionObserver` + `Options.DecisionObserver`,
  the emit site at the end of `searchByType`, and `ExecutionConfig.RetrievalFingerprint` (D7).
  `make per-pr` green; contract stays 0085 (D9 needed no bump).
- **Premium (`cambrian-premium/receipts` + `plugins/receiptsplugin`)**: canonical encoding and
  hashing (D1), the bounded non-blocking consumer with gap markers (D2/D6), the hash chain with
  resume (D6), Ed25519 signing that degrades to unsigned rather than disabled (D5), the Postgres
  store with prefix-only retention, the read-only `ReceiptLane` gRPC plane mounted via
  `AddGRPCService`, and the plugin with `CapabilityReporter` gating. Builds, `go vet`, plugin
  isolation and all tests pass under `-race`.

**Not done, and both are gates rather than polish:**

1. **The benchmark obligation below has not been run.** Receipts-on-every-retrieval is a
   hot-path behaviour change and must be measured as an arm against `current-kernel` before
   the lane is enabled anywhere real.
2. **No live end-to-end run against Postgres.** The store's SQL is exercised only by
   construction and review, not by an integration test — `PruneBefore`'s `ctid` subquery and
   the batch insert path are the two places to check first.

Also outstanding by design: stage attribution (D8), and the `model_id` / `prompt_hash` /
`output_hash` / `policy_decisions` / `verifier` fields, which the schema and wire format carry
but nothing populates yet — the retrieval seam does not see them. Wiring those is the natural
next slice and needs a second seam at the answer/policy call sites.

## Context

### The gap this closes

Competitive analysis (2026-08-01, `docs/research/`) established two facts about the market
Cambrian sells into:

1. **Every rival records actions; nobody records decisions.** Buzz signs that an event
   occurred but never saw the evidence — it has no retrieval. MCP gateways log a tool call
   and the rule that permitted it, but not what the model had read when it decided to make
   the call. Langfuse captures retrieval spans but does not know the ranking configuration,
   is client-instrumented, and does not sign. AWS Bedrock AgentCore Policy (GA March 2026)
   evaluates Cedar policy per tool call and logs the verdict — again, without the evidence.
2. **The composite cannot be assembled.** Wiring Letta + a policy gateway + Temporal +
   Langfuse yields four partial stories joined only by correlation IDs across four vendors,
   none of which signs anything. Provenance dies at the seams.

An **action record** answers *this agent called this tool and it was allowed*. A **decision
record** answers *this is what the system knew, how those passages were ranked, under which
configuration, which policy verdict permitted the consequent action, and whether the output
was supported by the evidence*. Only a component that owns the retrieval call site, the
ranking configuration, the policy decision point and the log simultaneously can emit the
second. Cambrian owns all four; this ADR makes that ownership legible as an artifact.

### Why the existing lanes do not cover it

- `domain.AuditStore` (ADR-0047 D15) records **operator-mutating actions** — actor, action
  type, target, before/after, command id. It is an action record for humans, deliberately
  scoped to the operator plane. Agent decisions never touch it.
- `cambrian-premium/records` (DRIFT-02/03, ADR-0081) is the **business record lane** —
  commitments, drift alerts, citations with quotes, retention and compaction. Adjacent in
  shape and a good source of patterns, but a different domain: what the business committed
  to, not how the system reached a conclusion.
- Langfuse tracing (ADR-0019) observes generator calls. It is premium, useful, and not
  attestable: it records what an instrumented call site chose to emit.

### What is actually available to build from

Verified against the working tree, 2026-08-01 — this corrects an earlier claim that receipts
compose "mostly from parts that already exist":

| Receipt field | Available today | Reality |
|---|---|---|
| `policy_decisions` | **Yes** | `domain.AccessDecision` carries a controlled `DecisionReason` vocabulary plus `PolicyContribution` — structured and explainable by construction (ADR-0085). |
| chunk ids + scores | **Yes** | `domain.SearchResult{Document, Score, RawScore, LexicalScore}`. |
| primary vs injected | **Yes, but discarded** | `primaryIDs` is a binary map built inside `searchByType` and never leaves the function. |
| `stage_that_admitted_it` | **No** | Nothing records which stage admitted a chunk. Threading it modifies the guarded non-displacing truncation path. |
| `ranking_config_hash` | **No** | `internal/config` has no snapshot or fingerprint function at all. |
| groundedness / unsupported spans | **No** | The verifier pool scores task quality, not claim support. |
| signing / chaining | **No** | Nothing in the workspace signs anything. |

So three of seven components are genuinely new, and one of the new ones (stage attribution)
touches a path protected by a measured invariant. The build is larger than the pitch implied.

## Decision

### D1 — A receipt is a decision record, referenced not copied

One receipt describes one retrieval-and-decision event:

```
Receipt {
  receipt_id, sequence, emitted_at
  query_id, session_id, principal_id
  query_text_hash                      // never the query text
  retrieved: [ { chunk_id, doc_id, score, raw_score, lexical_score, primary } ]
  ranking_config_hash                  // D7
  model_id, temperature, prompt_hash
  output_hash
  policy_decisions: [ { surface, resource_kind, effect, allowed, reason, rule_ids } ]
  verifier: { groundedness_score, unsupported_spans }   // populated when available
  prev_receipt_hash, chain_id
  signature
}
```

**Content is referenced by id and hash, never embedded.** This is deliberate and load-bearing:
a receipt must remain verifiable after the underlying chunk is deleted, so a subject-erasure
request does not force a choice between honouring the law and breaking the chain. The receipt
proves *what was used*; the corpus proves *what it said*, for as long as the corpus is allowed
to keep it.

### D2 — Emit on every retrieval call, including internal, and never block on it

Every retrieval — agent-facing answers, WorkspaceStage fetches, agentic sub-queries, operator
searches — emits a receipt. Coverage is the point: an auditor's question lands on one specific
decision, and sampling guarantees the interesting one is missing.

The cost of that choice is that emission sits on the hot path of the most frequently executed
code in the kernel. Therefore:

- The kernel hands the observer a **value-copy struct over a bounded channel** and returns.
  No I/O, no locks held, no error propagated to the caller.
- **Retrieval never fails because receipting failed.** This is memory invariant #5 (reads fail
  open) and is not negotiable — a compliance feature that can take down retrieval is a worse
  outcome than a missing receipt.
- When the buffer is full the emitter **drops and counts**, and the consumer writes an explicit
  **gap marker** into the chain rather than silently losing the event.

### D3 — The only OSS change is one inert extension point

`cambrian-core` gains a domain port and one add-many registry point, and nothing else:

```go
// domain
type RetrievalDecision struct { … }          // value copy, no premium vocabulary
type DecisionObserver interface {
    ObserveRetrieval(RetrievalDecision)       // MUST NOT block; MUST NOT error
}

// app.Registry
func (r *Registry) AddDecisionObserver(o domain.DecisionObserver)
```

With no plugin registered the call site is a nil check — OSS behaviour is unchanged and the
open-core boundary (invariant #1) holds: the OSS core never learns the word "receipt", and the
struct names only concepts OSS already owns (ADR-0057 D5).

### D4 — Everything else is premium

`cambrian-premium/receipts` owns the schema, chain, signing, storage, retention, verification
and the operator surface, as a licensed package per ADR-0082/0083 with `receipts` as the
entitlement key. Rationale: this is the compliance wedge and the thing a regulated buyer pays
for. The OSS kernel emits nothing until a premium build subscribes.

### D5 — Local signing key, externally anchored, with an honest threat model

An Ed25519 key held in the ADR-0101 encrypted secret store signs each receipt. The chain head
is periodically published to a sink the operator cannot silently rewrite (object-lock bucket,
signed git tag, or an external transparency log — deployment choice).

Stated plainly, because overclaiming here would be worse than the gap: **a live operator with
kernel access and the key can forge receipts going forward.** What the design does buy is that
they cannot *retroactively* rewrite history past the last anchor without the divergence being
detectable. Full protection against a malicious operator requires per-principal keys or an HSM;
both are deferred, and D5 is the pragmatic first step, not the endpoint.

### D6 — Integrity and completeness are reported separately

The chain gives tamper evidence. A monotonic per-chain `sequence` gives completeness. These are
distinct properties and verification reports them separately: a chain can be cryptographically
intact and still have a gap at sequence 4,102 because the buffer overflowed. Conflating them
would let a system that dropped a third of its receipts present as fully verified.

### D7 — A generic retrieval config fingerprint in OSS

`internal/config` gains a fingerprint over the retrieval-affecting surface: blend weights, the
enabled-stage flags, `k`, `rrf_k`, relevance floor, embedder id and dimension. It is generic
kernel capability, not a premium-named key, and it is independently useful — benchmark runs can
finally record exactly which configuration produced a result.

### D8 — Stage attribution deferred to a later, benchmarked change

v1 records `primary` (the existing binary primary-vs-injected distinction, which merely needs to
stop being discarded) and omits `stage_that_admitted_it`. Threading full stage provenance
modifies the two-pass non-displacing truncation — the code path whose invariant exists because
graph injection once nearly halved MuSiQue support-recall (0.285 → 0.158). That change earns its
own arm and its own DECISIONS.md entry. v1 answers *what did it know*; stage attribution later
completes *why was this ranked first*.

### D9 — A premium-owned gRPC service, so no operator contract bump

The receipts service (`GetReceipt`, `ListReceipts`, `VerifyChain`) is mounted through
`Registry.AddGRPCService` (ADR-0073/0074), with its proto living in `cambrian-premium/api`.
The OSS `operator.proto` is untouched and `contract_version` stays at 0085 — the vendored-proto
drift problem is not worth re-entering for a premium surface. Capabilities are advertised via
`PluginManifest.Capabilities` plus `CapabilityReporter.LiveCapabilities` so the panel does not
appear on a deployment without Postgres (the REC-02 lesson).

## Consequences

**Positive.** The demo no rival can produce: open a six-month-old answer, show the passages it
used with their scores, the configuration that ranked them, the policy checks it passed, and
proof that none of it has been edited since. It also generates exactly the funnel and verifier
data ROUTE-05/07 need, so the learned-routing arms get their offline corpus as a side effect.

**Negative / risks.**

- **Hot-path cost.** Emission on every retrieval must be benchmarked, not assumed. See the
  benchmark obligation below.
- **Storage growth** is proportional to total retrieval volume, not to interesting events.
  Retention and compaction (reusing the `records` lane patterns) are required at v1, not later.
- **A partial receipt can mislead.** A receipt whose `verifier` block is empty must be visibly
  distinguishable from one that was verified and passed.
- **The key is the weak point** (D5), and the marketing must not outrun it.

**Benchmark obligation (DDD mandate).** Receipts on every retrieval is a behaviour change to the
hot path, so it ships as one arm against `current-kernel`: p50/p95 retrieval latency and
throughput with the observer registered versus inert, plus recall unchanged (it must be exactly
unchanged — the observer is read-only). Result goes to DECISIONS.md with run-manifest IDs for
both arms. If p95 regresses materially, D2's coverage decision gets revisited before merge, not
after.

## Alternatives rejected

- **Sampling** (~10%, like the verifier pool). Cheap and adequate for quality monitoring, useless
  for compliance: the decision under scrutiny is precisely the one that was not sampled.
- **Extending `AuditStore`.** It is the operator-action lane with a command-id idempotency
  contract; agent decisions have different cardinality, different retention and no command id.
- **Receipts in OSS.** Rejected on business grounds per the D4 decision, not on architectural
  ones — the seam is deliberately shaped so this could be revisited without redesign.
- **Embedding retrieved text in the receipt.** Simplest to verify, but makes erasure and chain
  integrity mutually exclusive, and multiplies storage by the size of the corpus actually read.
- **Blocking emission for guaranteed completeness.** Trades an availability property (retrieval
  always answers) for a durability property, in the component with the strictest fail-open
  invariant in the kernel. D6's honest gap reporting is the better trade.
