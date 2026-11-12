#!/usr/bin/env bash
# Build the code sandbox image (tools/code_sandbox/image) and prove it works offline.
#
# Owner decision 2026-11-10 (option a, decisions/code-sandbox-files-per-run-mount.md): the
# code sandbox runs with no network, so its data libraries are baked in. The catalog's
# SANDBOX_IMAGE default and the use-case suite use this tag; tools/code_sandbox/server.py
# refuses to run Python when it is missing (it never pulls or pip-installs).
#
# Usage: scripts/build-sandbox-image.sh [tag]     (default cambrian/code-sandbox:py311-1)
set -euo pipefail
cd "$(dirname "$0")/.."
IMAGE="${1:-${SANDBOX_IMAGE:-cambrian/code-sandbox:py311-1}}"
docker build -t "$IMAGE" tools/code_sandbox/image
# The way the sandbox runs it: no network, read-only root, /tmp writable, no capabilities.
MSYS_NO_PATHCONV=1 docker run --rm --network none --read-only --tmpfs /tmp --cap-drop ALL \
  --security-opt no-new-privileges:true "$IMAGE" \
  python -c "import pandas, numpy, matplotlib, openpyxl; print('sandbox image ok:', pandas.__version__, numpy.__version__, matplotlib.__version__, openpyxl.__version__)"
