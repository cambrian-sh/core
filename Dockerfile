# syntax=docker/dockerfile:1
# Cambrian kernel image — PLAT-04 / ADR-0066.
#
# The kernel (Go) and its agents (Python) COLOCATE: the Go binary spawns the agents in
# agents/ as subprocesses using the baked venv (CAMBRIAN_PYTHON). Because the agent SDK
# lives in the sibling `sdk/` repo, the build context is the MONOREPO ROOT:
#
#     docker build -f cambrian-core/Dockerfile -t ghcr.io/cambrian-sh/core:dev .
#
# GPU support is a wheel swap, not a code change: build the `-cuda` variant with
# --build-arg TORCH_INDEX_URL / a CUDA base (see docs) — nothing here branches on it.

########################## Go builder ##########################
FROM golang:1.25 AS build
WORKDIR /src
COPY cambrian-core/go.mod cambrian-core/go.sum ./
RUN go mod download
COPY cambrian-core/ ./
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/orchestrator ./cmd/orchestrator \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" \
    -o /out/pagerank-recompute ./cmd/pagerank-recompute

########################## Python runtime ##########################
FROM python:3.12-slim AS runtime

# Least privilege (PLAT-08): a non-root system user owns the app.
RUN groupadd --system cambrian \
 && useradd --system --gid cambrian --home-dir /app --shell /usr/sbin/nologin cambrian

ENV VENV=/opt/venv
ENV PATH="$VENV/bin:$PATH"
RUN python -m venv "$VENV" && "$VENV/bin/pip" install --no-cache-dir --upgrade pip

WORKDIR /app

# The agent SDK first, from source — the union lockfile pins `cambrian-agent-sdk` but it
# is a private package, so it is installed from sdk/ and skipped from the lockfile pass.
COPY sdk/ /app/sdk/
RUN "$VENV/bin/pip" install --no-cache-dir /app/sdk

# The union lockfile (PLAT-01) — the transitive closure of every agent's deps.
# TORCH_INDEX_URL lets the -cuda variant pull CUDA wheels without a Dockerfile change.
ARG TORCH_INDEX_URL=""
COPY cambrian-core/agents/requirements.lock /app/agents/requirements.lock
RUN grep -vi '^cambrian-agent-sdk' /app/agents/requirements.lock > /tmp/req.txt \
 && if [ -n "$TORCH_INDEX_URL" ]; then EXTRA="--extra-index-url $TORCH_INDEX_URL"; else EXTRA=""; fi \
 && "$VENV/bin/pip" install --no-cache-dir $EXTRA -r /tmp/req.txt

# Kernel binary (+ the pagerank worker) + agents + reference configs + migrations.
COPY --from=build /out/orchestrator /app/orchestrator
COPY --from=build /out/pagerank-recompute /app/pagerank-recompute
COPY cambrian-core/agents/ /app/agents/
COPY cambrian-core/configs/ /app/configs/
COPY cambrian-core/db/ /app/db/

# Optional model pre-fetch (HF reranker/docling weights baked into the image cache), so
# a running container makes NO runtime model download. Off by default to keep the base
# image buildable without a multi-GB pull; enable with --build-arg PREFETCH_MODELS=1.
ARG PREFETCH_MODELS=0
ENV HF_HOME=/app/.cache/huggingface
RUN if [ "$PREFETCH_MODELS" = "1" ]; then \
      "$VENV/bin/python" -c "from huggingface_hub import snapshot_download; snapshot_download('BAAI/bge-reranker-v2-m3')"; \
    fi

# data_dir (bbolt + blobs) and the agent UDS socket dir (a tmpfs is mounted here in
# compose — PLAT-08).
RUN mkdir -p /app/data /run/cambrian \
 && chown -R cambrian:cambrian /app /run/cambrian "$VENV"

# Wiring defaults; secrets + DB/embedder endpoints come from the compose env (koanf
# CAMBRIAN_* overrides) or a mounted configs volume — never baked into the image.
ENV CAMBRIAN_PYTHON="$VENV/bin/python" \
    CAMBRIAN_AGENTS_DIR=/app/agents \
    CAMBRIAN_CONFIG=/app/configs/config.json \
    CAMBRIAN_STORAGE__DATA_DIR=/app/data \
    CAMBRIAN_SERVER__HEALTHZ_PORT=8090

USER cambrian
EXPOSE 50051 8090

# Liveness/readiness via the PLAT-03 /healthz shim (ADR-0065).
HEALTHCHECK --interval=15s --timeout=3s --start-period=45s --retries=5 \
  CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://localhost:8090/healthz').status==200 else 1)"

# storage.auto_migrate (default true) applies the DB migrations on boot (PLAT-02).
ENTRYPOINT ["/app/orchestrator"]
