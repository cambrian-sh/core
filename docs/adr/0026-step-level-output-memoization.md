# ADR-0026: Step-Level Output Memoization

## Status

Accepted

## Context

`REQ-CACHE-1` (plan-level exact-match fast-path) eliminates redundant LLM planning. However, within a single plan or across different plans, identical **steps** are still re-executed. For example:

- "Step 3: Summarise the Q3 revenue report" appears in three different financial-analysis plans
- "Step 0: Read `data/users.csv`" is a deterministic file-read that never changes

Re-executing these steps wastes LLM tokens, agent boot latency, and Auctioneer bidding cycles.

The `DAGExecutor` already orchestrates step dispatch but has no mechanism to skip execution when an identical step was recently completed.

## Decision

Introduce a `StepCache` domain interface, injected into `DAGExecutor`. Before dispatching a step to `stepFn`, the executor computes a deterministic cache key and queries the cache. On hit, it returns the cached `Handoff` directly.

### Integration Point: Inside DAGExecutor (Option A)

The cache check lives in `DAGExecutor.executeStep`, after the `Handoff` is constructed but before `stepFn` is called. This keeps the optimization at the orchestration layer (application core), not buried inside infrastructure adapters.

```go
type StepCache interface {
    Get(ctx context.Context, key string) (*domain.Handoff, bool, error)
    Put(ctx context.Context, key string, handoff *domain.Handoff, ttl time.Duration) error
}
```

### Cache Key: Hybrid — Plan + Step + DependsOn Output Hashes (Option C)

The key must be deterministic and bounded in size.

- **Dependent steps:** `SHA-256(plan.Subject + step.Query + hashOfDependsOnOutputs)`
  - Only includes direct dependency outputs, not the full accumulated context
  - Bounded size, changes when direct inputs change

- **Root steps (DependsOn == nil):** Same formula, but salted with the plan's `executionID` (random 8-byte hex) to prevent collision across different plan invocations.

This ensures "Step 0: Sort array" in Plan A doesn't return the result from "Step 0: Sort array" in Plan B, while "Step 2: Summarise step 1's output" correctly reuses the cache when step 1's output is identical.

### Backend: BBolt (Option B)

A new bucket `step_cache` in the existing BBolt DB stores key → marshaled `Handoff`. BBolt is fast, local, and already wired into the system. No new infrastructure (pgvector, Redis) is introduced.

Future: an in-memory LRU hot layer can wrap the BBolt cache for sub-millisecond lookups.

### TTL Policy: Planner Hint + Heuristic Fallback (Option C)

`domain.Step` gains `CacheTTLSeconds int`:

```go
type Step struct {
    ...
    CacheTTLSeconds int `json:"cache_ttl_seconds,omitempty"`
}
```

- Planner sets `CacheTTLSeconds` when it knows the step type (e.g., "write code" → 0)
- `DAGExecutor` falls back to heuristic if the field is zero:
  - `IsThought=true` → 1h
  - Query contains code-generation keywords → 0 (never cache)
  - `RecommendedModel != ""` → 24h
  - Default → 7d

This avoids a schema-migration burden (no enum field) while allowing explicit control.

### Invalidation: TTL-Only for MVP (Option A), Event-Driven Deferred

Cache entries expire naturally after TTL. No event-driven invalidation in the initial implementation. A deferred TODO marks the hook point for future `ExternalDocumentIngested` event listening.

## Consequences

### Positive

- Identical steps across plans complete in ≤40% of baseline time after warmup (benchmark target)
- No new infrastructure — BBolt bucket reuse
- Cache misses add <1ms overhead (BBolt lookup in local file)

### Negative

- Stale data bounded by TTL; no proactive invalidation until event-driven hook is implemented
- Root steps with identical queries but different implicit contexts (e.g., different sessions) still collide unless the executionID salt is active
- BBolt write load increases slightly; no risk at current scale but worth monitoring under 1000+ step/minute load

### Deferred Work

- In-memory LRU hot layer (sub-millisecond cache hits)
- Event-driven invalidation on `ExternalDocumentIngested`
- Cross-instance cache sharing (requires distributed backend like Redis)
