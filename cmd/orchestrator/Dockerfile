# Cambrian orchestrator — multi-stage build.
# Build context is the repo root: docker build -f cmd/orchestrator/Dockerfile .
#
# Runtime base: gcr.io/distroless/python3-debian12:nonroot
# This is the smallest image that provides python3 (required for tool execution
# via cfg.Metabolism.PythonExecutable) and runs as a non-root user (uid 65532).
# It has no shell, so the HEALTHCHECK uses python3 directly in exec form.
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-X main.version=$(git describe --tags --always 2>/dev/null || echo dev)" \
    -o /app/orchestrator ./cmd/orchestrator

# Pre-create the data directory with nonroot ownership so the volume mount
# inherits correct permissions (distroless has no shell to chown at runtime).
RUN mkdir -p /app/data && chown -R 65532:65532 /app/data

FROM gcr.io/distroless/python3-debian12:nonroot
WORKDIR /app

# Copy the built binary.
COPY --from=build /app/orchestrator /app/orchestrator

# Copy committed runtime directories. Discovery handles missing dirs gracefully,
# so tools/ and skills/ are omitted when not present in the build context.
COPY --from=build /src/agents /app/agents

# Copy committed config reference files. .dockerignore excludes gitignored secrets
# (config.json, embedder.json, providers.json, mcp.json, *.local.json, .env).
# The operator mounts their real configs (including config.json) at /etc/cambrian.
COPY --from=build /src/configs /app/configs

# Data directory — pre-created with nonroot ownership.
COPY --from=build /app/data /app/data

# Tell the config loader where to find the operator-provided config.json.
# Secondary config layers (tuning.json, embedder.json, providers.json, etc.)
# are resolved relative to this path, so the operator should place ALL needed
# config files in the mounted /etc/cambrian directory.
ENV CAMBRIAN_CONFIG=/etc/cambrian/config.json

# gRPC server port (default 50051) and health/readiness port (default 8080).
EXPOSE 50051 8080

# Distroless has no curl/wget, so we use python3's urllib for the health probe.
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD ["python3", "-c", "import urllib.request; urllib.request.urlopen('http://localhost:8080/healthz').read()"]

ENTRYPOINT ["/app/orchestrator"]
