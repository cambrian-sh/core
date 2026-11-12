# Cambrian code-sandbox image (owner decision 2026-11-10, option a;
# cambrian-knowledge/decisions/code-sandbox-files-per-run-mount.md).
#
# The code sandbox runs every execute_code call in a fresh container with NO network
# (SANDBOX_NETWORK_MODE=none), so a library cannot be pip-installed on demand. This
# image is the one llm-sandbox 0.3.46 uses for Python today, with the common data
# libraries preinstalled at pinned versions, so analysis code runs offline.
#
# Build:   make sandbox-image            (or scripts/build-sandbox-image.sh)
# Name:    cambrian/code-sandbox:py311-1 (the catalog's SANDBOX_IMAGE default and the
#          use-case suite's; bump the tag with any change to this file)
#
# The base is pinned by digest: ghcr.io/vndee/sandbox-python-311-bullseye as pulled
# 2026-11-10 (llm_sandbox.const.DefaultImage.PYTHON; image version label 0.3.44).
FROM ghcr.io/vndee/sandbox-python-311-bullseye@sha256:07cbf8eed46264b1f3eeca2bd3dac7e4af64c991bf64c0fa79b8ace3c3868dda

# Installed into the system interpreter, which tools/code_sandbox/server.py runs
# directly (it skips llm-sandbox's per-call venv + pip upgrade, which need network).
RUN pip install --no-cache-dir \
        pandas==2.2.3 \
        numpy==2.2.6 \
        matplotlib==3.10.3 \
        openpyxl==3.1.5 \
    && python -c "import pandas, numpy, matplotlib, openpyxl"

# matplotlib renders headless and keeps its cache in /tmp (the container's root
# filesystem is read-only under SANDBOX_READ_ONLY=true; the server gives /tmp and
# the working directory as tmpfs).
ENV MPLBACKEND=Agg \
    MPLCONFIGDIR=/tmp/matplotlib \
    PYTHONDONTWRITEBYTECODE=1

# Read by tools/code_sandbox/server.py: a requested library on this list is already
# here (dropped from the install), any other is refused while the network is off.
LABEL cambrian.sandbox.preinstalled="pandas,numpy,matplotlib,openpyxl" \
      cambrian.sandbox.version="py311-1" \
      org.opencontainers.image.title="cambrian code sandbox" \
      org.opencontainers.image.description="llm-sandbox Python 3.11 runtime with pandas, numpy, matplotlib, openpyxl preinstalled; runs with no network"

WORKDIR /sandbox
