# syntax=docker/dockerfile:1
# Cambrian kernel image — PLAT-04 / ADR-0066.
#
# The kernel (Go) and its agents (Python) COLOCATE: the Go binary spawns the agents in
# agents/ as subprocesses on the kernel-managed sandbox Python env (ADR-0157 D7), which
# is built INTO the image here, so a container boots Python agents at once, air-gapped
# included, and never downloads an env at first boot (ENG-2034). The env is built from
# the RELEASE base lock alone (the published agent SDK it pins ships the agent channel
# since ENG-2033), so the image needs nothing outside cambrian-core/ (ENG-2077). The
# build context stays the monorepo root, which the workspace compose files and the
# premium image already use; Dockerfile.dockerignore next to this file narrows it to
# cambrian-core/ minus local state (the managed env, config store and secrets):
#
#     docker build -f cambrian-core/Dockerfile -t ghcr.io/cambrian-sh/core:dev .

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

########################## Runtime ##########################
# python:3.12-slim only for the HEALTHCHECK below: agents and the Python system tools
# never run on this interpreter, only on the managed env's pinned CPython.
FROM python:3.12-slim AS runtime

# Least privilege (PLAT-08): a non-root system user owns the app.
RUN groupadd --system cambrian \
 && useradd --system --gid cambrian --home-dir /app --shell /usr/sbin/nologin cambrian

# PostgreSQL 16 client tools (pg_dump / pg_restore): the migration runner snapshots the
# database before a destructive migration and refuses without one (ENG-681), and
# `orchestrator backup` dumps through them. Debian's own postgresql-client is older
# than the pg16 server and cannot dump it, so they come from the PGDG apt repository
# (ENG-1833). The premium image inherits them (FROM the core image).
ARG PG_CLIENT_MAJOR=16
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && install -d /usr/share/postgresql-common/pgdg \
 && curl -fsSL -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc \
      https://www.postgresql.org/media/keys/ACCC4CF8.asc \
 && . /etc/os-release \
 && echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt ${VERSION_CODENAME}-pgdg main" \
      > /etc/apt/sources.list.d/pgdg.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends "postgresql-client-${PG_CLIENT_MAJOR}" \
 && apt-get purge -y --auto-remove curl \
 && rm -rf /var/lib/apt/lists/* \
 && pg_dump --version

# bubblewrap (bwrap): the native Linux agent runner's fs_confine, hide_state and
# network controls (ADR-0157 D1, ENG-2062). It is not setuid: the non-root kernel runs
# it in an unprivileged user namespace, which the container engine must allow. Docker's
# default seccomp profile does not, so the kernel container runs with
# deploy/seccomp-bwrap.json (docs/container-sandbox.md has the Docker, Podman and
# Kubernetes settings). Without them the agent containment report names fs_confine and
# hide_state missing (first-party degraded, third-party refused) — never enforced.
RUN apt-get update \
 && apt-get install -y --no-install-recommends bubblewrap \
 && rm -rf /var/lib/apt/lists/* \
 && bwrap --version

WORKDIR /app

# Kernel binary (+ the pagerank worker) + agents + reference configs + migrations.
COPY --from=build /out/orchestrator /app/orchestrator
COPY --from=build /out/pagerank-recompute /app/pagerank-recompute
COPY cambrian-core/agents/ /app/agents/
# The prompt library (ADR-0139 C4): the kernel embeds it, but the Python system agents
# (chat_agent, retrieval_agent) read it from disk — the nearest ancestor prompts/ of
# their file, i.e. /app/prompts. Without it they fail at start.
COPY cambrian-core/prompts/ /app/prompts/
COPY cambrian-core/configs/ /app/configs/
COPY cambrian-core/db/ /app/db/
# System skills (ADR-0066 / ADR-0139): the kernel loads them from the WORKING-DIRECTORY
# relative "skills" dir (app.go skilldiscovery.LoadRegistry("skills", …)), and WORKDIR
# is /app — without this copy a containerised kernel has no system skills at all.
# tools/ is not copied: it holds no files in the repo (tool manifests are user-supplied
# and mounted), and LoadRegistry treats a missing dir as empty.
COPY cambrian-core/skills/ /app/skills/

# data_dir (bbolt + blobs) and the agent UDS socket dir (a tmpfs is mounted here in
# compose — PLAT-08).
#
# /app/state holds the ADR-0101 config and secret store (compose points
# CAMBRIAN_CONFIG_STORE at /app/state/config.db). It must be WRITABLE and must live
# neither in the data directory (a corpus reset truncates that, silently un-configuring
# the deployment) nor in the config bundle (compose mounts it read-only). Created here,
# owned by the runtime user, because Docker seeds an empty named volume from the image
# directory INCLUDING its ownership — without it the volume arrives root-owned and the
# kernel refuses to boot. Same arrangement as cambrian-premium/Dockerfile.
RUN mkdir -p /app/data /app/state /app/runtime /run/cambrian \
 && chown -R cambrian:cambrian /app /run/cambrian

# The kernel-managed sandbox Python env (ADR-0157 D7), built at image build time by the
# same CLI an operator runs: the pinned, hash-verified CPython and uv, the release's
# hashed base lock (agents/base.lock, embedded in the binary), wheels only. Its root is
# the default execution.agents.python.root, <base dir>/runtime/python = /app/runtime/
# python: owned by the runtime user (built as it), outside /app/data (a corpus reset
# truncates that) and /app/state (the config store volume). No --sdk-dir: the generation
# is exactly what a released install builds (the lock's published SDK, the pin table's
# model assets), so the kernel's boot check finds it healthy without a dev-SDK
# exception and builds nothing at boot (ENG-2077; --sdk-dir stays for dev trees and CI).
# downloads/ and cache/ (the verified archives and the wheel cache, several GB) only
# serve rebuilds: an image is rebuilt, not repaired, so they are dropped in this layer.
USER cambrian
RUN /app/orchestrator python-env build --dir /app \
 && rm -rf /app/runtime/python/downloads /app/runtime/python/cache \
 && /app/orchestrator python-env status --dir /app

# Optional model pre-fetch (HF reranker/docling weights baked into the image cache), so
# a running container makes NO runtime model download. Off by default to keep the base
# image buildable without a multi-GB pull; enable with --build-arg PREFETCH_MODELS=1.
# Runs on the managed env's interpreter (the only Python the agents have), into the
# agents' shared model cache: the kernel points every agent's HF_HOME at
# <base dir>/runtime/models/huggingface (execution.agents.model_cache_dir, ADR-0157 S7),
# so weights baked anywhere else are never seen by a sandboxed agent.
ARG PREFETCH_MODELS=0
ENV HF_HOME=/app/runtime/models/huggingface
RUN if [ "$PREFETCH_MODELS" = "1" ]; then       py=$(/app/orchestrator python-env status --dir /app | awk '/^  interpreter:/{print $2}')       && "$py" -c "from huggingface_hub import snapshot_download; snapshot_download('BAAI/bge-reranker-v2-m3')";     fi
USER root

# Wiring defaults; secrets + DB/embedder endpoints come from the compose env (koanf
# CAMBRIAN_* overrides) or a mounted configs volume — never baked into the image.
# metabolism.agents_dir: CAMBRIAN_AGENTS_DIR, set here before, is not a config key
# (internal/config/unknown_keys.go), so an image run without a mounted config.json
# discovered no agents at all.
ENV CAMBRIAN_METABOLISM__AGENTS_DIR=/app/agents \
    CAMBRIAN_CONFIG=/app/configs/config.json \
    CAMBRIAN_STORAGE__DATA_DIR=/app/data \
    CAMBRIAN_SERVER__HEALTHZ_PORT=8090

USER cambrian
VOLUME ["/app/state"]
EXPOSE 50051 8090

# Liveness/readiness via the PLAT-03 /healthz shim (ADR-0065).
HEALTHCHECK --interval=15s --timeout=3s --start-period=45s --retries=5 \
  CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://localhost:8090/healthz').status==200 else 1)"

# storage.auto_migrate (default true) applies the DB migrations on boot (PLAT-02).
ENTRYPOINT ["/app/orchestrator"]
