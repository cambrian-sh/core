---
id: 0122
title: Self-Installing Kernel (`setup` subcommand, embedded assets, lifecycle)
status: Accepted
date: 2026-08-11
supersedes: []
superseded_by: []
depends_on:
  - 0064-embedded-db-migration-runner
  - 0065-grpc-health-service
  - 0066-container-distribution
  - 0101-config-and-secret-store
---

# ADR-0122: Self-Installing Kernel

## Status

Accepted

## Context

Installation lived in the Bun CLI (`cambrian init`, CLI-004/005) fed by
source-mode install scripts that clone three repos and build locally. The owner
decision is to **retire the Bun CLI completely**. That leaves no install path,
no lifecycle path (`cambrian start/stop/status`), and a distribution story
(CLI-003 GitHub Releases) that was specified but never implemented — while the
kernel itself is already a CGO-free, trivially cross-compilable Go binary.

The requirement, verbatim from the owner: an installer that installs and
configures missing dependencies, makes the needed configuration for the given
binary, and runs the binary. DB migrations, Python environment, paths, LLM
config — with LLM config explicitly optional (configurable later from the
operator console).

## Decision

### The binary is the installer

`cambrian-orchestrator setup` (dispatched in `cmd/orchestrator/main.go`,
implemented in `app/` next to `RunMigrate` so the premium binary can reuse it)
bootstraps a working deployment from a single downloaded file:

1. **Install** — materialize `<prefix>/{bin,configs,data,agents,logs,db}`
   (prefix: `--home` > `$CAMBRIAN_HOME` > `~/.cambrian`), copy the running
   executable into `<prefix>/bin`, chdir to the prefix (so `ResolveBaseDir`'s
   CWD sentinel resolves the bundle regardless of where the binary was run
   from).
2. **Preflight** — probe docker/ollama. Warnings only; nothing here degrades.
3. **Database** — local-Docker vs existing-Postgres choice. Reachability is a
   real pgx connect+ping (TCP opens before Postgres can auth). Local path
   generates a reduced compose file (pgvector/pgvector:pg16 only — the
   pagerank-recompute worker builds from the source tree and is omitted; the
   kernel only reads its table). pgvector availability is checked explicitly so
   a plain-Postgres remote fails early with a clear message, not mid-migration.
4. **Python runtime** — agents are unpacked from the embedded FS; uv is found
   or **downloaded into `<prefix>/bin`** (GitHub latest, per-platform asset),
   and `uv venv --python 3.12` provisions CPython itself on a python-less host.
   SDK from PyPI (`cambrian-agent-sdk`) or editable from a dev tree; union
   lockfile install; PLAT-01 per-agent import self-check.
5. **Models** — bge-large pull, reranker + docling pre-fetch. Never degrades.
6. **Config** — merge-write `configs/config.json` (setup owns database/
   storage/metabolism paths; hand-edits and existing embedder blocks are
   preserved), ensure `tuning.json` (the sentinel). **LLM providers are
   deliberately not configured** — the kernel boots without them; keys arrive
   later via the operator console (ADR-0101 store) or `providers.json`.
7. **Migrate** — `RunMigrate(ctx, ["up"])` in-process (ADR-0064).
8. **Operator account** — the console's only credential source is
   `CAMBRIAN_OPERATOR_USER`/`CAMBRIAN_OPERATOR_PASSWORD` at boot (ADR-0047
   D13); with neither set, no login can ever succeed, so a fresh install had
   an unloginnable console. Setup seeds that exact mechanism: prompt (env
   values as defaults), write into `<prefix>/.env` (0600) — which `app.Run`
   loads from the resolved base dir — and under `--yes` with no env,
   **generate a random password and print it once**. Never a fixed default
   (D13 secure-by-default). Idempotent: existing `.env` entries are never
   rewritten; rotation = edit `.env` + restart. Residual, pre-existing and
   deliberately untouched here: `StaticIdentity` compares plaintext,
   non-constant-time, with no durable account store — that is ADR-0047's V1
   posture, and hardening it is an auth change, not an installer change.
9. **Run** — by default fire-and-forget (owner decision: setup must not block
   on a kernel restart): spawn detached, check only that the process survived
   ~1.5s (an instant crash still fails loudly with the log path), and defer
   readiness to `status`. `--wait` restores the blocking poll below, which
   E2E/CI should use — spawn the installed binary detached (pid file
   `orchestrator.pid`, logs under `logs/`), then poll `grpc.health.v1` overall
   status. Because that status is DB-gated (ADR-0065), SERVING proves
   boot + config + database + migrations in one signal.

Every step is check-then-do and idempotent (inherited from CLI-005's sequential
runner); a degraded run exits 2 and a re-run repairs. Flags: `--yes`, `--home`,
`--skip-models`, `--skip-python`, `--no-start`. DB prompt defaults honor
pre-set `CAMBRIAN_DATABASE__*` env, so an unattended `setup --yes` can target a
specific database.

Two rules hardened by the live E2E (both regression-tested):
- **An unparseable `config.json` is refused, never clobbered** — a merge that
  treats "invalid" as "absent" silently destroys hand edits. UTF-8 BOMs
  (routine on Windows) are stripped before parsing.
- **"Already running" must mean OUR kernel**: pid-file liveness first, then a
  health probe on the port. A bare open port proves nothing — the first E2E
  found a foreign kernel on the default port and setup wrongly claimed the
  install was running.

### Premium parity

The premium binary dispatches the same `setup`/`status`/`stop` to the shared
`app` implementations (mirroring `RunMigrate`). Because setup installs the
running binary itself, a premium install is just "run the premium binary's
setup" — plugins ride along at compile time (ADR-0074), entitlement needs no
license step today (nil `EntitlementProvider` ⇒ all active), and premium env
knobs live in `<prefix>/.env`, which the premium main's `loadDotEnv(".env")`
resolves because the spawned kernel's cwd is the prefix. Premium's
`agents/` (airline benchmark artifacts) are deliberately NOT embedded — no
benchmark logic ships in an install; a real premium agent catalog would get an
embed seam when it exists (rule of three).

### Embedded assets (module-root `embed.go`)

The Python agents (`all:agents` — `all:` because `__init__.py` is
underscore-prefixed and default embed rules would drop it; `__pycache__`/*.pyc
are filtered at unpack) and the config templates (`config.example.json`,
`tuning.json`) are embedded in the binary via a module-root package. Binary
cost: ~1 MB (total ~28 MB, CGO-free, cross-compiles from any host with
`GOOS=linux|windows|darwin`).

### Minimal lifecycle (`status`, `stop`)

With the CLI gone the kernel must be self-sufficient: `status` (pid liveness +
health RPC, exit 0 iff SERVING) and `stop` (SIGTERM → bounded wait → kill on
unix; hard TerminateProcess on Windows, where there is no cross-process SIGTERM
— acceptable because the REACT-01 journal makes hard stops safe). Same
pid-file/log conventions the CLI used.

### Deliberate non-goals

- **No auto-install of Docker** — invasive, needs admin and often a reboot;
  setup prints the instruction and degrades instead.
- **No TUI/GUI** — plain stdin prompts, zero new dependencies (uv download and
  pgx/grpc reuse existing ones). Works over SSH and `curl | sh`.
- **No service registration** (systemd/launchd/Windows service) — deferred;
  `setup` + `stop` + detached spawn is the MVP lifecycle.
- **No secret prompting** — keys never pass through setup; the ADR-0101 seam
  is the write path, via the operator console.

## Consequences

- Distribution collapses to: download one binary per platform, run
  `./cambrian-orchestrator setup`. Install scripts become thin fetch wrappers.
- The Bun CLI's install/lifecycle role is fully absorbed; its operator-console
  role passes to the operator UI. CLI ADRs 003–006 are superseded in practice.
- The compose file setup generates is a copy, not the tracked
  `db/docker-compose.yml` — a schema change there must be mirrored in
  `setupComposeYML` (both are tiny; the duplication is one service block).
- Not benchmark-covered (DDD): installation is infrastructure, not retrieval/
  routing behaviour. Verification: unit tests; idempotent smoke (`setup --yes`
  twice, exit 0); full E2E on an isolated prefix + scratch database — fresh
  `migrate up`, kernel started and DB-gated SERVING confirmed, `status` exit
  codes correct in both states, `stop` clean — for BOTH the OSS and the
  premium binary; cross-compile of linux/amd64 + windows/amd64 for both.
