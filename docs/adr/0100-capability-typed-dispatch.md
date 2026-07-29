---
id: 0100
title: Capability-Typed Dispatch — Retiring the Auction as the Default Selection Mechanism
status: Proposed
date: 2026-07-29
supersedes: []
superseded_by: []
amends:
  - 0002-hybrid-gatekeeper
depends_on:
  - 0067-capability-vocabulary-canonicalization
  - 0068-bid-calibration
  - 0069-per-capability-merit
  - 0076-learned-gatekeeper-scorer
  - 0077-gatekeeper-route-preview
  - 0096-explicit-agent-pinning
  - 0013-mid-execution-semantic-checkpoint
  - 0050-gaia-benchmark-instrumentation
---

# ADR-0100: Capability-Typed Dispatch

## Status

Proposed (2026-07-29). Supersedes the auction as the **default** selection mechanism; the
auction is retained as a measurement arm, not deleted.

## Context

### The auction carries no signal

Verified end-to-end 2026-07-29 (`docs/research/agent-selection-mechanisms/SUMMARY.md`):

- Every shipped agent's `propose()` is a hand-written keyword table returning one of two or
  three constants; agents without an override bid the literal `0.5`
  (`sdk/cambrian_agent_sdk/runtime.py:146`). The entire bid space across the reference roster is
  the six-value set `{0.9, 0.85, 0.5, 0.3, 0.2, 0.1}`. There is no model call and no learned
  content anywhere in the bid path.
- Soliciting a bid **boots the agent process** — `requestProposalFromAgent` →
  `getOrDialClient` → `bootDialRegister` → `GetOrBootInstance`
  (`auctioneer.go:392→133→163`). Up to five cold starts per step, four discarded before any work
  begins. This is the known "agents re-spawn every query" symptom.
- The Zero-Hardcode Rule is satisfied in letter (no routing table in Go) and violated in spirit:
  `_MATH_KEYWORDS`, `_SHELL_KEYWORDS` and their siblings *are* routing tables, exported across
  the process boundary into agent Python where the kernel cannot audit, version, benchmark, or
  improve them.

ROUTE-01…08 (ADR-0067/0068/0069/0076/0077/0096) shipped nearly all of the 2026-07-13 routing
diagnosis, and every one of those fixes improved a **kernel-side input**. None could improve the
bid, because the bid has no improvable content. That is why fixing D1–D5 did not resolve the
dissatisfaction with routing.

### The external evidence agrees

No surveyed production framework selects agents by market — not OpenAI, Anthropic, Google,
Microsoft, AWS, LangChain, CrewAI or Cognition. MarketBench (arXiv:2604.23897) finds
self-assessment is *the* bottleneck for market-style coordination (only Opus 4.5 and Sonnet 4.5
achieve positive Brier skill; token estimates off ~50×). Agora (arXiv:2607.09600) reaches only
parity with a 1-NN router after adding a two-tier calibration stack.

The deeper reason is structural: in the multi-robot auctions this lineage descends from, a bid is
a **computed physical cost** (distance, battery, capacity) that the auctioneer cannot derive
itself. An LLM agent's confidence is an opinion the kernel can already estimate better from
verifier history. The Contract Net lineage does not transfer.

### The constraints that fix the answer

Three architectural vetoes are settled policy (operator decision, 2026-07-29):

1. **No LLM round trips for selection.** Bidding is out on latency grounds regardless of quality.
2. **No design-time authored graph as the default.** Operator-authored routes are available
   through the UI; they are never how a plan routes by default.
3. **No agent-to-agent handoff.** Agents never talk to each other; the kernel mediates every
   `Handoff`.

Those vetoes eliminate the market, authored graphs, peer handoff, and agent-as-tool. What remains
is the mechanism that already matches the substrate's topology: a hub-and-spoke scheduler with a
typed job spec, a typed worker registry, and an outcome log.

## Decision

### D1 — Selection is a pure function in the kernel, not a negotiation

Agent selection becomes an in-process ranking over data the kernel already owns. **Zero RPCs,
zero LLM calls, zero speculative process boots.** The winner is the only agent booted.

Three separable decisions, currently conflated by the auction:

| Decision | Kind | Substrate |
|---|---|---|
| **Eligibility** | hard gate | `step.RequiredCapabilities ⊆ manifest.Capabilities` + policy + formats (ADR-0067 / ROUTE-03, enforcing) |
| **Ranking** | soft order | `ScoreMerit` over per-capability success/trust, latency, cost, provisional flag (ADR-0069, ADR-0076) |
| **Recovery** | on failure | cascade to next rank with the typed failure carried forward |

### D2 — The plan already carries the complete job spec

No new schema is required. `domain.Step` already declares everything dispatch needs:
`RequiredCapabilities` (the contract), `MaxEnergy` (budget), `RecommendedModel`,
`CheckpointAfter`/`CheckpointQuery` (whether this step is verified), `PreferredAgent`/`AgentPin`
(ADR-0096), `FanOutOver`. The auction was sitting on top of a complete specification and adding
noise to it.

### D3 — Intelligence enters routing in exactly one place: the planner

The planner emits `required_capabilities` from the live capability vocabulary already in its
prompt. This is the Zero-Hardcode compliance point — the contract is derived from live manifests,
so a newly registered agent is routable immediately — and it is free, because the planner already
runs. It delivers supervisor-style selection without a supervisor call.

Everything downstream is deterministic arithmetic. **Consequence, stated explicitly: planner
output quality becomes the ceiling on routing quality.** Vague capabilities cannot be recovered
downstream. This makes planner output the highest-leverage investment after this ADR lands, and
this change is what makes its training data clean.

### D4 — Dispatch policy is per-step, driven by fields the planner already emits

Pure argmax-on-merit has a rich-get-richer failure: the top-ranked agent accumulates all the
evidence, every other agent stays cold, and the ranking freezes on early noise. Pure
cheapest-first pays a re-execution whenever it guesses wrong. Neither is right everywhere, and
the step itself already says which regime it is in.

```
eligible = { a : step.RequiredCapabilities ⊆ a.Capabilities }
ranked   = sortDesc(ScoreMerit(a, trait, requiredCaps, cfg, scorer) for a in eligible)

if step.CheckpointAfter && step.MaxEnergy is low:
        dispatch cheapest(a in ranked : merit(a) >= floor)
        on verifier failure -> next rank, typed failure carried forward
        on exhaustion       -> replan
else:
        dispatch ranked[0]
        exploration via the ROUTE-06 ExplorationBudget
```

The cheapest-competent branch is not merely a cost optimisation: **it supplies the discrimination
the auction pretended to provide.** "Ask every candidate how good it would be, then pick" becomes
"dispatch the cheap one, verify, escalate" — the same signal, nobody asked anything, paid only
when wrong, and exploration comes free because cheap agents get tried and the verifier supplies
the label.

Both branches keep `ExplorationRate` and the ROUTE-06 per-capability exploration budget.

### D5 — Unmatched capabilities resolve down a ladder, then fail loudly

A `required_capabilities` value matching no registered agent is a **live failure mode**, not a
hypothetical: ADR-0096 measured 66 L1 filter events and 4 `no candidates` with the task dying.
With the auction gone there is no accidental fallback left, so the ladder is load-bearing:

```
NormalizeCapability            (ADR-0067, deterministic)
  → authored alias map         (data, human-reviewed)
  → declared generalist tier   (agents declaring a generalist capability)
  → FAIL, naming the unmatched tag and the live vocabulary
```

**No embedding-nearest fallback.** ADR-0067 rejected fuzzy capability merging because `file-read`
and `file-write`, `read` and `delete`, are embedding-close and semantically opposite. That
rejection holds here: synonyms belong in a reviewed data file, never in cosine distance. The
generalist tier keeps plans alive without ever silently misrouting; exhausting the ladder is an
error that names the gap.

### D6 — The auction becomes an arm, not deleted code

`resource_selector="auction"` joins the existing `"efe"` arm. It is retained because it is the
control that makes the dispatch claim falsifiable (ADR-0050 discipline), and because the
mechanism becomes correct again if a future fleet ever has genuinely private, verifiable,
locally-computable costs.

The requirement sub-negotiation path (`auctioneer.go:561`) is the one substantive thing a bid
carries and **must be preserved** — re-expressed as a step-level requirement declaration rather
than a bid field. No phase may drop it silently.

## Consequences

| | Auction (today) | Capability-typed dispatch |
|---|---|---|
| External calls per decision | 3–5 RPCs | 0 |
| Process boots per decision | up to 5 | 0 — boot the winner only |
| Where the routing table lives | agent Python keyword sets | declared capabilities in manifests |
| Auditability | reconstruct from feed events | replay offline via `PreviewRoute` (ADR-0077) |
| Improvable by data | no | yes — verifier → merit → scorer |
| Escalation | none | cascade on typed failure |

**Costs accepted.** The channel for live agent load disappears (today's `estimated_latency_ms` is
an authored constant, so nothing real is lost, but the channel goes); recover via a declarative
non-LLM quote RPC only if measurement shows it matters. And the "market of agents" narrative is
retired in favour of an accurate one: Cambrian runs a **continuously-priced merit market** —
agents build reputation through verified outcomes and are selected on that price — rather than a
per-step bidding ritual.

## Sequence

| Phase | Change | Gate |
|---|---|---|
| **P0** | `execution.bid_round`, default **off**. Off ⇒ D1/D4 dispatch; on ⇒ today's auction. No schema change, no agent change. | orchestration suite: `routing_accuracy` ≥ auction arm; boots/task and routing wall-time down |
| **P1** | D5 resolution ladder + alias map; retire `propose()` keyword tables, converting each keyword set into declared manifest capabilities | `routing_accuracy` holds; zero silent misroutes; vocabulary review |
| **P2** | Promote the ROUTE-07 learned scorer once accrued funnel + verifier data clears its offline win gate | ADR-0076's existing offline-win gate |
| **P3** | Typed-failure cascade (D4 escalation branch) | per-step recovery rate up; wall-time p95 flat |
| **P4** | `Step.PreferredAgent`/`AgentPin` (ADR-0096) as the operator/UI-authored route channel | pin honour rate; misroute rate under pins |

## Falsification

The `orchestration` suite already computes `routing_accuracy`, `misroute_rate`, `bid_dispersion`,
`candidates_per_auction` and per-`failure_kind` counts against 120 gold-labelled tasks. Add two
counters: agent process boots per task, and routing-decision wall-time per step.

Arms: `auction` (current default) · `dispatch` (P0) · `dispatch-learned` (P2) ·
`bypass_auction` (ADR-0050 control).

**Predictions.** Routing accuracy flat or up; misroute rate flat or down; boots per task down by
roughly the candidate count; routing wall-time to near zero. And the decisive one: **if
`bid_dispersion` on the auction arm returns near zero — which the bid ledger predicts it must —
that is direct empirical proof the market carried no signal**, and it is the headline result
either way.

If routing accuracy drops materially on the `dispatch` arm, the cause is the planner's
`required_capabilities` quality (D3's stated ceiling), not the dispatch mechanism — diagnose there
before reverting.

## References

- `docs/research/agent-selection-mechanisms/SUMMARY.md` — the full survey, evidence, and option space
- `docs/research/task-routing-diagnosis/REPORT.md` — D1–D5 and the ROUTE campaign
- MarketBench (arXiv:2604.23897), Agora (arXiv:2607.09600), ChromaFlow (arXiv:2605.14102)
- `internal/metabolism/auctioneer/auctioneer.go` :392, :133, :163, :561
- `sdk/cambrian_agent_sdk/runtime.py` :138–146
- `internal/supervision/gatekeeper/gatekeeper.go` — `ScoreMerit` :438
- `domain/plan.go` — `Step`, `PinSoft`/`PinHard`
- `domain/capability_normalize.go` — `NormalizeCapability`
