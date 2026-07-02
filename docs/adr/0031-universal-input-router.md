# ADR-0031: Universal Input Router

**Status:** Implemented  
**Date:** 2026-06-01  
**Author:** Afsin
**Replaces:** Fragmented entry points (`ChatStream`, `Execute` unary RPC, `SignalStream`, `DirectoryWatcher`)

---

## Context

Cambrian currently has four disjoint entry points, each with its own routing logic (or none):

| Entry Point | Input Type | Routing | Problem |
|-------------|-----------|---------|---------|
| `ChatStream` | gRPC bidirectional | None — directly calls `Server.Execute` | TUI deleted; **Router IS the replacement** |
| `Execute` RPC | Unary gRPC | None — directly calls `Server.Execute` | No classification of intent |
| `SignalStream` | gRPC bidirectional | Old Watcher validates, enriches, presents to Planner | Watcher is passive; replaced by ReactiveEngine (ADR-0032) |
| `DirectoryWatcher` | Filesystem | Hardcoded: "file → ingest" | No support for "file → plan" or "file → watch" |

The result is a system that cannot reason about inputs holistically. A user who says "watch gold prices" hits `Execute` and gets a plan, not a persistent reactive rule. A user who drops a `.py` file into `data/inbox/` gets ingestion even if they configured a code-review watch rule.

## Decision

**A single `InputRouter` component is the universal entry point for all external input.**

The Router classifies every input into one of five decisions:

| Decision | Meaning | Routed To |
|----------|---------|-----------|
| **CHAT** | Answer a question from LTM. No plan, no daemon. | `ConversationEngine` (REQ-CHATBOT-001) — stub until implemented |
| **PLAN** | Build a multi-step `ExecutionPlan` and run it via `DAGExecutor`. | `Planner` → `DAGExecutor` |
| **INGEST** | Feed external knowledge into the Company Brain. | `Planner` → `DAGExecutor` (ingestion plan) |
| **WATCH** | Register a reactive rule. Spawns a daemon if needed. | `ReactiveEngine` (ADR-0032) — stub until implemented |
| **CLARIFICATION** | Confidence too low to act. Ask the user to choose. | Returns structured question + options to caller |

### Hexagonal Placement

The Router is a **domain component**, not an HTTP server. It follows the same pattern as `Planner` and `WorkspaceStage`:

- `domain.InputRouter` interface lives in `internal/domain/`
- `DefaultRouter` implementation lives in `internal/router/`
- `Server.Router domain.InputRouter` is a field wired by `kernel/provider.go`
- `Server.Execute` is a **thin adapter**: translates `pb.Handoff` → `RouterInput`, calls `s.Router.Resolve(ctx, routerInput)`, branches on the returned `RouterDecision`

The Router has zero gRPC imports. When the Company Gateway arrives, it creates its own `RouterInput` and calls the same `domain.InputRouter` — no duplication.

### CHAT and WATCH Stubs

`ConversationEngine` (ADR-0034) and `ReactiveEngine` (ADR-0032) do not exist yet. Until they do, `Server.Execute` returns a `pb.Handoff` with `payload.type = "not_implemented"` and `payload.data = classified decision` for CHAT and WATCH decisions. Classification is observable and testable before the handlers exist.

### Four-Layer Resolution

The Router resolves intent via four layers, ordered by cost:

#### Layer 0 — Gateway Pre-Classification (free)

`RouterInput.Intent` may be pre-populated by the Company Gateway when context makes the decision obvious (e.g., "this channel is always CHAT"). If `Intent` is a valid `DecisionType` constant, the Router returns that decision immediately without running Layers 1–3. Empty or unknown `Intent` falls through to Layer 1.

The Gateway is a trusted caller. This is not a security bypass — it is a latency optimisation for known contexts.

#### Layer 1 — Slash-Prefix Commands (free)

Unambiguous explicit markers at the start of `RouterInput.Body` (case-insensitive):

| Input prefix | Decision |
|-------------|----------|
| `/watch ...` | WATCH |
| `/plan ...` | PLAN |
| `/ingest ...` | INGEST |

Slash commands are structurally unambiguous — no user naturally types `/watch` unless they mean "create a watch rule." No marker → proceed to Layer 2.

#### Layer 2 — Heuristic Classification (free)

Word-boundary regex matching, **precompiled at startup**. Only high-confidence single-word triggers are included.

| Keywords (whole-word, case-insensitive) | Decision |
|----------------------------------------|----------|
| `watch`, `monitor`, `track`, `alert` | WATCH |
| `plan`, `execute`, `run` | PLAN |
| `ingest`, `import`, `upload` | INGEST |

**Multi-match rule:** If the input matches keywords from more than one category, Layer 2 produces no decision and falls through to Layer 3. Layer 3 (LLM) is the canonical authority for ambiguous inputs.

**Removed from Layer 2:** `"do"` (too common in natural language), `"drop"` (too common as a verb). Both would produce unacceptable false-positive rates.

No match → proceed to Layer 3.

#### Layer 3 — LLM Classification (~3s, ~250 tokens)

`DefaultRouter` receives `domain.Generator` as a **constructor dependency**. The composition root decides which generator to wire (initially `awarenessGen`; switchable to a dedicated `"router"` Langfuse subsystem in one line).

**Body truncation:** Only the first `RouterClassificationBodyChars` characters of `Body` are passed to the classification prompt (default 500). The intent signal is almost always in the first sentence.

**Prompt:** PROMPTREQ-compliant `PromptBuild` call, registered as `"router.classify"` in `PromptRegistry` at `init()` time. Empty `<Context>` block is intentional — the Router has no LTM enrichment to inject.

**Output schema:**

```json
{
  "decision": "chat|plan|ingest|watch",
  "confidence": 0.0,
  "alternatives": [
    {"decision": "chat|plan|ingest|watch", "confidence": 0.0}
  ],
  "reason": "string"
}
```

**Confidence gate:** If `confidence < RouterMinClassificationConfidence` (default 0.5), the Router returns `DecisionClarification` instead of acting on a weak classification. Generator failure or parse failure is a **hard error** — not a silent fallback.

**`default_decision_on_timeout` is removed entirely.** Silent degradation is replaced by explicit clarification.

### DecisionClarification — The Fifth Decision Type

When Layer 3 confidence is below the threshold, the Router asks the user to choose rather than acting on an uncertain classification.

```go
type ClarificationOption struct {
    Label       string       // "Build a multi-step execution plan"
    Decision    DecisionType // the decision if this option is chosen
    Recommended bool         // true for the highest-confidence Layer 3 candidate
}

type RouterDecision struct {
    Type                  DecisionType
    ClarificationQuestion string
    ClarificationOptions  []ClarificationOption // non-empty when Type == DecisionClarification
    ChatParams            *ChatParams
    PlanParams            *PlanParams
    IngestParams          *IngestParams
    WatchParams           *WatchParams
}
```

`ClarificationOptions` is populated from Layer 3's `alternatives` field: the top candidate becomes `Recommended: true`; alternatives with confidence >0.1 become additional options.

`Server.Execute` serializes `DecisionClarification` as:

```
payload.type = "clarification"
payload.data = JSON{
    "question": "I'm not sure what you'd like me to do.",
    "options": [
        {"label": "Build a multi-step execution plan", "decision": "plan", "recommended": true},
        {"label": "Answer directly from memory",        "decision": "chat", "recommended": false}
    ]
}
```

This follows the existing `payload.type` sentinel pattern (`"budget_signal"`, `"code"`, etc.). The response is not an error — it is a valid first-class response. The user's selection arrives as a new `Execute` call and hits Layer 1 (`/plan`) or Layer 2 (`plan`) with high confidence.

### Session Management

Session creation is **branch-local**, not a precondition of routing:

| Decision | Session created? | Reason |
|----------|-----------------|--------|
| PLAN | Yes | DAGExecutor scopes checkpoints to `sessionID` |
| INGEST | Yes | Checkpoint recovery for multi-file ingestion plans |
| CHAT | No | Managed by `ConversationEngine` when implemented |
| WATCH | No | Rule registration is sessionless |
| CLARIFICATION | No | A question, not an execution |

Mood context injection (`injectMoodContext`) is **PLAN-only**. The Router classifies raw `payload.Data`. Enrichment happens inside the branch that needs it.

### INGEST Execution

INGEST always routes through the **Planner → DAGExecutor**, not directly to `IngestionManager.Enqueue`. `buildIngestPrompt(routerInput)` constructs a source-aware prompt describing the source type (text body, URL, directory path, binary attachment with MIME type). The Planner generates the appropriate ingestion steps.

**Why not direct enqueue:** The Hippocampus fast-path makes this as fast as a hardcoded shortcut after the first execution — without a Zero-Hardcode violation. The first URL ingestion generates a plan; subsequent URL ingestions hit the Hippocampus template at similarity ≥ 0.85 with no LLM call. `PlanTemplateGeneralizer` (ADR-0030) promotes frequent ingestion patterns to generalised templates automatically.

**Default tag:** `["ingested_from_chat"]`. The Company Gateway overrides this via `RouterInput.Metadata["_ingest_tags"]`.

### DirectoryWatcher Refactor

`DirectoryWatcher` no longer calls `Router.Resolve` for per-file events. It sends a `Signal` directly to `ReactiveEngine`:

```
Signal{
    StreamID: "data/inbox/",
    Payload:  {path, extension, size, mime_type},
}
```

`ReactiveEngine` evaluates WatchConfig conditions for the matching `StreamID` and executes the configured action.

**Default catch-all WatchConfig** registered at startup:

```
WatchConfig{
    Source:        {Type: "filesystem", StreamID: "data/inbox/"},
    Condition:     "true",
    ConditionType: "deterministic",
    Action:        {Type: "ingest"},
}
```

This preserves the existing "ingest everything dropped into `data/inbox/`" behaviour. Operators override it with specific rules (e.g., `.py` → code review) via natural language through the Router's WATCH decision path.

**User-facing rule configuration:** Users create WATCH rules by typing a natural language request. The Router classifies it as WATCH and `ReactiveEngine.RegisterWatch` persists the `WatchConfig` to BBolt.

### Configuration

Two new fields in `ExecutionConfig` / `DefaultConfig()`:

```go
RouterMinClassificationConfidence float64 `json:"router_min_classification_confidence,omitempty"`
RouterClassificationBodyChars     int     `json:"router_classification_body_chars,omitempty"`
```

```go
RouterMinClassificationConfidence: 0.5,
RouterClassificationBodyChars:     500,
```

Env overrides: `CAMBRIAN_EXECUTION__ROUTER_MIN_CLASSIFICATION_CONFIDENCE`, `CAMBRIAN_EXECUTION__ROUTER_CLASSIFICATION_BODY_CHARS`.

### Zero-Hardcode Exception for Layers 1 and 2

Both layers use deterministic string matching rather than LLM classification. Both are accepted as **latency-optimisation exceptions**, not routing-decision exceptions. The precedent is ADR-0001's TraitTool static bidder.

The test: if Layers 1 and 2 are removed entirely, correctness is preserved — Layer 3 (LLM) produces the same decisions. Only latency and cost change. This is the correct framing for a performance exception.

## Consequences

### Good
- **Unified reasoning:** Every input — gRPC message, filesystem event, webhook — goes through the same decision pipeline.
- **Hexagonally correct:** `domain.InputRouter` is a pure domain interface. Adapters (gRPC, HTTP, filesystem) call it; they do not embed routing logic.
- **Extensible:** Adding a sixth decision type requires only a new `DecisionType` constant and a `*Params` struct.
- **Explicit clarification over silent fallback:** Low-confidence inputs surface as structured choices, not misclassified silently.
- **Hippocampus eliminates the INGEST fast-path problem:** No hardcoded direct-enqueue path needed; repeated ingestion patterns are automatically templated.
- **Testable:** Layers 0–2 are pure string logic with no dependencies. Layer 3 uses `FakeGenerator` from `internal/testing/harness/`. `PromptRegistry` existence test for `"router.classify"` at `init()`.

### Bad
- **Layers 1 and 2 are conditionals.** They violate the strict reading of the Zero-Hardcode Rule. The exception is documented and must be periodically revisited.
- **First INGEST request pays full Planner cost.** Subsequent requests hit the Hippocampus fast-path. This is a one-time cost, not a per-request cost.
- **Layer 3 adds ~3s to ambiguous inputs.** Layer 2 mitigates this for high-confidence keywords.

### Neutral
- **CHATBOT-001 is blocked on this ADR.** REQ-CHATBOT-001's entire CHAT path depends on a working `InputRouter`. CHATBOT-001 cannot start until ADR-0031 Phase 1–2 is implemented.
- **CHAT and WATCH are stubs initially.** The Router classifies correctly from day one; the handlers fill in as ADR-0032 and ADR-0034 ship.

## Related

- REQ-ROUTER-002 (full requirement document)
- REQ-CHATBOT-001 (depends on Router CHAT classification)
- ADR-0032 (ReactiveEngine — the WATCH execution layer)
- ADR-0033 (Daemon Agent Architecture — the WATCH spawning layer)
- ADR-0034 (Tag-Based Isolation — ConversationEngine and ScopeConfig)
- ADR-0001 (Trait classification — precedent for Zero-Hardcode exception)
- ADR-0024 (Koanf config — `DefaultConfig()` pattern for new config fields)
- ADR-0030 (PlanTemplateGeneralizer — Hippocampus fast-path for INGEST)
