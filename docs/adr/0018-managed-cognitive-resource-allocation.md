# ADR-0018: Managed Cognitive Resource Allocation — LLM Gateway and Token Governance

**Status:** Implemented  
**Revision:** v3 (2026-05-28)

**Context:**

Cambrian operates in a **World B** architecture: cognitive agents (`TraitCognitive`) are task executors, and LLM inference providers (`TraitModel`) are first-class agents allocated by the Auctioneer. ADR-0011 established the `TraitModel` trait, per-step `Step.MaxEnergy` budget, and `TokenUsage` tracking via the `ProfileAggregator` pipeline. ADR-0009 established `SignalStream` + `Watcher` for async agent-to-Planner communication.

The missing piece: no enforced gateway exists between a cognitive agent and the LLM it uses. Agents currently call `Generator.Generate` directly — the Substrate cannot intercept calls, enforce token budgets, or attribute consumption to a specific step. This creates two risks:

1. **Runaway token consumption** — a misbehaving or prompt-injected cognitive agent can burn unbounded tokens outside any budget guardrail.
2. **Unattributed cost** — token usage cannot be linked to a specific `planID + stepIndex`, making per-step cost attribution in `TaskEvent` impossible to enforce.

The biological metaphor is **Thalamic Gating**: the thalamus (Substrate) does not just relay signals — it mediates, filters, and meters which cognitive circuits (agents) receive which sensory resources (LLM calls) and at what intensity (token budget).

**Decision:**

### 1. Auctioneer auto-pairing at plan time with top-K `StepAllocation`

When a `TraitCognitive` agent wins a step, the Auctioneer immediately runs a `TraitModel` sub-selection using the same `computeMeritScore` formula (ADR-0011). It stores the **top-3 candidates** (winner + 2 fallbacks) in a `StepAllocation` struct per step. `DAGExecutor` passes the full `StepAllocation` to the Substrate gateway at dispatch. No LLM allocation occurs mid-execution; all model binding is complete before `DAGExecutor` begins.

### 2. `GenerateViaModelStream` — the only LLM call path

A new streaming RPC is added to the `Orchestrator` service in `cambrian.proto`:

```protobuf
rpc GenerateViaModelStream(GenerateStreamRequest) returns (stream GenerateChunk);
```

The Substrate acts as a **metered proxy**: it receives the model's token stream, applies the dual-pass token accounting policy (see Decision 4), forwards each `GenerateChunk` to the calling agent, and closes the stream when the real-time budget is reached. `AgentService.Execute(Handoff) returns (Handoff)` stays **unary** — the streaming concern is isolated to the LLM call path. Cognitive agents call `GenerateViaModelStream` from *within* their `Execute` handler, buffer chunks locally, then return a complete `Handoff` as before.

### 3. Per-step session token with keepalive TTL

At step dispatch, the Substrate issues a session token and stores a `SessionState` in a server-side map keyed by an opaque UUID:

```go
type SessionState struct {
    PlanID               string
    StepIndex            int
    StepAllocation       StepAllocation  // winner + 2 fallbacks
    TokenLimit           int
    ConsumedTokens       int             // running real-time estimate
    ActualTokensUsed     int             // reconciled at stream close
    ExpiresAt            time.Time       // updated on each chunk — keepalive TTL
    LastActivityAt       time.Time
}
```

The session token (opaque UUID) is set as a **typed field** `SessionToken *SessionToken` on `domain.Handoff` — not a magic string in the context bag (see Considered Options). `TokenLimit` maps to `Step.MaxEnergy` (ADR-0011).

**Keepalive TTL:** `ExpiresAt` is refreshed on every `GenerateViaModelStream` chunk received (`ExpiresAt = now + SessionTokenTTLMultiplier × estimatedStepDuration`). This makes the TTL a keepalive timeout since last activity, not a creation-time deadline. A slow but legitimate deep-reasoning step remains alive as long as chunks are flowing. A hung step (no chunks for `TTLMultiplier × estimatedStepDuration`) is correctly scavenged.

### 4. Dual-pass token accounting

Token budget enforcement uses two passes that leverage existing infrastructure in `internal/infrastructure/llm/token_extractor.go`:

**Pass 1 — Real-time per-chunk enforcement:**  
`EstimateTokens(chunk.text)` (existing `chars ÷ 4` heuristic) is applied to each chunk before forwarding. When `consumedTokens ≥ tokenLimit × 0.9`, the Substrate closes the stream. The 10% safety margin absorbs the ±15–20% estimation error inherent in the heuristic.

**Pass 2 — Post-stream exact reconciliation:**  
After the stream closes, the Substrate extracts the authoritative token count:
1. If the provider's final chunk includes a `usage` field (OpenAI, Anthropic), `TokenUsageExtractor.Extract(finalChunk)` is applied.
2. If no `usage` field is present (Ollama streaming may omit `eval_count`), fall back to `EstimateTokens(fullResponseText)`.

The result populates `SessionState.ActualTokensUsed` and `TaskEvent.TokenUsage`. The existing `ProfileAggregator` pipeline processes the `TaskEvent` unchanged.

If `ActualTokensUsed > TokenLimit`: set `TaskEvent.BudgetOverrun = true` and emit `budget_overrun=true` structured log. If the per-plan `BudgetOverrun` rate exceeds 1% of all `GenerateViaModelStream` calls over a 7-day rolling window, emit a `TOKENIZER_INACCURACY` telemetry event as a trigger for the future tokenizer ADR (see Future Work).

### 5. CONWIP gateway semaphore with jittered retry

A semaphore of size `LLMGatewayMaxConcurrency` (default 20) bounds concurrent in-flight `GenerateViaModelStream` stream initializations. `ErrGatewayOverloaded` is returned **only on stream open** — once a stream is established it proceeds to completion or budget exhaustion without interruption.

When the semaphore is full, the agent SDK retries with **full-jitter exponential backoff**: `backoff = rand(0, LLMGatewayRetryBackoffMs × 2^attempt)` for attempts 0–2. Maximum wait before escalation: `LLMGatewayRetryBackoffMs × 7` ms (sum of three jittered attempts). After exhaustion, the error enters the ADR-0010 tiered recovery chain.

### 6. Per-`TraitModel` health cache circuit breaker

The Substrate maintains an in-memory health map keyed by `TraitModel` agent ID, checked before routing each stream. States:

| State | Trigger | Duration |
|-------|---------|---------|
| `HEALTHY` | Successful chunk received | — |
| `UNHEALTHY` | Network error / HTTP 5xx / timeout | 30s |
| `RATE_LIMITED` | HTTP 429 | Until `Retry-After` (default 60s) |

Fallback order: `StepAllocation.Winner` → candidate 2 → candidate 3. All three degraded → `ErrModelUnavailable` → ADR-0010 tiered recovery.

The session token is **not** re-issued on fallback. `allocatedTraitModelID` records the initially allocated model. Actual model used is written to `TaskEvent`: `fallback_model_used=true`, `actual_model_id=<id>`.

### 7. Hard error on budget exhaustion + SPC alarm

When the stream closes due to budget exhaustion, the agent receives `ErrBudgetExhausted`. `DAGExecutor` follows the ADR-0010 tiered recovery chain. No new error paths are introduced.

**SPC alarm:** The `PLAN_BUDGET_INSUFFICIENT` signal is emitted via `SignalStream` → `Watcher` → Planner when **both** conditions hold:
- ≥ 2 steps in the current plan have triggered `ErrBudgetExhausted`, AND
- The exhaustion rate exceeds 5% of dispatched steps.

The signal is rate-limited to **once per plan** to prevent Planner spam. The Planner increases `Step.MaxEnergy` for the next replan.

### 8. Dual model selection signals

`AgentManifest` gains a `required_model_capabilities []string` field (hard floor). `Step.RecommendedModel` (ADR-0011) remains the soft signal. Auctioneer applies the capability filter first, then merit scoring.

### 9. Session token TTL + CircadianRhythm scavenger

A background scavenger runs as part of `CircadianRhythm` (reusing the existing tick loop). Every `SessionTokenSweepIntervalSeconds` (default 30s), it evicts `SessionState` entries where `now > ExpiresAt`. Since `ExpiresAt` is a keepalive TTL (refreshed on each chunk), only truly hung steps are evicted. `session_token_leak_count` is emitted as a structured log field on each sweep.

### 10. Agent-to-Orchestrator gRPC client (existing injection path)

Cognitive agents are separate processes launched by `AgentManager`. `instance_manager.go:128` already passes `--substrate-socket im.substrateAddr` to every agent at boot alongside `--socket`:

```go
cmd = exec.Command(im.pythonPath, def.ExecPath,
    "--socket", sockPath,
    "--substrate-socket", im.substrateAddr)
```

`GenerateViaModelStream` is on the `Orchestrator` service at that same `substrateAddr`. **No new injection is needed.** The agent SDK (Python gRPC stub) opens one `grpc.insecure_channel(substrate_addr)` at module load time and wraps it in a singleton `OrchestratorStub`. All `GenerateViaModelStream` calls on that agent reuse this connection. Connection overhead is paid once per agent lifetime; if the Substrate restarts, `AgentManager` evicts and reboots the agent as a unit — no reconnection logic required.

### 11. Closed-loop adaptive token sizing

`ProfileAggregator` accumulates a per-step-type histogram of `ActualTokensUsed / TokenLimit` ratios from `TaskEvent` data. Buckets: `[0, 0.5)`, `[0.5, 0.8)`, `[0.8, 1.0)`, `[1.0]` (exhausted). The Planner reads this histogram when tagging `Step.MaxEnergy`, subject to guardrails:

- **Minimum sample size:** No adjustment until a step type has ≥ 20 observations.
- **Damped update:** `newLimit = oldLimit × (1 − α) + targetLimit × α`, α = 0.2.
- **Adjustment cap:** Maximum change is ±20% per cycle (not −50% as a naïve halving would produce).
- **Bounds:** `MinStepEnergy ≤ newLimit ≤ MaxStepEnergy` (new `ExecutionConfig` fields).

For steps consistently in `[0, 0.5)`, target limit = `oldLimit × 0.8`; for steps in `[0.8, 1.0)`, target limit = `oldLimit × 1.25`.

**Considered Options:**

| Decision | Chosen | Rejected | Rationale |
|----------|--------|----------|-----------|
| When to allocate the TraitModel | Plan time (Auctioneer auto-pairing) | Runtime `acquire_cognitive_resource` syscall | Mid-execution allocation violates the Auction model's single-source-of-truth. Runtime allocation introduces non-determinism and replan complexity. |
| LLM call path | `GenerateViaModelStream` streaming metered proxy | Unary `GenerateViaModel` one-shot | Unary cannot enforce real-time token budgets — the gateway can only clamp `max_tokens` upfront, not interrupt an over-running response mid-generation. |
| Streaming integration | `AgentService.Execute` stays unary; new `Orchestrator.GenerateViaModelStream` RPC | Make `AgentService.Execute` streaming | Making Execute streaming restructures every cognitive agent. Isolating streaming to the LLM call path keeps the DAG execution model unchanged. |
| Token counting | Dual-pass hybrid: `EstimateTokens` per-chunk + `TokenUsageExtractor` reconciliation | Streaming tokenizer library (tiktoken-go) | `EstimateTokens` + `TokenUsageExtractor` already exist in `token_extractor.go`. Adding per-model tokenizer libraries multiplies dependency surface across every TraitModel provider. Deferred to future ADR triggered at 1% `budget_overrun` rate. |
| Pass 2 fallback (provider omits usage) | `EstimateTokens(fullResponseText)` | Reject the stream without reconciliation | Rejecting leaves `TaskEvent.TokenUsage` empty, breaking the ProfileAggregator pipeline. Estimation fallback maintains pipeline integrity with a known-approximate value. |
| Overload response | CONWIP semaphore + jittered retry (stream init only) | Bounded queue + timeout; no limit | Queuing stalls step goroutines silently. No limit is production-unsafe at scale. Jitter prevents thundering herd on retry; restricting `ErrGatewayOverloaded` to stream init keeps retry logic at one well-defined point. |
| Runtime model fallback | top-3 `StepAllocation` cached at plan time + health cache circuit breaker | Re-run Auctioneer sub-selection at fallback time | Re-running Auctioneer mid-execution is the runtime allocation pattern this ADR rejects. Plan-time top-K caching preserves the "all binding complete before execution" invariant. |
| Session token TTL semantics | Keepalive (refresh on each chunk) | Creation-time deadline | Creation-time TTL kills slow-but-legitimate deep-reasoning steps. Keepalive TTL correctly distinguishes active-but-slow from hung. |
| Session token carrier | Typed `Handoff.SessionToken *SessionToken` field | `_session_token` magic string in `Handoff.Context` | Context bag is `map[string]string` — a convention, not a type constraint. Any agent could accidentally overwrite a magic string key. A typed field is compiler-enforced and nil-checkable. |
| Agent Orchestrator client | Persistent singleton using existing `--substrate-socket` arg | Inject new endpoint; per-call dial | `buildAgentCmd` already injects `substrateAddr` as `--substrate-socket`. Per-call dial wastes a handshake per generation call. Singleton reuses the existing injection path — zero new infrastructure. |
| SPC alarm threshold | ≥ 2 exhausted steps AND > 5% rate, once per plan | Pure percentage threshold | Pure percentage fires on single-step plans (1 exhausted step = 100%). The floor of ≥ 2 steps prevents false alarms on small plans. |
| Histogram adjustment cap | ±20% per cycle, damped update α=0.2, min N≥20 | Immediate halving / doubling | Immediate large adjustments react to noise, not signal. Damped update with gain scheduling (α=0.2) and sample size guard prevents thrashing. |
| Model capability matching | `required_model_capabilities []string` (hard floor) | `minimum_cognitive_tier` enum | Enum requires code changes for new model classes. String array matches existing `SupportedFormats` pattern and is operator-configurable. |
| Async agent-to-Planner signalling | Existing ADR-0009 `SignalStream` + `Watcher` | New `StreamCognitiveSignal` / `syscall_think` | ADR-0009 already covers the full async signalling path. `priority_level` fields would violate the Zero-Hardcode Rule. |

**Consequences:**

- **Positive:** The Substrate becomes a genuine LLM resource controller. Token consumption is fully attributed, budget-enforced, and auditable at step granularity with per-chunk real-time precision.
- **Positive:** No new error recovery paths. `ErrBudgetExhausted`, `ErrGatewayOverloaded`, and `ErrModelUnavailable` all map to the existing ADR-0010 tiered recovery chain.
- **Positive:** Cognitive agents are simpler — they hold only a typed `SessionToken` field and call `GenerateViaModelStream` on the already-known `substrateAddr`. Zero new injection infrastructure.
- **Positive:** The CONWIP semaphore with jittered retry prevents the gateway from entering the M/M/1 instability region and eliminates thundering-herd retry storms simultaneously.
- **Positive:** Keepalive TTL correctly distinguishes hung steps from slow-but-legitimate deep-reasoning steps.
- **Positive:** Histogram guardrails (damped update, min N≥20, ±20% cap, floor/ceiling) prevent calibration thrashing on noisy or low-sample step types.
- **Negative:** `domain.Handoff` gains a new optional `SessionToken *SessionToken` field. Existing `Handoff` constructors must be updated; the field is nil-safe and backward-compatible.
- **Negative:** `AgentService.Execute` stays unary, but cognitive agents must open a `GenerateViaModelStream` connection mid-handler. One extra gRPC round-trip per step for agents that call the LLM.
- **Negative:** `CircadianRhythm` gains a second responsibility (session token scavenging). These must be clearly separated in implementation to avoid coupling.
- **Negative:** The dual-pass token counting is approximate for real-time enforcement. The 10% safety margin is designed to absorb this, but pathological chunks (very long tokens with few characters) could produce systematic underestimates on certain model families. The `TOKENIZER_INACCURACY` telemetry trigger bounds this risk.
- **Negative:** Histogram adaptive sizing improves across plan runs, not within a single plan (`ProfileAggregator` is asynchronous).

**Extends:**

- **ADR-0011** — Reuses `TraitModel`, `Step.MaxEnergy`, `Step.RecommendedModel`, `TokenUsage` in `TaskEvent`, `ProfileAggregator`, and `computeMeritScore`. `ProfileAggregator` extended with token utilisation histogram.
- **ADR-0010** — `ErrBudgetExhausted`, `ErrGatewayOverloaded`, and `ErrModelUnavailable` enter the existing tiered recovery chain.
- **ADR-0009** — `PLAN_BUDGET_INSUFFICIENT` signal routes through the existing `SignalStream` → `Watcher` → Planner path.

**Config additions to `ExecutionConfig`:**

```go
LLMGatewayMaxConcurrency        int     // default 20    — CONWIP semaphore size
LLMGatewayRetryBackoffMs         int     // default 100   — base backoff for jittered retry
SessionTokenSweepIntervalSeconds  int    // default 30    — CircadianRhythm scavenger tick
SessionTokenTTLMultiplier         float64 // default 5.0  — TTL = multiplier × estimatedStepDuration
BudgetExhaustionAlarmRate         float64 // default 0.05 — SPC alarm threshold (5%)
MinStepEnergy                     int     // default 256  — histogram adjustment floor (tokens)
MaxStepEnergy                     int     // default 32768 — histogram adjustment ceiling (tokens)
HistogramMinSamples               int     // default 20   — min observations before adjustment
HistogramAlpha                    float64 // default 0.2  — damped update gain
```

**Glossary additions:**

- **GenerateViaModelStream** — The single Substrate streaming RPC through which cognitive agents invoke an LLM. Substrate meters each chunk, closes stream on budget, routes fallback via health cache. Defined on the `Orchestrator` service.
- **GenerateStreamRequest** — Proto message carrying `(session_token_id, prompt, options)`.
- **GenerateChunk** — Proto message carrying one token group streamed from Substrate to agent, with `text` and `token_count` fields.
- **SessionState** — Server-side struct keyed by session token UUID, holding `StepAllocation`, `ConsumedTokens`, `ActualTokensUsed`, `ExpiresAt`, `LastActivityAt`.
- **StepAllocation** — Struct produced by Auctioneer at plan time: top-3 `TraitModel` candidates (winner + 2 fallbacks) for a given step.
- **Health Cache** — In-memory per-`TraitModel` map. States: `HEALTHY`, `RATE_LIMITED(until T)`, `UNHEALTHY(until T)`.
- **Dual-Pass Token Accounting** — Pass 1: `EstimateTokens` per-chunk at 90% of limit (real-time enforcement). Pass 2: `TokenUsageExtractor` on final chunk with `EstimateTokens(fullText)` fallback (exact reconciliation for `TaskEvent`).
- **BudgetOverrun** — `TaskEvent` flag set when `ActualTokensUsed > TokenLimit` after post-stream reconciliation.
- **TOKENIZER_INACCURACY** — Telemetry event emitted when `BudgetOverrun` rate exceeds 1% over a 7-day window. Triggers the future Streaming Tokenizer ADR.
- **Keepalive TTL** — Session token expiry policy: `ExpiresAt` is refreshed on each chunk received, so active steps never expire; only hung steps (silent for `TTL × estimatedDuration`) are scavenged.
- **ErrBudgetExhausted** — Stream closed because `consumedTokens ≥ tokenLimit × 0.9`. Enters ADR-0010 recovery.
- **ErrGatewayOverloaded** — Returned on stream init when CONWIP semaphore is full. Retried with jittered backoff; escalates to ADR-0010 after exhaustion.
- **ErrModelUnavailable** — All three `StepAllocation` candidates degraded per health cache. Enters ADR-0010 recovery.
- **PLAN_BUDGET_INSUFFICIENT** — Signal via `SignalStream` when ≥ 2 steps exhaust budget AND rate > 5%; once per plan; triggers Planner to increase `Step.MaxEnergy`.
- **TOKENIZER_INACCURACY** — Telemetry event when `BudgetOverrun` rate > 1% over 7 days; triggers future tokenizer ADR.
- **Token Utilisation Histogram** — Per-step-type histogram of `ActualTokensUsed / TokenLimit` maintained by `ProfileAggregator`. Feeds damped adaptive `Step.MaxEnergy` sizing in Planner.
- **Thalamic Gating** — The Substrate (thalamus) meters and mediates all LLM resource access.

**Rejected:**

- `StreamCognitiveSignal` / `syscall_think` / `priority_level` — ADR-0009 already covers async signalling. Priority levels violate Zero-Hardcode Rule.
- `acquire_cognitive_resource` runtime syscall — Mid-execution allocation. Plan-time `StepAllocation` is the correct locus.
- `minimum_cognitive_tier` enum — Replaced by `required_model_capabilities []string`.
- HTTP-layer interception proxy — Requires cancelled Wasm Layer 2.
- Unary `GenerateViaModel` — Cannot enforce real-time budgets mid-response.
- `_session_token` magic string in `Handoff.Context` — Replaced by typed `domain.Handoff.SessionToken`.
- Bounded call queue for overload — Silently stalls step goroutines.
- Re-running Auctioneer sub-selection at fallback — Mid-execution allocation.
- Creation-time TTL — Kills legitimate slow steps. Replaced by keepalive TTL.
- Fixed-backoff retry without jitter — Thundering herd on `ErrGatewayOverloaded`. Replaced by full-jitter exponential backoff.
- Immediate histogram halving/doubling — Reacts to noise. Replaced by damped update with min-sample guard.
- `ErrGatewayOverloaded` mid-stream — Impossible to resume a partial stream. Restricted to stream init only.

**Future Work:**

**Streaming Tokenizer ADR trigger condition:** If `BudgetOverrun = true` events exceed 1% of all `GenerateViaModelStream` calls over a 7-day rolling window, the dual-pass hybrid's estimation error is no longer economically acceptable. At that threshold, ADR-00XX (Streaming Tokenizer Abstraction) is triggered to implement per-model exact token counting. This threshold is the capital investment signal — the factory is not built until demand data justifies it.
