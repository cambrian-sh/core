#!/usr/bin/env pwsh
# Verify core packages have zero imports of go.opentelemetry.io or internal/premium
# Usage: pwsh -File scripts/check-separability.ps1

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot

$corePackages = @(
    "internal/domain",
    "internal/metabolism",
    "internal/awareness",
    "internal/supervision",
    "internal/substrate"
)

$failed = $false

foreach ($pkg in $corePackages) {
    $matches = Select-String -Path "$root\$pkg\**\*.go" -Pattern "go.opentelemetry.io" -SimpleMatch `
        2>$null
    if ($matches) {
        Write-Host "FAIL: $pkg imports go.opentelemetry.io"
        $matches | ForEach-Object { Write-Host "  $_" }
        $failed = $true
    }
}

foreach ($pkg in $corePackages) {
    $matches = Select-String -Path "$root\$pkg\**\*.go" -Pattern "internal/premium" -SimpleMatch `
        2>$null
    if ($matches) {
        Write-Host "FAIL: $pkg imports internal/premium"
        $matches | ForEach-Object { Write-Host "  $_" }
        $failed = $true
    }
}

if ($failed) {
    exit 1
}
Write-Host "PASS: No OTel or premium imports in core packages"
