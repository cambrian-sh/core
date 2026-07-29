---
id: 0100
title: Capability-Typed Dispatch — Retiring the Auction as the Default Selection Mechanism
status: Partial
date: 2026-07-29
supersedes:
  - 0037-central-executive-planner
  - 0068-bid-calibration
superseded_by: []
amends:
  - 0002-hybrid-gatekeeper
  - 0050-gaia-benchmark-instrumentation
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

**Partial** (2026-07-29). P0 and P1 implemented; P2–P8 outstanding.

- **P0** — `internal/metabolism/dispatch/` wired and default-on (`execution.bid_round=false`);
  the auction runs only when that flag is set.
- **P1** — D5 resolution ladder + authored alias map (`execution.capability_resolution`, default
  true); vocabulary review passed (table below). `propose()` retirement moved to P3.
- **`execution.capability_contract` flipped to default TRUE** as part of P1. It is what makes the
  planner emit `required_capabilities`; with it off, L1 is a no-op, the D5 ladder never fires, and
  dispatch degrades to merit-ranking alone. A flag that selection depends on is not an arm.

- **P2 harness built** — selection-cost instrumentation on both arms + suite aggregation +
  documented A/B protocol. **The full evidence run has not happened**; a 3-task live probe was
  run to shake out the path (below).

### Live probe, 2026-07-29 — three real defects found by running it

Unit tests passed throughout; only a live kernel exposed these. Progression on the same 3-task
orchestration probe:

| Run | candidates/auction | routing_accuracy | `no_candidate` rows |
|---|---|---|---|
| initial | 1.33 | 0.00 | 2 of 3 |
| + conjunction fix | 0.00 | — | 3 of 3 |
| + spelling fix | 2.00 | 0.50 | 2 of 3 |
| + L2 guard | 1.00 | 0.50 | **0** |

1. **Union membership is not eligibility.** `ResolveCapabilities` checked each requirement against
   the fleet-wide vocabulary union, but L1 needs ONE agent to declare the WHOLE set. A conjunction
   the fleet satisfies collectively but no single agent satisfies alone reported `TierExact`, then
   L1 filtered everyone. Fixed: resolution now takes per-agent capability sets and falls back when
   no single agent satisfies the conjunction.
2. **The resolver normalized; L1 compares verbatim.** With `canonical_vocab` off (the default), a
   planner tag differing only in spelling from the declared one resolved "exactly" and was then
   rejected by L1 on spelling alone. Fixed: the resolved set — which carries the fleet's DECLARED
   spelling — is now substituted on EVERY tier, not just on fallbacks.
3. **L2 could empty a slate L1 had approved.** The semantic gate eliminated the sole
   capability-eligible agent at similarity 0.0, killing the step. That is L2 doing L1's job with a
   fuzzy tool (routing diagnosis D3) and contradicts D1. Fixed: when the capability contract
   actually gated, L2 can no longer empty the slate — it reverts to the L1-eligible set and merit
   ranks it. Guarded to apply ONLY when L1 enforced requirements: with no contract, L1 is a free
   pass and L2 emptying the slate is legitimate (ADR-0023 tool-agent behaviour, still tested).

Selection cost measured on the dispatch arm exactly as D1 predicts:
`agent_boots_per_task = 0`, `selection_latency_ms_mean = 0`.

**Verified at unit level only.** The P0/P1 gates are the `orchestration` suite, which has not been
run — no runtime/E2E validation yet. Two related arms remain OFF and are worth revisiting at P2:
`canonical_vocab` (largely redundant now the D5 resolver normalizes both sides itself) and
`per_capability_merit` (ROUTE-06 tag-scoped ranking — without it, dispatch ranks on GLOBAL merit,
which is the original diagnosis defect D5).

Capability-typed dispatch becomes the **only** selection mechanism.
The auction (ADR-0002) and the EFE selector (ADR-0037) are **removed**, not retained as arms —
operator decision, 2026-07-29. A temporary flag exists solely to capture the A/B evidence and is
deleted in the same PR series (D6, Sequence P2→P3). Yield is re-bound onto dispatch rather than
relocated (D10).

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

### D6 — Both the auction and the EFE selector are REMOVED, not kept as arms

Operator decision, 2026-07-29. Dispatch is not one mechanism among several; it is **the** selection
mechanism. `resource_selector` and the `ResourceSelector` port go with them: a config axis whose
only remaining value is the default is not a choice, it is dead weight, and keeping a dormant
market invites its slow return.

The arm exists **only as migration scaffolding**. Order is measure, then delete (see Sequence):
the A/B is what converts "we removed the auction" into "we removed the auction and routing
accuracy held while `bid_dispersion` measured zero." Same PR series, different commits. Once the
numbers are recorded, the flag and both mechanisms are excised in full.

### D7 — `internal/centralexec/` is not the EFE selector, and must not be deleted wholesale

The package is named for ADR-0037 but contains machinery with **four importers outside itself**:

| Importer | Uses |
|---|---|
| `app/app.go`, `internal/kernel/provider.go` | `NewGatekeeperEFESelector` — the selector |
| `internal/substrate/network/yield_adapters.go` | `YieldCoordinator`, `YieldBinder`, `YieldCaller`, `YieldDriver` |
| `internal/substrate/network/server.go` | `AssignVariant`, `YieldDriver` |

**Agent yield/delegation lives inside the EFE package but is independent of selection.** Deleting
the directory removes yield along with it — a separate feature removal masquerading as a routing
change.

Therefore: delete the selector and its supporting machinery (`gatekeeper_efe.go`,
`inference_selector.go`, `model_selector.go`, `ab.go`, `belief/`, `ladder.go`,
`precision_shaper.go`, `scope_gate.go`, `seed_precision.go`, `verifier_signal.go`,
`capability_catalog.go`), and **extract the yield machinery to `internal/metabolism/yield/`** so
nothing depends on a package named for a retired mechanism. `internal/memory/procedure_store.go`
imports `centralexec` while referencing nothing from it — a stale import to drop.

`AssignVariant` (A/B variant assignment) is consumed by `server.go`; re-home or remove it
deliberately rather than by accident.

### D8 — The agent-plane RPC is deprecated before it is removed

`RequestProposal` appears twice in `api/proto/cambrian.proto` (lines 53 and 377) and is **served
by every published SDK agent** — `cambrian-agent-sdk` 0.1.4 shipped 2026-07-29 with `propose()`
intact. The kernel therefore **stops calling** `RequestProposal` and the RPC stays in the proto,
marked deprecated, for at least one SDK release. Removing it in the same change would break every
0.1.x agent in the field for no benefit: an uncalled RPC costs nothing.

`proposal_bid` and `propose()` are removed from the SDK on its own release cadence, after the
kernel has stopped calling them.

Operator-plane removal (`AuctionEventOp`, `GatekeeperFunnelOp`) is a contract bump and moves the
UI in the same change — it has no external implementors, so it is not subject to the same delay.

### D9 — Two capabilities the auction carried must be re-homed, not dropped by omission

- **Requirement sub-negotiation** (`auctioneer.go:561`) is the one substantive thing a bid carried.
  It is re-expressed as a step-level requirement declaration. No phase may drop it silently.
- **`ExplorationRate`** today randomises the auction pick. Exploration attaches to a ranking just
  as well as to a market: it moves onto the D4 ranking, bounded by the ROUTE-06 per-capability
  `ExplorationBudget`.

**Dead by consequence:** ADR-0068 bid calibration (`internal/metabolism/calibration/`,
`cmd/calibration-report`) calibrates self-reported bids. With no bids there is nothing to
calibrate, and it is removed rather than left as unreachable code.

### D10 — Yield is the second caller of dispatch, and is bound by it

D7 treated yield as a bystander to extract. It is not. Reading `yield_driver.go`, yield has **two
hard dependencies on exactly the machinery D6 deletes**:

- `YieldBinder` "binds a sub-goal intent to a resource (agent ID) **using the live selection
  layer**… Implemented by an adapter over the wired `ResourceSelector`."
- `YieldCaller` "dispatches a handoff to an agent… The adapter wraps `Auctioneer.CallAgent`."

Remove `ResourceSelector` and the `Auctioneer` without re-pointing yield and **yield stops
working**. It must be connected to the new structure, not merely relocated.

The connection is not a retrofit — yield was already built on the same invariant this ADR
enforces. `SubGoal.Intent` is documented as *"expressed in capability-space: a task description,
**NEVER an agent ID** — agents are blind to the resource population and the Central Executive is
the sole binder."* That is the D3 rule and the operator's no-peer-handoff veto, stated
independently in another subsystem. A yielding agent does not choose a successor; it describes
work and the kernel binds it.

**So the three callers unify onto one binder:**

| Trigger | Request | Bound by |
|---|---|---|
| Planner emits a step | `Step.RequiredCapabilities` | dispatch (D1/D4) |
| Agent yields a sub-goal | `SubGoal.Intent` (capability-space) | dispatch (D1/D4) |
| Verifier rejects a result | previous step + typed failure | dispatch (D1/D4), next rank |

Plan dispatch, yield binding, and cascade escalation are **the same operation with different
initiators**: bind a capability-space request to an executor. One binder, three callers.

Consequences for the build:

1. **`SubGoal` gains `RequiredCapabilities`**, mirroring `Step`. Yield binding stops being
   embedding-similarity over a free-text intent and becomes the same typed eligibility gate plus
   merit ranking — the exact D1→D3 upgrade the plan got, and it inherits the D5 resolution ladder
   for free. The intent embedding remains, but as the D15 narrowing guard's input, not as the
   router.
2. **`YieldBinder` re-points at the dispatch function.** The `ResourceSelector` port dies; the
   binder adapter calls dispatch directly.
3. **`CallAgent` survives the auction.** It is "dial the agent and Execute" — no bidding in it.
   It moves out of `auctioneer.go` into the dispatch package as the agent-invocation primitive,
   and `YieldCaller` wraps it there.
4. **The yield safety rails become the cascade's.** O(1) ancestry cycle detection, the D15
   semantic-narrowing guard, and `ErrMaxYieldDepth` already bound a yield chain; D4's escalation
   needs the same bounding, and exhaustion resolves to replan in both. Share one implementation
   rather than growing a second.

This makes the D7 extraction target concrete: yield moves to `internal/metabolism/yield/` **and
takes its binder from `internal/metabolism/dispatch/`**, so the dependency runs
yield → dispatch, never yield → a package named for a retired mechanism.

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

Measure, then delete. P0–P2 land dispatch and record the evidence; P3 excises. The flag is
scaffolding with a scheduled demolition date, not a permanent arm (D6).

| Phase | Change | Gate |
|---|---|---|
| **P0** | `internal/metabolism/dispatch/`: eligibility + ranking + `CallAgent` moved in. `execution.bid_round`, default **off** — off ⇒ D1/D4 dispatch, on ⇒ today's auction. No schema change, no agent change. | orchestration suite: `routing_accuracy` ≥ auction arm; boots/task and routing wall-time down |
| **P1** | D5 resolution ladder + authored alias map + vocabulary review. **Retiring `propose()` moved to P3** — see below. | `routing_accuracy` holds; zero silent misroutes; vocabulary review passes |
| **P2** | **Record the evidence.** Harness BUILT (2026-07-29): `AuctionEventOp.selection_latency_ms` + `selection_boots` emitted identically by BOTH arms (contract 0071, cap `selection-cost`), aggregated by the suite into `selection_latency_ms_mean/_p95`, `agent_boots_per_task/_total`; A/B protocol in `cambrian-benchmarks/docs/orchestration.md`. **The run itself is outstanding.** | the A/B completes; results written down |
| **P3** | **Excision.** Delete `internal/metabolism/auctioneer/`, `domain/auction.go`, `domain/selection.go`, the EFE selector set (D7), ADR-0068 calibration (D9), and the `bid_round` / `resource_selector` / `bypass_auction` / `min_auction_confidence` / `calibrated_bids` / `exploration_rate` config axes. Kernel stops calling `RequestProposal`; RPC marked deprecated, not removed (D8). | `go build ./...` + full suite green; no arm remains |
| **P4** | **Yield onto dispatch (D10).** Extract to `internal/metabolism/yield/`; `SubGoal` gains `RequiredCapabilities`; `YieldBinder` re-points at dispatch; share the cycle/narrowing/depth rails with the cascade. | yield E2E green; sub-goal binding typed, not embedding-routed |
| **P5** | Typed-failure cascade (D4 escalation branch), on the shared rails from P4 | per-step recovery rate up; wall-time p95 flat |
| **P6** | Operator-plane removal (`AuctionEventOp`, `GatekeeperFunnelOp`) + contract bump + UI; SDK drops `propose()`/`proposal_bid` on its own cadence | contract handshake updated; UI skew handled |
| **P7** | Promote the ROUTE-07 learned scorer once accrued funnel + verifier data clears its offline win gate | ADR-0076's existing offline-win gate |
| **P8** | `Step.PreferredAgent`/`AgentPin` (ADR-0096) as the operator/UI-authored route channel | pin honour rate; misroute rate under pins |

**P3 is the point of no return** and the reason P2 exists: after it, there is no arm to compare
against, so the comparison must already be written down.

### Sequencing correction (2026-07-29, during P1)

The original P1 line bundled "retire the `propose()` keyword tables" with the resolution ladder.
**That was a sequencing error and is corrected above.** `propose()` is the auction's ONLY consumer:
stripping it before P2 would make every agent bid the constant `0.5`, collapsing the auction arm
into a degenerate baseline and destroying the very A/B that P2 exists to record. It now lands in
P3, where the auction is deleted and `propose()` is dead code by definition.

What P1 keeps from that half is the part dispatch actually depends on — the **vocabulary review**,
confirming the declared capabilities cover the routing distinctions the keyword tables encoded.

**Result: the review passes.** Every keyword table maps onto an already-declared capability:

| Agent | Keyword table | Declared capability |
|---|---|---|
| analyst | `_ANALYSIS_KEYWORDS` | `analysis` |
| calculator | `_MATH_KEYWORDS` | `calculation` |
| code_executor | `_CODE_EXEC_KEYWORDS` | `code_execution` |
| code_generator | `_CODE_KEYWORDS` | `code_generation` |
| research | `_RESEARCH_KEYWORDS` | `research` |
| summariser | `_SUMMARY_KEYWORDS` | `summarisation` |
| terminal | `_SHELL_KEYWORDS` | `shell_execution` |

The *negative* tables (`_SHELL_NEGATIVE_KEYWORDS`, `_CODE_EXEC_NEGATIVE_KEYWORDS`) encoded "this is
not for me" — subsumed by the capability contract, which makes an agent lacking the required tag
ineligible rather than merely low-bidding. **Retiring `propose()` therefore loses no routing
information**, which is what makes P3's deletion safe. All eight reference agents already declare
`general_purpose`, so the D5 generalist tier has a population from day one.

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
