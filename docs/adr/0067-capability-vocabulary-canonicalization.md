---
id: 0067
title: Capability Vocabulary — Retire the Clusterer, Use Declared Caps + Deterministic Normalization
status: Accepted
date: 2026-07-16
supersedes: []
superseded_by: []
depends_on:
  - 0014-capability-clustering
  - 0048-capability-contract-route03
  - 0002-gatekeeper
---

# ADR-0067: Capability Vocabulary Canonicalization

## Status

Accepted (reframes ROUTE-04; retires the CapabilityClusterer (ADR-0014))

## Context

ROUTE-03 (the capability contract) made **both** the planner and the L1 Declaration
filter read `manifest.Capabilities` — the strings agents *declare*. ROUTE-04's premise
was that this hard matching is "brittle to synonyms (`browser` vs `web-navigation`) and
typos", and proposed a registration-time **embedding-based canonicalizer** that clusters
capability strings and merges synonyms into canonical tags.

Two things reframe that premise:

1. **The synonym problem is smaller than it looks.** The planner is *shown* the exact
   declared vocabulary (`buildCapabilityClusterFromManifests`) and emits
   `required_capabilities` **from that list** — it does not invent `browser` when agents
   only declare `web-navigation`. The residual risk is narrow: format/typo variance, and
   two agents independently declaring cross-word synonyms.

2. **Fuzzy merging is the wrong tool and is actively dangerous.** Embedding-cosine
   merges are non-deterministic and can produce **wrong merges** — collapsing
   `file-read` and `file-write`, or `read` and `delete`, which are embedding-close but
   semantically opposite. A wrong merge causes *misroutes to a consequential wrong
   action* — strictly worse than the format variance it fixes. The ROUTE-04 acceptance
   itself betrays this by requiring "zero wrong merges on manual review": a mechanism you
   must manually audit for safety is not one to run automatically at registration.

Separately, the ADR-0014 **CapabilityClusterer** — which groups agents by
description-embedding and overwrites `AgentRecord.Capabilities` with an LLM-invented
cluster *name* — is now redundant and harmful: after ROUTE-03 the routing vocabulary is
`manifest.Capabilities`, so the clusterer does wasteful LLM work whose output the routing
path ignores, and it was a source of vocabulary divergence before ROUTE-03 fixed the
read path.

## Decision

### 1. Retire the CapabilityClusterer (ADR-0014)

Remove the `CapabilityClusterer` from the `SupervisionStack` and delete the
`internal/supervision/clusterer` package. Capabilities are the ones agents **declare**
(`manifest.Capabilities`), full stop. The InterviewWorker `SweepTrigger` (its only
consumer) is left nil (already optional). No LLM clustering, no invented labels.

### 2. Deterministic normalization only (`execution.canonical_vocab`)

Add `domain.NormalizeCapability`: lowercase, trim, and collapse runs of whitespace /
`_` / `-` into a single `-`. So `Web-Navigation`, `web_navigation`, and `web navigation`
all fold to `web-navigation`. It is **purely lexical** — it never merges distinct words.
Under the `canonical_vocab` arm it is applied to **both sides** of the L1 subset check
(`PassesDeclaration`) and to the planner's displayed vocabulary, so format/typo variance
matches with **zero wrong-merge risk**. Off ⇒ byte-identical to ROUTE-03 (verbatim).

### 3. Cross-word synonyms are an authoring concern, not a fuzzy-merge one

`browser` ≡ `web-navigation` is deliberately **not** solved here. The safe answer is a
**curated capability vocabulary** agent authors pick from (or a reviewed alias table) —
data, not code, and reviewed by a human — not an unsupervised embedding merge that can
silently collapse opposites. This is recorded as a follow-up option; it is explicitly
preferred over reviving fuzzy merging.

## Consequences

**Positive.**
- Removes an LLM subsystem (clusterer) that did wasteful work the routing path ignored —
  less cost, less code, no vocabulary divergence. (The user asked for exactly this.)
- Format/typo variance now matches, with a mechanism that **cannot** misroute (no fuzzy
  merges) — the safety property the fuzzy design could only *hope* for.
- The change is tiny and fully deterministic/unit-testable; no embedder dependency, no
  vocabulary store, no benchmark gate to protect against over-merging (there is no
  over-merging).

**Negative / costs.**
- True cross-word synonyms across independently-authored agents still require the same
  tag (or a curated alias table). This is a deliberate scope boundary, not an oversight.
- The `canonical_vocab` arm can be A/B'd against ROUTE-03, but since deterministic
  normalization can only *add* matches it could never previously make (never remove a
  correct one), the A/B can only hold or improve `routing_accuracy` — the gate is a
  formality, not a risk check.

**Neutral.**
- The ADR-0014 clusterer config (`capability_cluster_*`) and the `SetClusterName` storage
  methods become dead; left in place to avoid churn, marked for a later cleanup.
- The L2 Interview-threshold sweep from ROUTE-04's spec (its job changed under the
  contract: soft relevance on top of hard L1) remains a separate, still-open benchmark
  exercise.

## References

- ROUTE-04 (`docs/backlog/ROUTE-04-capability-vocabulary-canonicalization.md`); REPORT.md
  R2. ADR-0014 (CapabilityClusterer, retired here), ADR-0048/ROUTE-03 (capability
  contract — the read path this builds on), ADR-0002 (Gatekeeper L1).
