---
id: 0066
title: Container Distribution (Dockerfile, top-level compose, GHCR publish, GPU variant)
status: Accepted
date: 2026-07-16
supersedes: []
superseded_by: []
depends_on:
  - 0064-embedded-db-migration-runner
  - 0065-grpc-health-service
  - 0057-open-core-boundary
---

# ADR-0066: Container Distribution

## Status

Accepted

## Context

This is PLAT-04 (`distribution-production-readiness.md` §4.1). The primary distribution
channel does not exist: no kernel image, no full-stack compose, and models download
lazily at first use (multi-GB cold start + a runtime egress dependency). PLAT-01 (union
lockfile), PLAT-02 (migration runner), and PLAT-03 (health service) are the prerequisites
this ADR assembles into a shippable image.

## Decision

### Multi-stage image, kernel + agents colocated

`cambrian-core/Dockerfile` is a two-stage build: a `golang:1.25` builder compiles the
kernel (and the pagerank worker) with `CGO_ENABLED=0`; a `python:3.12-slim` runtime
carries the binary plus `agents/` and a venv built from the PLAT-01 union lockfile. The
kernel spawns agents as subprocesses (`CAMBRIAN_PYTHON` → the baked venv), so they must
colocate. A non-root `cambrian` user owns the app (PLAT-08); the agent UDS socket dir
(`/run/cambrian`) is a mounted tmpfs in compose.

### Monorepo-root build context (the SDK lives in a sibling repo)

The agents import `cambrian_agent_sdk`, which is the sibling `sdk/` repo — outside
`cambrian-core/`. So the **build context is the monorepo root**: the Dockerfile COPYs
`cambrian-core/` and `sdk/`, installs the SDK from source (the lockfile pins
`cambrian-agent-sdk` but it is a private package, so that line is filtered out of the
lockfile pass), then installs the rest of the lockfile. The GHCR workflow checks out both
repos under one workspace to reproduce this layout.

### GPU support = a wheel swap, not a code change

The `-cuda` variant is produced entirely by build args: `TORCH_INDEX_URL` points pip at
the CUDA torch wheels. Nothing in the Dockerfile or the kernel branches on GPU — the same
image recipe yields CPU and CUDA variants. (A future `nvidia/cuda` runtime base is a
drop-in for deployments that need the CUDA userspace.)

### Model pre-fetch is opt-in at build

HF model weights (reranker/docling) are baked into the image HF cache **only** when
`--build-arg PREFETCH_MODELS=1` — the default is off so the base image builds without a
multi-GB pull. The release workflow sets it to 1. Ollama models are a compose-side pull
(the `ollama` profile), not baked.

### Top-level compose + health-gated wiring

A monorepo-root `docker-compose.yml` runs `cambrian-db` (pgvector) + `kernel` +
`pagerank-recompute`, with an optional in-compose `ollama` (profile-gated; the kernel
otherwise talks to the host's ollama at `host.docker.internal:11434`, with a Linux
`extra_hosts` mapping). Wiring is koanf env (`CAMBRIAN_*`) — DB host, ports, ollama
endpoint — with secrets from `.env`, never inline. The kernel `depends_on` the DB's
`service_healthy`; the migration runner (PLAT-02, `auto_migrate` default) applies the
schema on boot; the kernel's own healthcheck uses the PLAT-03 `/healthz` shim. Volumes
persist `data_dir` and the pgdata; `configs/` is a read-only mount so operators bring
their own `config.json` / `embedder.json` (the image ships only the `.example` files).

### Publish on tag

`.github/workflows/publish-image.yml` builds and pushes `ghcr.io/cambrian-sh/core` (CPU)
and the `-cuda` variant on a `v*` tag, with OCI labels, provenance, and an SBOM. The
premium image is a separate private GHCR repo with the same compose shape (not in this
OSS repo).

## Consequences

**Positive.**
- A real primary distribution channel: `docker compose up` brings up a health-gated
  full stack; the benchmark transport (kernel as a black box) can point at the container.
- No runtime model downloads when the image is built with `PREFETCH_MODELS=1`.
- GPU is a wheel swap — one recipe, two variants, zero code divergence.
- No secrets or local configs in the image; wiring is env + mounts.

**Negative / costs.**
- The build context is the monorepo root (the SDK is a sibling repo); a
  `Dockerfile.dockerignore` must exclude the other repos and Windows/tooling special
  files (e.g. `.codegraph` symlinks) or the context load fails.
- The runtime stage installs torch/docling/transformers — a multi-GB image and a slow
  cold build; the `PREFETCH_MODELS` layer adds several more GB.
- Full validation (clean-machine `docker compose up` with models, image-size targets,
  GHCR tag push) is release-time — it needs the model pulls, a registry token, and a host
  with the GPU userspace for the `-cuda` variant.

**Neutral.**
- The pagerank worker ships in the same image (a second small Go binary) rather than its
  own image, so the compose reuses one image for both services.

## References

- PLAT-04 (`docs/backlog/PLAT-04-dockerfile-compose-ghcr.md`);
  `distribution-production-readiness.md` §4.1, §3 GPU matrix, §7 topology.
- ADR-0064 (migrate-on-boot), ADR-0065 (healthcheck), PLAT-01 (union lockfile),
  PLAT-08 (non-root + tmpfs socket dir).
