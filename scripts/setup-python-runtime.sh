#!/usr/bin/env bash
# setup-python-runtime.sh — build the Cambrian Python runtime (ADR-0023).
#
# Creates a repo-local .venv, installs the cambrian-agent-sdk (editable) with
# dev extras, and verifies the SDK + all 5 agents import. The integration
# benchmark and run-all-tests.ps1 auto-detect this venv.
#
# Usage: ./scripts/setup-python-runtime.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VENV="$ROOT/.venv"
PY="$VENV/bin/python"

echo "=== Cambrian Python Runtime Setup ==="

if [ ! -x "$PY" ]; then
  echo "Creating venv at $VENV ..."
  python3 -m venv "$VENV"
else
  echo "venv already exists at $VENV"
fi

echo "Upgrading pip ..."
"$PY" -m pip install --upgrade pip --quiet

echo "Installing cambrian-agent-sdk (editable, with dev extras) ..."
"$PY" -m pip install -e "$ROOT/python-sdk[dev]" --quiet

echo "Verifying SDK import ..."
"$PY" -c "from cambrian_agent_sdk import Agent, assemble_context, build_prompt, find_step_ref, extract_code_block, BudgetExceededError; print('  SDK OK')"

echo "Verifying all 3 cognitive agents import ..."
"$PY" -c "import sys; sys.path.insert(0, '$ROOT/agents'); import code_generator_agent, summariser_agent, analyst_agent; print('  3 agents OK')"

echo ""
echo "=== Done ==="
echo "Python runtime: $PY"
echo ""
echo "To run the orchestrator with this runtime, export:"
echo "  export CAMBRIAN_PYTHON='$PY'"
echo "  export CAMBRIAN_AGENTS_DIR='$ROOT/agents'"
echo "  export CAMBRIAN_DATA_DIR='$ROOT/data'"
