#!/usr/bin/env bash
# setup-python-runtime.sh — build the kernel-managed sandbox Python env for a
# dev tree (ADR-0157 D7).
#
# Python agents and the Python system tools launch ONLY on the kernel-managed
# env (execution.agents.python.root, default <kernel dir>/runtime/python): the
# pinned, hash-verified CPython and uv, the release's hashed base lock, wheels
# only. There is no host-Python or repo-venv fallback and no CAMBRIAN_PYTHON
# (retired with metabolism.python_executable, config schema v4). This script
# runs the same build an operator runs (`cambrian-orchestrator python-env
# build`), with the agent SDK built from the sibling sdk/ checkout, as
# `cambrian-orchestrator setup` does in a dev tree.
#
# Usage: ./scripts/setup-python-runtime.sh [SDK_DIR]   (default: ../sdk)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SDK="${1:-$ROOT/../sdk}"

if [ ! -f "$SDK/pyproject.toml" ]; then
  echo "no agent SDK checkout at $SDK (pass its directory as the first argument)" >&2
  exit 2
fi

cd "$ROOT"
echo "=== Building the kernel-managed sandbox Python env (SDK from $SDK) ==="
go run ./cmd/orchestrator python-env build --dir "$ROOT" --sdk-dir "$SDK"
echo ""
go run ./cmd/orchestrator python-env status --dir "$ROOT"
