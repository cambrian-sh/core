#!/usr/bin/env bash
# ADR-0057 premium-leak guard: fail if any OSS Go file imports a premium path.
# The OSS module must never depend on internal/premium (gone) or the premium module.
set -euo pipefail

cd "$(dirname "$0")/.."

# Match import lines referencing a forbidden premium path (incl. _test.go).
hits=$(grep -rn --include='*.go' -E '"github\.com/cambrian-sh/(cambrian-runtime/internal/premium|cambrian-premium)' . \
  | grep -vE '/\.(claude|venv)/' || true)

if [ -n "$hits" ]; then
  echo "FAIL: premium import found in OSS source:"
  echo "$hits"
  exit 1
fi
echo "PASS: no premium imports in OSS source."
