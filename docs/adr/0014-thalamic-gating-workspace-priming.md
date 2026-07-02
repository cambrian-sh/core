# ADR-0014: Thalamic Gating — Pre-execution Workspace Priming

**Status:** Implemented  
**Date:** 2026-05-17  
**Deciders:** Afsin, Claude  
**Prerequisites:** ADR-0003 (Awareness Redesign), ADR-0006 (Capability-Based Orchestration)

---

## Context

The Planner's `GetExecutionPlan` currently injects the **complete registered agent list** into its system prompt on every call — every agent's ID and full `Description`, every time, regardless of what the request is. This creates two compounding problems:

1. **Sensory overload:** The Planner must scan and reason about every agent in the registry to draft each plan, even when the request maps to a small capability domain. Token cost and hallucination risk scale with list size.

2. **Implicit routing coupling:** Because the Planner sees agent IDs, it is tempted to name specific agents in step queries. This short-circuits the Gatekeeper's autonomous Interview/Auction selection — a soft violation of the Zero-Hardcode Rule extended to the prompt layer.

The biological analogue is the **Thalamus**: all sensory input passes through it before reaching the cortex. The Thalamus does not forward raw sensory streams — it filters, labels, and routes only the signals relevant to the cortex's current task. If the Thalamus forwarded everything, the cortex would be overwhelmed by sensory noise.

This ADR replaces the raw agent dump with a structured capability cluster map — the Thalamus filters, the Planner (Cortex) reasons on the filtered signal.

---

## Decision

### 1. Schema: `Capabilities []string` on `AgentDefinition`

```go
type AgentDefinition struct {
    ID              string       `json:"ID"`
    Name            string       `json:"Name"`
    Description     string       `json:"Description"`
    Runtime         AgentRuntime `json:"Runtime"`
    ExecPath        string       `json:"ExecPath"`
    Dir             string       `json:"Dir"`
    A2AEndpoint     string       `json:"a2a_endpoint,omitempty"`
    SourceHash      string       `json:"source_hash,omitempty"`
    ManifestVersion string       `json:"manifest_version,omitempty"`
    Provisional     bool         `json:"provisional,omitempty"`
    Trait           AgentTrait   `json:"trait,omitempty"`
    Capabilities    []string     `json:"capabilities,omitempty"` // ADR-0014
}
```

The field is `omitempty` — existing persisted records without this key deserialise to a nil slice without error. An empty `Capabilities` slice means the agent is uncategorized and falls into a fallback bucket in the cluster map.

**Capabilities are not manually declared.** They are populated automatically by the `CapabilityClusterer` (see §3) and written back to `AgentRecord` in BBolt via `SetCapabilities`. The Planner reads them at plan-request time via the existing `GetAllAgents` call — no Planner-side change to the data access path.

### 2. `buildCapabilityCluster` — Private Helper in `planner.go`

A pure function. No dependencies beyond the agent slice. Replaces the `agentsDescriptions` builder loop. Testable in isolation.

```go
func buildCapabilityCluster(agents []domain.AgentDefinition) string {
    clusters := map[string][]string{}
    var uncategorized []domain.AgentDefinition

    for _, a := range agents {
        if a.Trait == domain.TraitModel {
            continue // models handled separately by the existing modelsDescriptions block
        }
        if len(a.Capabilities) == 0 {
            uncategorized = append(uncategorized, a)
            continue
        }
        for _, cap := range a.Capabilities {
            clusters[cap] = append(clusters[cap], a.ID)
        }
    }

    var sb strings.Builder
    sb.WriteString("CAPABILITY CLUSTERS (active agents grouped by domain):\n")

    keys := make([]string, 0, len(clusters))
    for k := range clusters { keys = append(keys, k) }
    sort.Strings(keys)

    for _, k := range keys {
        sb.WriteString(fmt.Sprintf("- %s: %s\n", k, strings.Join(clusters[k], ", ")))
    }
    for _, a := range uncategorized {
        sb.WriteString(fmt.Sprintf("- (uncategorized): %s — %q\n", a.ID, a.Description))
    }
    return sb.String()
}
```

### 3. System Prompt Change: Full Replacement of Agent Dump

The `AVAILABLE AGENTS` section in `GetExecutionPlan` is replaced by the cluster block. The `AVAILABLE MODELS` section is **unchanged** — model selection (`recommended_model`) is a legitimate Planner decision based on cost/quality tradeoffs and is not agent-to-task routing.

**Before (current):**
```
AVAILABLE AGENTS:
1. ocr-agent
   AGENT_DESCRIPTION: Extracts text from images using OCR...
2. summarizer
   AGENT_DESCRIPTION: Summarises long documents into key points...
```

**After (ADR-0014):**
```
CAPABILITY CLUSTERS (active agents grouped by domain):
- structured-data-querying: sql-agent, csv-processor
- text-generation: summarizer, translator
- visual-recognition: image-analyzer, ocr-agent, screenshot-agent
- (uncategorized): file-writer — "Generic file writer for local filesystem operations"
```

Cluster names are LLM-generated and domain-specific. The Planner writes natural-language step queries against capability domains. The Gatekeeper resolves which specific agent wins via Interview/Auction — as it always has.

### 4. Prompt Instruction Update

The `STRICT DECISION RULES` section already contains: *"Describe what the step needs in natural language. The runtime will discover the right agent automatically."* This instruction is correct and requires no change.

A clarifying line is added:
```
- You may reference a specific agent ID from the capability clusters if the task is
  unambiguously domain-specific, but prefer capability-level descriptions.
  The runtime's Gatekeeper always makes the final agent selection via competitive auction.
```

### 5. `CapabilityClusterer` — New Component in `supervision/clusterer/`

The `CapabilityClusterer` is a background component responsible for autonomously discovering capability domains from agent embeddings and writing them back to the registry. It lives in `internal/supervision/clusterer/` and is wired in `SupervisionStack`.

#### 5.1 Algorithm: Cosine-Similarity Threshold Grouping

No external library required. For all non-`TraitModel` agents, retrieve their embedding vectors from pgvector. Compute pairwise cosine similarity. Agents above `capability_cluster_threshold` (default `0.80`) are connected; transitive closure of connected pairs forms clusters (equivalent to single-linkage hierarchical clustering). Agents with no neighbours above the threshold become singletons and fall into the `(uncategorized)` bucket.

#### 5.2 TraitTool Agent Embedding

TraitTool agents bypass Interview and currently have an empty embedding (`[]float32{}`). To include them in clustering, the InterviewWorker's TraitTool fast-path is extended to embed the agent's `Description` (read from the manifest) using `w.Embedder.Embed` before saving the profile. The resulting embedding is stored alongside the profile in pgvector — making TraitTool agents visible to the clusterer without LLM involvement.

#### 5.3 Cluster Naming: LLM via `Generator` Interface

For each discovered cluster, the `CapabilityClusterer` calls a `Generator` (consumer-side interface defined locally in the package, same pattern as `awareness/planner.go`) with a prompt containing the representative agent's description and a sample of member descriptions. The LLM returns a concise domain name (e.g. `"structured-data-querying"`, `"visual-recognition"`). The result is cached in the `"capability_clusters"` BBolt bucket.

#### 5.4 Sticky Representative with Hysteresis

The representative of a cluster is the agent with the highest average cosine similarity to all other members. To prevent vocabulary drift from floating-point noise ("Representative Flipping"), a hysteresis rule applies: the previous sweep's representative retains its role unless another agent's average similarity exceeds it by more than `capability_cluster_epsilon` (default `0.02`).

```go
func determineRepresentative(members []agentEmbedding, previousRepID string, epsilon float64) string {
    var bestID string
    var bestScore, prevRepScore float64 = -1, -1

    for _, m := range members {
        score := avgSimilarity(m, members)
        if m.agentID == previousRepID {
            prevRepScore = score
        }
        if score > bestScore {
            bestScore = score
            bestID = m.agentID
        }
    }

    if prevRepScore > 0 && (bestScore-prevRepScore) < epsilon {
        return previousRepID
    }
    return bestID
}
```

Cluster names are stored in the `"capability_clusters"` BBolt bucket keyed by `representativeAgentID`. If the representative is unchanged between sweeps, the cached name is reused and the LLM is not called.

#### 5.5 Hybrid Triggering Mechanism

The clusterer runs on two tracks:

**Track 1 — Event-driven `TriggerSweep()`:**
A thread-safe method on `CapabilityClusterer` that signals an immediate sweep. Called by:
- `InterviewWorker`: after any agent transitions Provisional → Active (both TraitCognitive and TraitTool paths)
- The buffered `triggerChan` (size 1) acts as a natural debouncer — if a sweep is already queued or running, the signal is dropped silently.

```go
func (c *CapabilityClusterer) TriggerSweep() {
    select {
    case c.triggerChan <- struct{}{}:
    default: // sweep already pending — drop
    }
}
```

**Track 2 — Defensive reconciliation ticker:**
Fires every `capability_cluster_interval_seconds` (default `3600`s). Acts as a safety net that catches missed events, database interruptions, or state drift. Most hourly ticks cost only an embedding read + cosine computation (no LLM call) because the SourceHash fingerprint detects no registry change.

#### 5.6 Sweep Guards

A sweep is skipped when:
- Registry size < `capability_cluster_min_agents` (default `3`) — clustering is meaningless on a sparse registry; all agents remain `(uncategorized)`
- SourceHash fingerprint (FNV-1a over all agent SourceHashes) matches the fingerprint from the last successful sweep — registry is unchanged, cached names are valid

#### 5.7 New Config Fields in `ExecutionConfig`

| Field | Default | Purpose |
|-------|---------|---------|
| `capability_cluster_threshold` | `0.80` | Cosine similarity floor for cluster membership |
| `capability_cluster_epsilon` | `0.02` | Hysteresis cushion for representative stability |
| `capability_cluster_min_agents` | `3` | Minimum registry size before clustering runs |
| `capability_cluster_interval_seconds` | `3600` | Defensive reconciliation ticker interval |

---

## What This Does Not Change

- **Auction mechanism** — Gatekeeper, Interview, Auctioneer are untouched. The Planner's query is still matched to agents via semantic Interview scoring.
- **Zero-Hardcode Rule** — No routing logic moves into Go. The cluster map is a display transformation of registry data, not a routing decision.
- **Model routing** — `AVAILABLE MODELS` section and `recommended_model` field are unchanged.
- **TraitModel handling** — `buildCapabilityCluster` skips TraitModel agents (handled by models section). Excluded from clustering.
- **`buildCapabilityCluster` logic** — The Planner-side function is unchanged. It reads `AgentDefinition.Capabilities` as before; the only difference is who writes that field (clusterer, not operator).

---

## Rejected Alternatives

| Alternative | Reason Rejected |
|-------------|-----------------|
| Manual capability declaration in `AGENT_MANIFEST` | Zero-maintenance cost unachievable; operators must annotate every agent; vocabulary drifts as agents evolve; new capability domains require manual taxonomy updates |
| LLM tool-calling (InquiryHandler intercepts mid-generation) | Requires multi-turn `Generate()` loop, streaming support, new tool-intercept layer — disproportionate complexity for the benefit |
| Group by `Trait` only (cognitive / tool / model) | Three coarse buckets provide no meaningful domain signal to the Planner |
| Keyword heuristic on `Description` | Fragile, non-deterministic, breaks on description phrasing changes |
| Supplement (cluster + full list) | Keeps the sensory overload problem; cosmetic change only |
| K-Means clustering | Requires choosing K; auto-detecting K adds O(n²) computation; no mature pure-Go implementation; cosine-similarity threshold grouping achieves the same result with zero dependencies |
| HDBSCAN | No pure-Go implementation; requires CGo or subprocess — incompatible with edge deployment constraints |
| Fixed capability vocabulary (config-driven) | Defeats the zero-maintenance goal; new agent domains require config updates and redeployment |

---

## Implementation File Map

| File | Change |
|------|--------|
| `internal/domain/agent.go` | Add `Capabilities []string` to `AgentDefinition` |
| `internal/storage/dto.go` | Add `Capabilities []string` to `AgentRecord`; new `"capability_clusters"` bucket constant |
| `internal/storage/bbolt_adapter.go` | Add `SetCapabilities(agentID string, caps []string) error`; create `"capability_clusters"` bucket in `Seed` |
| `internal/mapper/agent_mapper.go` | Round-trip `Capabilities` in `ToDomain` / `ToRecord` |
| `internal/metabolism/interview/interview_worker.go` | TraitTool fast-path: embed `Description` instead of `[]float32{}`; both paths call `clusterer.TriggerSweep()` on Provisional→Active transition |
| `internal/supervision/clusterer/clusterer.go` | New `CapabilityClusterer` — cosine grouping, sticky representative, hybrid trigger, LLM naming, BBolt write-back |
| `internal/awareness/planner.go` | Add `buildCapabilityCluster` helper; replace `agentsDescriptions` block with cluster call; add clarifying instruction line |
| `internal/kernel/supervision_stack.go` | Wire `CapabilityClusterer`; inject `Generator` from `AwarenessStack` |
| `internal/config/config.go` | Add four new `ExecutionConfig` fields |
| `configs/config.json` | Add four new execution config entries |

---

## Relationship to Self-Reflective Awareness Plan

This ADR implements **Phase 2 (Thalamic Gating)** of the Self-Reflective Awareness plan (`plans/Self Reflective Awareness.md`). Phase 1 (InquiryHandler / tool-calling) was deferred in favour of this simpler pre-planning enrichment approach — same filtering benefit, no multi-turn generation loop required.

**Phase 3 (Neural Plasticity — learning from failures)** is not addressed here. It will be covered in a future ADR once ADR-0013's `SemanticCheckpointError` data is available as a training signal. The `CapabilityClusterer`'s cluster history (representative agent ID, member set, sweep timestamp) is a natural input for Phase 3 — clusters that consistently produce failed plans signal capability domain misclassification.

---

## Relationship to GWT

Global Workspace Theory's Thalamus analogy is precise here: the Thalamus does not suppress information arbitrarily — it **labels and routes** signals so the Cortex receives structured input, not raw sensory noise. `buildCapabilityCluster` is that labelling step: it transforms a flat registry list into a domain-structured map. The Planner (Cortex) receives capability vocabulary, not agent documentation. The `CapabilityClusterer` is the biological process by which the Thalamus learns which signals belong to which sensory category — unsupervised, adaptive, and zero-maintenance.
