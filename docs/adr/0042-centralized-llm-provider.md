# ADR-0042: Centralized LLM Provider (Health-Guarded Model Brokering)

**Status:** Proposed (2026-06-07) — design recorded; not implemented. Big-bang replacement of the flat `models[]` registry + duplicated top-level `llm` config block + hardcoded `PrimaryGenerator()`, gated on the test suite below.
**Date:** 2026-06-07
**Author:** Afsin
**Depends on:** ADR-0024 (koanf config engine — the layered loader this new schema rides on), ADR-0031 (Universal Input Router — Layer 3 classification, the organ whose silent failure motivated this), ADR-0037 (Central-Executive Planner — **D16** `ModelSelector` cost-first EFE, the *preference* authority this finally wires).
**Supersedes (in part):** ADR-0011 (cost-aware routing) and ADR-0018 (Managed Cognitive Resource Allocation — gateway model sub-selection): both folded into the Provider's failover ladder and price ledger; their *selection* semantics are delegated to ADR-0037, their *availability* semantics absorbed here.
**Relates to:** ADR-0019 (Langfuse generator wrappers — the decorator pattern the instrumented generator reuses).
**Theory basis:** Separation of mechanism from policy (the Provider owns *mechanism* — which endpoints are reachable; the inference layer owns *policy* — which model is preferred); the Circuit Breaker / Bulkhead stability patterns (Nygard, *Release It!*) for graceful degradation under partial outage; active inference as the preference oracle (FEP — models are a learned population, ADR-0037) kept strictly separate from deterministic safety-path failover.

---

## Context

Cambrian routes LLM calls through two unrelated, half-wired paths, and a third that is hardcoded:

1. **System cognition** (router Layer 3, memory ops, supervision, interview grading) all run on a single generator resolved by `ProviderRegistry.PrimaryGenerator()`, which is literally `if r.Ollama != nil { return r.Ollama }` — **the primary is hardcoded to Ollama**, undeclarable in config.
2. **Agent task steps** *may* carry a `RecommendedModel` string dispatched at `server.go:724` via `parseModelRef` — but `parseModelRef` does `SplitN(ref, ":", 2)`, so `"llm:openai:gpt-4o"` parses to provider `"llm"` (unknown) and **silently falls back to the primary**. The registry keys clients by *provider*, not model, so two OpenAI-compatible models collapse to one slot ("last wins").
3. **Cost** is read from `cfg.Models[0]` (`metabolism_stack.go:60`), the embedder's `dimensions` from a *different* block (`cfg.LLM.Dimensions`), and the interview-session model ID is string-built as `"llm:ollama:"+cfg.LLM.Model` (`main.go:443`). Three couplings, three places to drift.

### The motivating failure

A live chat returned `router: router: Layer 3 invalid JSON response: unexpected end of JSON input`, with **no logs**. Root cause: `OllamaClient.Generate` (unlike `OpenAIClient.Generate`) does **not** check `resp.StatusCode` and unmarshals any body into `{Response string}`. An Ollama error body (`{"error":"model not found"}`, HTTP 404) unmarshals cleanly to an empty `Response`, so the client returns `("", nil)` — a **silent empty success**. Three layers up, `json.Unmarshal("")` fails with the cryptic message, and the real error is gone.

This is not one bug. It is the symptom of having **no component that owns endpoint health**. A swallowed empty response is a health signal the system throws away because nothing is listening.

### Architectural framing

The kernel already decided, in ADR-0037, that *which model serves an agent task* is **learned, not declared** — "models are just another population," region-resolved quality + cost via EFE, and the Zero-Hardcode Rule (CONTEXT.md) forbids agent-to-task routing in Go `if/else`. What is missing is the dual concern: *which endpoints are actually reachable right now*, and *who hands out a generator when the preferred one is down*. That is a **mechanism** concern (availability), categorically distinct from the **policy** concern (preference). Conflating them is what produced both the hardcoded primary and the silent failure.

---

## Considered Options

- **A — Status quo: flat `models[]` + hardcoded `PrimaryGenerator`.** Leaves all three couplings and the silent-failure class. Rejected.
- **B — Config rename only: `embedder{}` + `generators[]` + a `primary:true` flag.** Cleans the schema and lets the primary be any provider, but the registry still keys by provider (N models still collapse), the dispatch is still broken, and **nothing owns health** — the motivating bug survives. Rejected as cosmetic.
- **C — Provider as the model *decider* for everything (deterministic Go logic picks the agent-step model).** Centralizes cleanly, but moves agent-to-model routing into deterministic code — a direct **Zero-Hardcode Rule violation** that orphans ADR-0037's EFE selector. Rejected.
- **D — Centralized LLM Provider as a health-guarded *broker*: it owns availability/health/price and provisioning; the EFE/auction layer remains the preference authority; deterministic config governs only *system roles* (not agents).** Chosen.

---

## Decision

Introduce a single domain port, **`LLMProvider`**, that is the sole authority on LLM *availability and provisioning*. Every organ acquires a generator through it; no organ constructs or names a model client directly. The Provider owns endpoint health (circuit-breaker), the price ledger, and the failover ladder. It **delegates preference** to ADR-0037's `ModelSelector` (agent steps) and to deterministic role config (system organs). This is a **big-bang replacement** of the `models[]` array, the top-level `llm` block, and `PrimaryGenerator()`.

### D1 — Mechanism/policy split: Provider owns availability, EFE owns preference

The Zero-Hardcode Rule is preserved by splitting authority along the existing seam (CONTEXT.md:37 exception for "safety and latency"):

- **Preference** (which model is *best* for an agent task) stays in the **inference layer** — ADR-0037 `ModelSelector` / auction, finally wired as the agent-step preference source. A step's `SuggestedModelID` is a **prior**, never a command.
- **Availability** (which model is *reachable* and the deterministic failover when it is not) is the Provider's **mechanism** — legitimately deterministic, the same category as the Reflexive Path (Omurilik) exception.

Deterministic `role → model` config for **system organs** (planner, verifier, interview, router, memory) is Zero-Hardcode-legal because roles are *not* agents bidding for tasks; this is infrastructure wiring, not agent-to-task routing.

### D2 — id-keyed identity, end to end

Generators are identified by a stable `id` (e.g. `qwen-local`, `deepseek`), not by `provider`. The id is the single identity used by: the client registry (`map[id]→client`, N clients not one-per-provider), the TraitModel auction agent (`llm:<id>`), the belief-store `ResourceID`, and the price-ledger key. This is what lets two OpenAI-compatible endpoints coexist — the defect B could not fix.

### D3 — The contract: `Acquire` returns an instrumented Generator

Organs call:

```
gen, err := provider.Acquire(ctx, domain.LLMRequest{
    Purpose:          domain.PurposeRouter,   // or Planner/Verifier/Interview/Memory/AgentStep
    CapabilityHints:  []string{...},          // optional
    SuggestedModelID: "deepseek",             // optional prior (agent steps)
})
out, err := gen.Generate(ctx, prompt)
```

The returned `domain.Generator` is **already resolved to a healthy model and wrapped in a health/cost-recording decorator** (the ADR-0019 Langfuse-wrapper pattern). Every `Generate` transparently feeds outcome (success / latency / failure / empty-response) into the Provider's health + price ledger. **Organs stay blind to model identity and health** — preserving "agents stay blind to the model population" (model_selector.go:14). No `Report()` boilerplate at call sites.

### D4 — Passive circuit-breaker health (the silent-failure fix, generalized)

Health is inferred **only from real traffic the Provider already routes** (no background probing in v1). A call is a **failure** iff: transport error, HTTP ≠ 200, timeout, **or empty/unparseable response**. The last clause is the motivating bug promoted to a first-class signal.

Per-id state machine:

```
healthy --(≥ failure_threshold consecutive failures)--> OPEN
OPEN    --(cooldown_ms elapsed)-------------------------> HALF-OPEN
HALF-OPEN --(probe call ok)--> healthy   |   --(fail)--> OPEN
```

Mechanically this *also* requires fixing `OllamaClient` to check `resp.StatusCode` and reject an empty `response` (mirroring `OpenAIClient`), so the decorator sees a real error instead of `("", nil)`.

### D5 — The failover ladder, terminating in a hard error

When the resolved model's circuit is OPEN, `Acquire` walks one unified ladder:

1. the **suggested** model (if its circuit is closed);
2. the **purpose preference** — EFE-ranked candidates for an agent step, the configured `role → id` for a system role;
3. the global **`default`** generator;
4. **any** healthy generator satisfying `CapabilityHints`;
5. none healthy → a **clear, logged error**.

The Provider **never returns a nil/empty generator silently** — the exact anti-pattern that caused the motivating incident. Roles use a *simple `role → id` map*; there is one failover policy (the ladder), not a per-role second concept.

### D6 — Single price *ledger* feeds EFE cost, metabolism, and future cost optimization

The Provider owns a per-id **price ledger** that is the **single source of truth** for cost. It feeds (a) ADR-0037 `ModelSelector.Costs` (the EFE cost term), (b) metabolism token accounting (replacing the `cfg.Models[0]` default), and the default-cost concept resolves to the `default` generator's price.

Crucially, the config `cost_per_1m_*` fields are a **seed**, not the live value: the ledger is Provider-owned and **mutable**, so a later mechanism may update prices dynamically (a pricing feed, or observed per-call cost) without a config edit. Consumers read the **ledger via a read port**, never the raw config — so the seam is stable when prices stop being static.

This is the deliberate substrate for a **future cost/performance optimizer**, which is explicitly *not* in this ADR. That optimizer is a **preference-layer** concern (an extension of ADR-0037, its own future ADR), because choosing a model by cost-vs-quality is a *preference* judgment, not an *availability* one (D1). It will consume exactly two things this ADR guarantees: the price **ledger** (above) and the **learned, region-resolved quality beliefs** (ADR-0037). Correspondingly, cost is *declared-then-tracked* (config seed → live ledger) while quality is *always learned* — there is no config `tier`/`quality` field, keeping cold-start honest and Zero-Hardcode intact. No optimization *policy* (budgets, SLA-driven downgrading, price feeds) enters config or the Provider now.

### D7 — Config schema: standalone embedder + `llm_provider` block

The top-level `llm` block and `models[]` array are **removed**. New shape:

```jsonc
{
  "embedder": {                       // exactly one; standalone, not brokered/failed-over in v1
    "provider": "ollama", "model": "nomic-embed-text",
    "endpoint": "http://localhost:11434", "dimensions": 768, "timeout_ms": 10000
  },
  "llm_provider": {
    "default": "qwen-local",          // global default generator (ladder step 3 + interview base + default cost)
    "generators": [
      { "id": "qwen-local", "provider": "ollama", "model": "qwen3:8b",
        "endpoint": "http://localhost:11434", "timeout_ms": 180000,
        "cost_per_1m_input": 0.0, "cost_per_1m_output": 0.0, "capabilities": ["reasoning"] },
      { "id": "deepseek", "provider": "openai", "model": "deepseek-v4-flash",
        "endpoint": "https://opencode.ai/zen/go/v1", "api_key_env": "OPENCODE_API_KEY",
        "cost_per_1m_input": 0.0015, "cost_per_1m_output": 0.002, "capabilities": ["reasoning","code"] }
    ],
    "roles": { "planner": "deepseek", "verifier": "qwen-local",
               "interview": "qwen-local", "router": "qwen-local", "memory": "qwen-local" },
    "health": { "failure_threshold": 3, "cooldown_ms": 30000 }
  }
}
```

Validation (at load): generator `id`s unique; `default` ∈ ids; every `roles` value ∈ ids; non-ollama generator ⇒ `api_key_env` set; embedder present. API keys stay in `.env` / OS env via `api_key_env` (never literal in JSON).

---

## Consequences

**Positive**

- The motivating silent-failure class is eliminated by construction: empty/error responses become health failures with real logs and deterministic failover, instead of a cryptic JSON error three layers away.
- The primary/default generator is **declarable** and may be any provider (local *or* cloud) — the hardcoded `if r.Ollama != nil` dies.
- N generators per provider finally coexist (id-keyed); a step can suggest a specific model and actually get it.
- One price ledger; the three drifting cost/dimension/interview couplings collapse to one source.
- ADR-0037's EFE selector, dormant since it was written, becomes the live agent-step preference oracle — without violating Zero-Hardcode, because preference and availability are cleanly split.

**Negative / risks**

- Big-bang: the build is red until the whole Provider lands, and every `cfg.LLM`/`cfg.Models` reference (config tests, benchmarks, ltm_integration, metabolism_stack) changes at once. Mitigated by the test suite below.
- A mis-tuned circuit-breaker could trip on a transient blip and fail over unnecessarily (mitigated by `failure_threshold` + half-open recovery, both config-tunable).
- No active probing means an endpoint that recovers with zero traffic is only re-tested on the next half-open call (accepted for v1; active probing is a documented follow-up).
- The embedder is single and **not** failed over — if it is down, indexing is down (accepted; embedder brokering is out of scope).

---

## Testing decisions

Tests assert *external behavior*, not breaker internals.

- **Config (D7):** valid schema loads; duplicate id, `default` ∉ ids, role value ∉ ids, non-ollama without `api_key_env`, and missing embedder each produce a `ConfigError`.
- **id-keyed registry (D2):** two `openai`-provider generators with distinct ids both resolve to distinct clients (the "last wins" regression guard).
- **Silent-failure fix (D4):** `OllamaClient` returns an **error** (not `("", nil)`) on HTTP ≠ 200 and on an empty `response`; the decorator records it as a health failure.
- **Circuit-breaker (D4):** `failure_threshold` consecutive failures trip OPEN; calls during OPEN do not hit the dead endpoint; after `cooldown_ms` a half-open probe restores `healthy` on success and re-opens on failure.
- **Failover ladder (D5):** suggested-but-OPEN → next rung; all role/EFE/default OPEN → capability-matched healthy; none healthy → a non-nil **error** (never a nil generator). Each rung covered.
- **Blindness (D3):** an organ calling `Acquire(...).Generate(...)` never observes a model id or health; health/cost are recorded without a call-site `Report`.
- **Cost ledger (D6):** a generation updates metabolism token cost from the resolved generator's price, and the EFE `Costs` map reads the same table.

---

## Falsification

The Provider is accepted when, on the local suite: (1) the config + breaker + failover unit tests above are green; (2) a chat that previously produced `Layer 3 invalid JSON response: unexpected end of JSON input` now either succeeds via failover or returns an **explicit, logged** model-unavailable error; (3) marking the primary's endpoint unreachable causes system cognition to fail over to a healthy generator rather than erroring. Until then the status remains **Proposed**, mirroring the ADR-0037/0041 acceptance discipline.

---

## Out of scope

- Active background health probing (v1 is passive-only).
- Embedder brokering / multi-embedder failover (single embedder stays standalone).
- Latency- or load-based routing within the *healthy* set (preference stays with EFE; the Provider only gates on health).
- **Cost/performance optimization policy** (budgets, SLA-driven model downgrading, dynamic price feeds) — a future *preference-layer* mechanism extending ADR-0037. This ADR only guarantees the **price ledger** (D6) and the **learned quality beliefs** it will consume; it adds no optimization knobs to config or the Provider.
- Per-role fallback lists (one global ladder, by D5).
- Streaming-path health accounting beyond the non-streaming `Generate` contract (revisit when the streaming clients route through `Acquire`).
