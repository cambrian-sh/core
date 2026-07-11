# Deployment Runbook — cambrian-runtime

Single-host Docker Compose deployment for the Cambrian Substrate (OSS kernel).
This runbook assumes the Docker workstream has produced:

- `cmd/orchestrator/Dockerfile` — the orchestrator image build
- `docker-compose.yml` (repo root) — the Compose stack
- `.dockerignore`

> **Port defaults** (set by the Docker workstream; verify in `docker-compose.yml`):
> - gRPC: `50051`
> - Health / HTTP: `8080`
> - Prometheus metrics: configured via `telemetry.prometheus_port` in `config.json`

---

## Prerequisites

- **Docker** ≥ 24.0 and **Docker Compose** ≥ 2.20
- **Go 1.25.5** (only if building from source; not needed for image-only deploy)
- **Postgres 15+** with `pgvector` extension (can run inside Compose or externally)
- A Linux host with `amd64` or `arm64` architecture

---

## 1. Build the image

From the repo root:

```bash
docker build -f cmd/orchestrator/Dockerfile -t cambrian-runtime:latest .
```

Or, if the project provides a Make target:

```bash
make docker-build
```

> The image is **build-only** — no registry push is configured. The `.goreleaser.yaml` in this repo is also build-only (`release.disable: true`).

---

## 2. Prepare configuration

### 2.1 Copy templates

```bash
sudo mkdir -p /etc/cambrian
sudo cp configs/config.example.json /etc/cambrian/config.json
sudo cp configs/embedder.json.example /etc/cambrian/embedder.json
sudo cp configs/providers.json.example /etc/cambrian/providers.json
cp .env.example .env
```

### 2.2 Edit `/etc/cambrian/config.json`

At minimum, set:

- `database.host`, `database.port`, `database.user`, `database.dbname`
- `database.password` — leave empty here and set via env (see §2.3)
- `metabolism.python_executable` — point at the Python runtime inside the container (e.g. `/app/.venv/bin/python`) or a mounted venv
- `metabolism.agents_dir` — path to the `agents/` tree (mount as a volume in Compose)
- `server.port` — `50051` (gRPC; must match the Compose port mapping)
- `telemetry.prometheus_port` — e.g. `9090` (optional; set `0` to disable)
- `telemetry.otlp_endpoint` — e.g. `http://jaeger:4317` (optional; leave empty to disable)

### 2.3 Secrets in `.env`

Fill `.env` with real values. The variables referenced in `config.json` via `api_key_env` must match:

```bash
# LLM provider keys (examples from .env.example)
OPENCODE_API_KEY=sk-...
GEMINI_API_KEY=...

# Database password (overrides config.json via the CAMBRIAN_* env layer)
CAMBRIAN_DATABASE__PASSWORD=...
```

> The `CAMBRIAN_*` convention uses `__` as the hierarchy separator. Env vars are layer 11 (highest priority) in the 11-layer Koanf pipeline.

### 2.4 Set `CAMBRIAN_CONFIG`

The orchestrator and CLI tools read the config path from the environment:

```bash
export CAMBRIAN_CONFIG=/etc/cambrian/config.json
```

Pass this into the container via Compose:

```yaml
services:
  orchestrator:
    image: cambrian-runtime:latest
    environment:
      - CAMBRIAN_CONFIG=/etc/cambrian/config.json
    volumes:
      - /etc/cambrian:/etc/cambrian:ro
      - ./agents:/app/agents:ro
      - ./data:/app/data
    ports:
      - "50051:50051"
      - "8080:8080"
```

---

## 3. Run the stack

```bash
docker compose up -d
```

Wait for Postgres to be ready (the Compose healthcheck or an init container should gate the orchestrator start).

---

## 4. Verify health

The orchestrator exposes HTTP health endpoints on the **HealthPort** (default `8080`):

```bash
curl -sf http://localhost:8080/healthz   # liveness
curl -sf http://localhost:8080/readyz  # readiness (gates on DB + agent manager)
```

Both return `200 OK` when healthy.

---

## 5. Observability

### Prometheus metrics

If `telemetry.prometheus_port` is non-zero (e.g. `9090`), scrape:

```bash
curl http://localhost:9090/metrics
```

### OTLP traces

Set `telemetry.otlp_endpoint` in `config.json` (e.g. `http://jaeger:4317`). The runtime exports traces via the OpenTelemetry bridge in `internal/telemetry/`.

---

## 6. Schema versioning & migrations

The runtime auto-migrates Postgres on boot using the embedded migration set in `db/migrations/`. A schema version table tracks the current revision.

For manual or CI-driven migration, run the `migrate` subcommand (available as a standalone binary built from the same source tree):

```bash
./bin/migrate --config $CAMBRIAN_CONFIG up
```

> The schema version table and `cmd/migrate` are delivered by the core-wiring workstream.

---

## 7. Backups

### Postgres

Use the provided helper (or adapt it):

```bash
scripts/backup-postgres.sh /etc/cambrian/config.json /backups/cambrian-$(date +%F).sql.gz
```

### BBolt (agent registry)

```bash
scripts/backup-bbolt.sh data/agents.db /backups/agents-$(date +%F).db.gz
```

### Crontab example

```cron
0 3 * * * /opt/cambrian/scripts/backup-postgres.sh /etc/cambrian/config.json /backups/cambrian-$(date +\%F).sql.gz
0 3 * * * /opt/cambrian/scripts/backup-bbolt.sh /opt/cambrian/data/agents.db /backups/agents-$(date +\%F).db.gz
```

> Backups are **operator-run** — there is no built-in scheduler.

---

## 8. Upgrades & rollback

1. **Pull / build the new image**:
   ```bash
   docker build -f cmd/orchestrator/Dockerfile -t cambrian-runtime:latest .
   ```

2. **Run migrations** (if the release added migrations):
   ```bash
   docker compose run --rm orchestrator ./bin/migrate --config $CAMBRIAN_CONFIG up
   ```

3. **Restart the stack**:
   ```bash
   docker compose up -d
   ```

4. **Rollback** (image + DB):
   - Re-tag the previous image and `docker compose up -d`.
   - If a migration was applied, restore from the pre-upgrade backup; there is no down-migration path.

---

## 9. Logs

```bash
docker compose logs -f orchestrator
docker compose logs -f pagerank-recompute   # if running as a separate service
```

---

## 10. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `Configuration error — database.host is required` | `CAMBRIAN_CONFIG` not set or config file missing | Mount `/etc/cambrian/config.json` and set `CAMBRIAN_CONFIG=/etc/cambrian/config.json` |
| `database.password is required` | `CAMBRIAN_DATABASE__PASSWORD` not set | Add it to `.env` or the Compose `environment` block |
| `pgvector connect failed` | Postgres not ready or `pgvector` extension missing | Ensure the DB container passes its healthcheck before the orchestrator starts; run `CREATE EXTENSION IF NOT EXISTS vector;` |
| `sslmode` errors | The `database.sslmode` field (available via the core-wiring workstream) is unset and Postgres requires SSL | Set `database.sslmode` in `config.json` to `disable` (local) or `require` (remote) |
| `connection refused` on `:50051` | Port mapping mismatch or container crash | Check `docker compose ps` and `docker compose logs orchestrator` |
| `/healthz` returns 503 | DB or agent manager not ready | Wait for the readiness gate; check Postgres logs |
| Agent boot loops | Missing Python venv or `agents_dir` not mounted | Ensure `metabolism.python_executable` points at a valid interpreter inside the container |

---

## Assumptions from parallel workstreams

| Artifact | Assumed path / default | Owner |
|---|---|---|
| Dockerfile | `cmd/orchestrator/Dockerfile` | Docker workstream |
| Compose file | `docker-compose.yml` (repo root) | Docker workstream |
| Health port | `8080` | Docker + core-wiring workstreams |
| gRPC port | `50051` | Docker workstream |
| `CAMBRIAN_CONFIG` | `/etc/cambrian/config.json` | This runbook |
| Health endpoints (`/healthz`, `/readyz`) | Available (gRPC recovery interceptor + HTTP health) | Core-wiring workstream |
| Prometheus `/metrics` | Available when `telemetry.prometheus_port > 0` | Core-wiring workstream |
| Schema version table + `cmd/migrate` | Available | Core-wiring workstream |
| DB `sslmode` config | Available in `config.json` | Core-wiring workstream |
| Backup scripts | `scripts/backup-postgres.sh`, `scripts/backup-bbolt.sh` | Existing / ops |
