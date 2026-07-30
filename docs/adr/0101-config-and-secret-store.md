---
id: 0101
title: Config and Secrets in an Embedded Store, with Per-Key Provenance
status: Proposed
date: 2026-07-29
supersedes: []
superseded_by: []
amends:
  - 0024-layered-configuration
depends_on:
  - 0057-open-core-boundary
  - 0054-automated-tuning-seam
---

# ADR-0101: Config and Secrets in an Embedded Store

## Status

Proposed — 2026-07-29. **D1, D2, D4, D5, D6 and D7 implemented; D3 implemented 2026-07-29**
(contract 0073: `SetConfig` / `DeleteConfig` / `SetGeneratorKey` / `ClearGeneratorKey`, with
per-key `ConfigWriteOutcomeOp` reporting `live` / `restart_required` / `shadowed` / `rejected`).

Residual, and it is the honest limit of the current write path: only the seven Stage-A blend
weights have a live-apply route (ADR-0054's `SetRuntimeConfig` seam is the only thing that
mutates a running tunable). Every other key stores durably and reports `restart_required`
rather than claiming an effect it did not have. Widening hot-apply means giving each consuming
component an atomically-swappable field, which is per-field work with its own race surface —
tracked in `cambrian-core/CONTEXT.md` Known Gaps rather than bundled here.

## Context

### The operator plane can write config, but nothing can read it back

`SetRuntimeConfig` (ADR-0054) hot-applies numeric tunables into the running kernel. It
has two properties that were acceptable for an automated tuning seam and are not
acceptable for a console:

1. **It is write-only.** Nothing on the operator plane reads a current value, so the
   Dispatch & retrieval screen shows the kernel's *documented defaults* labelled as
   such, with a callout admitting it cannot tell whether a field is pinned elsewhere.
2. **It is ephemeral.** Edits live in the process; `configs/*.json` remain the boot
   default, so a restart silently reverts them.

The operator UI refactor (`operator_ui_refactor/KERNEL-REQUIREMENTS.md`) names the read
half — `GetConfigSchema` — as the single highest-value addition left, and names
`value_source` as the field that closes "you change a value, it saves, and nothing
happens because something upstream pins it."

### Config lives in eleven layers and none of them remembers who won

`LoadConfig` (`internal/config/config.go`) merges eleven layers through Koanf: Go
defaults, four pairs of `*.json` / `*.local.json` files, `mcp.json`, then `CAMBRIAN_*`
env vars at the top. The merged `*Config` is correct and carries **no record of which
layer supplied each field**. That information is discarded inside `k.Load`.

This is already a live cost. `configs/tuning.local.json` is gitignored and per-machine,
so two developers can hold different values for the same key with nothing in the
process able to say so; the Scout was once silently disabled for exactly this class of
reason (recorded in `ResolveBaseDir`'s own doc comment).

### Secrets have no store, and the one that exists is ad hoc

`GeneratorConfig.APIKeyEnv` holds the *name of an env var*, never a key. That
indirection is what makes the "secrets travel via `.env` / `CAMBRIAN_*`" rule in
`AGENTS.md` sufficient, and it is why `cambrian-core/.gitignore` is a real boundary.

The exception is the Telegram bot token (`cambrian-premium/plugins/telegram/admin.go`),
which is written to a plaintext file at `tokenPath` and removed with `os.Remove`. It
works, but it is a one-off: its own path, its own on-disk format, its own enabled-flag
sidecar file. Every further credential the console wants to own — generator keys, the
Langfuse secret key — would repeat it.

## Decision

### D1 — An embedded store becomes a config layer, not a replacement for the pipeline

Config moves into an embedded key-value store. The layered pipeline **stays**, and the
store is inserted into it as a new layer:

```
1..10   Go defaults → tuning{,.local} → config{,.local} → embedder{,.local}
        → providers{,.local} → mcp.json          (unchanged)
11      the embedded store                        (NEW)
12      CAMBRIAN_* environment variables          (unchanged, still highest)
```

Rejected: making the store the sole source of truth. `CAMBRIAN_*` must keep winning —
containers (ADR-0066), CI, and the benchmark rig all configure by environment, and the
harness's authority depends on driving a released binary the way a user's process runs
it. Files must keep working for bootstrap and for the committed `tuning.json` starter.

The store therefore sits **above the files** (an operator's write outlives a restart and
beats the shipped defaults) and **below the environment** (deployment still wins).

### D2 — bbolt, not sqlite

`go.etcd.io/bbolt v1.4.3` is already a direct dependency and already holds agent state,
run checkpoints, the step cache and the reactive journal. Config is a key→value tree
read whole at boot and written rarely — it needs no query engine.

Rejected: sqlite. Pure-Go (`modernc.org/sqlite`) is a large new dependency; CGO sqlite
would breach the pure-Go / NO-CGO posture SEC-01 established for the memory caps. Either
choice adds a **third** storage engine beside Postgres and bbolt. The one real argument
for sqlite — queryable config history — is already served by the operator audit log,
which records every mutation with actor, reason and `command_id`.

### D3 — Precedence is enforced at WRITE time, not only reported at read time

A store write that an env var will shadow is the defect this ADR exists to prevent, and
the store *creates new instances of it*: before this change there was no durable write
path from the console at all.

So `SetConfig` resolves the key against the layers above it before returning. When
`CAMBRIAN_*` pins the key, the value is still stored (it is the operator's stated
intent, and it takes effect if the env var is later removed) and the ack says so
explicitly, naming the variable. The console renders that as a warning rather than a
success.

Reporting it only through `GetConfigSchema.value_source` on the next read would mean the
operator sees "saved" and learns the truth only if they go looking.

### D4 — Provenance is recorded during the merge, by snapshot diff

`LoadConfig` records, per flat Koanf key, which layer last set it:

```go
type Provenance map[string]string   // "execution.ewma_alpha" -> "tuning.local.json"
```

Each layer is wrapped: snapshot `k.All()` before and after, and attribute every key whose
value changed (or first appeared) to that layer. This adds no coupling to Koanf
internals and no change to merge semantics — it observes the same merge that already
runs.

Rejected: reconstructing provenance after the fact by re-reading each file and
re-applying precedence. That is a second implementation of the merge, and a second
implementation that disagrees with the first is worse than no answer — the same argument
the UI refactor made against its client-side reachability evaluator.

### D5 — Secrets share the store but never the read path

Secrets live in a separate bucket, keyed by logical name (`generator:<id>:api_key`,
`telemetry:secret_key`, `telegram:bot_token`) rather than by config path, so a secret can
never be returned by a config read that forgets to filter.

**No read RPC returns a secret value, ever.** The only exported reads are
`Configured() bool` and `LastFour() string`. There is no `GetSecret` on any plane.

Resolution order matches D1 — an env var wins over the stored value — so an existing
`.env`-based deployment keeps working unchanged after migrating, and D3's shadow warning
applies to secrets too.

### D6 — Secrets are encrypted at rest under a key held outside the store

Today's stored credential is a plaintext file, so plaintext bbolt would not be a
regression in the cryptographic sense. What changes is **blast radius**: before this
change the data directory held no credentials. After it, `~/.cambrian/data` (PLAT-05) is
the file operators back up, copy between machines, mount into containers (PLAT-04) and
attach to bug reports — and it would hold every key at once.

Secret *values* are therefore encrypted with AES-256-GCM under a key resolved from
`CAMBRIAN_SECRET_KEY`, or from a key file next to the store with `0600` permissions,
generated on first use. The store file alone is then useless.

This is deliberately modest. It does not defend against an attacker who already has both
the data directory and the environment of the running process — nothing at this layer
can. It defends against the realistic path: a database file that travels somewhere its
environment does not.

**Consequence to state plainly:** losing the key means the stored secrets are
unrecoverable and must be re-entered. `cambrian init` warns, and the key file is included
in the documented backup set.

### D7 — `GetConfigSchema` stays scoped to the numeric tuning surface

`SetRuntimeConfigRequest.params` is `map<string, double>`. `GetConfigSchema.current_values`
mirrors it exactly, so read and write agree on shape and booleans travel as 1/0 in both
directions.

The store will hold non-numeric config too (generators, MCP servers, the embedder), but
those get **typed accessors** — `ListGenerators` and `ListMCPServers` are already two of
them. Widening `current_values` into a value union to cover both jobs produces one RPC
that is awkward for each.

## Consequences

- **A UI promise becomes false.** The Dispatch & retrieval footer states that runtime
  edits are ephemeral and a restart reverts them. After this change it is wrong, in the
  direction that costs an operator something: someone who makes a mistake and reaches for
  "just restart it" will find the mistake still there. The UI copy changes with this ADR,
  not after it.
- **The data directory becomes sensitive** even with D6, because the key file defaults to
  living beside it. Backup and container documentation must say so.
- **`configs/*.json` remain authoritative for bootstrap** and for everything the operator
  never edits. This ADR does not delete them, and a deployment that never writes through
  the console behaves exactly as it does today.
- **Benchmark reproducibility is unaffected**: the harness configures by environment,
  which still wins, and an arm that must ignore operator writes can point at an empty
  store path.
- The migration runner (ADR-0064, `internal/migrate`) governs Postgres schema only. The
  bbolt store versions its own bucket layout.

## Falsification

This ADR is wrong if, after implementation:

- an operator write silently fails to take effect without the ack saying why (D3 failed);
- `value_source` disagrees with the value the kernel actually uses (D4 failed — the
  snapshot diff is not observing the real merge);
- any read path can be made to return a secret value (D5 failed);
- a deployment configured entirely by `CAMBRIAN_*` behaves differently after the store
  lands than before it (D1 failed).

## References

- ADR-0024 — layered configuration (amended here: eleven layers become twelve)
- ADR-0054 — the automated-tuning seam that `SetRuntimeConfig` was built for
- ADR-0057 D5/D8 — config schema as a held-stable contract; OSS names no premium feature
- ADR-0064 — the Postgres migration runner, deliberately not extended to bbolt
- ADR-0066 / PLAT-04, PLAT-05 — container distribution and the `~/.cambrian/data` default
- `operator_ui_refactor/KERNEL-REQUIREMENTS.md` §2.9 — the requirement this answers
