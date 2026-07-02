# ADR 0003 — Awareness Layer Redesign: Step.RequiredTools and The Lobes

**Status:** Implemented  
**Date:** 2026-05-01  
**Deciders:** Afsin

---

## Context

The Awareness layer (`internal/awareness/planner.go`) was a one-shot string template
that violated the system's own Zero-Hardcode Rule and contained several silent failure
modes, discovered during the CURRENT_PROBLEMS.md audit.

### Plan Schema — Dead Field and Disabled Filter

`ExecutionPlan.Step` contained an `Agent string` field — a specific agent ID that the
LLM Planner named per step. This caused two silent problems:

1. **Dead code:** `Step.Agent` was never read by the execution path. `server.go` built
   `AuctionTask` from `step.Query` only; the named agent was silently ignored and the
   Gatekeeper always ran a full semantic search across all agents regardless.

2. **Zero-Hardcode violation at the Planner level:** The LLM was naming specific agents
   ("use browser_agent for this step") — hardcoded routing inside the Awareness layer
   itself. The correct model: the LLM identifies *what capability is needed*, the
   Gatekeeper decides *who has it*.

3. **Declaration Filter disabled:** `AuctionTask.RequiredTools` was never populated, so
   the Gatekeeper's Layer 1 hard compatibility check always received empty lists and
   never fired. The `2+1 → browser_agent` misrouting was a direct consequence.

### Planner Architecture — One-Shot String Template

Beyond the schema problem, the Planner itself had six compounding failure modes:

1. **Prompt tab corruption** — the Go template literal embedded two levels of tab
   indentation in every instruction line sent to Ollama, degrading LLM response quality.
2. **`required_tools` hallucination had no safety net** — if the LLM emitted a tool
   name not declared by any registered agent, Declaration Filter eliminated all
   candidates and the step failed silently with `NO_WINNER`.
3. **Fragile JSON extraction** — `TrimPrefix("` ``` `json")` / `TrimSuffix("` ``` `")`
   failed on Llama3's common output variations (newlines, spaces, trailing text).
4. **No context propagation** — `GetExecutionPlan` took no `context.Context`; the LLM
   call could not be cancelled by the DAG plan-level timeout.
5. **No procedural memory** — every request was planned from scratch; structurally
   identical past plans were never reused.
6. **`required_tools` not validated before the Gatekeeper** — the LLM's tool names
   were trusted as-is with no registry cross-check.

The `TIERED_AWARENESS_FOR_CAMBRIAN.md` design document proposed a four-lobe
architecture (Thalamus, Cortex, Homeostat, Hippocampus) to address these problems.
This ADR records all decisions from that review, including what was accepted, modified,
and dropped.

---

## Decision

### 1. Step.RequiredTools Replaces Step.Agent

`Step.Agent string` is removed. `Step.RequiredTools []string` takes its place.

- The LLM Planner prompt lists each agent with its declared tools from `AgentManifest`
  (via `AgentProvider.GetManifest`) and instructs the LLM to emit
  `required_tools: [...]` per step — not agent names.
- `server.go` wires `step.RequiredTools` into `AuctionTask.RequiredTools`.
- The Gatekeeper Declaration Filter now receives real tool requirements and hard-filters
  candidates before every auction.
- `resp.FromAgent` (stamped with `bestProposal.AgentID` in `stepFn`) replaces
  `Step.Agent` as the source of `AgentID` in `TaskEvent` records.

### 2. Lobe I — Thalamus: DROPPED

**Proposed:** A pgvector search against agent cognitive fingerprints at the Planner
level, pre-filtering the registry to Top-K agents before the Cortex generates the plan.

**Dropped because:**

- The Gatekeeper (`FindCandidates`) already runs the identical vector search per step
  with better context (step-level query vs. raw user input).
- A pre-plan filter on the full user input breaks multi-step decomposition: "play KMFDM
  and summarize lyrics" needs `text_analysis_agent` in step 2, which a Thalamus search
  on the full input would exclude before the Cortex could request it.
- Provisional agents (no pgvector fingerprint yet) would be silently invisible to any
  Thalamus search; the Gatekeeper handles them explicitly via `DefaultProvisionalScore`.
- Prompt bloat is already addressed by `Step.RequiredTools` tool annotations.

**Replacement:** The Gatekeeper remains the sole vector-search filtering layer,
operating per step with step-level query context.

### 3. Lobe II — Cortex: ONE CALL + ONE HOMEOSTAT RETRY

The Cortex is a single `Generate` call with a well-structured prompt. A multi-turn
think loop is not feasible on the edge-device target: Ollama/Llama3 latency is 5–30 s
per call; two "think" turns consume 10–60 s before the first agent boots, approaching
the 120 s plan timeout ceiling.

**Constraint: maximum 2 LLM calls per plan** (initial attempt + one Homeostat-triggered
retry). This fits within `llm.timeout_ms (60 s) × 2 = 120 s = plan_timeout_ms`.

**Concrete prompt improvements (to be implemented):**

- Remove leading tab indentation from the template literal.
- Replace `TrimPrefix/TrimSuffix` JSON extraction with a regex extractor
  (`\{[\s\S]*\}`) robust to Llama3 output variations.
- Thread `context.Context` through `GetExecutionPlan` and `Generate` so the
  plan-level `context.WithTimeout` can cancel a slow LLM call.
- Inject a Hippocampus template hint as a `PRIOR SUCCESSFUL PLAN` section (see §5).

### 4. Lobe III — Homeostat: `validatePlan` INSIDE `planWithValidation`

The Homeostat is a process, not a placeholder. It is implemented as a
`validatePlan(plan *domain.ExecutionPlan, knownTools map[string]struct{}) error`
function called inside `planWithValidation` after JSON parse and before returning the
plan. It is not a new package or struct boundary.

**Checks performed (in order):**

1. **Syntax** — already covered by `json.Unmarshal` in `GetExecutionPlan`.
2. **DAG integrity** — already covered by `TopologicalSort`; promoted from
   non-retryable to retryable (returns `PlanValidationError`).
3. **Capability check** — NEW: every tool name in `step.RequiredTools` must exist in
   the registry's known tool vocabulary. Unknown tool names constitute a Pain Signal.

**Pain Signal — `PlanValidationError`:**

A single error type replaces all ad-hoc retry triggers:

```go
type PlanValidationError struct {
    Signal string // human-readable description sent back to the Cortex
}
```

`planWithValidation` catches `PlanValidationError`, appends `Signal` to the original
`userInput` as a `PREVIOUS PLAN ERROR:` block, and re-calls `GetExecutionPlan` once.
The second attempt is validated again; if it fails, `planWithValidation` hard-fails —
no third attempt.

The existing `CyclicPlanError` retry path is **replaced** by this generalised
mechanism. `CyclicPlanError` becomes one possible `Signal` value produced by
`validatePlan`.

**Known-tool vocabulary source:** `validatePlan` receives a `map[string]struct{}` of
all tool names across all registered agent manifests, built once per `Execute` call
by ranging over `registry.GetAllAgents()` + `registry.GetManifest(id)`. O(N) over
agent count; acceptable at plan time, not per step.

### 5. Lobe IV — Hippocampus: PROCEDURAL TEMPLATES IN PGVECTOR

Successful DAG patterns are stored in pgvector as Procedural Templates and injected
into the Cortex prompt for structurally similar future requests.

**Embedding key — Hybrid Intent + Structure (single vector):**

```
"Intent: {Subject} | Tools: {sorted_unique_required_tools}"
```

Example: `"Intent: Music Playback | Tools: music-player, search"`

A single `Embed` call on this canonical string captures both the semantic intent
(Subject) and the structural pattern (tool sequence). Tools are sorted alphabetically
so structural equivalence is preserved regardless of step order. The string is
lowercased and punctuation-stripped before embedding to reduce `Subject` brittleness
(LLM-generated, inconsistent capitalisation).

**Storage:** One pgvector document per successful plan. Document body = full
`ExecutionPlan` JSON. Metadata includes `mean_auction_confidence` (see §6) and
`stored_at` timestamp.

**Retrieval:** At the start of `GetExecutionPlan`, embed the intent half of the current
request (Subject not yet known; use normalised `userInput` as the intent signal).
Retrieve the top-1 match above similarity threshold **0.85**. If found, inject as a
`PRIOR SUCCESSFUL PLAN (mean_confidence: X)` section in the Cortex prompt. The Cortex
may follow, adapt, or ignore the template — it is a hint, not a constraint.
Templates with `mean_auction_confidence < 0.5` are excluded from retrieval.

**Write path:** `Server.Execute` writes to the Hippocampus after `DAGExecutor.Execute`
returns without error. Subject comes from `plan.Subject`; tools are collected from all
`step.RequiredTools` across the completed plan.

### 6. Proprioceptive Feedback: POST-EXECUTION CONFIDENCE TAGGING

Mid-execution re-planning is rejected. Cancelling in-flight DAG goroutines mid-plan
and re-planning conflicts with `DAGExecutor`'s unconditional `WaitGroup` drain
guarantee. Partially-executed steps leave orphaned `masterContext` keys that a
re-planned step index map cannot safely consume.

**Post-execution proprioception:** After a plan completes, `Server.Execute` computes
the mean auction confidence across all steps (winning `AgentProposal.Confidence` values
propagated from `stepFn`). This value is stored as `mean_auction_confidence` metadata
on the Hippocampus template. Templates below `0.5` are excluded from future retrieval —
they succeeded structurally but with low agent conviction.

---

## Implementation Order

1. **Step.RequiredTools schema change** — `plan.go`, `planner.go`, `server.go`,
   `dag_executor.go`; update all tests. *(Already implemented.)*
2. **Planner prompt fixes** — remove tab indentation, regex JSON extraction, context
   threading through `GetExecutionPlan` and `Generate`.
3. **`PlanValidationError` + `validatePlan`** — generalise `planWithValidation`,
   add capability check, build known-tool vocabulary from registry.
4. **Hippocampus storage** — new pgvector document type for Procedural Templates;
   write path in `Server.Execute`; retrieval + prompt injection in `GetExecutionPlan`.
5. **Proprioception** — propagate mean auction confidence from `stepFn` to
   `Server.Execute`; store with Hippocampus template; filter low-confidence templates
   from retrieval.

---

## Alternatives Considered

**Keep `Step.Agent` and also add `Step.RequiredTools`** — introduces ambiguity: which
field drives routing? Two fields with overlapping semantics would confuse future
contributors. Rejected.

**Keep `Step.Agent` as a Gatekeeper priority hint** — the named agent gets a scoring
boost but doesn't hard-bypass Gatekeeper. Still a Zero-Hardcode violation at the
Planner level. Rejected.

**Thalamus as pre-planning vector filter** — the Gatekeeper already provides per-step
filtering with superior context; multi-step decomposition makes pre-plan filtering
unreliable; Provisional agent blind spot. Rejected.

**Multi-turn Cortex think loop** — latency budget on edge hardware does not permit
more than 2 LLM calls per plan (5–30 s × N turns approaches 120 s plan ceiling).
Rejected.

**Dual-vector Hippocampus key (intent + structure as separate embeddings)** — requires
`VectorStore` interface extension and two vector operations per plan; single
concatenated embedding achieves the same clustering with zero interface changes.
Rejected.

**Mid-execution re-planning on low-confidence auction** — conflicts with `WaitGroup`
drain guarantee; requires event-sourced execution model out of scope for edge target.
Rejected.

**Homeostat as a separate Go package/struct** — the Homeostat is a validation process,
not an architectural boundary. A named function inside `planWithValidation` is
sufficient and avoids a premature abstraction. Rejected.

**Separate retry paths for CyclicPlanError and capability errors** — doubles retry
code; can trigger two retries on a plan with both a cycle and an unknown tool.
Rejected in favour of unified `PlanValidationError`.

---

## Consequences

**Positive:**
- Declaration Filter is now active on every step. The LLM expresses capability
  requirements, not agent identity — fully consistent with the Zero-Hardcode Rule.
- `AgentID` in `TaskEvent` reflects the actual runtime winner (`resp.FromAgent`)
  rather than the Planner's suggestion.
- Prompt tab corruption and fragile JSON extraction are fixed by Cortex improvements.
- `required_tools` hallucination has a safety net: `validatePlan` rejects unknown tools
  and sends a structured Pain Signal for one Cortex retry.
- Recurring request patterns amortise LLM planning cost via Hippocampus templates.
- Post-execution proprioception demotes low-conviction templates without any changes to
  the DAG execution model.
- `PlanValidationError` unifies all retry triggers; `planWithValidation` has a single,
  testable two-attempt pipeline.

**Negative:**
- Requires every `AgentManifest` to have a meaningful `Tools` list for the Planner
  prompt to be useful. Agents with empty manifests still work (empty `required_tools`
  → no Declaration filtering) but provide no routing signal to the LLM.
- If the LLM emits a tool name that no agent declares and the retry also fails, the
  step hard-fails with `NO_WINNER`. This is the correct failure mode — the developer
  must add an agent with that tool.
- Hippocampus requires a new pgvector document schema and a write path in
  `Server.Execute` — one `Embed` + one `Save` call on every successful plan.
- Known-tool vocabulary must be rebuilt per `Execute` call (O(N) registry scan);
  should be cached if agent count grows beyond ~100.
- `GetExecutionPlan` signature must change to accept `context.Context` — all callers
  and tests must be updated.
- Mean auction confidence propagation requires threading `AgentProposal.Confidence`
  values from `stepFn` back to `Server.Execute` — currently those values are logged
  but not accumulated.
