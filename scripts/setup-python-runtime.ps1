#!/usr/bin/env pwsh
# setup-python-runtime.ps1 — build the Cambrian Python runtime (ADR-0023).
#
# Creates a repo-local .venv, installs the cambrian-agent-sdk (editable) with
# dev extras, and verifies the SDK + all 5 agents import. The integration
# benchmark and run-all-tests.ps1 auto-detect this venv.
#
# Usage: .\scripts\setup-python-runtime.ps1

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$venv = Join-Path $root ".venv"
$py = Join-Path $venv "Scripts\python.exe"

Write-Host "=== Cambrian Python Runtime Setup ===" -ForegroundColor Cyan

if (-not (Test-Path $py)) {
    Write-Host "Creating venv at $venv ..." -ForegroundColor Yellow
    python -m venv $venv
} else {
    Write-Host "venv already exists at $venv" -ForegroundColor DarkGray
}

Write-Host "Upgrading pip ..." -ForegroundColor Yellow
& $py -m pip install --upgrade pip --quiet

Write-Host "Installing cambrian-agent-sdk (editable, with dev extras) ..." -ForegroundColor Yellow
& $py -m pip install -e (Join-Path $root "python-sdk[dev]") --quiet

Write-Host "Verifying SDK import ..." -ForegroundColor Yellow
& $py -c "from cambrian_agent_sdk import Agent, assemble_context, build_prompt, find_step_ref, extract_code_block, BudgetExceededError; print('  SDK OK')"

Write-Host "Verifying all 3 cognitive agents import ..." -ForegroundColor Yellow
$agentsDir = Join-Path $root "agents"
& $py -c "import sys; sys.path.insert(0, r'$agentsDir'); import code_generator_agent, summariser_agent, analyst_agent; print('  3 agents OK')"

Write-Host ""
Write-Host "=== Done ===" -ForegroundColor Cyan
Write-Host "Python runtime: $py" -ForegroundColor Green
Write-Host ""
Write-Host "To run the orchestrator with this runtime, set:" -ForegroundColor Cyan
Write-Host "  `$env:CAMBRIAN_PYTHON = '$py'" -ForegroundColor White
Write-Host "  `$env:CAMBRIAN_AGENTS_DIR = '$agentsDir'" -ForegroundColor White
Write-Host "  `$env:CAMBRIAN_DATA_DIR = '$(Join-Path $root "data")'" -ForegroundColor White
