---
id: 0075
title: AgentSource Seam — Unifying Agent Registration Behind a Provider Interface
status: Accepted
date: 2026-07-16
supersedes: []
superseded_by: []
depends_on:
  - 0074-plugin-architecture
  - 0002-gatekeeper-auction
  - 0034-tag-based-isolation
---

# ADR-0075: AgentSource Seam

## Status

Accepted — fully unified. Phase 1: the `AgentSource` interface + registry
`AddAgentSource`/`AddAgent`/`AddSystemAgent`. Phase 2: `Seed` inverted into
discover-then-persist (`storage.DiscoverFilesystemAgents` + `upsertDiscovered`),
`FilesystemAgentSource`, sidecar-preference, health-gate, plugin MCP servers
(`AddMCPServer`). Phase 3 (this revision): the seam carries the **manifest** —
`DiscoveredAgent{Definition, Manifest}` + an idempotent `SetAgentWithManifest` /
`UpsertDiscoveredAgent` persist path — so the built-in filesystem scan is now **literally
one `AgentSource`**: `BootstrapStorage` opens the store with `NewBBoltAdapterNoScan` and
the composition root registers the agents dir through a system-aware
`FilesystemAgentSource`. Live-validated: a fresh boot registers all 12 filesystem agents
through the source with `manifest=true` and the correct system flags
(scout/kg_extractor/reranker/retrieval = system), no wrongful prune; SourceHash
idempotency (no re-interview on reboot) is unit-proven.

## Context

Agents enter the registry three unrelated ways today: (1) a **filesystem scan** —
`BBoltAdapter.Seed` walks `agents_dir` for `*_agent.py` / packages / `*.manifest.json`,
parsing an `AGENT_MANIFEST = '''…'''` block **out of the Python source with a regex**; (2)
**model-config registration** — `registerModelAgents` upserts `llm:*` agents from
`providers.json`; (3) **A2A dynamic registration** at runtime via the auctioneer. There
was no way for a plugin (ADR-0074) to contribute agents, and no unifying seam.

Directory-scan is a legitimate, widely-used pattern — *file-based service discovery*
(systemd unit dirs, `nginx conf.d/`, k8s static pods, this tool's `~/.claude/agents/`).
Its virtue is zero-ceremony extensibility. But the current implementation has real weak
spots: the **manifest-from-source regex** is brittle; the **FS-as-truth + bbolt-mirror**
needs boot-time reconcile (two sources of truth); directory-presence is **ambient
authority** (writing the dir registers an agent, and a `system/` subtree could imply
privilege); discovery ≠ availability. And the three registration paths share no interface,
so "a plugin adds an agent" had nowhere to plug in.

## Decision

Introduce **`AgentSource`** — the provider interface that agent registration flows
through — and register plugin-contributed sources alongside the built-in ones.

```go
type AgentSource interface {
    Name() string
    DiscoverAgents(ctx context.Context) ([]domain.AgentDefinition, error)
}
```

- **Registry surface (ADR-0074 Tier-2 add-many):** `Registry.AddAgentSource(src)` for a
  custom source; `AddAgent(def)` for a single regular agent; `AddSystemAgent(def)` for a
  privileged organ.
- **Boot wiring:** `registerPluginAgents` discovers each source and upserts every
  definition through the SAME `reg.SetAgent` path as `registerModelAgents` — so a plugin
  agent participates in the auction / scope / merit machinery like any other agent. No
  change to the bbolt seeder; additive and upsert-only.

### The privilege boundary is explicit, not ambient

System agents bypass auction/Gatekeeper by construction — that is a **policy** grant, not
mechanism. So:
- `AddAgent` **forces `System=false`** — a regular-agent registration can never confer
  system status.
- `AddSystemAgent` **stamps `System=true`** and the composition root **logs the grant** at
  registration (`ADR-0075: registering PRIVILEGED system agent from plugin`). The grant is
  visible and auditable, never inferred from a filename or folder. Only a compiled-in
  (trusted) plugin can reach it; an untrusted plugin must never get system status (that is
  the out-of-process/sandbox case, ADR-0074). This makes the ADR-0074 Tier-3 boundary
  visible at the seam instead of relying on a writable `system/` directory convention.

## Consequences

**Positive.**
- Plugins can contribute agents (regular or, explicitly, privileged) — the capability the
  plugin work was building toward, as a first-class provider, not a special case.
- One interface now describes "where agents come from"; the built-in filesystem and model
  sources are conceptually two providers, with plugins as a third.
- The privilege grant is explicit + logged, tightening the ambient-authority weakness.
- Zero change to the working bbolt seeder / reconcile — bounded blast radius.

**Negative / costs.**
- `BBoltAdapter.Seed` still exists (for tests + the calibration-report cmd) even though
  the kernel boot path now uses `NewBBoltAdapterNoScan` + the source. Two ways to seed a
  store is mild redundancy; Seed could later be expressed in terms of the source too.
- The persist path is idempotent by `SourceHash` — a real subtlety: routing through a
  blind `WriteAgentRecord` re-provisioned (re-interviewed) every filesystem agent on every
  boot; `UpsertDiscoveredAgent` restores the in-Seed idempotency (unit-proven,
  `TestUpsertDiscoveredAgent_PreservesProvisionalOnUnchangedHash`).
- No plugin ships an agent or MCP server yet, so the *plugin* paths are unit-tested
  (`app/plugin_test.go`, `app/agent_source_test.go`), not live-exercised. The built-in
  source path IS live-validated (12 agents + manifests + system flags on a fresh boot).

## References

- ADR-0074 (plugin architecture / tiering — this is a Tier-2 add-many extension point),
  ADR-0002 (auction/Gatekeeper the agents participate in), ADR-0034 (scope — regular
  plugin agents get scoped like any other). Code: `app/plugin.go` (`AgentSource`,
  `AddAgentSource`/`AddAgent`/`AddSystemAgent`), `app/app.go` (`registerPluginAgents`),
  `internal/storage/bbolt_adapter.go` (the built-in filesystem source, unchanged).
