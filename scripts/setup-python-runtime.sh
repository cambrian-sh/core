#!/usr/bin/env bash
# setup-python-runtime.sh — build the kernel-managed sandbox Python env
# (ADR-0157 D7).
#
# Python agents and the Python system tools launch ONLY on the kernel-managed
# env (execution.agents.python.root, default <kernel dir>/runtime/python): the
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
# dev tree with a sibling sdk/.
#
# Usage: ./scripts/setup-python-runtime.sh [SDK_DIR]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SDK="${1:-}"

args=(python-env build --dir "$ROOT")
if [ -n "$SDK" ]; then
  if [ ! -f "$SDK/pyproject.toml" ]; then
    echo "no agent SDK checkout at $SDK (omit the argument to build the release env)" >&2
    exit 2
  fi
  args+=(--sdk-dir "$SDK")
fi

cd "$ROOT"
if [ -n "$SDK" ]; then
  echo "=== Building the kernel-managed sandbox Python env (dev SDK from $SDK) ==="
else
  echo "=== Building the kernel-managed sandbox Python env (release base lock) ==="
fi
go run ./cmd/orchestrator "${args[@]}"
echo ""
go run ./cmd/orchestrator python-env status --dir "$ROOT"
