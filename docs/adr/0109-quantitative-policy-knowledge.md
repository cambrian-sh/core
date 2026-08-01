# ADR-0109: Quantitative Policy Knowledge (Knowledge Substrate Phase 5)

**Status:** Proposed — core built, wired, and GATE RUN (2026-08-01, same day): §7D
measured at 12/18 = 0.667 deterministic with 6 abstentions and 6/6 P/R on the gate
corpus; every pre-registered prediction held. See the DECISIONS entry for the runs and
the honest limits (synthetic corpus; the "most policy is quantitative" hypothesis
still needs a real handbook)
**Date:** 2026-08-01
**Relates to:** ADR-0106 (items + statement_values — where policy statements live),
ADR-0108 (the outbox transformer seam policy extraction rides), the memo §7D and §13
(`statement` authority), the drift guards (DECISIONS 2026-08-01, precision 0.421→0.615)

## Context

§7D's design claim: extract the policy side ONCE at handbook ingest
(`refund_policy / max_refund_days = 30`), and the per-message model call collapses to a
deterministic comparison (`60 > 30 is arithmetic`) — with the honest caveat, twice
reviewed, that the MESSAGE side still needs interpretation and the claim "most policy
is quantitative" is a hypothesis. Phase 5's gate is that hypothesis measured:

> **What percentage of policy checks reduce to deterministic comparison once both
> sides have been interpreted?**

## Decisions

### D1 — Both extractors are deterministic rule stacks, configured by DATA

`cambrian-premium/policycheck`: a handbook-side extractor (limit cue + quantity +
topic → a bound with a direction) and a message-side extractor (quantity + topic in
one clause → a stated claim). Topics are `TopicRule{Keywords, Unit, Predicate, Kind}`
supplied by the deployment — the package ships NO domain vocabulary of its own, the
same rule that keeps benchmark logic out of the kernel. No LLM anywhere: this is the
floor the measurement compares an LLM arm against later.

### D2 — The message side reuses drift's guards, not a re-implementation

`drift.AssessClaim` (the exported entry to the hypothetical/negated/quoted clause
guards) fires on the stated quantity's clause. The guards' measured behaviour —
catches what it targets, zero over-reach — carries over; re-implementing them
slightly differently and drifting would silently fork the abstention semantics.

### D3 — A check classifies into a closed five-way outcome

`deterministic_consistent · deterministic_contradictory · abstained ·
no_policy_bound · no_quantitative_claim`. The §7D percentage is
`(deterministic_consistent + deterministic_contradictory) / gold policy checks`,
published WITH the abstention count beside it (the drift lesson: a high abstention
rate means "the deterministic arm cannot tell", never "the product is precise").

### D4 — Storage and wiring ride what exists

Handbook statements become `KnowledgeItem{Kind:"policy_statement"}` +
`statement_values` through `domain.KnowledgeStore`; extraction runs as an
`EvidenceTransformer` claiming handbook evidence by configured stream/tag (the
ADR-0108 seam; targeting is data, memo §6). Per-message checks read the CURRENT
resolution of the policy statement set — the `statement` authority shape (§13),
order-independent by construction.

### D5 — The measurement protocol (the gate)

A corpus (harness-side, data only): one handbook (~30-60 rules, mixed quantitative
and prose-only) + N support messages with gold labels
(`violates | consistent | non_quantitative | deceptive-shape`). Two arms:
deterministic-only (this package) vs the same corpus with the LLM stage (later).
Recorded per DDD: the percentage, the abstentions, precision/recall of
`deterministic_contradictory` against gold, run manifests, DECISIONS entry. The
"most policy is quantitative" hypothesis is judged by the handbook-side extraction
coverage on a REAL handbook, not the synthetic one — flagged as such.

## Status

The pure core (extractors, guards wiring, comparator, five-way classification) is
implemented and unit-tested. The transformer/plugin wiring and the corpus run are the
next slice; **no §7D number exists yet, and nothing may cite one until the run does.**
