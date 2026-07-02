# ADR-0027: Semantic Plan Similarity with Adjustable Thresholds

## Status

Accepted

## Context

`REQ-CACHE-1` introduced an exact-match fast-path (`similarity >= 0.95 && confidence >= 0.90`). Between exact-match and miss lies the **semantic match zone** (`0.85–0.94`), where the Hippocampus retrieves a prior plan and injects it as a hint into the Planner prompt.

The current thresholds are hardcoded (`similarity=0.85`, `confidence=0.70`). This is suboptimal because:

- **Code generation plans** change frequently with requirements; a 0.85 threshold may return stale structural templates
- **FAQ-style cognitive queries** are highly repetitive; a 0.85 threshold may be too conservative, missing valid reuse
- **Tool-heavy plans** (file reads, data transforms) are deterministic; lower thresholds are safe

A single global threshold cannot serve all query types.

## Decision

Introduce a `cache_policy` field in `ExecutionPlan`, populated by the Planner via LLM classification. The actual numeric thresholds live in Koanf config. The Hippocampus looks up thresholds by policy name at retrieval time.

### Planner Emits Policy Name (Option B)

The Planner's output JSON gains a new optional field:

```json
{
  "steps": [...],
  "subject": "...",
  "cache_policy": "cognitive"
}
```

The Planner prompt includes the available policy names as hints (Option C):

```
Set cache_policy based on the dominant capability of the request:
- "codegen" — when the plan involves writing, generating, or refactoring code
- "cognitive" — when the plan involves analysis, summarisation, comparison, or reasoning
- "tool" — when the plan involves file reads, data transforms, or deterministic operations
- "research" — when the plan involves web search, paper reading, or information gathering
- "default" — when none of the above clearly applies
```

**Zero-Hardcode compliance:** The Go code does not contain keyword-based classification logic. The LLM selects the policy name. The Go code only validates that the selected name exists in config.

### Thresholds Live in Koanf Config (Option B)

```go
type HippocampusPolicy struct {
    SimilarityThreshold float64 // minimum raw cosine similarity for retrieval
    ConfidenceFloor     float64 // minimum stored mean auction confidence
    MaxAgeHours         int     // reject templates older than this
}

type ExecutionConfig struct {
    ...
    HippocampusPolicies map[string]HippocampusPolicy
    HippocampusDefaultPolicy string // fallback when cache_policy is empty or unknown
}
```

Example `configs/config.json`:

```json
{
  "hippocampus_policies": {
    "codegen":    {"similarity": 0.92, "confidence": 0.85, "max_age_hours": 24},
    "cognitive":  {"similarity": 0.85, "confidence": 0.70, "max_age_hours": 168},
    "tool":       {"similarity": 0.80, "confidence": 0.60, "max_age_hours": 720},
    "research":   {"similarity": 0.88, "confidence": 0.75, "max_age_hours": 72},
    "default":    {"similarity": 0.85, "confidence": 0.70, "max_age_hours": 168}
  },
  "hippocampus_default_policy": "default"
}
```

### Hippocampus Policy Lookup

The `Hippocampus` gains a `PolicyProvider` dependency:

```go
type PolicyProvider interface {
    GetPolicy(name string) (HippocampusPolicy, bool)
    DefaultPolicy() HippocampusPolicy
}
```

At retrieval time:

```go
func (h *Hippocampus) RetrieveWithPolicy(ctx context.Context, userInput string, policyName string) (*domain.ExecutionPlan, float64, float64, error) {
    policy, ok := h.policyProvider.GetPolicy(policyName)
    if !ok {
        policy = h.policyProvider.DefaultPolicy()
    }
    // Use policy.SimilarityThreshold and policy.ConfidenceFloor instead of hardcoded values
    ...
}
```

The existing `Retrieve` method (without policy) falls back to the default policy, preserving backward compatibility.

### MaxAgeHours Enforcement

The `Hippocampus.RetrieveWithPolicy` method also checks `MaxAgeHours`:

```go
storedAt, _ := results[0].Document.Metadata["stored_at"].(string)
if t, err := time.Parse(time.RFC3339, storedAt); err == nil {
    if time.Since(t) > time.Duration(policy.MaxAgeHours) * time.Hour {
        return nil, 0, 0, nil // too old, treat as miss
    }
}
```

This prevents the retrieval of stale procedural templates (e.g., a code-generation plan from 6 months ago when the tech stack has changed).

## Consequences

### Positive

- Code-generation queries get a stricter threshold (0.92), reducing stale-template pollution
- FAQ-style cognitive queries get the standard threshold (0.85), preserving high-recall reuse
- Tool plans get a lower threshold (0.80), maximizing reuse of deterministic operations
- `MaxAgeHours` automatically retires old templates without manual cleanup
- Threshold changes are operator-configurable via Koanf; no code deployment needed

### Negative

- Planner prompt grows slightly (5 policy names)
- `ExecutionPlan` schema gains one optional field; existing tests/plans unaffected
- Policy name validation adds one error path (unknown policy → fallback)

### Deferred Work

- Policy effectiveness telemetry: record `policy_name` in `PlanEvent` to measure hit/miss rates per policy
- Auto-tuning: adjust thresholds based on observed success/failure rates per policy
- Policy-specific template generalization (ADR-0027 active learning)
