# ADR-0023: Real Cambrian Agents and Multi-Agent Integration Benchmark

**Status:** Implemented 2026-05-30 (grilling session 2026-05-29 — 9 decisions resolved; SDK audit 2026-05-29 — 5 pre-flight fixes + D6 event-driven replacement; IE/SE review 2026-05-30 — BudgetExceededError semantics hardened, polling replaced; all 15 vertical slices in `docs/issues/adr23/` delivered via TDD — 35/35 Go packages + 118 Python tests pass)  
**Date:** 2026-05-29  
**Deciders:** Afsin  
**Depends on:** ADR-0022 (Global Workspace Context), ADR-0021 (System Quality Measurement), ADR-0014 (Capability Clustering), ADR-0001 (Agent Trait Classification)  

---

## 1. Executive Summary

This ADR introduces five production-quality Python agents (`code_generator`, `code_executor`, `terminal`, `summariser`, `analyst`) and a `//go:build integration` benchmark in `cmd/orchestrator/` that exercises the full Cambrian kernel — Planner, Auctioneer, AgentManager, Gatekeeper, DAGExecutor — with real Python agent subprocesses. Each benchmark case runs twice: once with a **Planner-generated plan** (Path A) and once with a **manually-written reference plan** (Path B), enabling isolation of Planner quality from agent execution quality.

---

## 2. Design Decisions

### D1 — Architecture: Full Real Stack (not in-process simulation)

**Decision:** The benchmark boots real Python agent subprocesses via `AgentManager.bootAgent()` and runs the full Auctioneer/gRPC dispatch loop. The Cambrian Planner (LLM) generates the execution plan for Path A.

**Rejected:** In-process simulation (role-based prompt dispatch, no gRPC) — it would test prompt engineering, not the actual Cambrian agent protocol.

**Rejected:** Subprocess from `internal/benchmarks/` — `bootstrapKernel` is `package main` and cannot be imported from outside `cmd/orchestrator/`.

---

### D2 — Dual-Path per Benchmark Case

**Decision:** Every benchmark case has both:
- **Path A (Planner):** A natural language `user_input` string → Planner (LLM) → `ExecutionPlan` → Auctioneer → agents
- **Path B (Manual):** A hardcoded `ExecutionPlan` with `expected_capability` per step → Auctioneer → same agents

Path B isolates agent execution quality from Planner quality. If Path A < Path B, the Planner is the bottleneck. If both are low, the agents are the bottleneck.

---

### D3 — Routing Mismatch: Log-Only

**Decision:** When the winning agent's capability does not match `expected_capability` in a Path B step, log a `routing_miss` event and continue. Do not fail the benchmark.

**Rationale:** A wrong agent producing a correct answer is signal, not noise. It may indicate the `AGENT_DESCRIPTION` similarity space needs calibration, or that the task is genuinely ambiguous. Hard failures here would suppress useful data.

---

### D4 — Test Placement: `cmd/orchestrator/` as `package main`

**Decision:** The integration benchmark lives in `cmd/orchestrator/multiagent_bench_test.go` with `//go:build integration`. Since it is `package main`, it can call `bootstrapKernel(ctx, cfg, lis)` directly with zero refactoring.

**Rejected:** Promoting `bootstrapKernel` to `internal/kernel/` — unnecessary refactoring; `cmd/orchestrator/` is the correct composition root and the test belongs there.

---

### D5 — Code Handoff via Global Workspace (`get_context_node`)

**Decision:** `code_executor_agent` retrieves the code to execute by iterating `request.working_memory`, finding a ref with `type="step_result"` and `labels` containing `"step_0"` (or the declared `DependsOn` step index), and calling `agent.substrate.get_context_node(ref.cid)` to fetch the full code bytes from the ContentStore.

**Rationale:** With `use_global_workspace=true`, `handoff.Context` is nil. The ContentStore CAS is the correct source of full step result content. Using `get_context_node` tests the complete Phase 3 Global Workspace pipeline end-to-end.

**Degraded mode:** If `get_context_node` fails (ContentStore hiccup), the agent falls back to `ref.snippet` (first 500 chars). This handles the case where the code fits in the snippet.

---

### D6 — Agent Readiness: Event-Driven via `AgentReadyEvent` + `ReadyChan()`

**Decision:** ~~Poll ProfileStore at 1-second intervals~~ — **replaced by event-driven notification.** Two coordinated changes:

#### D6A — Domain event: `AgentReadyEvent`

Add `AgentReadyEvent` to `internal/domain/event.go` (sealed event hierarchy, alongside `StatusUpdateEvent`, `ThoughtChunkEvent`, etc.):

```go
// domain/event.go
type AgentReadyEvent struct {
    AgentID      string
    SourceHash   string
    TrustScore   float64
    Capabilities []string
    InterviewMs  int64
}

func (AgentReadyEvent) domainEvent() {}
```

`InterviewWorker` emits it immediately after `SetProvisional(false)` via its existing `EventSink`:

```go
// interview_worker.go — after SetProvisional(false)
if iw.EventSink != nil {
    _ = iw.EventSink.Send(domain.AgentReadyEvent{
        AgentID:      agent.ID,
        SourceHash:   agent.SourceHash,
        TrustScore:   1.0,
        Capabilities: manifest.Tools,
        InterviewMs:  time.Since(start).Milliseconds(),
    })
}
```

`SynapticWatcher` already tails the event log — it will observe `AgentReadyEvent` and can immediately trigger `CapabilityClusterer.TriggerSweep()` without waiting for the next defensive ticker. This is emergent biological behavior: the system self-reorganises the moment a new agent becomes ready.

#### D6B — Benchmark mechanism: `ReadyChan()`

The benchmark does **not** subscribe to the full `SynapticWatcher` chain (that would require additional infrastructure coupling inside `package main`). Instead, `InterviewWorker` exposes a narrow channel that the benchmark drains directly:

```go
// interview_worker.go — new field
type InterviewWorker struct {
    // ... existing fields ...
    readyCh chan string // buffered cap=32; closed on Shutdown; non-blocking send
}

func (iw *InterviewWorker) ReadyChan() <-chan string { return iw.readyCh }

// after SetProvisional(false):
select {
case iw.readyCh <- agent.ID:
default: // non-blocking — production code path unaffected when nobody is listening
}
```

The benchmark `waitForAgentReadiness` becomes:

```go
func waitForAgentReadiness(worker *interview.InterviewWorker, agents []string, timeout time.Duration) error {
    ready := make(map[string]bool, len(agents))
    target := make(map[string]bool, len(agents))
    for _, id := range agents { target[id] = true }

    deadline := time.After(timeout)
    for len(ready) < len(target) {
        select {
        case id := <-worker.ReadyChan():
            if target[id] { ready[id] = true }
        case <-deadline:
            var missing []string
            for id := range target {
                if !ready[id] { missing = append(missing, id) }
            }
            return fmt.Errorf("agents not ready after %v: %v", timeout, missing)
        }
    }
    return nil
}
```

**Zero CPU burn during the wait.** No goroutine wakes until an Interview actually completes. The 60-second timeout is retained as a hard guard; the benchmark calls `b.Skip()` if it fires.

**Rationale for rejecting pure polling:** Cambrian is built on asynchronous signals and event-driven biology (SynapticWatcher, Watcher, CircadianRhythm). A 1-second poll loop inside a system whose entire design philosophy is *"react to events, never query"* is an architectural contradiction. Polling also wastes CPU during the 5–30s Interview window and makes the test flaky under Ollama load spikes (a 1.1s Interview finishes between polls; a 0.9s one doesn't). The channel approach has O(0) latency overhead and O(0) CPU cost during the wait.

**Production benefit beyond the benchmark:** `AgentReadyEvent` flowing through `SynapticWatcher` gives the running system live awareness of capability changes. A new agent finishing its Interview can immediately be reflected in the Planner's capability cluster view without waiting for the next `CapabilityClusterIntervalSeconds` tick.

---

### D7 — Code Executor Safety: Subprocess + Timeout (Dev-Mode)

**Decision:** `code_executor_agent` runs code via:
```python
subprocess.run(
    [sys.executable, "-c", code],
    timeout=10,
    capture_output=True,
    cwd=tempdir,   # per-execution isolated temp directory
    text=True,
)
```
No import restrictions. The agent is classified `TraitTool` and documented as *"dev-mode only — not sandboxed."* The `cwd=tempdir` prevents filesystem writes to project directories. The 10s timeout prevents hangs.

**Rejected:** AST import inspection — trivially bypassed via `__import__`, `importlib.import_module`, or `exec(compile(...))`. Creates false security.

**Rejected:** RestrictedPython — breaks legitimate numeric/algorithmic code that imports `math`, `itertools`, `random`.

---

### D8 — Terminal Agent: Command Allowlist

**Decision:**
```python
ALLOWED_COMMANDS = {
    "ls", "cat", "head", "tail", "grep", "find",
    "echo", "pwd", "wc", "python", "git",
    "which", "env", "date", "uname",
}
BLOCKED_SUBSTRINGS = ("rm", "mv", "cp", "chmod", "chown", "sudo",
                      "curl", "wget", "pip", "apt", "brew",
                      ">", ">>", "|")
```
The first token of the command is checked against `ALLOWED_COMMANDS`. The full command string is checked for `BLOCKED_SUBSTRINGS`. On rejection: `payload.data = b"BLOCKED: <reason>"`, `confidence=0.0`. This returns a valid response (not an error), so the DAGExecutor does not trigger SelfHealer.

**Note:** Pipes (`|`) are blocked to prevent `cat /etc/passwd | grep root`-style chaining. If compound queries are needed, the Planner should generate separate steps.

---

### D9 — Fixture Plans: 3 Cases, Quality via LLM-as-Judge + Routing Accuracy

**Decision:** Three benchmark cases exercise all five agents:

| Case | User Input | Agents Exercised |
|---|---|---|
| `multi-01` | "Write a Python function to find all prime numbers up to N using the Sieve of Eratosthenes, run it with N=50, and explain the output and time complexity." | code_generator → code_executor → analyst |
| `multi-02` | "Summarise the key differences between event sourcing and CRUD, then compare which is better for a financial audit system." | summariser → analyst |
| `multi-03` | "List the Python files in the agents directory, then explain what each agent is responsible for." | terminal → analyst |

**Quality metrics per case:**
- `quality_score` (1–5, LLM-as-judge) — measured for both Path A and Path B
- `routing_accuracy` (0.0–1.0) — fraction of steps where winning agent matched `expected_capability`; logged, not asserted
- `planner_delta` = Path A quality − Path B quality — measures whether the Planner helped or hurt
- `agent_interview_wait_ms` — recorded for observability

---

## 3. Agents

### 3.1 `code_generator_agent.py`
- **Trait:** `cognitive`  
- **Capability:** `code_generation`
- **LLM access:** `agent.substrate.generate()` (via `GenerateViaModelStream` RPC)
- **Output:** Python code block in `payload.data`; `context["language"] = "python"` for downstream agents
- **Proposal scoring:** Bids `confidence=0.9` when task description contains code/write/implement/function/class

### 3.2 `code_executor_agent.py`
- **Trait:** `tool` (born Active, bypasses Interview, bids `confidence=1.0`)
- **Capability:** `code_execution`
- **Input:** Reads code from `get_context_node(ref.cid)` where ref has `type="step_result"`. Falls back to `ref.snippet` on CAS failure.
- **Output:** `stdout`, `stderr`, `exit_code` in `payload.data` as JSON; additive context `{"exit_code": "0"}`
- **Safety:** subprocess + 10s timeout + isolated `tempdir`; dev-mode only

### 3.3 `terminal_agent.py`
- **Trait:** `tool` (born Active, bids `confidence=1.0`)
- **Capability:** `shell_command`
- **Input:** Shell command parsed from `payload.data`
- **Output:** stdout as UTF-8 text in `payload.data`
- **Safety:** ALLOWED_COMMANDS allowlist + BLOCKED_SUBSTRINGS; returns `BLOCKED:` payload on rejection

### 3.4 `summariser_agent.py`
- **Trait:** `cognitive`
- **Capability:** `summarisation`
- **LLM access:** `agent.substrate.generate()` with a summarisation system prompt
- **Output:** Bullet-point summary in `payload.data`
- **Proposal scoring:** Bids `confidence=0.9` when task contains summarise/summarize/tldr/concise/overview

### 3.5 `analyst_agent.py`
- **Trait:** `cognitive`
- **Capability:** `analysis`
- **LLM access:** `agent.substrate.generate()` with a chain-of-thought reasoning system prompt
- **Output:** Structured analysis (observations, reasoning, conclusion) in `payload.data`
- **Proposal scoring:** Bids `confidence=0.85` when task contains compare/analyse/analyze/evaluate/explain/trade-off

---

## 4. Pre-Implementation SDK and Kernel Fixes

SDK audit performed 2026-05-29 identified gaps that must be resolved before agents can function. Listed by severity.

---

### Fix 1 — `_session_token_id` never injected (**Critical**)

**Location:** `internal/substrate/network/server.go` — `stepFn` closure  
**Problem:** `LLMGateway.Acquire()` is fully implemented in `SubstrateLLMGateway` and the `LLMGateway` interface is declared on `DAGExecutor`, but `executeStep` never calls `Acquire`. The session token is never created and never placed in the agent handoff. `GenerateViaModelStream` returns `codes.Unauthenticated` when `session_token_id == ""` — every cognitive agent call to `agent.substrate.generate()` fails immediately.

**Fix:** In `server.go`'s `stepFn`, before calling `s.Auctioneer.Execute`, acquire a session token via `s.LLMGateway.Acquire()` and inject it into `handoff.Context["_session_token_id"]`. Call `s.LLMGateway.Complete(ctx, tokenID)` in a defer. Skip when `s.LLMGateway == nil` (backward-compatible).

```go
// server.go stepFn — session token injection
var tokenID string
if s.LLMGateway != nil {
    tokenID, _ = s.LLMGateway.Acquire(ctx, domain.StepAllocation{}, 4096, 30*time.Second)
    handoff.Context["_session_token_id"] = tokenID
    defer func() { _, _ = s.LLMGateway.Complete(ctx, tokenID) }()
}
handoff.Context["_step_index"] = fmt.Sprintf("%d", i)  // Fix 3 bundled here
```

---

### Fix 2 — `_capability` never injected (**High**)

**Location:** `internal/metabolism/auctioneer/auctioneer.go` — `Execute` method  
**Problem:** `_dispatch_execute` in Python SDK routes to the correct capability handler via `request.context.get("_capability", "")`. The Auctioneer knows the winning agent ID after `ConductAuction` but never injects the matched capability name. Falls back to `_score_capability()` (description text matching), which works for single-capability agents but is unreliable for multi-capability ones. All five new agents have a primary capability that should be explicitly injected.

**Fix:** After `ConductAuction` selects `bestProposal`, look up the winning agent's manifest and inject its first matching tool as `_capability`:

```go
// auctioneer.go — after bestProposal is selected
if in.Context == nil {
    in.Context = make(map[string]string)
}
in.Context["_capability"] = bestProposal.MatchedCapability  // primary capability that won
```

This requires `AgentProposal` to carry `MatchedCapability string` — the capability name from the manifest that the Gatekeeper/auction matched against the task description.

---

### Fix 3 — `_step_index` never set (**Low**)

**Location:** `internal/substrate/network/server.go` — `stepFn` closure  
**Problem:** `server.py` reads `step_index = int(ctx.get("_step_index", "0"))` and exposes it on `ExecuteRequest.step_index`. It is always 0 because the Go side never sets it. Agents use it for logging and for knowing their position in the plan.

**Fix:** Bundle with Fix 1 — `handoff.Context["_step_index"] = fmt.Sprintf("%d", i)` in the `stepFn` where `i` is the step index.

---

### Fix 4 — SDK helpers missing (`python-sdk/cambrian_agent_sdk/helpers.py`)

**Problem:** Three patterns every agent must implement from scratch:

#### `find_step_ref(working_memory, step_index) → Optional[ContextRef]`
`code_executor_agent` must find the prior step's CAS CID in `working_memory` by checking `ref.labels` for `"step_N"`. Without a helper this is 8+ lines repeated in every tool agent that reads prior step content.

```python
def find_step_ref(working_memory: List[ContextRef], step_index: int) -> Optional[ContextRef]:
    label = f"step_{step_index}"
    for ref in working_memory:
        if label in ref.labels and ref.type == "step_result":
            return ref
    return None
```

#### `extract_code_block(text) → str`
LLMs wrap generated code in markdown fences (`` ```python\n...\n``` ``). `code_executor_agent` must strip this before passing to `subprocess`. Without it, Python receives `` ```python `` as line 1 and raises `SyntaxError`. Falls back to the full text if no fence is found.

```python
def extract_code_block(text: str) -> str:
    """Extract the first fenced code block from LLM output. Returns raw text if no fence found."""
    ...
```

#### `build_prompt(system: str, user: str, context_str: str = "") → str`
All three cognitive agents assemble: system role + optional LTM context block + user task. Without a shared helper this pattern is copy-pasted with inconsistent formatting. Affects prompt coherence and makes prompt engineering harder to tune in one place.

```python
def build_prompt(system: str, user: str, context_str: str = "") -> str:
    parts = [f"<system>\n{system.strip()}\n</system>"]
    if context_str.strip():
        parts.append(f"<context>\n{context_str.strip()}\n</context>")
    parts.append(f"<task>\n{user.strip()}\n</task>")
    return "\n\n".join(parts)
```

---

### Fix 5 — `BudgetExceededError` missing from SDK (**Low**)

**Problem:** Agents have no typed exception to raise when their estimated compute cost would exceed `Step.MaxEnergy`. They can return a low-confidence response but `confidence=0.0` is **semantically ambiguous** — it is identical to "I have no idea how to do this task." The DAGExecutor cannot distinguish budget exhaustion from genuine incapability; both trigger the same SelfHealer/fallback path. This ambiguity compounds over time: the ProfileAggregator records a low-confidence event that penalises the agent's TrustScore even when the task was refused for budget reasons, not quality reasons.

**Fix:** Add `class BudgetExceededError(Exception)` to `cambrian_agent_sdk`. The error carries a `reason: str` and an optional `estimated_cost: float`. `server.py` catches it at the dispatch boundary and returns a sentinel payload:

```python
# server.py — in the Execute handler, wrapping _dispatch_execute
try:
    result = self._agent._dispatch_execute(domain_req)
except BudgetExceededError as e:
    return cambrian_pb2.Handoff(
        id=request.id,
        from_agent=self._agent.agent_id,
        payload=cambrian_pb2.Object(
            data=f"BUDGET_EXCEEDED:{e.reason}".encode(),
            type="budget_signal",           # typed — DAGExecutor can detect on type field
        ),
        confidence=0.0,
    )
```

The DAGExecutor detects `resp.Payload.Type == "budget_signal"` and routes to budget-specific handling (emit `BudgetExceededError` on the Go side, trigger HITL `InterventionRequest`) rather than treating it as a generic low-confidence failure. This preserves the agent's TrustScore — the VerificationWorker skips events where the response type is `"budget_signal"` since there is nothing to verify.

**Why `payload.type` not `confidence`:** `confidence` is a scalar on `[0, 1]` — it cannot encode *why* a value is 0. `payload.type` is a free string field that the Go side already reads to determine how to interpret `payload.data`. Using it as a discriminator is consistent with how `_signal_type` distinguishes neural signals from regular Handoffs.

**Agent usage:**
```python
from cambrian_agent_sdk import BudgetExceededError

@agent.capability("code_generation")
def generate(request):
    estimated = estimate_tokens(request.payload.text) * COST_PER_TOKEN
    if estimated > MAX_AGENT_BUDGET:
        raise BudgetExceededError(
            f"estimated ${estimated:.4f} exceeds agent budget ${MAX_AGENT_BUDGET:.4f}",
            estimated_cost=estimated,
        )
    # ... normal generation ...
```

---

### Summary Table

| Fix | File(s) | Severity | Blocks agents? |
|---|---|---|---|
| 1 — session token injection | `server.go` | 🔴 Critical | code_generator, summariser, analyst |
| 2 — capability injection | `auctioneer.go` | 🟡 High | multi-capability agents |
| 3 — step index injection | `server.go` (bundled with Fix 1) | 🟢 Low | logging only |
| 4 — SDK helpers | `helpers.py` (new) | 🟡 High | code_executor (`find_step_ref`, `extract_code_block`) |
| 5 — BudgetExceededError | `errors.py` (new) + `server.py` | 🟢 Low | none — but prevents TrustScore corruption on budget refusals |
| D6 — AgentReadyEvent | `domain/event.go` + `interview_worker.go` | 🟡 Architecture | benchmark correctness; production CapabilityClusterer freshness |

Fixes 1, 3 are bundled in `server.go`. All are backward-compatible (nil-safe, additive, non-breaking to existing tests).

---

## 5. Implementation Plan

### Phase 0 — Pre-flight Kernel and SDK Fixes

#### 0A — Go-side kernel fixes
- [ ] `internal/domain/event.go` — add `AgentReadyEvent` (D6A)
- [ ] `internal/metabolism/interview/interview_worker.go` — emit `AgentReadyEvent` via `EventSink` after `SetProvisional(false)`; add `readyCh chan string` + `ReadyChan() <-chan string` (D6B)
- [ ] `internal/substrate/network/server.go` — inject `_session_token_id` (via `LLMGateway.Acquire`) + `_step_index` in `stepFn` (Fixes 1 + 3)
- [ ] `internal/metabolism/auctioneer/auctioneer.go` — inject `_capability` after auction winner selected; add `MatchedCapability string` to `AgentProposal` (Fix 2)

#### 0B — Python SDK fixes
- [ ] `python-sdk/cambrian_agent_sdk/helpers.py` — new file: `find_step_ref`, `extract_code_block`, `build_prompt` (Fix 4)
- [ ] `python-sdk/cambrian_agent_sdk/errors.py` — new file: `BudgetExceededError(reason, estimated_cost)` (Fix 5)
- [ ] `python-sdk/cambrian_agent_sdk/server.py` — catch `BudgetExceededError` in Execute handler; return sentinel `payload.type="budget_signal"` (Fix 5)
- [ ] `python-sdk/cambrian_agent_sdk/__init__.py` — export `BudgetExceededError`, `find_step_ref`, `extract_code_block`, `build_prompt`
- [ ] `python-sdk/tests/test_helpers.py` — unit tests for all new helpers
- [ ] `python-sdk/tests/test_errors.py` — unit tests for `BudgetExceededError` dispatch path

#### 0C — Verify green before proceeding
- [ ] `go test ./internal/...` — 35/35 pass
- [ ] `pytest python-sdk/tests/` — all pass (includes new helper + error tests)

### Phase 1 — Python Agents
- [ ] `agents/code_generator_agent.py` (cognitive, `code_generation`)
- [ ] `agents/code_executor_agent.py` (tool, `code_execution`)
- [ ] `agents/terminal_agent.py` (tool, `shell_command`)
- [ ] `agents/summariser_agent.py` (cognitive, `summarisation`)
- [ ] `agents/analyst_agent.py` (cognitive, `analysis`)

### Phase 2 — Fixture Plans
- [ ] `internal/benchmarks/testdata/multiagent_plans.json` (3 cases: `user_input` + `steps[]` + `expected_capability`)

### Phase 3 — Integration Benchmark
- [ ] `cmd/orchestrator/multiagent_bench_test.go` (`//go:build integration`, `package main`)
  - `bootstrapBenchmarkKernel` (wraps `bootstrapKernel`, random port, temp data dir)
  - `waitForAgentReadiness` (polls ProfileStore until `Provisional=false` for all 3 cognitive agents, 60s timeout)
  - `BenchmarkMultiAgent_PlannerPath` (Path A — natural language → Planner → Auctioneer → agents)
  - `BenchmarkMultiAgent_ManualPath` (Path B — fixed plan → Auctioneer → agents)
  - `routingAccuracy(results []stepRoutingResult) float64` helper
  - `plannerDelta(pathA, pathB float64) float64` helper

### Phase 4 — Run Script
- [ ] Add `[8/8] Integration benchmark` to `scripts/run-all-tests.ps1` (gated by `-SkipIntegration` flag)
- [ ] Prerequisite check: Python SDK installed (`pip show cambrian-agent-sdk`), Ollama reachable, Postgres reachable

---

## 5. Fixture Plan Schema Extension

```json
{
  "id": "multi-agent-01",
  "subject": "Write, run, and explain a prime number sieve",
  "user_input": "Write a Python function to find all prime numbers up to N using the Sieve of Eratosthenes, run it with N=50, and explain the output and time complexity.",
  "steps": [
    {
      "query": "Write a Python function find_primes(n) that returns all prime numbers up to n using the Sieve of Eratosthenes algorithm.",
      "depends_on": [],
      "expected_capability": "code_generation"
    },
    {
      "query": "Execute the find_primes function from the prior step with n=50 and return the output.",
      "depends_on": [0],
      "expected_capability": "code_execution"
    },
    {
      "query": "Explain the output, verify it is correct, and analyse the time and space complexity of the Sieve of Eratosthenes.",
      "depends_on": [0, 1],
      "expected_capability": "analysis"
    }
  ]
}
```

---

## 6. Observability

New slog events emitted by the system (kernel + benchmark):

```
# domain/event.go — emitted by InterviewWorker (production path)
agent_ready                 agent_id, source_hash, trust_score, capabilities, interview_ms

# SynapticWatcher reaction to AgentReadyEvent
capability_cluster_trigger  source=agent_ready, agent_id

# benchmark-specific events (cmd/orchestrator/multiagent_bench_test.go)
multiagent_routing_check    step, expected_capability, winning_capability, matched=true/false
multiagent_plan_result      path=A|B, quality_score, routing_accuracy, planner_delta
multiagent_code_exec        exit_code, stdout_bytes, stderr_bytes, duration_ms
multiagent_terminal_exec    command, exit_code, stdout_bytes, blocked=true/false

# SDK — emitted by server.py BudgetExceededError catch
budget_exceeded_signal      agent_id, reason, estimated_cost, step_index
```

**Note:** `agent_ready` is emitted on the **production** path — every Interview completion in any Cambrian deployment will emit this event, not just the benchmark. It is a permanent addition to the domain event vocabulary.

---

## 7. Success Criteria

| Metric | Threshold | Measurement |
|---|---|---|
| TraitTool agents (code_executor, terminal) Active at boot | immediate | `ProfileStore.GetProfile` |
| `AgentReadyEvent` emitted per cognitive agent after Interview | 3 events within 60s | `ReadyChan()` drain in `waitForAgentReadiness` |
| `CapabilityClusterer.TriggerSweep` called on each `AgentReadyEvent` | 3 sweeps | `slog` event `capability_cluster_trigger` |
| Path B routing accuracy | ≥ 0.8 (2/3 steps correct) | `routingAccuracy` helper |
| Path B quality score (all 3 cases) | ≥ 3.5 / 5 | LLM-as-judge |
| Path A − Path B quality delta | > −0.5 (Planner doesn't degrade quality) | `planner_delta` |
| Code executor exit_code = 0 for valid code | 100% | `exit_code` in response |
| Budget refusal preserves agent TrustScore | TrustScore unchanged | VerificationWorker skips `payload.type == "budget_signal"` |

---

## 8. References

- ADR-0001: Agent Trait Classification (TraitTool vs TraitCognitive)
- ADR-0005: Self-Healing Replanning (why BLOCKED returns valid payload, not error)
- ADR-0014: Capability Clustering (agent description embeddings for Gatekeeper routing)
- ADR-0022: Global Workspace Context (get_context_node for cross-step content)
- `internal/testing/harness/` — SystemHarness pattern (for fake-LLM reference)
- `cmd/orchestrator/main.go:bootstrapKernel` — the function the benchmark calls directly
