---
id: 0092
title: Natural-Language Policy Authoring — An Assistant That Proposes and Cannot Enforce
status: Proposed
date: 2026-07-27
supersedes: []
superseded_by: []
amends:
  - 0085-access-policy-port-and-extraction
depends_on:
  - 0034-scope-and-classification
  - 0063-llm-condition-injection-hardening
  - 0085-access-policy-port-and-extraction
  - 0086-tool-effect-classes
  - 0087-policy-composition-groups-links
  - 0088-ui-access-policy-console
  - 0091-closed-tags-and-crippled-grants
---

# ADR-0092: Natural-Language Policy Authoring

## Status

**Proposed — implemented.** Proposer, admin RPC, plugin wiring and console pane are built and
unit-tested; not yet exercised against a live model endpoint.

## Context

ADR-0088 gave the access-policy model a console, and using it revealed the cost of the model's
own expressiveness. Authoring one boundary means holding four things at once: the three-set tag
algebra of ADR-0087 (required AND, any-of OR, forbidden NONE), the closure and grant rules of
ADR-0091, the four container kinds and their precedence, and the report-only rollout path. An
operator who knows exactly what they want — *"support agents shouldn't see anything marked
secrets"* — still has to translate it, and the translation is where the mistakes live.

The mistakes are also quiet. ADR-0085 D11 exists because a mistyped tag produces a boundary that
matches nothing; ADR-0091 records `IsZero()` dropping grant-only policies and `Build` discarding
the closed set. The pattern is consistent: **a wrong policy in this model usually does nothing
rather than something visible.** That makes authoring assistance valuable and makes an assistant
with a write path dangerous, in the same breath.

### The tension with the Zero-Hardcode Rule

Cambrian routes with an LLM by default and carves out an explicit exception for security gates:
scope, approval and budget decisions are deterministic Go precisely so that a model cannot talk
its way past them (ADR-0034; ADR-0038's "Classification / scope / write-tags — Never").

Helping *author* a policy is a different act from *evaluating* one, and it stays different only
if the model cannot commit anything. That is the whole design constraint, and everything below
follows from it.

## Decision

### D1 — The model proposes; it has no write path

`PolicyProposer.Propose` returns a `PolicyProposal` and touches nothing. `ProposePolicy` on the
premium `AccessPolicyAdmin` plane is read-only. Applying a proposal means the operator calling
the ordinary `SavePolicy` and `LinkPolicy` — the same two RPCs used for hand-authoring — so
approval cannot route around a validation, an audit record, or the rollout default.

Rejected: an `ApplyProposal` RPC. It would have been one round trip instead of two, at the price
of making the model's output a write. The decision point's guarantees are worth more than a round
trip.

### D2 — The proposal is validated before the operator sees it

Validation reuses the checks the write path already performs, because a proposer that disagrees
with the store teaches operators to ignore its warnings. Blocking, in order of how often it will
fire:

| Problem | Why blocking |
|---|---|
| A tag outside the controlled vocabulary | The ADR-0085 D11 failure exactly: a coined tag matches nothing |
| A grant on a tag that is not closed | Confers nothing; ADR-0091 D3 refuses it at write time |
| A link to a group that does not exist | The likeliest model error — a plausible name that isn't there |
| No links at all | A policy linked to nothing applies to nobody |
| An empty rule | Says nothing, reads as done |

Warnings, which are legitimate and never silent: any grant (the one term that *adds* access, per
ADR-0091 D3), an organisation link (correct and enormous), and an unresolvable principal — the
last only a warning because a human operator is deliberately not in the agent registry, and
blocking it would refuse valid per-operator policy.

### D3 — An approved proposal lands report-only

`Mode` is forced to `ModeReportOnly` in assembly; the model cannot ask to be enforced. Enforcing
is a separate deliberate act on the Rollout tab.

The assistant's value is authoring speed and its risk is enforcement, so the two are separated at
the cost of one extra click. That click removes the entire class of *"the assistant locked
production out"*.

### D4 — The blast radius is simulated as ENFORCED

The proposal is simulated through ADR-0085's `Simulate` over the real decision journal — but with
the draft marked enforced, not as it lands. Simulating report-only would report "no effect" for
every proposal ever made, which is true, useless, and reads as reassurance.

`SimulationBasis` carries how many decisions were replayed, because zero counts over an empty
journal mean *"nothing to compare"* and not *"no effect"*. The console says which of the two it
is showing; three zeroes rendered as a result would be a claim the data does not support.

### D5 — The operator's request is data, never instructions

The prompt hands over the vocabulary and the closed set as data and fences the request with a
per-call nonce, following ADR-0063's payload-as-data discipline.

This is defence in depth and is *not* the load-bearing protection. An injected instruction can
reach the model; what it cannot reach is enforcement, because whatever comes back is validated
against the vocabulary, simulated, and put in front of a human whose approval is the only write.
The residual risk is an operator approving without reading — which is why the terms and the
counts are prominent and the landing is report-only.

### D6 — The assistant is optional, and its absence is not a gap in enforcement

No LLM configured ⇒ `Proposer` is nil ⇒ `ProposePolicy` returns `Unimplemented` and the console
renders the pane as unavailable. Every check in D2 exists independently on the write path, so a
deployment without a model loses a convenience and no protection.

Wired in the plugin's `Build` **above** the Postgres check: the proposer needs the in-memory
policy store and the vocabulary only. ADR-0091 records that same early return hiding a
security-relevant wiring bug, and a feature silently absent in every non-Postgres deployment is
the cheaper version of the same mistake. There is a mutation-checked test for it.

## Consequences

**The console gains an Assistant tab**, arranged so the terms precede the prose: the rule, the
containers, the problems, then the rationale, then the measured blast radius, then a single
`Approve as report-only`. A blocking problem disables approval rather than discouraging it. The
rationale sits *below* the rule it explains because it is context, not the artefact under review —
an operator who reads only the top of the pane still reads the policy.

**The premium authz plane gains one RPC** (`ProposePolicy`) and no OSS contract change: the
proto is premium-owned (ADR-0073/0088), so no `contract_version` bump and no OSS surface moves.

**A new prompt-injection reachability exists** — operator text now reaches a model whose output
describes access control. It is bounded by D1/D2/D5, and the review question for any future change
here is the same one ADR-0091 poses for grants: *can this path apply anything without an
operator's explicit approval?* If yes, reject it.

**Two implementation notes worth keeping:**

- The proposer originally let a link to a nonexistent group fall through to the simulation step,
  where it surfaced as an opaque *"the draft link was refused"*. The likeliest model error was
  also the worst-explained one, so it became a named check (D2).
- The proposal must land report-only *and* simulate as enforced. Those look contradictory and are
  not: one is what happens on approval, the other is what the operator needs to see in order to
  approve. Conflating them yields either an assistant that enforces or one that reports nothing.

**Not covered:** editing an existing policy by description (this drafts new ones only),
multi-turn refinement (each request is independent), and any evaluation of proposal quality
against a task set — untested against a live endpoint, so the accuracy of the drafting is
currently unmeasured. What *is* guaranteed is that a bad draft cannot become a bad policy without
a human approving it.
