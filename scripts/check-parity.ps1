# check-parity.ps1 - the ADR-0126 D11 parity-ledger gate (PowerShell twin of
# check-parity.sh; keep the two in sync).
$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

$ledger = "docs/published-tool-parity.md"
$coretools = "internal/infrastructure/mcpserve/coretools.go"
$tasktools = "internal/infrastructure/mcpserve/tasktools.go"

function Fail($msg) { Write-Host "FAIL: $msg"; exit 1 }

if (-not (Test-Path $ledger)) { Fail "parity ledger missing: $ledger" }
if (-not (Test-Path $coretools)) { Fail "core tools file missing: $coretools" }
if (-not (Test-Path $tasktools)) { Fail "task tools file missing: $tasktools" }

$ledgerText = Get-Content $ledger -Raw
$coreText = (Get-Content $coretools -Raw) + (Get-Content $tasktools -Raw)
$tick = [char]0x60  # backtick, kept out of string literals for PS 5.1's sake

# Every published core tool name must have a ledger row. tasktools.go joined
# the scanned set with ADR-0126 phase 4 (submit_task / get_task_status).
$missing = $false
$names = [regex]::Matches($coreText, 'Name:\s+"([a-z0-9_]+)"') |
    ForEach-Object { $_.Groups[1].Value } | Sort-Object -Unique
foreach ($name in $names) {
    $needle = "$tick$name$tick"
    if (-not $ledgerText.Contains($needle)) {
        Write-Host "FAIL: published tool '$name' has no row in $ledger"
        $missing = $true
    }
}
if ($missing) { exit 1 }

# The founding exclusions must remain written down, with their reasons.
if ($ledgerText -notmatch "Foreign MCP tools") { Fail "the foreign-MCP-tools exclusion is gone from the ledger" }
if ($ledgerText -notmatch "(?i)subprocess tools") { Fail "the subprocess-tools exclusion is gone from the ledger" }
if ($ledgerText -notmatch "(?i)open proxy") { Fail "the open-proxy reason is gone from the ledger" }

# 'remember' stays deferred until the ledger says otherwise.
if ($coreText -match 'Name:\s+"remember"' -and $ledgerText -match "deferred by owner ruling") {
    Fail "'remember' is published but the ledger still records it as deferred - update the ledger (owner decision) first"
}

Write-Host "PASS: published tools match the parity ledger; exclusions intact."
