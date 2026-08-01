#!/usr/bin/env pwsh
# Separability gate — PowerShell parity with scripts/check-separability.sh.
# Usage: pwsh -File scripts/check-separability.ps1
#
# Enforces two rules (see the .sh for the full rationale):
#   1. No core package imports the premium module.
#   2. go.opentelemetry.io appears only in internal/telemetry (the bridge seam).
#
# Three defects in the previous version are fixed here:
#   - it scanned "internal/domain", which does not exist (domain/ is at the repo
#     root), so the most important package was never checked;
#   - it grepped for "internal/premium", a path retired by ADR-0057, so it could
#     not match a real violation;
#   - it used `Select-String -Path "...\**\*.go"`, and `**` does not recurse in
#     Windows PowerShell 5.1 — only one directory level was ever scanned.
#     Get-ChildItem -Recurse is used instead.
# It also no longer assigns to $matches, which is a PowerShell automatic variable.

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot

# ── Resolve the premium module path ──────────────────────────────────────────
$premiumModule = "github.com/cambrian-sh/cambrian-premium"
$premiumGoMod = Join-Path (Split-Path -Parent $root) "cambrian-premium/go.mod"
if (Test-Path $premiumGoMod) {
    $moduleLine = Get-Content $premiumGoMod | Where-Object { $_ -match '^module\s+(\S+)' } | Select-Object -First 1
    if ($moduleLine -and $moduleLine -match '^module\s+(\S+)') {
        $premiumModule = $Matches[1]
    }
}

function Get-GoFiles([string[]]$Trees) {
    $files = @()
    foreach ($tree in $Trees) {
        $path = Join-Path $root $tree
        if (Test-Path $path) {
            $files += Get-ChildItem -Path $path -Filter *.go -Recurse -File
        }
    }
    return $files
}

$failed = $false

# ── Check 1: premium imports anywhere in core (incl. app/ and cmd/) ─────────
$premiumScan = @("domain", "internal", "pkg", "app", "cmd")
$premiumFiles = Get-GoFiles $premiumScan
if ($premiumFiles.Count -eq 0) {
    Write-Host "FAIL: no core source files found under $root"
    exit 1
}
$premiumHits = $premiumFiles | Select-String -Pattern $premiumModule -SimpleMatch
if ($premiumHits) {
    Write-Host "FAIL: core imports the premium module ($premiumModule)"
    $premiumHits | ForEach-Object { Write-Host "  $($_.Path):$($_.LineNumber): $($_.Line.Trim())" }
    $failed = $true
}

# ── Check 2: OTel outside the designated bridge ─────────────────────────────
$bridge = [IO.Path]::GetFullPath((Join-Path $root "internal/telemetry"))
$otelHits = Get-GoFiles @("domain", "internal", "pkg") |
    Where-Object { -not $_.FullName.StartsWith($bridge, [StringComparison]::OrdinalIgnoreCase) } |
    Select-String -Pattern "go.opentelemetry.io" -SimpleMatch
if ($otelHits) {
    Write-Host "FAIL: go.opentelemetry.io imported outside internal/telemetry"
    $otelHits | ForEach-Object { Write-Host "  $($_.Path):$($_.LineNumber): $($_.Line.Trim())" }
    $failed = $true
}

if ($failed) {
    exit 1
}
Write-Host "PASS: no premium imports in core; OTel confined to internal/telemetry"
Write-Host "      (scanned: $($premiumScan -join ', '); premium module: $premiumModule)"
