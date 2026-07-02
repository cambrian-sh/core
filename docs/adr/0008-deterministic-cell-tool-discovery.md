# ADR 0008 — Deterministic Cell: Tool-Agent Discovery, Static Bidding, and Merit Integration

**Status:** Implemented  
**Date:** 2026-05-08  
**Deciders:** Afsin  

---

## Context

CONTEXT.md defines `AgentTrait`, `Tool-Agent`, and `Static Bidder` as first-class terms, and `docs/adr/0001-agent-trait-classification.md` records the decision to express deterministic tools as `TraitTool` agents rather than a separate struct. However, none of these were implemented: `domain.AgentDefinition` has no `Trait` field, `BBoltAdapter` only discovers `*agent.py` files, the Auctioneer always makes a gRPC `RequestProposal` call for every candidate, and the TUI renders all bids in a single grey style.

This ADR specifies the complete wiring: how `AgentTrait` lands in the domain model, how Tool-Agents are discovered, how the Static Bidder integrates into the auction without branching, how the Gatekeeper pipeline treats them, and how Merit data flows through `InterviewWorker`, `ProfileAggregator`, and `VerificationWorker`. Sandbox isolation for Tool-Agent processes is deferred to ADR-0009.

---

## Decision

### 1. `AgentTrait` Field — Domain Model

A `Trait AgentTrait` field is added to `domain.AgentDefinition`. The zero value of the `AgentTrait` string type is `TraitCognitive`:

```go
type AgentTrait string

const (
    TraitCognitive AgentTrait = ""     // zero value — backward-compatible
    TraitTool      AgentTrait = "tool"
)
```

All existing `AgentDefinition` records in BBolt deserialise with `Trait == ""`, which equals `TraitCognitive`. No migration is required.

### 2. Discovery — Python: Embedded Manifest

Python Tool-Agents declare themselves inside the existing `AGENT_MANIFEST` block that `BBoltAdapter` already parses via regex. A `"trait": "tool"` field in the manifest JSON is the signal:

```python
AGENT_MANIFEST = '''
{
  "trait": "tool",
  "version": "1.0",
  "description": "Writes content to a file at a given path.",
  "supported_formats": ["text/plain"]
}
'''
```

`BBoltAdapter` reads `manifest.Trait` after parsing and sets `AgentDefinition.Trait = TraitTool`. No new scanner, no new file format for Python agents.

### 3. Discovery — Non-Python: Sidecar Manifest

TypeScript, JavaScript, and binary Tool-Agents use a sidecar `<name>.manifest.json` file. `BBoltAdapter` scans `agentsDir` for `*.manifest.json` in addition to `*agent.py`:

```json
{
  "trait": "tool",
  "version": "1.0",
  "exec_path": "./file_writer",
  "description": "Writes content to a file at a given path.",
  "supported_formats": ["text/plain"]
}
```

`exec_path` is explicit (not inferred from the manifest filename) and resolves relative to the manifest file's own directory. This makes the manifest + binary a portable, self-contained unit.

### 4. `SourceHash` for Sidecar Tool-Agents

For sidecar Tool-Agents, `ComputeSourceHash` hashes both files:

```
FNV-1a(manifest.version + hash(manifest.json bytes) + hash(exec_binary bytes))
```

A change to either the manifest or the binary triggers re-registration and a fresh `InterviewWorker` fast-path run. Silent binary updates without a version bump are detected.

### 5. Static Bidder — Integration Point

The Static Bidder is a short-circuit inside `requestProposalFromAgent`, before any gRPC call:

```go
func (a *Auctioneer) requestProposalFromAgent(ctx context.Context, agent domain.AgentDefinition, ...) (*domain.AgentProposal, error) {
    if agent.Trait == domain.TraitTool {
        return &domain.AgentProposal{
            AgentID:    agent.ID,
            TaskID:     task.ID,
            Confidence: 1.0,
            Latency:    5,
            Rationale:  agent.Description,
        }, nil
    }
    // ... existing gRPC path
}
```

`ConductAuction` is untouched. Tool-Agent bids arrive through the same buffered channel as cognitive bids. A `Confidence=1.0` bid wins winner selection naturally — no branching in the auction loop, Zero-Hardcode Rule preserved.

### 6. Gatekeeper Pipeline for Tool-Agents

| Layer | Behaviour |
|---|---|
| **Layer 1 — Declaration** | Applies. Tool-Agent `SupportedFormats` must satisfy `task.RequiredFormats`. A file-writer tool does not appear in audio-processing auctions. |
| **Layer 2 — Semantic ANN gate** | Bypassed. Tool-Agents have no cognitive fingerprint in pgvector. `FindCandidates` skips the `SearchByEmbedding` call for `TraitTool` agents (same nil-safe path as Provisional cognitive agents). |
| **Layer 3 — Merit ranking** | Applies naturally. Static Merit (`TrustScore=1.0, SuccessRate=1.0, Latency=5ms`) gives Tool-Agents the highest possible `GatekeeperScore` without special treatment. |

### 7. `InterviewWorker` Fast-Path

`InterviewWorker.processAgent` detects `agent.Trait == TraitTool` and takes a fast-path: no scenario generation, no `RequestProposalFrom` gRPC calls, no pgvector fingerprint write. It directly calls `ProfileStore.SaveProfile` with static Merit values and then `SetProvisional(false)`:

```
enqueue (BBoltAdapter, Provisional=true)
  → InterviewWorker.processAgent
  → detect TraitTool
  → SaveProfile(TrustScore=1.0, SuccessRate=1.0, Latency=5ms)
  → SetProvisional(false)
```

This keeps `BBoltAdapter` free of any pgvector dependency. The static Merit values are a cold-start bootstrap, not a permanent override — `ProfileAggregator` will replace them with real data over time.

### 8. `ProfileAggregator` — No Special Case

`ProfileAggregator.RunOnce` processes Tool-Agents identically to cognitive agents. Real execution data overwrites the static bootstrap values via EWMA. A Tool-Agent that consistently fails (e.g., file system permission errors) will see its `TrustScore` degrade, lowering its `GatekeeperScore` and reducing future auction invitations. The static values provide a strong cold-start advantage; reality corrects them when warranted.

### 9. `VerificationWorker` and `VerifierPool`

Tool-Agent completions are sampled by `VerificationWorker` at the same FNV-1a ~10% baseline as cognitive agents. Surveillance mode (100% sampling below `trust_boost_threshold`) applies automatically if a Tool-Agent's TrustScore degrades. This is required for the `ProfileAggregator` TrustScore update to have a `verifier_score` signal.

`VerifierPool.Select` excludes `TraitTool` agents: a deterministic process scoring another deterministic process produces no useful signal. Only `TraitCognitive` agents may serve as verifiers.

### 10. TUI — `BidEntry` Proto Extension

`BidEntry` gains a `bool is_tool = 5` field. The Auctioneer sets it to `true` when the proposal came from the Static Bidder path:

```protobuf
message BidEntry {
  string agent_id   = 1;
  float  confidence = 2;
  string rationale  = 3;
  int32  latency_ms = 4;
  bool   is_tool    = 5;
}
```

`AuctionPane.View` renders Tool-Agent bid rows in Electric Blue (`lipgloss.Color("12")`) when `bid.IsTool == true`, distinguishing them visually from cognitive agent bids. This change is outside the scope of the Proto Freeze (issue #016), which covered `ProposalRequest`, `VerifyRequest/Response`, and `MemoryRequest/Response`.

### 11. Process Lifecycle — gRPC Wrapper

Tool-Agents implement the full `AgentService` gRPC interface using the Python SDK (or a language-equivalent thin wrapper). Boot sequence is identical to cognitive agents: port assignment from the `basePort` pool, `waitForReady`, `dialAgent`. No second execution path in `AgentManager`.

A `// TODO(ADR-0009): sandbox` comment marks the `callAgent` branch for Tool-Agents. Process-level isolation (gVisor on Linux, AppContainer on Windows) is deferred to ADR-0009.

---

## Alternatives Considered

| Concern | Alternative Rejected | Reason |
|---|---|---|
| `AgentTrait` zero value | `TraitCognitive = "cognitive"` (non-empty) | Requires a one-time BBolt migration pass with no new migration infrastructure; empty-string zero value is idiomatic Go and free |
| Static Bidder placement | Pre-flight split in `ConductAuction` — partition tools from cognitive before goroutine loop | Adds branching to `ConductAuction`, breaks the uniform "every candidate through the same pipe" invariant |
| Discovery (non-Python) | Infer `exec_path` from manifest filename | Silent break on rename; ambiguous on Windows (`.exe` vs no extension) |
| Discovery (Python) | Companion `.json` sidecar for all runtimes | Duplicates an existing working mechanism; increases file count per tool |
| Static Merit injection | `BBoltAdapter` writes to pgvector at registration | Creates a new `storage → infrastructure/postgres` import arc; `InterviewWorker` is already the single Profile writer |
| `VerifierPool` | Allow `TraitTool` agents as verifiers | Deterministic-scores-deterministic produces no quality signal; wastes pool capacity |
| `BidEntry` trait signal | Infer `is_tool` from `Confidence == 1.0 && LatencyMs == 5` in TUI | Fragile — a high-confidence cognitive agent would be miscoloured |
| Process lifecycle | Substrate shells out directly via stdin/stdout for Tool-Agents | Adds a second execution path and a new wire protocol; gRPC loopback overhead is negligible |

---

## Consequences

- **`domain.AgentDefinition` gains `Trait AgentTrait`** — all code that constructs `AgentDefinition` literals must compile; existing BBolt records deserialise safely with zero-value `TraitCognitive`.
- **`BBoltAdapter` scanner gains a `*.manifest.json` branch** — any file matching that pattern in `agentsDir` is treated as a Tool-Agent registration.
- **`InterviewWorker` gains a `TraitTool` fast-path** — the goroutine pool and queue are unchanged; the fast-path is a short branch before `buildScenarios`.
- **`VerifierPool.Select` excludes `TraitTool` agents** — pool membership logic gains one filter; existing pool size thresholds and health guard remain unchanged.
- **`BidEntry` proto gains `bool is_tool = 5`** — backward-compatible (proto3 default `false`); generated Go and Python code must be regenerated.
- **`AuctionPane.View` gains trait-aware bid colouring** — Electric Blue for `is_tool == true`; no other TUI changes.
- **Sandbox deferred** — Tool-Agent processes run without isolation until ADR-0009 is implemented. This is an accepted risk for the development phase.
