# ADR-0035: Kernel-Derived Write Classification (ADR-0034 D8 Revision)

**Status:** Accepted (2026-06-03) — *grilled during REQ-SDK re-grill; supersedes the write-side
half of ADR-0034 D8. Read-side scoping (ScopedVectorStore, EffectiveScope, ScopeResolver) is
unchanged. Provenance stamping is unchanged. This ADR changes only how a write's **classification
tags** are decided.*
**Date:** 2026-06-03
**Author:** Afsin
**Amends:** ADR-0034 (Tag-Based Data Access Scoping) — decision D8
**Depends on:** ADR-0034 (ScopeConfig, EffectiveScope, ScopeResolver, controlled vocabulary, provenance)

---

## Context

ADR-0034 D8 ("Write-side tag provenance & trust model") set the write model as: *an agent **requests**
classification tags; the kernel **validates** them against the agent's effective `ForbiddenTags` and
the controlled vocabulary, then stamps provenance.* The `ScopedStoreWriter` decorator and the
`UploadArtifact` RPC were both built on this model.

A re-grilling of the SDK requirements surfaced a hole this model does **not** close:

> **Server-side `ForbiddenTags` validation prevents an agent from using tags it is forbidden from
> using — it does NOT prevent an agent from MIS-classifying content.**

Concretely: a support agent allowed to write `public_kb` (it is not in its `ForbiddenTags`) can take
genuinely sensitive content it was permitted to read and tag it `public_kb`, making it world-readable.
The kernel validated *which tags the agent may use*, not *whether the content deserves that tag*.
Provenance records *who* did it, but the data is already mis-scoped and the leak has happened. This is
a **broaden-to-leak** vector intrinsic to letting an untrusted agent choose its own classification.

The "agents are untrusted cells" framing makes agent-chosen classification indefensible: a compromised
or careless agent author is exactly who you cannot trust to label sensitivity correctly.

## Considered Options

- **A — Agent proposes, kernel validates (ADR-0034 D8 as shipped).** Leaves the broaden-to-leak
  vector open. Rejected.
- **B — Kernel decides classification via LLM / heuristics.** Tempting, but puts a **non-deterministic
  model in the security boundary** — the exact anti-pattern ADR-0034 D11 forbids ("the LLM only
  generalizes, it is never the security boundary"). A jailbroken or hallucinating classifier
  mislabels, and now the model *is* the access-control decision. Rejected.
- **C — Kernel derives classification deterministically; agent may only narrow.** Chosen.

## Decision

**A write's classification is DERIVED by the kernel, deterministically, from operator-configured
per-agent `DefaultWriteTags`. The agent cannot choose its own classification; it may only *narrow*
(further-restrict) within the controlled vocabulary, never broaden. No LLM or heuristic participates
in the classification decision. Provenance remains kernel-stamped and non-forgeable.**

### D8′-1 — `DefaultWriteTags` is a genotype property (C2)

`AgentDefinition` gains a `DefaultWriteTags []string` — a flat, operator-set classification list that
is **the** classification stamped on this agent's writes. It is distinct from `ScopeProfile` (the read
predicate):

- `ScopeProfile` answers *"what may this agent read?"* (a three-set predicate).
- `DefaultWriteTags` answers *"what classification do this agent's writes carry?"* (a flat tag set).

Keeping them separate honors ADR-0034 D8's deliberate **read/write asymmetry**: a `ConsolidatorAgent`
may have `DefaultWriteTags = ["company_wide","analytics","derived"]` while its `ScopeProfile` reads
only `chat_raw` — it writes broad, derived knowledge it cannot itself read back as raw.

`DefaultWriteTags` is authoritative in the same PostgreSQL `agent_scopes` table as `ScopeProfile`
(ADR-0034 R1), resolved via `ScopeResolver`, set via the admin API. An agent with no configured
`DefaultWriteTags` defaults to the **empty set** (an *unclassified* write — see D8′-4).

### D8′-2 — The agent may only NARROW

The SDK exposes at most an optional `restrict_to` / narrow-only hint. The kernel computes the final
classification as:

```
final_tags = DefaultWriteTags ∩ (hint or DefaultWriteTags)   // intersection — hint can only remove
```

A hint tag that is **not** already in `DefaultWriteTags` is ignored (it cannot *add* a classification).
A hint tag outside the controlled vocabulary is rejected (`InvalidArgument`) — coinage is still
forbidden. The agent therefore can make its output *more* restricted (e.g. drop `company_wide`, keep
only `analytics`), **never more visible**. The broaden-to-leak vector is structurally eliminated: the
maximum visibility of any agent write is fixed by the operator at registration, not by the agent.

### D8′-3 — No LLM/heuristic in the classification boundary

Classification is pure set arithmetic over deterministic, operator-configured data. No model, no
content inspection, no heuristic decides a security label. (A future *advisory* LLM suggestion that
still cannot broaden is permitted in principle but is out of scope for this ADR.)

### D8′-4 — Unclassified writes and the read-side fail-safe

A write whose `final_tags` is empty (agent has no `DefaultWriteTags`) is *unclassified*. By the
ADR-0034 read predicate, an unclassified document (`tags == ∅`) is matched only by an **empty /
unrestricted** effective scope or `ScopeSystem` — any agent whose scope carries a
`Required`/`AnyOf`/`Forbidden` constraint sees it as **absent**. This is the safe default: unclassified
writes are visible only to unrestricted readers, never broadly leaked. The same rule governs
**legacy untagged artifacts** under the additive migration (no drop-and-rebuild).

### D8′-5 — Provenance unchanged

`provenance:source=<agent_id>` (and connector provenance) remain kernel-stamped from the authenticated
execution context, never copied from agent input. `ScopedStoreWriter` continues to strip any
agent-supplied `provenance:*` tag and stamp the real one.

### D8′-6 — Promotion is still the only path that broadens

The `ConsolidatorAgent` promotion pipeline (ADR-0034 D11) remains the sole mechanism that crosses into
a broader scope, and it remains deterministic (k-anonymity floor + regex PII scrub + scope-filtered
read set). Promotion writes carry the Consolidator's `DefaultWriteTags`; the LLM stays confined to
generalization. Nothing here weakens D11.

## Consequences

### Good
- **Closes the broaden-to-leak hole.** An untrusted agent can never make its output more visible than
  the operator's per-agent ceiling. Mis-classification-to-leak is structurally impossible.
- **Determinism preserved.** No model in the security boundary; classification is set arithmetic over
  operator data — auditable and reproducible.
- **Simpler SDK.** Agent authors no longer carry authoritative classification tags. `memory.remember()`
  / `artifacts.save()` expose at most a narrow-only hint. "What tenant/scope does this belong to?" is
  not the author's question to answer.
- **Read/write asymmetry intact.** `DefaultWriteTags` ⟂ `ScopeProfile` cleanly supports write-broad /
  read-narrow system agents.

### Bad / Cost
- **Operators must configure two properties per agent** (`ScopeProfile` + `DefaultWriteTags`), not one.
  A misconfigured `DefaultWriteTags` on a widely-used agent mis-classifies all its writes (too narrow →
  invisible; the operator sees writes that nothing can read — diagnosable via the unclassified
  fail-safe + audit logs, but still a footgun).
- **Loss of agent expressiveness.** An agent that legitimately knows a piece of output is *less*
  sensitive than its default cannot broaden it; it must be promoted (D11) or the operator must register
  a second agent definition with broader `DefaultWriteTags`. This is the accepted cost of removing the
  leak vector.
- **`ScopedStoreWriter` / `UploadArtifact` rework.** The validate-requested-tags path is replaced by
  derive-then-narrow. (Provenance path unchanged.)

### Neutral
- The controlled vocabulary (ADR-0034 D8) still exists — it now bounds the *narrow-only hint* and the
  operator-set `DefaultWriteTags`, not free agent tags.
- SDK exceptions: out-of-vocabulary hint → `InvalidArgument` (`InvalidTagError`); a forbidden read →
  the existing `found=false` collapse. No new server-side denial class is needed for writes, because
  the agent can no longer request a broadening tag to be denied.

## Related
- ADR-0034 (Tag-Based Data Access Scoping — D8 amended here; read-side, ScopeResolver, vocabulary,
  provenance, promotion all unchanged)
- REQ-SDK-cognitive-agent-sdk (re-grilled 2026-06-03; `memory.remember()` / `artifacts.save()` carry a
  narrow-only hint, not authoritative tags)
