#!/usr/bin/env pwsh
# run-all-tests.ps1 — Cambrian test suite runner
# Usage: .\scripts\run-all-tests.ps1 [-SkipBench] [-SkipFuzz] [-SkipPython] [-SkipE2E] [-SkipIntegration]

param(
    [switch]$SkipBench,
    [switch]$SkipFuzz,
    [switch]$SkipPython,
    [switch]$SkipE2E,
    [switch]$SkipIntegration
)

$ErrorActionPreference = "Continue"
$root = Split-Path -Parent $PSScriptRoot

$timestamp = Get-Date -Format "yyyyMMdd-HHmmss"
$outDir = "$root\test-results\$timestamp"
New-Item -ItemType Directory -Path $outDir -Force | Out-Null

Write-Host "=== Cambrian Full Test Suite ===" -ForegroundColor Cyan
Write-Host "Output: $outDir" -ForegroundColor Cyan
Write-Host ""

# ------------------------------------------------------------------
# 1. Unit tests (36 packages)
# ------------------------------------------------------------------
Write-Host "[1/7] Unit tests (36 packages)" -ForegroundColor Yellow
go test -count=1 -timeout 180s "$root\internal\..." *>&1 | Tee-Object -FilePath "$outDir\01-unit-tests.txt"
if ($LASTEXITCODE -ne 0) { Write-Host "  FAIL" -ForegroundColor Red } else { Write-Host "  PASS" -ForegroundColor Green }

# ------------------------------------------------------------------
# 2. Separability gate
# ------------------------------------------------------------------
Write-Host "[2/7] Separability gate" -ForegroundColor Yellow
& "$PSScriptRoot\check-separability.ps1" *>&1 | Tee-Object -FilePath "$outDir\02-separability.txt"
if ($LASTEXITCODE -ne 0) { Write-Host "  FAIL" -ForegroundColor Red } else { Write-Host "  PASS" -ForegroundColor Green }

# ------------------------------------------------------------------
# 3. Race detector
# ------------------------------------------------------------------
Write-Host "[3/7] Race detector" -ForegroundColor Yellow
go test -race -count=1 -timeout 300s "$root\internal\..." *>&1 | Tee-Object -FilePath "$outDir\03-race.txt"
if ($LASTEXITCODE -ne 0) { Write-Host "  FAIL" -ForegroundColor Red } else { Write-Host "  PASS" -ForegroundColor Green }

# ------------------------------------------------------------------
# 4. Micro benchmarks (save baseline)
# ------------------------------------------------------------------
if (-not $SkipBench) {
    Write-Host "[4/7] Micro benchmarks" -ForegroundColor Yellow
    Set-Location $root
    go test -bench=BenchmarkMicro -benchmem -count=1 -timeout 120s ./internal/benchmarks/... *>&1 | Tee-Object -FilePath "$outDir\04-benchmarks.txt"
    Set-Location $PSScriptRoot
    if ($LASTEXITCODE -ne 0) { Write-Host "  FAIL" -ForegroundColor Red } else { Write-Host "  PASS" -ForegroundColor Green }
} else {
    Write-Host "[4/7] Benchmarks skipped (-SkipBench)" -ForegroundColor DarkGray
}

# ------------------------------------------------------------------
# 5. Fuzzing (quick, 10 seconds)
# ------------------------------------------------------------------
if (-not $SkipFuzz) {
    Write-Host "[5/7] Fuzz suite (10s)" -ForegroundColor Yellow
    Set-Location $root
    go test -fuzz=FuzzProtoToHandoff -fuzztime=10s -timeout 30s ./internal/substrate/network/... *>&1 | Tee-Object -FilePath "$outDir\05-fuzz.txt"
    Set-Location $PSScriptRoot
    if ($LASTEXITCODE -ne 0) { Write-Host "  FAIL" -ForegroundColor Red } else { Write-Host "  PASS" -ForegroundColor Green }
} else {
    Write-Host "[5/7] Fuzzing skipped (-SkipFuzz)" -ForegroundColor DarkGray
}

# ------------------------------------------------------------------
# 6. E2E Quality Benchmarks (real qwen3:8b, skips gracefully if Ollama down)
# ------------------------------------------------------------------
if (-not $SkipE2E) {
    Write-Host "[6/7] E2E quality benchmarks (real LLM + PostgreSQL)" -ForegroundColor Yellow
    $ollamaUp = $false
    try {
        $resp = Invoke-WebRequest -Uri "http://localhost:11434/api/tags" -TimeoutSec 3 -UseBasicParsing -ErrorAction Stop
        $ollamaUp = ($resp.StatusCode -eq 200)
    } catch {
        $ollamaUp = $false
    }
    $pgUp = $false
    try {
        $pgConn = New-Object System.Net.Sockets.TcpClient
        $pgConn.Connect("localhost", 5432)
        $pgUp = $pgConn.Connected
        $pgConn.Close()
    } catch {
        $pgUp = $false
    }
    if (-not $ollamaUp) {
        Write-Host "  SKIP - Ollama not reachable at localhost:11434" -ForegroundColor DarkYellow
    } elseif (-not $pgUp) {
        Write-Host "  SKIP - PostgreSQL not reachable at localhost:5432" -ForegroundColor DarkYellow
    } else {
        Set-Location $root
        go test -tags=e2e -bench=BenchmarkE2E -benchtime=1x -timeout 600s ./internal/benchmarks/... *>&1 | Tee-Object -FilePath "$outDir\06-e2e-quality.txt"
        Set-Location $PSScriptRoot
        if ($LASTEXITCODE -ne 0) { Write-Host "  FAIL" -ForegroundColor Red } else { Write-Host "  PASS" -ForegroundColor Green }
    }
} else {
    Write-Host "[6/7] E2E benchmarks skipped (-SkipE2E)" -ForegroundColor DarkGray
}

# ------------------------------------------------------------------
# 7. Python SDK tests (ENG-2058)
#    The SDK is the sibling sdk/ repo (there is no python-sdk/ here). Its tests
#    run ONLY through its own memory-capped runner (AGENTS.md §4.7: the Python
#    3.13.1 recursion bug can exhaust memory), never a bare pytest.
# ------------------------------------------------------------------
if (-not $SkipPython) {
    Write-Host "[7/8] Python SDK tests (sdk/, memory-capped)" -ForegroundColor Yellow
    $sdkRoot = Join-Path (Split-Path -Parent $root) "sdk"
    $capped = Join-Path $sdkRoot "scripts\pytest_capped.ps1"
    if (-not (Test-Path $capped)) {
        Write-Host "  SKIP (no sdk checkout with scripts\pytest_capped.ps1 next to cambrian-core: $sdkRoot)" -ForegroundColor DarkGray
    } else {
        Set-Location $sdkRoot
        & $capped tests -q *>&1 | Tee-Object -FilePath "$outDir\07-python-sdk.txt"
        $pyExit = $LASTEXITCODE
        Set-Location $PSScriptRoot
        if ($pyExit -ne 0) { Write-Host "  FAIL" -ForegroundColor Red } else { Write-Host "  PASS" -ForegroundColor Green }
    }
} else {
    Write-Host "[7/8] Python tests skipped (-SkipPython)" -ForegroundColor DarkGray
}

# ------------------------------------------------------------------
# 8. Multi-agent integration benchmark (ADR-0023)
#    Requires: Ollama, PostgreSQL, and a healthy kernel-managed sandbox Python
#    env: since ADR-0157 D7 agents boot only on that env (there is no repo .venv),
#    so the step runs only when `python-env status` reports it healthy (build it
#    with scripts\setup-python-runtime.ps1).
# ------------------------------------------------------------------
if (-not $SkipIntegration) {
    Write-Host "[8/8] Multi-agent integration benchmark" -ForegroundColor Yellow
    Set-Location $root
    go run ./cmd/orchestrator python-env status --dir $root *>&1 | Tee-Object -FilePath "$outDir\08-python-env-status.txt" | Out-Null
    if ($LASTEXITCODE -ne 0) {
        Write-Host "  SKIP (kernel-managed Python env is not healthy - see 08-python-env-status.txt; run scripts\setup-python-runtime.ps1)" -ForegroundColor DarkGray
    } else {
        go test -tags=integration -bench=BenchmarkMultiAgent -benchtime=1x -timeout=600s ./cmd/orchestrator/... *>&1 | Tee-Object -FilePath "$outDir\08-integration-benchmark.txt"
        if ($LASTEXITCODE -ne 0) { Write-Host "  FAIL" -ForegroundColor Red } else { Write-Host "  PASS" -ForegroundColor Green }
    }
    Set-Location $PSScriptRoot
} else {
    Write-Host "[8/8] Integration benchmark skipped (-SkipIntegration)" -ForegroundColor DarkGray
}

Write-Host ""
Write-Host "=== Done ===" -ForegroundColor Cyan
Write-Host "Results saved to $outDir" -ForegroundColor Cyan
