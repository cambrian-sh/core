#!/usr/bin/env pwsh
# setup-python-runtime.ps1 — build the kernel-managed sandbox Python env for a
# dev tree (ADR-0157 D7).
#
# Python agents and the Python system tools launch ONLY on the kernel-managed
# env (execution.agents.python.root, default <kernel dir>\runtime\python): the
# pinned, hash-verified CPython and uv, the release's hashed base lock, wheels
# only. There is no host-Python or repo-venv fallback and no CAMBRIAN_PYTHON
# (retired with metabolism.python_executable, config schema v4). This script
# runs the same build an operator runs (`cambrian-orchestrator python-env
# build`), with the agent SDK built from the sibling sdk\ checkout, as
# `cambrian-orchestrator setup` does in a dev tree.
#
# Usage: .\scripts\setup-python-runtime.ps1 [-Sdk <dir>]   (default: ..\sdk)
param([string]$Sdk = "")

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
if ($Sdk -eq "") { $Sdk = Join-Path (Split-Path -Parent $root) "sdk" }

if (-not (Test-Path (Join-Path $Sdk "pyproject.toml"))) {
    Write-Error "no agent SDK checkout at $Sdk (pass its directory with -Sdk)"
    exit 2
}

Set-Location $root
Write-Host "=== Building the kernel-managed sandbox Python env (SDK from $Sdk) ===" -ForegroundColor Cyan
go run ./cmd/orchestrator python-env build --dir $root --sdk-dir $Sdk
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
Write-Host ""
go run ./cmd/orchestrator python-env status --dir $root
exit $LASTEXITCODE
