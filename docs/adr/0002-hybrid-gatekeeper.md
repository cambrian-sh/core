# ADR 0002 — Hybrid Gatekeeper: Three-Layer Agent Selection

**Status:** Implemented  
**Date:** 2026-04-30  
**Deciders:** Afsin

---

## Context

The current `Gatekeeper` (`internal/metabolism/gatekeeper.go`) is a stub that returns all registered agents as candidates for every auction. This means every auction invites every agent regardless of relevance, capability, or track record. On a growing ecosystem of specialised agents, this produces three compounding problems:

1. **Noise in the auction** — agents bidding on tasks they cannot perform, wasting the 2 000 ms bid window.
2. **No capability verification** — `AgentDefinition.Description` is a freeform string extracted from a Python constant. There is no structured contract, no schema check, and no proof that the agent's self-description reflects its actual reasoning behaviour.
3. **No integrity signal** — the auction winner is the highest `Confidence` bidder with no historical accountability. An agent can inflate its bid indefinitely with no structural consequence.

---

## Decision

Replace the stub with a **Hybrid Gatekeeper** — a three-layer sequential filter that narrows the full agent registry to the 3–5 best-qualified candidates before the auction begins. The three layers are:

### Layer 1 — Declaration (Technical Filter)

The agent publishes an **A2A Manifest** at registration time: a structured document declaring static competencies (tools, formats, supported languages) and a typed technical contract (input/output schemas). `BBoltAdapter` parses and persists the manifest alongside the `AgentDefinition`.

At Gatekeeper query time, Declaration performs a hard binary filter: if the task's required input format or tool is absent from the manifest, the agent is eliminated before any semantic or historical computation runs. This layer is fast (bbolt KV read, no LLM, no vector search).

### Layer 2 — Interview (Cognitive Filter)

When an agent first registers, a background Interview worker runs three dummy test scenarios via LLM. The agent's responses are vectorised and stored as a **cognitive fingerprint** in pgvector, keyed by `(AgentID, SourceHash)`. Together with the Merit metrics, this fingerprint forms the agent's **Agent Profile**.

At Gatekeeper query time, the task description is embedded and compared against the cognitive fingerprints of Declaration-passing agents. Agents below a semantic similarity threshold are eliminated. This layer catches the gap between an agent's stated capabilities (manifest) and its actual reasoning behaviour (fingerprint).

**Versioning — SourceHash:** The Agent Profile key is `(AgentID, SourceHash)` where `SourceHash = hash(manifest.Version + bbolt_file_hash)`. This ensures that both a self-declared version bump and an unversioned file change produce a distinct profile key. On SourceHash change, the agent enters **Re-interviewed** state: the old profile is archived, a new Interview runs asynchronously, and inherited Merit is decayed by a fraction derived from the embedding distance between old and new release notes — larger semantic diff → less inherited trust.

**Provisional state:** An agent that has passed Declaration but whose Interview has not yet completed is **Provisional**. Provisional agents bypass the Interview gate with a cold-start penalty score and proceed directly to Merit ranking, placed last among fully-interviewed candidates. They are not excluded from auctions.

### Layer 3 — Merit (Performance Filter)

After Declaration and Interview gates, surviving candidates are ranked by their **GatekeeperScore**:

```
GatekeeperScore = w1*SuccessRate + w2*TrustScore + w3*(1/NormalisedLatency)

NormalisedLatency = NetworkLatency + ComputationLatency + ContextGrowthPenalty

NetworkLatency      = gRPC round-trip time − agent's self-reported computation time
ComputationLatency  = agent's self-reported computation time (from AgentProposal.Latency)
ContextGrowthPenalty = k * bytes_added_to_resp_Context
```

`ContextGrowthPenalty` captures the downstream cost an agent imposes on the DAGExecutor's context snapshot and merge cycle. An agent that habitually floods `resp.Context` with large payloads increases serialisation and memory pressure for every successor step — this overhead belongs in its latency cost, not hidden as a neutral side-effect. The scalar `k` is a config value. All three latency components are recorded in the Task Event and aggregated by `ProfileAggregator`.

Weights `w1`, `w2`, `w3` and the constant `k` are static config values. The function resolves ordering within the qualified set — it does not compensate for failed gates. The top 3–5 candidates by GatekeeperScore are forwarded to the Auctioneer.

**Merit storage:** Raw per-task records (**Task Events**) are written to bbolt on the hot path after every task completion. A background **ProfileAggregator** worker periodically recomputes the derived metrics (EWMA Success Rate, Trust Score, Latency composite) and writes them as JSONB to the Agent Profile in pgvector. The Gatekeeper reads only pgvector — never raw events.

Verifier critiques — the natural language feedback a Verifier Pool member produces alongside its 0–1 quality score — are vectorised and stored in pgvector as **Judicial Records**, keyed by `(AgentID, SourceHash, taskID)`. Judicial Records serve two purposes: (1) semantic failure pattern detection across tasks (clustering recurring critique themes to surface systemic weaknesses), and (2) seeding future Interview test scenarios — the Interview worker can draw from Judicial Records to probe known failure modes when an agent is Re-interviewed.

### Integrity: Verifier Pool

To prevent **Confidence Inflation** (agents learning to overbid), a **Verifier Pool** — a subset of high-Merit agents designated as the system's internal "Judicial Branch" — independently scores a sampled 10% of completed tasks. The Trust Score of the original winner is updated as:

```
TrustScore = EWMA(verifier_score / bid_confidence, alpha)
```

Ratios above 1.0 (under-promised, over-delivered) raise the score; ratios below 1.0 (over-promised, under-delivered) lower it. Agents enter the Verifier Pool by crossing a Merit threshold; they are evicted if their own verification accuracy degrades. The Verifier Pool uses a separate registry from the main auction path.

---

## Alternatives Considered

| Concern | Alternative Rejected | Reason |
|---|---|---|
| Cold start | Block system on all Interviews completing at boot | Incompatible with edge-device startup constraint; 50+ agents × 3 LLM calls = minutes of unavailability |
| Cold start | Lazy Interview (profile on first real task) | Defeats the purpose — Interview exists to assess capability before live work, not during it |
| Versioning | Tie experience to `AgentID` only | Hides regressions — a bad new version inherits full trust from prior version |
| Versioning | Tie experience to `AgentID + AgentVersion` with no inheritance | Every deploy resets Trust Score to zero; discourages continuous delivery of specialised agents |
| Manifest evolution | Blue/green coexistence of old and new SourceHash | Two concurrent versions of the same logical agent doubles port, process, and profile management cost on edge devices |
| Manifest evolution | Manifest-diff gated re-interview (skip if cosmetic change) | Introduces a second embedding-distance threshold; adds configuration surface without clear gain over the simpler hard-cutover rule |
| Trust Score | Outcome-only (binary success signal) | "No error" is a weak quality signal; gameable by agents that complete but produce low-quality output |
| Trust Score | Confidence-band gating (cap bid at historical accuracy) | Couples bidding protocol to Trust Score calculation; makes both harder to reason about independently |
| Storage | All events in pgvector | Network hop on every task completion on the hot write path; unnecessary for a store optimised for semantic queries |
| Storage | All events in bbolt, no pgvector aggregates | Gatekeeper must scan raw event history at query time; O(n) per agent per query; degrades with scale |
| Combination function | Additive weighted sum across all three layers | High Semantic Similarity can compensate for a failed Declaration check; violates the gate semantics the three-layer architecture intends |

---

## Consequences

- `AgentDefinition` requires new fields: `ManifestVersion string`, `SourceHash string`, and a reference to the A2A Manifest document.
- A new `AgentManifest` type is needed in `internal/domain/` for typed capability declarations.
- `BBoltAdapter` must compute `SourceHash` during the scan of `agents_dir` and detect changes between runs.
- pgvector `documents` table must accommodate Agent Profiles alongside memory documents — a `document_type` discriminator field (or a separate table) is required to avoid mixing memory retrieval with agent profile queries.
- The `ProfileAggregator` and Interview worker are new background goroutines; their lifecycle must be managed alongside `MemoryWorker` in `cmd/orchestrator/main.go`.
- The Verifier Pool registry is a new subsystem. Initial implementation can be a filtered view of the agent registry (agents above a Merit threshold) rather than a separate store.
- Gatekeeper roadmap Phase 1 (vector search stub → real semantic filter) is now fully specified. Phase 2 (Agent Self-Reflection for bid scoring) remains open.
