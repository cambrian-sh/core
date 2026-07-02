#!/usr/bin/env pwsh
# Cambrian Runtime — Test & Benchmark Runner (Windows / PowerShell)
# Usage: pwsh -File scripts/run-tests.ps1 -Group <group>
#
# Groups:
#   unit              All unit tests
#   separability      OTel / premium import gate
#   integration       SystemHarness E2E tests
#   chaos             Per-PR chaos scenarios (in-process, no Docker)
#   chaos-real        Real-service chaos (requires Docker Compose)
#   leak              Package-level goroutine leak detection
#   leak-integration  Full-kernel goroutine leak test
#   bench-micro       Micro benchmarks
#   bench-macro       Macro benchmarks (nightly runner)
#   bench-compare     Micro benchmarks + benchstat diff vs baseline
#   fuzz              Fuzzing, 10 minutes
#   fuzz-release      Fuzzing, 1 hour
#   contract          Agent mock contract tests (Python)
#   contract-release  Agent real-Substrate contract tests
#   corpus            Generate synthetic corpus (1000 records, baseline scenario)
#   export            Export bbolt events to JSONL
#   per-pr            Full per-PR pipeline (unit + separability + integration + chaos + bench-micro)
#   nightly           Nightly pipeline (bench-macro + fuzz + leak)
#   release-gate      Full release gate (Docker + 1h fuzz, ~30 min)

param(
    [Parameter(Mandatory = $true)]
    [ValidateSet(
        "unit", "separability", "integration",
        "chaos", "chaos-real",
        "leak", "leak-integration",
        "bench-micro", "bench-macro", "bench-compare",
        "fuzz", "fuzz-release",
        "contract", "contract-release",
        "corpus", "export",
        "per-pr", "nightly", "release-gate"
    )]
    [string]$Group
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot

function Run-Step {
    param([string]$Label, [scriptblock]$Block)
    Write-Host ""
    Write-Host "--- $Label ---" -ForegroundColor Cyan
    & $Block
    if ($LASTEXITCODE -ne 0) {
        Write-Host "FAILED: $Label" -ForegroundColor Red
        exit $LASTEXITCODE
    }
    Write-Host "PASS: $Label" -ForegroundColor Green
}

# ─── Individual steps ────────────────────────────────────────────────────────

function Step-Unit {
    Run-Step "Unit tests (all packages)" {
        go test ./internal/...
    }
}

function Step-Separability {
    Run-Step "OTel / premium separability gate" {
        pwsh -File "$root\scripts\check-separability.ps1"
    }
}

function Step-Integration {
    Run-Step "SystemHarness E2E integration tests" {
        go test -tags integration ./internal/testing/harness/... -v
    }
}

function Step-Chaos {
    Run-Step "Per-PR chaos scenarios (in-process, 6 scenarios, <5s)" {
        go test -tags chaos ./internal/testing/chaos/... -v -timeout 30s
    }
}

function Step-ChaosReal {
    Run-Step "Real-service chaos — starting Docker Compose" {
        docker compose -f "$root\scripts\chaos-compose.yml" up -d
    }
    try {
        Run-Step "Real-service chaos scenarios (4 scenarios, up to 20 min)" {
            go test -tags chaos ./internal/substrate/network/... -v -timeout 30m
        }
    } finally {
        Write-Host "--- Tearing down chaos infrastructure ---" -ForegroundColor Cyan
        docker compose -f "$root\scripts\chaos-compose.yml" down -v
    }
}

function Step-Leak {
    $packages = @(
        "./internal/supervision/aggregator/...",
        "./internal/supervision/clusterer/...",
        "./internal/metabolism/interview/...",
        "./internal/supervision/verify/...",
        "./internal/supervision/synaptic/...",
        "./internal/supervision/circadian/..."
    )
    foreach ($pkg in $packages) {
        Run-Step "Goroutine leak — $pkg" {
            go test $pkg -v
        }
    }
}

function Step-LeakIntegration {
    Run-Step "Integration-level goroutine leak test (full kernel)" {
        go test -tags chaos ./cmd/orchestrator/... -run TestKernel_NoGoroutineLeak -v
    }
}

function Step-BenchMicro {
    Run-Step "Micro benchmarks" {
        go test -bench=BenchmarkMicro -benchmem ./internal/benchmarks/...
    }
}

function Step-BenchMacro {
    Run-Step "Macro benchmarks (benchtime=10s per benchmark)" {
        go test -bench=BenchmarkMacro -benchmem -benchtime=10s ./internal/benchmarks/...
    }
}

function Step-BenchCompare {
    Run-Step "Micro benchmarks — compare against baseline" {
        go test -bench=BenchmarkMicro -benchmem -count=5 ./internal/benchmarks/... `
            | Out-File -FilePath "$env:TEMP\cambrian-bench-new.txt" -Encoding utf8
        benchstat "$root\internal\benchmarks\baseline.txt" "$env:TEMP\cambrian-bench-new.txt"
    }
}

function Step-Fuzz {
    Run-Step "Fuzzing protoToHandoff (10 minutes)" {
        go test -fuzz=FuzzProtoToHandoff -fuzztime=10m ./internal/substrate/network/...
    }
}

function Step-FuzzRelease {
    Run-Step "Fuzzing protoToHandoff (1 hour, release gate)" {
        go test -fuzz=FuzzProtoToHandoff -fuzztime=1h ./internal/substrate/network/...
    }
}

function Step-Contract {
    Run-Step "Agent mock contract tests (Python / strict mock)" {
        pytest agents/contract_test.py -v
    }
}

function Step-ContractRelease {
    Run-Step "Agent real-Substrate contract tests (release gate)" {
        bash "$root/scripts/run-agent-contract-release.sh"
    }
}

function Step-Corpus {
    Run-Step "Generate synthetic corpus (1000 records, baseline scenario)" {
        go run ./tools/mockgen-cli/main.go `
            -scenario baseline -n 1000 -seed 42 -output synthetic_corpus.jsonl
    }
    Write-Host "Corpus written to: synthetic_corpus.jsonl"
}

function Step-Export {
    Run-Step "Export bbolt events to JSONL" {
        go run ./tools/export-events/main.go `
            --db data/cambrian.db `
            --output events.jsonl
    }
    Write-Host "Events exported to: events.jsonl"
}

# ─── Pipeline compositions ────────────────────────────────────────────────────

function Pipeline-PerPR {
    Write-Host ""
    Write-Host "========================================" -ForegroundColor Yellow
    Write-Host "  Per-PR Pipeline  (~2 minutes)" -ForegroundColor Yellow
    Write-Host "========================================" -ForegroundColor Yellow
    Step-Unit
    Step-Separability
    Step-Integration
    Step-Chaos
    Step-BenchMicro
    Write-Host ""
    Write-Host "=== Per-PR pipeline complete ===" -ForegroundColor Green
}

function Pipeline-Nightly {
    Write-Host ""
    Write-Host "========================================" -ForegroundColor Yellow
    Write-Host "  Nightly Pipeline  (~15 minutes)" -ForegroundColor Yellow
    Write-Host "========================================" -ForegroundColor Yellow
    Step-BenchMacro
    Step-Fuzz
    Step-Leak
    Write-Host ""
    Write-Host "=== Nightly pipeline complete ===" -ForegroundColor Green
}

function Pipeline-ReleaseGate {
    Write-Host ""
    Write-Host "========================================" -ForegroundColor Yellow
    Write-Host "  Release Gate Pipeline  (~30 minutes)" -ForegroundColor Yellow
    Write-Host "  Requires: Docker, grpc_health_probe" -ForegroundColor Yellow
    Write-Host "========================================" -ForegroundColor Yellow
    Step-BenchMacro
    Step-ChaosReal
    Step-ContractRelease
    Step-FuzzRelease
    Step-LeakIntegration
    Write-Host ""
    Write-Host "=== Release gate pipeline complete ===" -ForegroundColor Green
}

# ─── Dispatch ─────────────────────────────────────────────────────────────────

switch ($Group) {
    "unit"               { Step-Unit }
    "separability"       { Step-Separability }
    "integration"        { Step-Integration }
    "chaos"              { Step-Chaos }
    "chaos-real"         { Step-ChaosReal }
    "leak"               { Step-Leak }
    "leak-integration"   { Step-LeakIntegration }
    "bench-micro"        { Step-BenchMicro }
    "bench-macro"        { Step-BenchMacro }
    "bench-compare"      { Step-BenchCompare }
    "fuzz"               { Step-Fuzz }
    "fuzz-release"       { Step-FuzzRelease }
    "contract"           { Step-Contract }
    "contract-release"   { Step-ContractRelease }
    "corpus"             { Step-Corpus }
    "export"             { Step-Export }
    "per-pr"             { Pipeline-PerPR }
    "nightly"            { Pipeline-Nightly }
    "release-gate"       { Pipeline-ReleaseGate }
}
