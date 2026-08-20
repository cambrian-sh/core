#!/usr/bin/env bash
# check-parity.sh — the ADR-0126 D11 parity-ledger gate.
#
# Mechanical half of the parity principle: every tool published from core
# appears in docs/published-tool-parity.md, and the ledger's two founding
# exclusions are still written down. A tool with no ledger row, or a vanished
# exclusion, fails the build.
set -euo pipefail
cd "$(dirname "$0")/.."

LEDGER="docs/published-tool-parity.md"
CORETOOLS="internal/infrastructure/mcpserve/coretools.go"

fail() { echo "FAIL: $*" >&2; exit 1; }

[ -f "$LEDGER" ] || fail "parity ledger missing: $LEDGER"
[ -f "$CORETOOLS" ] || fail "core tools file missing: $CORETOOLS"

# Every published core tool name must have a ledger row.
missing=0
for name in $(grep -oP 'Name:\s+"\K[a-z0-9_]+' "$CORETOOLS" | sort -u); do
  if ! grep -q "\`$name\`" "$LEDGER"; then
    echo "FAIL: published tool '$name' has no row in $LEDGER" >&2
    missing=1
  fi
done
[ "$missing" -eq 0 ] || exit 1

# The founding exclusions must remain written down, with their reasons.
grep -q "Foreign MCP tools" "$LEDGER" || fail "the foreign-MCP-tools exclusion is gone from the ledger"
grep -qi "subprocess tools" "$LEDGER" || fail "the subprocess-tools exclusion is gone from the ledger"
grep -qi "open proxy" "$LEDGER" || fail "the open-proxy reason is gone from the ledger"

# `remember` stays deferred until the ledger says otherwise: publishing it from
# core while its ledger row still reads deferred is exactly the drift this
# script exists to catch.
if grep -qP 'Name:\s+"remember"' "$CORETOOLS" && grep -q "deferred by owner ruling" "$LEDGER"; then
  fail "'remember' is published but the ledger still records it as deferred — update the ledger (owner decision) first"
fi

echo "PASS: published tools match the parity ledger; exclusions intact."
