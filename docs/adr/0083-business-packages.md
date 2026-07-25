---
id: 0083
title: Business Packages — Composed SKUs of Plugins, Agents, and Memory
status: Proposed
date: 2026-07-24
supersedes: []
superseded_by: []
depends_on:
  - 0082-additive-licensed-plugins
  - 0075-agent-source-seam
  - 0057-open-core-boundary
  - 0034-tag-based-isolation
---

# ADR-0083: Business Packages

## Status

Proposed. Defines the commercial unit that sits **above** the plugin mechanism of ADR-0082
and owns everything ADR-0082 deliberately excluded: what a package is, and how the *data*
half of a package (agents and memory) is distributed, installed, scoped, and versioned.

## Context

Cambrian will be sold as **vertical business packages** — Law, Company Brain, ERP, Customer
Chatbots, Employee Chatbots, Coding — not as a kernel with à-la-carte features. Each package
is a collection of **plugins + pre-shipped memory + agents**.

ADR-0082 built the plugin mechanism (manifest, entitlement chokepoint, one-binary
distribution) and its amended D6 resolves entitlement *through* packages. It does not define
the package itself. That gap matters because the three things a package bundles have
fundamentally different physics: one is compiled code, two are data.

Three inherited constraints shape every decision below:

- **Kernels may run offline / air-gapped** (ADR-0082 D5). No network call on the boot path,
  and artifact delivery cannot assume connectivity.
- **The embedder is load-bearing and its dimension is destructive to change.** The stack is
  bge-large at 1024 dims; changing embedding dimension drops and recreates `documents`
  (a known gap, guarded by `ALLOW_DESTRUCTIVE_DIM_MIGRATION`).
- **`ScopedVectorStore` fails closed.** An unknown principal gets zero hits with no error —
  the single most silent failure mode in the system.

## Decision

### D1 — The package is the SKU; it is a data manifest

```go
type PackageManifest struct {
    ID          string          // "law" — the entitlement key
    DisplayName string          // "Law Package"
    Version     string
    Plugins     []string        // plugin IDs this package activates
    AgentPacks  []AgentPackRef  // ADR-0075 sources + their files
    MemoryPacks []MemoryPackRef
}
```

Like the plugin manifest, it is **data, not Go type identity** — so the same manifest
describes a package whether it is resolved in-process, served by the registry, or read from
an offline bundle.

### D2 — Entitlement resolves package → plugin union (amends ADR-0082 D6)

A license grants **packages**; the union of their `Plugins` is what the ADR-0082 chokepoint
gates. A plugin shared by several entitled packages activates once. Plugins are shared
infrastructure that packages compose — the reactive engine underpins Customer Chatbots *and*
Company Brain without either owning it.

### D3 — Three artifact classes, three distribution physics

This asymmetry is the central engineering fact of this ADR:

| Class | What it is | Ships how | Offline upgrade |
|---|---|---|---|
| **Plugins** | Compiled Go | Already in the binary; runtime-gated (ADR-0082 D4) | ✅ license file alone |
| **Agent packs** | Python + deps | Files overlaid into the runtime `agents_dir` | ⚠️ file delivery required |
| **Memory packs** | Corpora / structure | Rows in Postgres; potentially GBs | ❌ artifact delivery required |

ADR-0082's "upgrades are just a license file" property therefore holds **only for
plugin-only upgrades**. A firm buying the Law Package needs its corpus physically delivered.
This is stated up front rather than discovered at install time.

### D4 — Memory packs come in two kinds: seeded corpora and structure templates

Not all pre-shipped memory is content. The distinction is commercially and legally
significant:

- **Seeded corpus** — *we* ship the content (statutes and case law, ERP reference schemas,
  regulatory text). High value, high redistribution exposure.
- **Structure template** — we ship the *shape*, the customer supplies the content: ingestion
  profiles, section taxonomies, KG extraction patterns, anchor normalization rules, and
  retrieval tuning for that vertical. **Company Brain is almost entirely this** — it is the
  customer's own documents processed through our structure.

Many packages are mostly template. Templates carry much of the domain value with **none** of
the third-party redistribution exposure, so where a vertical can be served by structure
rather than content, prefer it.

### D5 — Two delivery modes for memory; pre-embedded pins the embedder

- **Pre-embedded (default, fast).** Ships vectors. The pack declares
  `{embedder_model, dim, schema_version}` and the kernel **refuses** a mismatched install —
  a hard error, never a degraded one. A corpus embedded under a different model is not
  "slightly worse", it is semantically meaningless. Install is a bulk load.
- **Source (portable, slow).** Ships documents; ingested at install through the normal
  pipeline. Embedder-agnostic, but at ~6.7 s/item a 10k-document corpus is roughly 18 hours.

Both modes exist because neither alone is sufficient: pre-embedded is the only practical way
to ship a large corpus, and source is the only option when a customer's embedder differs.

**Consequence, stated plainly:** for any customer on a package with a pre-embedded corpus,
the embedder ceases to be a config knob and becomes a **product-level constraint** across
the whole catalog.

### D6 — Installation is atomic with scope grants

A pack that writes documents without the matching `agent_scopes` rows produces a product
that looks broken in the worst possible way: ingest succeeds, queries return nothing, and no
error is raised anywhere. This is the `TRUNCATE agent_scopes` failure mode in a new hat.

Therefore the pack manifest declares `scope_grants`, and installation writes **documents and
grants in one transaction**, rolling back together. Verification that a freshly installed
pack actually returns hits for its intended principal is part of install, not of support.

### D7 — Provenance tagging makes packs separable, updatable, and removable

Every pack-installed row carries `pack_id` + `pack_version` in its metadata. This is
non-negotiable, because without it a corpus update either destroys customer-authored memory
or is impossible. It buys three things:

- **Update** — diff/replace by `pack_id`, never touching customer memory.
- **Uninstall / downgrade** — delete by `pack_id`.
- **Support** — an unambiguous answer to "is this our content or theirs?"

Corpora change (legal ones, quarterly), so update is a routine operation, not an exception.

### D8 — Distribution: authenticated registry by default, signed bundle for air-gapped

- **Default — the Cambrian registry** (`cambrian.sh`). Artifacts are **content-addressed**
  (sha256) and **signed with the same Ed25519 key as licenses** (ADR-0082 D5), so one trust
  root covers entitlement and artifacts. The license token doubles as the download
  credential, so **entitlement gates delivery** rather than only activation. Transfers are
  resumable/chunked — GB corpora over imperfect links are the normal case.
- **Air-gapped — `cambrian pack export` / `cambrian pack install <file>`.** A single signed
  bundle moved by any means. Identical signature verification; no network.

Alternatives considered: an unauthenticated CDN (simpler, but delivery would not be gated by
entitlement — the corpus is the most valuable artifact, so it should not be anonymously
fetchable), and shipping corpora inside the binary (impossible at GB scale).

Inherited from ADR-0082 D5: **never on the boot path.**

### D9 — Operator surface: `ListPackages` alongside `ListPlugins`

Customers recognize packages, not plugins. The UI renders package cards from `ListPackages`;
individual panels still come from their plugins' capabilities (ADR-0082 D9). Package states
extend the plugin states with two that only exist at this layer:

| State | Meaning |
|---|---|
| `ACTIVE` | entitled, artifacts installed |
| `NOT_ENTITLED` / `EXPIRED` | as ADR-0082 |
| `UPDATE_AVAILABLE` | a newer pack version exists |
| `ARTIFACTS_MISSING` | **entitled but data not yet delivered** — the normal air-gapped intermediate state |

`ARTIFACTS_MISSING` is the state that prevents the worst support call: a customer who has
paid for Law, whose plugins activated, and whose corpus never arrived.

### D10 — The IP reality of memory packs

A pre-embedded corpus is the most valuable and least protectable artifact in the product —
it is rows in the customer's own Postgres. There is no meaningful technical protection, and
the same open-core bargain as ADR-0082 D4 applies: the license is the protection.

The harder constraint is **legal, not technical**: for third-party licensed content (very
likely in Law), the rights holder may prohibit redistributing embeddings at all. Redistribution
rights must be confirmed **before** engineering a corpus pipeline for a vertical. Where rights
are unavailable, D4's structure-template path is the fallback that still ships the vertical.

## Migration

Sequenced after ADR-0082 Phase 2 (the entitlement chokepoint must exist first).

| Phase | Scope |
|---|---|
| **1. Package manifest + resolution** | `PackageManifest`, package→plugin union in `EntitlementProvider`, `ListPackages` RPC. No artifacts yet — packages are plugin bundles only. |
| **2. Agent packs** | Package-contributed `AgentSource` (ADR-0075) + file overlay via the PLAT-05 `cambrian init` path + PLAT-01 dependency generation. Mostly existing rails. |
| **3. Memory packs — source mode** | Manifest, provenance tagging (D7), atomic scope-granted install (D6), offline bundle install. Source mode first: no embedder pinning, so it derisks the format before the fast path. |
| **4. Memory packs — pre-embedded** | Bulk vector load, `{embedder_model, dim, schema_version}` compatibility refusal, update/diff by `pack_id`. |
| **5. Registry** | Authenticated content-addressed delivery on `cambrian.sh`, resumable transfer, signature verification shared with licensing. |

## Consequences

**Positive.**
- The commercial unit finally matches how the product is sold; plugins become reusable
  infrastructure rather than SKUs.
- Provenance tagging makes shipped memory a first-class, updatable, removable asset instead
  of an irreversible bulk import.
- One trust root (Ed25519) covers licenses and artifacts.
- Structure templates give a rights-safe path to verticals whose content cannot be shipped.

**Negative / costs.**
- Memory-bearing packages make the embedder a product-level constraint (D5).
- Air-gapped delivery of GB corpora is genuinely awkward and needs an operational story, not
  just an API.
- Corpus update/versioning touches the consolidation/dedup path.
- A new registry service becomes production infrastructure with availability obligations.

**Neutral.**
- Agent packs need little new machinery — ADR-0075, PLAT-05, and PLAT-01 already cover them.
- Package states are additive to the ADR-0082 plugin states.

## References

- ADR-0082 (plugin manifest, entitlement chokepoint, one-binary distribution, Ed25519
  licensing — this ADR amends its D6), ADR-0075 (`AgentSource` seam), ADR-0057 (open-core
  boundary), ADR-0034/0035 (scope access control — the fail-closed behaviour D6 guards
  against), ADR-0060 (chunking/structure pipeline — what a structure template configures),
  ADR-0064 (migration runner — the schema-version dependency for pre-embedded packs).
- Backlog: PLAT-01 (per-agent requirements), PLAT-05 (`cambrian init` agent overlay).
