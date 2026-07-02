# ADR-0020: Production Hardening & Chaos Validation

**Status:** Implemented
**Revision:** v4 (2026-05-29)
**Depends on:** ADR-0019 (complete)

---

## Context

ADR-0019 equips Cambrian with proprioception — runtime observability via OTel, LLM call tracing via Langfuse, and deterministic system testing via `SystemHarness`. These tools answer *"what is happening?"* and *"is the logic correct?"* They do not answer the harder production questions: *"does it survive?"*, *"does it scale?"*, and *"does the agent protocol hold under stress?"*

Three compounding gaps motivate this ADR:

1. **No throughput baselines.** We know CONWIP semaphore logic is correct (unit tests), but we do not know if 50 agents can auction within 2 seconds. We know `ProfileAggregator.RunOnce` recomputes EWMA correctly, but we do not know if it completes in <5s with 100K events. Without baselines, every refactor is a performance gamble.

2. **No failure mode validation.** Cambrian has five external dependencies (PostgreSQL, BBolt, LLM endpoint, agent processes, gRPC clients). Each has documented failure modes (timeout, corruption, hang, crash, malformed payload) but none are tested. `DialAgent` hangs indefinitely. `MemoryWorker.consolidateCluster` has a known race. The system has never been run with a dead PostgreSQL instance.

3. **No agent protocol hardening.** The Handoff protocol is the security boundary between untrusted Python agents and the kernel. `protoToHandoff` is the only file importing both `pb` and `domain` — it is a parser bug away from a remote exploit. There are no Python-side contract tests verifying that an agent can complete a full `Execute → RequestProposal → VerifyOutput` cycle against the real Substrate.

This ADR is structured as an **umbrella ADR with three sub-ADRs**:

| Sub-ADR | Scope | When | Infrastructure |
|---|---|---|---|
| **ADR-0020-A** | Kernel benchmarks & regression detection | Post-ADR-0019 | Standard Go test runner |
| **ADR-0020-B** | Failure mode validation (chaos + leak detection) | Post-0020-A baselines | Docker Compose (release gates only) |
| **ADR-0020-C** | Agent protocol contract & fuzzing | Post-0020-A baselines | Python + Go interop (release gates) |

**Mandatory sequence:** ADR-0019 complete → per-PR portions of ADR-0020-B and ADR-0020-C can start immediately → ADR-0020-A baselines committed → release-gate portions of ADR-0020-B and ADR-0020-C in parallel.

---

## Decision

### 1. Structure: Umbrella ADR with Three Sub-ADRs

ADR-0020 is not a single implementation PR. It is a coordination document. Each sub-ADR has its own acceptance criteria, files, and merge gate. They are implemented independently after the sequence dependency is satisfied.

**ADR-0020-A** produces committed baseline numbers. **ADR-0020-B** and **ADR-0020-C** consume those baselines as ground truth for degradation assertions.

---

### 2. ADR-0020-A: Kernel Benchmarks & Regression Detection

#### 2.1 Benchmark Granularity: Micro + Macro

Two tiers of benchmarks serve different purposes:

**Microbenchmarks** — algorithmic hot paths, run per-PR, <1s each:

| Benchmark | Target | What It Measures |
|---|---|---|
| `BenchmarkMicro_TopologicalSort_10/100/1000` | `executer.TopologicalSort` | Plan validation scaling with step count |
| `BenchmarkMicro_EWMA_1K` | `aggregator.EWMA` | Merit metric computation throughput |
| `BenchmarkMicro_RollingMedian_1K` | `aggregator.RollingMedian` | Latency window computation |
| `BenchmarkMicro_MeritScore_50` | `gatekeeper.computeMeritScore` | Candidate ranking with 50 agents |
| `BenchmarkMicro_RecordTokenUtilisation_1K` | `ProfileAggregator.RecordTokenUtilisation` | Histogram mutex contention under concurrent updates |
| `BenchmarkMicro_GetAdaptiveMaxEnergy` | `ProfileAggregator.GetAdaptiveMaxEnergy` | Token limit read latency |
| `BenchmarkMicro_ClusterSweep_50` | `clusterer.runSweep` | Cosine-similarity pairwise computation |

**Macrobenchmarks** — subsystem throughput, run nightly, 30–60s each:

| Benchmark | Target | What It Measures |
|---|---|---|
| `BenchmarkMacro_ExecutePlan_10step_50agents` | `SystemHarness.ExecutePlan` | End-to-end plan execution with fake LLM |
| `BenchmarkMacro_Auction_10/50/100agents` | `Auctioneer.ConductAuction` | Bid collection scaling with agent pool size |
| `BenchmarkMacro_Gatekeeper_50agents` | `Gatekeeper.FindCandidates` | Three-layer pipeline throughput |
| `BenchmarkMacro_ProfileAggregator_1K/10K/100K` | `ProfileAggregator.RunOnce` | Full-bucket scan + EWMA recomputation. 100K seeded via `mockgen.GenerateTelemetryCorpus` into temp bbolt before `b.ResetTimer`. |
| `BenchmarkMacro_LLMGateway_CONWIP_20` | `SubstrateLLMGateway.StreamChunks` | Semaphore throughput at max concurrency |
| `BenchmarkMacro_ProfileAggregator_Concurrent_100` | `ProfileAggregator.RecordTokenUtilisation` | 100 goroutines updating histogram simultaneously; measures mutex contention |

**Explicitly excluded from v1:**
- `MemoryAgent.ProcessAndStoreAsync` — I/O-bound by pgvector, not kernel logic
- `VerificationWorker` — bounded by verifier agent response time
- `SynapticWatcher`, `CircadianRhythm` — event-driven, no throughput SLO
- `Tier-2 scorer` — bounded by LLM-as-Judge latency
- Micro-utility functions (`ComputeSourceHash`, `clamp`, `contextByteSize`) — inlined by compiler

#### 2.2 Baseline Policy: Dual

| Benchmark Type | Run Frequency | Baseline Storage | Regression Threshold |
|---|---|---|---|
| Micro | Per-PR | Git-committed `internal/benchmarks/baseline.txt` | ±15% coarse guard; `benchstat` p-value < 0.05 for precise regression detection |
| Macro | Nightly | CI artifact store, 7-day rolling median | ±10% (dedicated runner, lower noise) |

**Micro baseline file format:**
```
BenchmarkMicro_TopologicalSort_100-8    123456    9876 ns/op    2048 B/op    12 allocs/op
BenchmarkMicro_EWMA_1K-8                987654    1234 ns/op     512 B/op     3 allocs/op
```

**Macro baseline storage:** Upload `bench.out` as CI artifact. Compare with `benchstat` against last 7 successful nightly runs.

**Dedicated runner requirement:** Macro benchmarks require a self-hosted runner with pinned CPU frequency (`cpufreq-set -g performance`), isolated execution (no other jobs), and ≥4 cores. Shared CI runners produce ±30% noise, making regression detection impossible.

#### 2.3 The Fake-LLM Caveat

Macro benchmarks use `SystemHarness` with fake `Generator` and `Embedder` (microsecond responses). This measures **kernel scheduling overhead only** — goroutine dispatch, mutex contention, context snapshot copy. Real plan latency is dominated by LLM response time (1–30s), which is orthogonal to kernel throughput.

A note in `internal/benchmarks/README.md` documents this explicitly:

> These benchmarks measure kernel control-path overhead with zero LLM latency. Real throughput is bounded by the slowest LLM call in the critical path. Use these numbers to detect kernel regressions, not to size production capacity.

**Benchmark load vs. production relationship:**
The macrobenchmark targets (10-step plan, 50–100 agents) are chosen to stress the kernel's scheduling and contention paths at 2–5× the expected initial production load (5-step plans, 10–20 agents). They are **not** capacity planning numbers. If `BenchmarkMacro_ExecutePlan_10step_50agents` shows 500ms kernel overhead, the real end-to-end latency for that plan with a live LLM will be `500ms + Σ(LLM latencies)`, where each LLM call is 1–30s. The benchmark answers: "Can the kernel dispatch 50 agents without choking?" not "Will my plan finish in 2 seconds?"

The `BenchmarkMacro_ProfileAggregator_Concurrent_100` benchmark uses 100 goroutines because that represents ~5× the default `LLMGatewayMaxConcurrency` (20). Each concurrent stream can trigger a `RecordTokenUtilisation` call, so 100 concurrent updaters tests histogram mutex contention at a realistic stress multiple.

#### 2.4 Files

| File | Purpose |
|---|---|
| `internal/benchmarks/micro_test.go` | 7 microbenchmark functions |
| `internal/benchmarks/macro_test.go` | 6 macrobenchmark functions |
| `internal/benchmarks/baseline.txt` | Committed micro baseline |
| `internal/benchmarks/README.md` | Fake-LLM caveat + runner requirements |
| `.github/workflows/benchmark.yml` | Nightly macro benchmark job |

**CI dependency pinning:**
`benchstat` is installed in `.github/workflows/benchmark.yml` with an explicit pinned version:
```yaml
- run: go install golang.org/x/perf/cmd/benchstat@v0.0.0-20250305212502-19ac84d57cf4
```
Floating `@latest` is prohibited — `benchstat` output format has changed between versions and reproducibility is mandatory for regression detection.

---

### 3. ADR-0020-B: Failure Mode Validation

#### 3.1 Hybrid Model: Fast In-Process + Slow Real-Service

Two suites serve different validation goals:

| Suite | Count | Speed | Infrastructure | What It Validates |
|---|---|---|---|---|
| Per-PR (in-process) | 6 scenarios | <30s total | Standard `go test` | Error handling logic exists and executes |
| Release gate (real-service) | 4 scenarios | 2–5 min each | Docker Compose + `toxiproxy` | Real failure detection (TCP timeouts, process death) |

**Rationale for hybrid:** Per-PR tests verify *"does the fallback code path exist and run?"* Real-service tests verify *"does the kernel actually detect a TCP timeout after 30s of idle connection?"* An in-process adapter returning `ErrConnClosed` immediately does not test the kernel's TCP stack behaviour. Both are necessary; neither is sufficient alone.

#### 3.2 Per-PR In-Process Chaos Scenarios (6)

All use `SystemHarness` with `Faulty*` adapter swap. No real external services.

| # | Scenario | Fault | Adapter | Injected At | Expected Behaviour | MTTR / Recovery Bound |
|---|---|---|---|---|---|---|
| 1 | PostgreSQL timeout — Gatekeeper | `pgx.ErrConnClosed` after 1st call | `FaultyVectorStore` | `Gatekeeper.FindCandidates` | Falls back to Declaration-only (Layer 1) | <50ms (single function call) |
| 2 | PostgreSQL timeout — WorkspaceStage | `pgx.ErrConnClosed` after 1st call | `FaultyVectorStore` | `WorkspaceStage.PrimeForPlanning` | Returns empty enrichment; cold-start path activates | <20ms (cached fallback) |
| 3 | BBolt write failure | `os.ErrPermission` | `FaultyTaskEventWriter` | `DAGExecutor.EventWriter` | Logs `slog.Warn`; step completes; `TaskEvent` lost | <5ms (step execution continues unblocked; event loss is the degradation, not a stall) |
| 4 | LLM timeout | `context.DeadlineExceeded` | `FaultyGenerator` | `Planner.GetExecutionPlan` | Returns error; `Server.Execute` returns `codes.DeadlineExceeded` | <10ms (no retry at planner level) |
| 5 | LLM 429 rate limit | `ErrRateLimited` | `FaultyGenerator` | `SelfHealer.Wrap` | SelfHealer retries immediately up to `MaxAttempts` (default 3); no jitter/backoff exists today. After `MaxAttempts` failures returns `HealingExhaustedError`. | <50ms (3 immediate retries) |
| 6 | Agent proposal hang | No response for 200ms (auction timeout 250ms) | `FaultyAgentClient` | `Auctioneer.RequestProposal` | Bid discarded; winner selected from remaining candidates | <300ms (auction timeout + selection) |

**SelfHealer retry policy and the Zero-Hardcode Rule:** `SelfHealer.MaxAttempts` is populated from `ExecutionConfig.MaxHealAttempts` at composition root (`kernel/metabolism_stack.go`). The value 3 is a safety floor when the config field is zero — it is not a routing decision and therefore falls under the Reflexive Path exemption (deterministic safety logic). The retry loop itself is hardcoded (immediate retry, no backoff), which is a known gap tracked in the `HealingExhaustedError` telemetry signal; adaptive backoff is deferred to a future ADR.

**Why 200ms, not 30s:** The per-PR chaos suite must complete in <30s total. A 30s hang in one scenario would consume the entire budget. The 200ms delay tests the same code path (proposal deadline exceeded → bid discarded) with a proportionally shorter auction timeout, keeping the full 6-scenario suite under 5s.

**Implementation:** `internal/testing/chaos/faulty_adapters.go` provides `FaultyVectorStore`, `FaultyTaskEventWriter`, `FaultyGenerator`, `FaultyAgentClient`. Each accepts an `inner` interface and a `faultConfig` struct controlling which calls to fail, with what error, and after how many successes.

```go
// internal/testing/chaos/faulty_adapters.go
type faultConfig struct {
    AfterSuccesses int           // fail after N successful calls
    Error          error         // error to return
    Delay          time.Duration // optional delay before failing
}
```

#### 3.3 Real-Service Release Gate Scenarios (4)

Gated behind `//go:build chaos`. Run on self-hosted runner with Docker Compose.

| # | Scenario | Fault | Mechanism | Expected Behaviour | Recovery Bound |
|---|---|---|---|---|---|
| 7 | PostgreSQL TCP blackout | All packets dropped | `toxiproxy` latency = -1 | Gatekeeper degrades to Layer 1; no panic; retry with backoff | <2s† |
| 8 | Agent process SIGKILL | Process killed mid-auction | `docker kill` on agent container | `Auctioneer` detects disconnect; fallback to runner-up; `TaskEvent` records failure | <5s (gRPC error propagation + re-auction) |
| 9 | BBolt disk full | Write returns `ENOSPC` | `tmpfs` mount + `dd if=/dev/zero` | `EventWriter` logs `slog.Warn`; subsequent steps continue; operator alerted via structured log only | <1s (fire-and-forget logging, no retry) |
| 10 | LLM streaming hang | Connection open, zero chunks | `httptest` server that accepts connection but never responds | `StreamChunks` context cancellation after `timeout_ms`; CONWIP slot released; `BudgetOverrun=false` (no tokens consumed) | <10s (test config sets `timeout_ms=5000`; cancellation path is identical regardless of timer duration) |

**V3-1 resolution (bbolt error observability):** The `cambrian_bbolt_write_error` counter is not defined in the ADR-0019 `TelemetryObserver` interface. Adding it would require an ADR-0019 addendum touching 5 core packages. The chosen resolution: bbolt write failures are observed via `slog.Warn` only in scenario 9. A storage-error counter is deferred to a future ADR that revisits the observer interface.

**V3-2 substantiation (†):** The <2s recovery bound for scenario 7 requires the test configuration to set `pgx.ConnConfig.ConnectTimeout = 1500ms` (or `statement_timeout=1500` in the PostgreSQL DSN). Without an explicit short timeout, the Go `database/sql` driver falls back to OS TCP keepalive, which typically fires after 2+ minutes on Linux. The release-gate test config (`test-config.json`) must include this timeout to satisfy the bound.

**Docker Compose file:** `scripts/chaos-compose.yml` defines PostgreSQL, Ollama, and a Python agent container. `toxiproxy` sits between Cambrian and PostgreSQL/Ollama. The `toxiproxy` image is pinned to `ghcr.io/shopify/toxiproxy:2.9.0` — v1 and v2 CLI interfaces are incompatible; a floating `:latest` tag is prohibited.

#### 3.4 Go Leak Detection

Two layers:

**Package-level `TestMain`** for background worker packages (per-PR):

| Package | Worker | `goleak` Strategy |
|---|---|---|
| `supervision/aggregator` | ProfileAggregator ticker | `VerifyTestMain` |
| `supervision/clusterer` | CapabilityClusterer ticker + event loop | `VerifyTestMain` |
| `metabolism/interview` | InterviewWorker 5-goroutine pool | `VerifyTestMain` + drain queue in test teardown |
| `supervision/verify` | VerificationWorker panic-recovery loop | `VerifyTestMain` |
| `supervision/synaptic` | SynapticWatcher event tail | `VerifyTestMain` |
| `supervision/circadian` | CircadianRhythm daily scan + scavenger | `VerifyTestMain` |

**Packages excluded from package-level:**
- `metabolism/executer` — DAGExecutor goroutines drain inside `Execute()`
- `substrate/network` — per-request gRPC handler goroutines
- `awareness` — no background workers
- `memory` — `ProcessAndStoreAsync` goroutine outlives test context by design; handled by `context.WithoutCancel`; excluded to avoid false positives. The `consolidateCluster` non-atomic Ingest+DeleteBatch race is a known data-integrity issue, not a goroutine leak, and is therefore out of scope for ADR-0020. It is tracked separately in CURRENT_CODEBASE_STATE.md.

**Integration-level** (release gate):

```go
// cmd/orchestrator/leak_test.go
//go:build chaos

func TestKernel_NoGoroutineLeak(t *testing.T) {
    k := bootstrapKernel(ctx, testCfg, lis)
    // Execute a plan
    // Shutdown

    // Retry with backoff: slow CI runners may need >100ms for goroutines to exit.
    var lastErr error
    for i := 0; i < 10; i++ {
        if lastErr = goleak.Find(); lastErr == nil {
            return
        }
        time.Sleep(50 * time.Millisecond)
    }
    t.Fatalf("goroutine leak detected: %v", lastErr)
}
```

#### 3.5 Files

| File | Purpose |
|---|---|
| `internal/testing/chaos/faulty_adapters.go` | `FaultyVectorStore`, `FaultyTaskEventWriter`, `FaultyGenerator`, `FaultyAgentClient` |
| `internal/testing/chaos/scenarios_test.go` | 6 per-PR chaos scenarios |
| `internal/substrate/network/chaos_test.go` | 4 real-service chaos scenarios (`//go:build chaos`) |
| `scripts/chaos-compose.yml` | Docker Compose for real-service suite |
| `cmd/orchestrator/leak_test.go` | Integration-level goroutine leak test (`//go:build chaos`) |

---

### 4. ADR-0020-C: Agent Protocol Contract & Fuzzing

#### 4.1 Dual Agent Contract Tests

**Per-PR: Strict mock gRPC server (Python)**

A Python pytest suite (`agents/contract_test.py`) mocks the Substrate's `AgentService` with a strict validator:

```python
# agents/contract_test.py
class StrictSubstrateMock(pb2_grpc.AgentServiceServicer):
    def Execute(self, request, context):
        assert request.id, "Handoff.ID is required"
        assert request.from_agent, "Handoff.FromAgent is required"
        assert request.to_agent, "Handoff.ToAgent is required"
        assert request.payload.id, "Payload.ID is required"
        assert request.payload.type, "Payload.Type is required"
        assert 0 <= request.confidence <= 1, "Confidence must be in [0,1]"
        # ...
```

The test boots a Python agent process, connects it to the mock, and asserts:
1. Agent parses `--substrate-addr` and `--substrate-socket`
2. Agent includes `x-agent-id` and auth token in gRPC metadata
3. `RequestProposal` returns `ProposalResponse` with `Confidence` in [0,1]
4. `VerifyOutput` returns `VerifyResponse` with `Score` in [0,1]
5. Full cycle: `Execute` → `RequestProposal` → `VerifyOutput` completes without error

**Release gate: Real Substrate (Python + Go)**

Compiles `cmd/orchestrator`, starts it with a test config, registers a Python agent, and runs the same cycle against the real gRPC server. Validates protocol drift between mock and real implementation.

```bash
# scripts/run-agent-contract-release.sh
go build -o /tmp/cambrian-test ./cmd/orchestrator
/tmp/cambrian-test --config test-config.json &
CAMBRIAN_PID=$!

# Ensure the orchestrator is always killed on script exit, even if pytest fails.
trap 'kill $CAMBRIAN_PID 2>/dev/null; wait $CAMBRIAN_PID 2>/dev/null' EXIT

# Health-check loop: wait until gRPC server is ready before running tests.
# grpc_health_probe is a lightweight standalone binary; add it to the release-gate Docker image.
until grpc_health_probe -addr=localhost:50051; do
  sleep 0.1
done

pytest agents/contract_test.py --real-substrate
```

#### 4.2 Fuzzing: `protoToHandoff` Only

**Surface:** `protoToHandoff(pb *pb.Handoff, obs domain.TelemetryObserver)` — the only file importing both `pb` and `domain`.

**Target:** Parser panics, nil dereferences, infinite loops on malformed input.

**Seed corpus — semantic boundary coverage:**

A single valid seed mutated by the default fuzz engine will not reach the *valid-proto-but-invalid-domain* boundary where the critical bugs live. Provide at least 6 seeds:

```go
func FuzzProtoToHandoff(f *testing.F) {
    // 1. Valid full Handoff (baseline)
    f.Add(seedValidHandoffBytes)
    // 2. Nil Payload (domain constraint violated)
    f.Add(seedNilPayloadBytes)
    // 3. Confidence = -1 (out of range)
    f.Add(seedNegativeConfidenceBytes)
    // 4. Confidence = 2.0 (out of range)
    f.Add(seedOverconfidenceBytes)
    // 5. Empty required string fields (FromAgent / ToAgent)
    f.Add(seedEmptyAgentsBytes)
    // 6. Maximum-length strings (buffer bounds)
    f.Add(seedMaxLengthStringsBytes)
    // 7. Unknown payload type (schema mismatch)
    f.Add(seedUnknownPayloadTypeBytes)

    f.Fuzz(func(t *testing.T, data []byte) {
        var pbHandoff pb.Handoff
        if err := proto.Unmarshal(data, &pbHandoff); err != nil {
            return // invalid protobuf, skip
        }
        // Must not panic
        _, _ = protoToHandoff(&pbHandoff, domain.NoopTelemetryObserver{})
    })
}
```

**Duration:**
- Per-PR: Not run (too slow)
- Nightly: `go test -fuzz=FuzzProtoToHandoff -fuzztime=10m`
- Pre-release: `go test -fuzz=... -fuzztime=1h`

**Acceptance:** Zero panics after 1 hour of fuzzing. Any panic found is a P0 security bug.

#### 4.3 Files

| File | Purpose |
|---|---|
| `agents/contract_test.py` | Python pytest suite — strict mock + real Substrate modes |
| `agents/conftest.py` | pytest fixtures for mock server and real Substrate lifecycle |
| `internal/substrate/network/handoff_fuzz_test.go` | Go fuzzing target for `protoToHandoff` |
| `scripts/run-agent-contract-release.sh` | Release gate script for real Substrate test |

---

## Consequences

### Positive

- **Benchmark baselines make refactors safe.** A PR that slows `Auctioneer.ConductAuction` by 25% is caught before merge.
- **Chaos tests prove degradation bounds.** "Auction latency increases by <20% when PostgreSQL is unavailable" is an assertion, not a hope.
- **Fuzzing finds parser bugs before attackers do.** `protoToHandoff` is the security-critical boundary; 1 hour of fuzzing is cheap insurance.
- **Agent contract tests prevent protocol drift.** The mock spec is the contract; the release gate validates the contract against reality.
- **Go leak detection catches resource exhaustion.** Background worker leaks are silent killers; `goleak` makes them visible.
- **Strict separation of suites keeps CI fast.** Per-PR runs in <2 minutes. Slow tests (macro benchmarks, real-service chaos, fuzzing) run nightly or on release gates only.

### Negative

- **Dedicated runner cost.** Macro benchmarks require a self-hosted runner (~$50–100/month). Without it, benchmark noise produces false positives that erode trust.
- **Docker Compose complexity for release gates.** Real-service chaos tests require `toxiproxy`, PostgreSQL, Ollama, and a Python agent container. Setup is non-trivial; flaky infrastructure produces flaky tests.
- **Python + Go interop friction.** Agent contract tests require both toolchains in CI. The release gate script must handle Go build, binary startup, health check polling, and pytest teardown cleanly.
- **Fuzzing duration.** 1 hour per release gate adds latency to the release pipeline. If a release is urgent, the fuzzing gate may be skipped — creating a bypass path.
- **Baseline maintenance.** Micro baselines in git require manual updates when a legitimate optimisation improves performance. Developers must run `go test -bench=. > baseline.txt` and commit the result.

### Risk: Baseline Drift on Shared CI

If the dedicated macro benchmark runner is unavailable and tests fall back to shared runners, the 7-day rolling median becomes noisy. The mitigation: macro benchmarks are **optional** for PR merge but **mandatory** for release. A noisy baseline blocks release, not development.

---

## Dependency Sequence

```
ADR-0019 (complete)
    │
    ├──► Unblocked immediately ────────────────────────────────┐
    │    ADR-0020-B per-PR chaos scenarios 1–6                 │
    │    ADR-0020-C per-PR agent mock tests                    │
    │    go leak detection (package-level + integration)       │
    │    fuzzing nightly                                       │
    │                                                          │
    └──► ADR-0020-A — baselines committed to git + CI artifact │
         store                                                 │
              │                                                │
              └──► Release-gate only (blocked by 0020-A) ◄─────┘
                   ADR-0020-B real-service scenarios 7–10
                   ADR-0020-C release gate agent real-substrate tests
```

**0020-A blocks only the release-gate portions of 0020-B and 0020-C because:**
- Real-service chaos scenarios 7–10 assert degradation ratios ("latency < 500ms under failure"). Without the healthy baseline from 0020-A, those numbers are fictional.
- Release-gate agent contract tests assert the protocol works at a given throughput. Without the macro benchmark baseline, "works" is undefined.

**Per-PR portions are unblocked because:**
- Per-PR chaos scenarios 1–6 use `SystemHarness` with `Faulty*` adapters and only assert that error handling executes — they do not compare against baseline latency numbers.
- Per-PR agent mock tests validate protocol shape, not performance.
- Fuzzing and leak detection are independent of throughput baselines.

---

## Extends

- **ADR-0018** — Chaos scenario 6 (agent proposal hang) tests the LLM Gateway's fallback routing under health cache failure. Scenario 10 tests streaming timeout handling.
- **ADR-0019** — `SystemHarness` from ADR-0019 is reused for per-PR chaos tests and macro benchmarks. `tools/export-events` baseline dataset feeds agent contract test fixtures.
- **ADR-0015/0016/0017** — PostgreSQL timeout chaos scenarios (1, 2, 7) validate WorkspaceStage and Tier-2 scorer degradation under pgvector failure.

---

## Considered Options

| Decision | Chosen | Rejected | Rationale |
|---|---|---|---|
| Sub-ADR count | 3 (A, B, C) | 1 monolithic ADR | Umbrella ADRs become unmergeable. Benchmarks, chaos, and agent tests have different consumers, infrastructure, and schedules. |
| Benchmark granularity | Micro + macro | Micro only; macro only | Micro catches algorithmic regressions but misses scheduling contention. Macro catches scheduling but is noisy. Both are needed. |
| Macro benchmark baseline | CI artifact (7-day median) | Git-committed; no baseline | Git baselines drift with hardware changes. No baseline means no regression detection. 7-day median smooths daily noise. |
| Chaos test model | Hybrid (in-process + real-service) | Real-service only; in-process only | In-process alone tests fallback code paths but not real TCP detection. Real-service alone is too slow for CI. Hybrid gives both. |
| Per-PR chaos infrastructure | SystemHarness + Faulty* adapters | Real services | Real services require Docker Compose and take 5–10s per test. Per-PR suite must complete in <2 minutes total. |
| Go leak detection | Package-level + integration | Package-level only; integration only | Package-level names the leaking source. Integration validates the full shutdown sequence. Both are needed. |
| Agent contract tests | Mock (per-PR) + real Substrate (release) | Mock only; real only | Mock is fast and isolates agent bugs. Real Substrate catches protocol drift. Both are needed. |
| Fuzzing scope | `protoToHandoff` only | All gRPC handlers; config JSON | `protoToHandoff` is the only agent-controlled parser boundary. Config JSON is internal. Other handlers use proto unmarshalling (handled by `proto.Unmarshal`). |
| Fuzzing duration | 10m nightly, 1h pre-release | Per-PR; 24h | Per-PR is too slow. 24h blocks release pipeline. 1h is sufficient for parser boundary coverage. |

---

## New Packages & Files

### ADR-0020-A

| Path | Purpose |
|---|---|
| `internal/benchmarks/micro_test.go` | 7 microbenchmark functions |
| `internal/benchmarks/macro_test.go` | 6 macrobenchmark functions |
| `internal/benchmarks/baseline.txt` | Committed micro baseline |
| `internal/benchmarks/README.md` | Fake-LLM caveat + runner requirements |
| `.github/workflows/benchmark.yml` | Nightly macro benchmark CI job |

### ADR-0020-B

| Path | Purpose |
|---|---|
| `internal/testing/chaos/faulty_adapters.go` | Fault injection adapters |
| `internal/testing/chaos/scenarios_test.go` | 6 per-PR chaos scenarios |
| `internal/substrate/network/chaos_test.go` | 4 real-service chaos scenarios (`//go:build chaos`) |
| `scripts/chaos-compose.yml` | Docker Compose for real-service suite |
| `cmd/orchestrator/leak_test.go` | Integration goroutine leak test (`//go:build chaos`) |

### ADR-0020-C

| Path | Purpose |
|---|---|
| `agents/contract_test.py` | Python pytest suite |
| `agents/conftest.py` | pytest fixtures |
| `internal/substrate/network/handoff_fuzz_test.go` | Go fuzz target |
| `scripts/run-agent-contract-release.sh` | Release gate script |

---

## Glossary Additions

- **Baseline** — Committed benchmark result used as ground truth for regression detection. Micro baselines live in git; macro baselines live in CI artifacts.
- **Chaos test** — A test that validates system behaviour under external dependency failure. Per-PR chaos uses in-process fault injection; release gate chaos uses real service degradation.
- **Faulty* adapter** — A test-only wrapper (e.g., `FaultyVectorStore`) that implements a domain interface but injects configurable errors. Lives in `internal/testing/chaos/`.
- **Macrobenchmark** — A benchmark measuring subsystem throughput under realistic load (e.g., `ExecutePlan` with 50 agents). Run nightly on a dedicated runner.
- **Microbenchmark** — A benchmark measuring a single hot function (e.g., `EWMA`). Run per-PR with git baseline comparison.
- **Mock spec** — The strict validation rules enforced by the Python mock gRPC server in agent contract tests. Defines the agent protocol contract independently of the Go implementation.
- **Real-service test** — A test using actual external dependencies (PostgreSQL, Ollama, agent processes) with injected faults (e.g., `toxiproxy`, `docker kill`). Gated behind `//go:build chaos`.
- **Release gate** — A CI check that runs before deployment (nightly or on release branch). Includes macro benchmarks, real-service chaos, agent real-substrate tests, and 1-hour fuzzing.
- **`benchstat`** — Go tool for comparing benchmark results with statistical significance. Used in CI to detect regressions against the 7-day rolling median.
