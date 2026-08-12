---
id: 0125
title: First-Class Bun/Node Agent Runtimes (kernel-side)
status: Accepted
date: 2026-08-12
supersedes: []
superseded_by: []
depends_on:
  - 0008-deterministic-cell-tool-discovery
  - 0036-trait-aligned-agent-sdk
  - 0057-open-core-boundary
  - 0075-agent-sources
  - 0122-self-installing-kernel-setup
---

# ADR-0125: First-Class Bun/Node Agent Runtimes

## Status

Accepted

## Context

Agent sidecars are spawned by the kernel as OS processes speaking the agent-plane
gRPC contract (`api/proto/cambrian.proto`) over a kernel-chosen unix socket. The
contract, the boot/health handshake (socket appears ≤10s, `grpc.health.v1`
SERVING ≤5s), and the SEC-01 sandbox (env allowlist, Job Object / rlimit, both
PID-based) are already language-neutral. ADR-0008 §3 even names "TypeScript,
JavaScript, and binary Tool-Agents" as the sidecar-manifest audience.

What is NOT language-neutral is everything around the spawn:

- `buildAgentCmd` has only `python` and `binary` branches, and the kernel
  carries exactly one interpreter path (`metabolism.python_executable`).
- Filesystem discovery recognizes only Python shapes (`*agent.py`, package dirs
  with `__init__.py` + `agent.py`) and hardcodes `Runtime: "python"`; the
  `AGENT_MANIFEST` regex is Python-heredoc-specific (`'''…'''`).
- The `*.manifest.json` sidecar path — the one shape with an explicit
  `exec_path` + `runtime` — is restricted to `trait: "tool"` and unused.
- PLAT-01 (per-agent requirements, union lockfile, drift gate) and the
  ADR-0122 setup step (`stepPython`) are pip/venv-shaped.
- On Linux the SEC-01 memory cap uses `RLIMIT_AS` (virtual address space),
  which V8-based runtimes violate by design (large virtual reservations).

The owner decision (2026-08-12): support **Bun and Node** as first-class agent
runtimes, kernel-side only — no TypeScript SDK yet. Config carries the
interpreters as **discrete keys**. The install/setup story ships now
(full parity), not deferred.

## Decision

### D1 — Runtime values `bun` and `node`

`domain.AgentRuntime` gains `RuntimeBun = "bun"` and `RuntimeNode = "node"`.
The storage→domain mapper maps both explicitly (an unknown string still
defaults to `binary`, unchanged). `domain.IsJSRuntime` names the pair.

### D2 — Per-runtime interpreter map behind discrete config keys

`metabolism` config gains `bun_executable` and `node_executable` alongside
`python_executable` (additive schema change; nothing renamed). The
`InstanceManager` holds a per-runtime executable map seeded from config via
`SetRuntimeExecutable`; `NewInstanceManager(pythonPath, …)` keeps its signature
(python seeds the map). When a JS interpreter is unconfigured, spawn falls back
to `$PATH` lookup (`bun` / `node`) and only errors — naming the config key —
when that also fails. Python behavior is byte-identical.

### D3 — Spawn branch

`buildAgentCmd` gains one branch for both JS runtimes:
`<interp> <exec_path> --socket <path> --substrate-addr <addr>` with the SEC-01
allowlisted env plus `NO_COLOR=1`, `FORCE_COLOR=0` (keeps stdout parseable as
one-line JSON for the kernel's log forwarder). All post-switch flags
(`--auth-token`, daemon flags, `--agent-id`, `--daemon-params`) apply as-is.

### D4 — Discovery shapes for JS agents

`DiscoverFilesystemAgents` additionally recognizes, next to the Python shapes:

1. **Single file** `*agent.ts` / `*agent.js` — id = filename minus extension.
2. **Package dir** containing `package.json` + `agent.ts` (or `agent.js`) —
   id = dir name, entry = the agent file. Checked only if the dir is not a
   Python agent package (Python wins when both are present).
3. `node_modules` subtrees are skipped entirely (a dependency may legitimately
   ship a file named `agent.js`; it is not an agent of ours).

Runtime derivation: a sidecar/sibling manifest `runtime` field wins; otherwise
`.ts` → `bun`, `.js` → `node`.

Manifest resolution mirrors ADR-0075 sidecar-preference: a sibling
`<id>.manifest.json` is canonical. As a convenience symmetric with Python's
heredoc, an embedded ``AGENT_MANIFEST = `{…}` `` (backtick template literal)
regex is honored when no sidecar exists; `AGENT_DESCRIPTION = "…"` is parsed by
the existing regex (it is anchor-free and matches `const AGENT_DESCRIPTION =`).
`storage.ManifestRecord` gains an optional `runtime` field so a sibling
manifest can carry the override; the field is discovery-time metadata and does
not surface in `domain.AgentManifest`.

### D5 — Standalone sidecars open to all traits

`sidecarAgentRecord` accepts any **explicit** trait from
{`tool`, `cognitive`, `model`, `daemon`} (`cognitive` normalizes to the domain
zero value `""`). A missing/empty trait still skips — an unrelated
`*.manifest.json` must not silently become a cognitive agent. A sidecar whose
`<id>.py`/`<id>.ts`/`<id>.js` sibling source exists is skipped in the walk:
that manifest belongs to the source-shape record (prevents double
registration). The exec-exists health gate stays. On source-hash change,
`upsertDiscovered` now refreshes `ExecPath`/`Runtime` for every record, not
only `trait == "tool"` (the old condition predates non-tool sidecars).

### D6 — PLAT-01 parity for JS

- **Boot-time check**: before spawning a `bun`/`node` agent, if a
  `package.json` governs it (in the entry file's dir or the agents root) and
  no `node_modules` exists in either place, boot fails naming the fix
  (`bun install`), mirroring `verifyPythonDeps`' fail-fast-with-the-dep-named.
  `python_deps` stays Python-only; JS deps are declared where the ecosystem
  declares them (`package.json`), not duplicated into the manifest.
- **Drift gate**: `scripts/gen_agent_packages.py` (same toolchain as the
  PLAT-01 generator) scans JS agent units, extracts external imports
  (`import … from '<pkg>'` / `require('<pkg>')`, excluding relative and
  `node:` builtins), and verifies each unit's `package.json` declares them —
  and maintains the union workspace `agents/package.json` (bun workspaces; one
  `bun install` at the agents root = the union-lockfile analog, `bun.lock`).
  `make agent-packages` generates; `make agent-packages-check` is the drift
  gate, chained into `agent-reqs-check`. With zero JS agents both are no-ops
  and no root `package.json` is created.

### D7 — Setup step (ADR-0122 amendment)

A new step "5. JS agent runtime" runs after the Python step (later sections
renumber): probe `node`; probe `bun` and, when JS agent units exist under
`agents/` and bun is absent, download the official release asset into
`<prefix>/bin` (same pattern as the uv bootstrap); run
`bun install --frozen-lockfile` (plain `install` when no lockfile) in each
agent dir carrying a `package.json`, root first. `--skip-js` mirrors
`--skip-python`. `writeConfigBundle` records `bun_executable` /
`node_executable` when resolved. Node is never downloaded (too large; probing
is enough — bun covers the batteries-included path). `unpackAgents` filters
`node_modules` and `*.map` alongside `__pycache__`/`*.pyc`, and
`node_modules/` under `agents/` is gitignored so `go:embed all:agents` (built
from a clean checkout) never swallows a dependency tree into the binary.

### D8 — Linux memory-cap exemption for V8

On Linux the per-agent memory cap is **not applied** to `bun`/`node` agents:
`RLIMIT_AS` limits virtual address space, and V8 reserves multi-GB virtual
ranges it never commits — a cap sized for Python kills a healthy JS agent at
boot. Windows Job Objects limit committed process memory and apply unchanged.
Lifetime containment (Pdeathsig / kill-on-job-close) is unaffected. Revisit
with cgroup v2 (`memory.max`) as part of the SEC-01 Unix follow-up.

## What does NOT change

- The agent-plane proto, boot/health handshake, auth-token flow, `CallAgent`,
  A2A short-circuit — untouched; **no contract bump** (no proto change).
- SEC-01 env allowlist and Windows Job Objects — already runtime-neutral.
- Kernel *tools* (`internal/tool/proc`) remain Python-only — separate surface,
  out of scope.
- The Python SDK, the Python agents, and their PLAT-01 pipeline — byte-identical
  behavior.
- Zero-Hardcode: runtime selection is data (discovery/manifest), not task
  routing; no agent-identity branching is introduced.

## Measurement (DDD note)

No benchmark suite exercises agent spawn mechanics; the retrieval/orchestration
scoring paths are untouched (the Python fleet is the only fleet in-tree, and its
spawn path is unchanged — verified by the existing agentmgr/storage suites plus
new unit tests for the JS branches). If/when a JS agent ships in-tree, the
orchestration suite covers it end-to-end; a dedicated `js-agent-boot` smoke
needs bun in CI and is deferred until then (recorded as a Known Gap).

## Consequences

- A Bun/Node agent is launchable today by dropping `*agent.ts`/`*agent.js` (or
  a standalone sidecar manifest) under `agents/` — no Go changes needed later
  for the TS SDK to arrive; the SDK will only have to implement the existing
  process contract (argv flags, UDS bind, health servicer, `AgentService`).
- Two new config keys; empty values behave like today plus `$PATH` fallback.
- Known Gaps: no TS SDK yet (kernel support only); node is probed, never
  provisioned; Linux JS agents run memory-uncapped (D8); no `js-agent-boot`
  CI smoke until a JS agent exists in-tree.
