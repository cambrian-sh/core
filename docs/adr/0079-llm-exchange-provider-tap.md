---
id: 0079
title: LLM-Exchange Provider Tap — Full Agent Reasoning-Turn Capture for Benchmarking
status: Accepted
date: 2026-07-18
supersedes: []
superseded_by: []
depends_on:
  - 0047-operator-transport-plane
  - 0042-managed-cognitive-resource-allocation
  - 0057-open-core-boundary
---

# ADR-0079: LLM-Exchange Provider Tap

## Status

Accepted — kernel side delivered: a live-only `AgentLLMExchangeOp` operator event carrying
the full prompt+completion of every managed-proxy agent generation, gated behind
`execution.capture_llm_exchanges` (default off). Operator contract bumped `0058 → 0059`
with a conditional `llm-exchange` capability. The benchmark consumer (the
`task-accomplishment` suite in `cambrian-benchmarks`) reconstructs each agent's internal
ReAct loop from the exchange sequence.

## Context

The `task-accomplishment` benchmark must review **every agent output and every internal
loop step** to diagnose *where* an end-to-end task broke (planning / routing / a step
output / an agent's own loop / a tool / a policy). The observability audit
(`cambrian-benchmarks/docs/e2e-orchestration-benchmark-spec.md` §2) found the trajectory
fully materialized but not exportable:

- The DAG step result lives in `masterContext["step_{i}_result"]` but is never published.
- The SDK ReAct loop (`sdk/cambrian_agent_sdk/react.py`) holds a complete typed trajectory
  (memory_query / tool_call / reflection / veto / final_answer) in `WorkingMemory`, but only
  the memory-query rung crosses the gRPC boundary (kernel intercept → `AgentStepOp`).

The originally-scoped fix was a bespoke SDK trajectory-export event plumbed SDK→core→feed.
That is invasive (touches the SDK, the agent-plane proto, and the kernel relay) and
duplicates state the kernel already sees.

## Decision

Tap the **managed LLM provider chokepoint** instead. Every agent reasoning turn is a
`GenerateViaModelStream` call routed through the kernel's managed proxy — the exact point
where `AgentCallLogger` already forks the call to Langfuse
(`internal/substrate/network/generate_via_model_stream.go`). We fork the same
`(req.Prompt, completion, agentID, modelID, stepIndex)` to the operator feed as a new
live-only `AgentLLMExchangeOp`, via a `Server.LLMExchangeSink` mirroring the existing
`TokenSink`.

Because each ReAct prompt embeds the running trajectory and each completion is exactly one
action, the **ordered sequence of exchanges per (session, agent) reconstructs an agent's
entire internal loop** — every output and every loop step — with **zero SDK change** and no
new agent-plane surface.

Properties:
- **Live-only / never replayed** (like `TokenChunkOp`, ADR-0047 D12): prompts are large; the
  spool must not retain them. A benchmark subscribes live and windows per task.
- **Gated** behind `execution.capture_llm_exchanges` (default **off**). Prompts/completions
  can be large and sensitive, so this is a benchmark/diagnostic lane, not a production
  default. The `llm-exchange` capability is advertised only when the flag is on.
- **Truncated** by the emitter (8192 runes each) with untruncated lengths preserved
  (`request_chars`/`response_chars`) so truncation is visible.
- **Zero behavior change**: emission is fire-and-forget after the agent stream completes and
  never affects the agent's result — the same discipline as `AgentStepOp`/`VerifierRoundOp`.

## Boundaries / limitations

- Captures **managed-proxy generations only** — an agent that calls its own LLM client
  bypasses the kernel and emits nothing (the same population as the `TokenChunkOp` lane). The
  reference SDK agents all generate via the managed proxy.
- Not a persisted audit trail (live-only). A durable per-step result record (`StepResultOp`
  over `masterContext["step_{i}_result"]`) remains available as future polish if a
  replayable, DAG-level authoritative step output is wanted; it is out of scope here because
  the exchange lane already satisfies the review requirement.
- The premium `AgentCallLogger` (Langfuse) is unchanged and independent; this tap is an OSS
  feed fork, not a replacement.

## Consequences

- Operator contract `0058 → 0059`; `ui`/`cli` vendored protos do not consume the new event
  and remain behind (they already are; they degrade on the handshake) — recorded skew, no
  re-vendor required. The benchmark Python stubs were regenerated (`scripts/regen-stubs.sh`).
- New OSS config key `execution.capture_llm_exchanges` (default false) — additive,
  non-breaking, names no premium feature (ADR-0057 D5).
- The `task-accomplishment` suite now emits per-row `loop_reviews` (per-agent ordered turn
  list + action sequence + grounded-before-answer + final answer), closing the "every agent
  output + loop step reviewed" requirement when capture is on.
