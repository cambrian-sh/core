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

Accepted (the `AgentSource` interface + registry `AddAgentSource`/`AddAgent`/
`AddSystemAgent` + boot-time registration delivered and unit-tested; plugins can now
contribute agents. Folding the built-in filesystem scan into a literal
`FilesystemAgentSource`, preferring the declared/sidecar manifest over the source-regex,
health-gated activation, and MCP sources are phased follow-ups — see Consequences).

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

**Negative / deferred (the honest follow-ups).**
- The **built-in filesystem scan is not YET literally a `FilesystemAgentSource`** — it
  still runs inside `BBoltAdapter.Seed`. Folding it behind the interface (so *all*
  registration is uniform, and an external/plugin agents-directory is just another
  `FilesystemAgentSource{dir}`) is the next step; it means inverting `Seed` into
  discover-then-persist.
- **Manifest-from-regex remains** for the Python path. Preferring the declared
  `AgentDefinition` (from a source) or the `*.manifest.json` sidecar over the
  `AGENT_MANIFEST='''…'''` regex is a robustness follow-up.
- **Health-gated activation** — separating "discovered" from "auction-eligible" so a
  present-but-broken agent can't be injected — is not part of this seam.
- **MCP servers** follow the SAME pattern (`AddMCPServer` / an `MCPSource`, Tier-2
  add-many) but wire deeper — into the MCP manager's boot-time connect (ADR-0043) — so
  they are a separate phase.
- No plugin ships an agent yet, so the path is unit-tested (`app/plugin_test.go`
  `TestApplyPlugins_AgentSources`), not live-exercised.

## References

- ADR-0074 (plugin architecture / tiering — this is a Tier-2 add-many extension point),
  ADR-0002 (auction/Gatekeeper the agents participate in), ADR-0034 (scope — regular
  plugin agents get scoped like any other). Code: `app/plugin.go` (`AgentSource`,
  `AddAgentSource`/`AddAgent`/`AddSystemAgent`), `app/app.go` (`registerPluginAgents`),
  `internal/storage/bbolt_adapter.go` (the built-in filesystem source, unchanged).
