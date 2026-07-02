# ADR-0011: The Neuromodulator — Cost-Aware Multi-Model LLM Routing

**Status:** Implemented

**Context:**
Cambrian currently uses a single Ollama-backed `LLMClient` (`internal/awareness/client.go`) with hardcoded `/api/generate` and `/api/embeddings` endpoints. The system cannot route planning or generation tasks to OpenAI, Anthropic, Gemini, or local models based on task complexity, cost, or past performance. The biological metaphor is **Neuromodulation**: different brain regions (models) activate based on task demands, and synaptic strength (merit) evolves through use.

**Decision:**

1. Introduce `TraitModel` — a third `AgentTrait` alongside `TraitCognitive` and `TraitTool` — for LLM inference providers registered as first-class `AgentDefinition`s.
2. Extend the existing Gatekeeper `computeMeritScore` formula with a fourth cost-penalty term instead of replacing it:
   ```
   score = w1*successRate + w2*trustScore + w3*(1/normLatency) - w4*normalizedCost
   ```
   where `normalizedCost = unitCost / maxUnitCostAcrossAllModels` (scales to [0,1]).
3. Store per-model definitions in `config.json` as a `models[]` array with `provider`, `model`, `endpoint`, `cost_per_1m_input/output`, `timeout_ms`, and `capabilities`.
4. Add token-usage fields (`PromptTokens`, `CompletionTokens`, `TotalTokens`, `EstimatedCost`) to `domain.TaskEvent` and extend `AgentProfile` with `ModelMetrics` JSONB for per-model EWMA tracking via the existing `ProfileAggregator` pipeline.
5. Add `Step.MaxEnergy` (per-step budget) and `ExecutionConfig.MaxPlanCost` (plan-level budget). When the running cost exceeds `MaxPlanCost`, the DAGExecutor pauses, sets `budgetExceeded=true`, and triggers the ReplanHandler. The Planner retags all remaining steps with the cheapest available model. If the cheapest model still exceeds per-step budget, escalate to HITL (`InterventionRequest` to TUI).
6. Step-level model selection: the Planner tags each `Step` with `RecommendedModel` based on task complexity. The Auctioneer treats LLM models as candidates in the same auction pipeline as task agents, scoring them with the extended merit formula.

**Considered Options:**

| Decision | Chosen | Rejected | Rationale |
|----------|--------|----------|-----------|
| Trait for LLM models | `TraitModel` (new) | `TraitCognitive` or `TraitTool` | Cognitive agents require Interview + Verification; Tool agents are deterministic scripts. LLM providers are neither — they are the substrate itself. A dedicated trait lets the Gatekeeper skip Interview but still track Merit. |
| Merit formula | Extend additive (`w4` penalty) | Multiplicative numerator + denominator | The existing additive formula is calibrated and backward-compatible. Adding a penalty term preserves existing behavior when `w4=0`. Multiplicative formulas allow cheap garbage to dominate. |
| Config source | `config.json` array | Env vars or dynamic API | Pricing changes monthly; operators must control costs without code changes. A config array is single-source-of-truth and matches existing `Config` struct pattern. |
| Budget exceeded | Graceful degradation → HITL | Hard stop or advisory only | Mirrors biological homeostasis: autonomic response (replan to cheap model) first, conscious intervention (TUI prompt) only if autonomic fails. |
| Merit tracking | Extend `ProfileAggregator` + `AgentProfile` | New `CognitiveAuditor` component | The existing pipeline already computes EWMA SuccessRate, TrustScore, and latency medians. Adding ModelMetrics JSONB reuses the same background worker, storage schema, and decay logic. A parallel auditor would create architectural debt. |

**Consequences:**

- **Positive:** Cambrian becomes model-independent. Adding a new LLM is a config change, not a code change. Cost guardrails prevent runaway spending. The biological metaphor (Neuromodulation, Foraging Theory, Synaptic Plasticity) is preserved and strengthened.
- **Positive:** The existing auction pipeline (`Auctioneer`, `Gatekeeper`, `ProfileAggregator`) is reused for LLM model selection. No parallel infrastructure.
- **Negative:** `AgentProfile` schema grows with `ModelMetrics` JSONB. Existing pgvector rows must be migrated or the field defaults to empty JSON.
- **Negative:** The Planner prompt must be updated to include model capabilities and cost constraints when tagging steps with `RecommendedModel`.
- **Negative:** Token usage must be parsed from every provider's response format (OpenAI, Anthropic, Ollama, Gemini all use different field names). A `TokenUsageExtractor` interface abstracts this.

**Glossary additions:**
- **TraitModel** — `AgentTrait` value for LLM inference providers (e.g., GPT-4o, Llama-3). Skips Interview and Verification but participates in Merit tracking.
- **ModelMetrics** — JSONB field inside `AgentProfile` tracking per-model token totals, estimated cost totals, and average cost per task.
- **NormalizedCost** — `unitCost / maxUnitCostAcrossAllModels`, scales model cost to [0,1] for the Gatekeeper penalty term.
- **MaxEnergy** — Per-step budget cap (`Step.MaxEnergy`). If a step's estimated cost exceeds this, the step is deferred to the cheapest model or escalated to HITL.
- **Foraging Ratio** — `estimatedCost / gatekeeperScore`. Lower is better. The Auctioneer can optionally sort candidates by this ratio for pure cost-efficiency mode.

**Rejected:**
- Replacing the Gatekeeper formula entirely (would break existing config and calibration).
- Hardcoded cost tables in provider client code (would require code changes for price updates).
- Dedicated `CognitiveAuditor` component (would duplicate `ProfileAggregator`).

