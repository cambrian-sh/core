#!/usr/bin/env pwsh
# setup-python-runtime.ps1 — build the kernel-managed sandbox Python env
# (ADR-0157 D7).
#
# Python agents and the Python system tools launch ONLY on the kernel-managed
# env (execution.agents.python.root, default <kernel dir>\runtime\python): the
# pinned, hash-verified CPython and uv, the release's hashed base lock, wheels
# only. There is no host-Python or repo-venv fallback and no CAMBRIAN_PYTHON
# (retired with metabolism.python_executable, config schema v4). This script
# runs the same build an operator runs (`cambrian-orchestrator python-env
# build`).
#
# By default it builds exactly the RELEASE env: the base lock's published agent
# SDK, which ships the agent channel (ENG-2033), so the kernel's boot check
# reports it healthy as a released install would (ENG-2077). Pass an SDK
# checkout only when developing the SDK itself: its wheel then replaces the
# published SDK (--sdk-dir, D7.4.5), as `cambrian-orchestrator setup` does in a
# dev tree with a sibling sdk\.
#
# Usage: .\scripts\setup-python-runtime.ps1 [-Sdk <dir>]
param([string]$Sdk = "")

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot

$buildArgs = @("python-env", "build", "--dir", $root)
if ($Sdk -ne "") {
    if (-not (Test-Path (Join-Path $Sdk "pyproject.toml"))) {
        Write-Error "no agent SDK checkout at $Sdk (omit -Sdk to build the release env)"
        exit 2
    }
    $buildArgs += @("--sdk-dir", $Sdk)
}

Set-Location $root
if ($Sdk -ne "") {
    Write-Host "=== Building the kernel-managed sandbox Python env (dev SDK from $Sdk) ===" -ForegroundColor Cyan
} else {
    Write-Host "=== Building the kernel-managed sandbox Python env (release base lock) ===" -ForegroundColor Cyan
}
go run ./cmd/orchestrator @buildArgs
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
Write-Host ""
go run ./cmd/orchestrator python-env status --dir $root
exit $LASTEXITCODE
