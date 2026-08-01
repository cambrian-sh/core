#!/usr/bin/env bash
#
# Separability gate — enforces the ADR-0057 open-core boundary and the
# OTel-isolation rule. Run from anywhere: `bash scripts/check-separability.sh`
#
# What it checks
#
#   1. No core package imports the premium module.
#      The premium module path is `github.com/cambrian-sh/cambrian-premium`.
#      The old gate grepped for `internal/premium`, a path that stopped existing
#      when ADR-0057 split premium into its own module — so it could never match
#      a real violation. The module path is read from cambrian-premium/go.mod
#      when that module is present, and falls back to the known literal.
#
#   2. OpenTelemetry appears only in the designated bridge package.
#      `internal/telemetry` is the seam that adapts OTel to the kernel's own
#      interfaces; everywhere else in core must stay OTel-free so the OSS build
#      carries no tracing dependency.
#
# Why the package list is derived, not hardcoded
#
#   The previous gate hardcoded `internal/domain`, which does not exist — the
#   domain package is at the repository ROOT (`domain/`). Every scan of it
#   silently matched nothing and the gate passed vacuously. This version
#   enumerates the real top-level Go trees, so a package added tomorrow is
#   covered without anyone remembering to add it here.
#
set -uo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root" || exit 1

failed=0

# ── Resolve the premium module path ──────────────────────────────────────────
premium_mod="github.com/cambrian-sh/cambrian-premium"
premium_gomod="$root/../cambrian-premium/go.mod"
if [ -f "$premium_gomod" ]; then
    detected="$(awk '/^module /{print $2; exit}' "$premium_gomod")"
    [ -n "$detected" ] && premium_mod="$detected"
fi

# ── The core trees to scan ───────────────────────────────────────────────────
# Every top-level directory that holds first-party Go source. `api/` is excluded
# because it is generated, and `cmd/` + `app/` are the composition roots, which
# are allowed to reference anything core-internal (but still never premium — see
# the premium check below, which scans them too).
core_trees=()
for d in domain internal pkg; do
    [ -d "$d" ] && core_trees+=("$d")
done

if [ ${#core_trees[@]} -eq 0 ]; then
    echo "FAIL: no core source trees found under $root (expected domain/, internal/, pkg/)"
    exit 1
fi

# ── Check 1: premium imports anywhere in core (including app/ and cmd/) ──────
premium_scan=("${core_trees[@]}")
for d in app cmd; do
    [ -d "$d" ] && premium_scan+=("$d")
done

hits="$(grep -rn --include='*.go' -F "$premium_mod" "${premium_scan[@]}" 2>/dev/null || true)"
if [ -n "$hits" ]; then
    echo "FAIL: core imports the premium module ($premium_mod)"
    echo "$hits" | sed 's/^/  /'
    failed=1
fi

# ── Check 2: OTel outside the designated bridge ──────────────────────────────
otel_hits="$(grep -rn --include='*.go' -F 'go.opentelemetry.io' "${core_trees[@]}" 2>/dev/null \
             | grep -v '^internal/telemetry/' || true)"
if [ -n "$otel_hits" ]; then
    echo "FAIL: go.opentelemetry.io imported outside internal/telemetry"
    echo "$otel_hits" | sed 's/^/  /'
    failed=1
fi

if [ "$failed" -ne 0 ]; then
    exit 1
fi

echo "PASS: no premium imports in core; OTel confined to internal/telemetry"
echo "      (scanned: ${premium_scan[*]}; premium module: $premium_mod)"
