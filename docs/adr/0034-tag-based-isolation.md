# ADR-0034: Tag-Based Data Access Scoping for Agents & Sources

**Status:** Phase 1 IMPLEMENTED (2026-06-03) — *D1–D13 grilled; 5 blocking risks (R1–R5) resolved. Phase 1 (`agent_scope`-only enforcement) is shipped: `domain.ScopeConfig`/`EffectiveScope`, `ScopedVectorStore` (read, fail-closed), `ScopedStoreWriter` (write, R3), `ScopeResolver` (Postgres `agent_scopes` + LISTEN/NOTIFY, R1), controlled vocabulary, artifact tag primitives, promotion deterministic core (D11 ledger + `KAnonymityFloor` + masker), and core audit logs (R4). The Phase 2 (`caller_scope`) mechanism — `domain.Session.CallerScope` + `EffectiveForCaller` re-derivation — is implemented but its LIVE wiring (persist at StartConversation, re-derive per-RPC via session token, plus globally wrapping the shared store + tagging every system read `ScopeSystem`, plus the artifact gRPC RPCs and promotion event-bus wiring) is HITL-gated pending review. Do NOT advertise caller_scope protection until that wiring lands. See `docs/issues/adr34/` for the 8-slice breakdown and CURRENT_CODEBASE_STATE.md for the component map.*
**Date:** 2026-06-01 (revised 2026-06-02)
**Author:** Afsin
**Depends on:** ADR-0015 (Engram Engine), ADR-0028 (External Ingestion), ADR-0031 (Universal Router), ADR-0033 (Daemon Agent Architecture)

> **Revision note (2026-06-02):** This ADR was originally framed around *multi-tenant isolation*.
> A grilling session re-anchored it on **per-agent / per-source least-privilege data access**.
> Multi-tenancy is now an explicit **non-goal** (see Non-Goals). Decisions resolved during the
> session are recorded in **Resolved Decisions**; the single remaining open branch is in
> **Open Questions**. Sections below have been rewritten to match the resolved direction; older
> multi-tenant framing has been removed.

---

## Context

Cambrian is a **kernel** — a cognitive backend companies integrate into their own applications.
Within a single deployment, different **agents** and **data sources** must see different subsets of
LTM (the "Company Brain"). The motivating requirement:

> A customer-support agent must **never** be able to retrieve company secrets — regardless of who is
> asking or how the calling application is configured.

This is an **access-control safety property**, not a value-add feature. It is fundamentally about
*least privilege on data access per agent/source*, enforced inside Cambrian's retrieval path.

Auth, customer records, and PII remain the **Company's** responsibility. Cambrian continues to avoid
storing PII as a matter of hygiene, but — post-reframe — that rule is no longer the *load-bearing*
justification for this ADR.

## Non-Goals

- **Multi-tenant isolation is NOT solved here.** Tenancy is a **deployment concern**: each tenant
  gets its own **binary + database**. Process/data separation is a stronger posture than logical
  tag separation (it eliminates the cross-tenant leak class entirely, makes "right to be forgotten"
  a `DROP DATABASE`, and removes noisy-neighbor risk). The original `tenant_acme` / `tenant_globex`
  tag framing is **deleted**.
- ADR-0034 governs **intra-deployment** access scoping only: *which agents/sources may read/write
  which data within one tenant's deployment.*

## Decision (summary)

**Access is scoped by opaque tags using a three-set model, enforced fail-closed inside the retrieval
path. The enforcement *mechanism* is CORE; the *governance* layer (policy authoring, audit,
per-agent intrinsic profiles) is PREMIUM.**

---

## Resolved Decisions (Grilling 2026-06-02)

### D1 — Core / Premium split
- **CORE (always compiled):** the enforcement mechanism — `domain.ScopeConfig`, the three-set tag
  filter, the `ScopedVectorStore` read decorator, the `ScopedStoreWriter` write decorator (R3),
  fail-closed propagation, **and basic access-decision logging** (R4). Rationale: a security
  primitive must not be paywalled (OSS would be *insecure by default*), and it is the substrate
  other core components depend on, so it cannot sit behind a `//go:build premium` tag.
- **Basic audit logging is CORE (R4):** a single structured `slog` line per **deny**, per
  `ScopeSystem` use, per promotion write, and per **unsatisfiable effective scope** (R5) —
  who/what/which-tags/decision. Enforcement without *any* visibility is a silent-failure black box;
  an OSS operator who suspects a leak must be able to investigate. This is zero infrastructure.
- **PREMIUM:** the governance **product** on top — per-agent intrinsic scope-profile authoring &
  policy management, the queryable/retained/tamper-evident **audit store** (not the log lines
  themselves), compliance exports, dynamic policy distribution, and the `ConversationEngine`
  front-end (already premium). The split is: *enforcement + basic logs free; management, retention,
  and reporting paid.*

### D2 — `ScopeConfig` placement & deduplication
- A **single canonical `ScopeConfig` lives in `internal/domain/`** (it is a plain value object
  describing an access boundary).
- The pre-existing duplicate in `internal/premium/reactive/conversation_engine.go` (introduced under
  ADR-0033, 5-field shape) is **removed**; the reactive package imports `domain.ScopeConfig`.

### D3 — Three-set tag model
Replaces the original flat `AllowedTags` (which was self-contradictory — struct comment said AND,
SQL said OR — and *neither* pure operator safely expressed the requirements):

```go
type ScopeConfig struct {
    RequiredTags  []string // AND  — doc must carry EVERY one  (boundary)
    AnyOfTags     []string // OR   — doc must carry AT LEAST ONE (visibility/source)
    ForbiddenTags []string // NONE — exclude if doc carries ANY  (deny)
}
```

`ScopeConfig` is **purely access-control**. Policy knobs (`MaxPlanSteps`, `ToolAllowList`,
`ToolDenyList`, `LLMHints`) that existed in the prior `ScopeConfig` shape (from
`conversation_engine.go`, ADR-0033) are split into a separate `PolicyConfig` struct carried
in the caller payload. This separation prevents the access-control struct from becoming a
kitchen-sink bag, and keeps the tag-filtering logic clean.

Per-side SQL predicate (applied to a single `ScopeConfig` before intersection):
`(metadata @> ALL required) AND (metadata @> ANY any-of) AND NOT (metadata @> ANY forbidden)`

This expresses e.g. *"must be `order_db`, AND one of (`published` ∨ `public_kb`), AND not
`internal_only`"* — which a single flat list with one boolean operator cannot.

> **Intersection predicate (effective scope):** See D12 for the CNF `AnyOfClauses` model used
> when combining `caller_scope` and `agent_scope`. The per-side predicate above is a degenerate
> case with one clause.

### D4 — Effective scope = `caller_scope ∩ agent_scope` (least privilege)
- **caller_scope:** supplied per-turn by the calling application (it knows the end-user's
  permissions; Cambrian cannot).
- **agent_scope:** intrinsic to an agent's **genotype** (`AgentDefinition.ScopeProfile`). This is
  set by the Cambrian operator/admin at agent registration time — **not** by the agent author.
  A support agent is forbidden `secrets` *regardless of caller* because its genotype declares it.
- **Build the seam, not the subsystem:** `agent_scope` defaults to **empty / unrestricted** today,
  so functionally everything is caller-supplied now. The intersection point is implemented
  immediately so that adding per-agent profiles later (premium governance, D1) requires **no rewiring**.
- **Invariant:** intersection means neither side can escalate the other.
- **Genotype vs Phenotype:** Scope is a **genotype property**, not a phenotype property. Every
  instance spawned from the same `AgentDefinition` carries the identical intrinsic scope. The
  phenotype cannot diverge from the genotype's access boundary. Different instances of the same
  agent do **not** get different intrinsic tags — this is a deliberate security choice for
  auditability ("what can the support agent access?" has one answer). Per-turn variation comes
  only from `caller_scope` intersection, not from instance-specific overrides.

### D5 — Propagation: hybrid (explicit at the chokepoint, ctx-value above it)
- **Explicit** `Scope` field added to `domain.SearchOptions`. `VectorStore.Search` is the single
  point that builds SQL — it is **compiler-forced** to receive a scope.
- **Implicit (ctx-value)** carries scope through intermediate OSS helpers (`memory.Manager.Query`,
  `WorkspaceStage.PrimeForStep`, `Agent.FetchContext`) so their signatures are **not churned**.
  Boundary injects once: `ctx = domain.WithScope(ctx, effective)`.
- **Fail-closed:** if the `Search` chokepoint receives a nil/zero scope, it returns an error rather
  than unfiltered rows. This converts any dropped-scope bug (e.g. a retrieval started under a fresh
  `context.Background()`) into a loud failure instead of a silent leak.
- **Invariant (from D4):** agents are wired with the `ScopedVectorStore` **only**, never the base
  store — there is no code path to bypass the decorator.

### D6 — `ScopeSystem` sentinel for system reads
System/maintenance reads (temporal decay & GC `GetStaleMemories`,
spreading-activation graph expansion, episodic indexing) run on behalf of no agent and have no
caller scope.
- `Scope == nil` / zero at the chokepoint → **fail-closed error** ("you forgot").
- `Scope == domain.ScopeSystem` → bypass tag filtering, but as an **explicit, greppable, auditable
  token** — never an accident. A security review can enumerate every `ScopeSystem` use.
- `Scope` with tags → normal three-set filtering.
- Cost (accepted): every internal retrieval call site is audited once and tagged `ScopeSystem`.

### D7 — Reuse existing index; wire the dead `Filter` field
- The GIN index **already exists**: `pgvector_adapter.go:206` —
  `idx_doc_metadata ... USING gin (metadata jsonb_path_ops)`. `jsonb_path_ops` supports the `@>`
  containment the tag filter needs. The ADR's originally-proposed `CREATE INDEX ... USING GIN
  (metadata)` (default `jsonb_ops`) is **redundant and dropped** (it would create a second
  overlapping index).
- `SearchOptions.Filter` is currently **dead** — declared (`vector_store.go:28`) but ignored by
  `fetchCandidates` (`pgvector_adapter.go:384`). The three-set predicate is wired through here
  (non-breaking: the field already exists in the port).

---

### D8 — Write-side tag provenance & trust model (RESOLVED)

**Decision:** ADR-0034 owns the write-side trust model. Read-side filtering is theater unless
writes are themselves scoped. The invariant is:

> **"An agent cannot write a document with a classification tag that appears in its effective
> scope's `ForbiddenTags`."**
>
> *Read/write asymmetry is intentional:* an agent may be allowed to write to a scope it cannot
> read (e.g. a `ScopeConsolidator` writes `company_wide` but reads only `chat_raw`). Write
> validation checks `ForbiddenTags` only, not read capability.

**Three enforcement mechanisms:**

1. **Provenance tags** (`provenance:source=<agent_id>`, `provenance:connector=<name>`)
   are stamped by the **trusted kernel** from the authenticated execution context. They are
   immutable, non-forgeable, and never copied from agent-supplied metadata. The kernel
   knows which agent is executing because the gRPC metadata carries the authenticated
   `x-agent-id` token issued by `AgentManager` at boot time.

2. **Classification tags** (`public_kb`, `secrets`, `customer_safe`, …) come from a
   **deployment-time controlled vocabulary** (a config map or database table, not hardcoded).
   Agents request them via `memory.remember()` or `artifacts.save()`, but the Substrate validates
   each requested classification against the agent's effective read scope. If `secrets` is in
   the agent's `ForbiddenTags`, the RPC rejects the write.

3. **Unifying invariant at the RPC boundary:**
    ```go
    // In the IngestMemory handler (memory.remember())
    func (s *Server) IngestMemory(ctx context.Context, req *IngestMemoryRequest) (…) {
        agentScope, found := s.scopeResolver.Get(req.AgentID) // cached genotype profile
        if !found {
            // Unknown principal: an authenticated agentID with no genotype is an
            // anomaly (deregistered mid-session, or cache/registry skew). Fail CLOSED.
            return nil, status.Error(codes.PermissionDenied,
                "no scope profile for agent: "+req.AgentID)
        }
        effective := domain.EffectiveScope(
            domain.ScopeFromContext(ctx), // caller_scope, per-turn
            agentScope,                   // agent_scope, genotype
        )
        for _, tag := range req.Tags {
            if isClassificationTag(tag) && effective.Forbids(tag) {
                return nil, status.Error(codes.PermissionDenied,
                    "agent may not write classification in its ForbiddenTags: "+tag)
            }
        }
        // Provenance tags stamped by kernel
        effectiveTags := merge(req.Tags, provenanceTags(ctx))
        // Store with effectiveTags…
    }
    ```

    **`ScopeResolver` — cached scope profile with cross-replica revocation:**
    The Substrate maintains an in-memory cache of `agentID → ScopeConfig` warmed at startup from the
    PostgreSQL `agent_scopes` table (R1). On `POST /v1/admin/agents/{id}/scope`, the cache entry is
    invalidated across all replicas via `LISTEN/NOTIFY`, re-reading on the next miss. The contract is
    `Get(agentID) (ScopeConfig, found bool)`.

    **Storage & cross-replica revocation (R1).** Scope profiles are **not** stored in per-process
    BBolt cache with local invalidation — that only works for a single never-restarting process, and
    "binary + DB per tenant" (Non-Goals) does not forbid running multiple HA replicas of one tenant's
    binary. Local invalidation in replica A never reaches a warm replica B, which would serve stale
    scope indefinitely. Instead:
    - Authoritative scope lives in a **PostgreSQL `agent_scopes` table** (the same Postgres that backs
      pgvector — already a shared dependency). BBolt keeps only the `AgentDefinition` genotype record.
    - `ScopeResolver` **warms from PostgreSQL at boot** and holds an in-memory cache (O(1) lookups, no
      per-RPC query).
    - On `POST /v1/admin/agents/{id}/scope`, the write commits to PostgreSQL and emits a
      **`LISTEN/NOTIFY`** on an `agent_scope_changed` channel. **Every replica** is subscribed and
      invalidates its cache entry → true cross-replica revocation, bounded by notify latency (not by
      process restart, not unbounded). A short safety TTL (e.g. 60s) bounds staleness even if a NOTIFY
      is missed during a reconnect.
    - The contract guarantees: **no hot path** (cache), and **revocation within notify latency across
      all replicas** — the honest replacement for the earlier (false) "immediate, single-process"
      claim.

    **Two distinct absence cases — never conflated:**
      - `found == true` with an empty `ScopeConfig{}` → **registered but un-profiled**: unrestricted
        by design (backward-compat default, D9). `caller_scope` still narrows it.
      - `found == false` → **unknown agentID**: an authenticated principal with no genotype.
        This is **fail-closed** — the write RPC returns `PermissionDenied`, and read paths treat it
        as eligible for `ScopeSystem` only (never unrestricted-by-default). This keeps the write
        path's posture consistent with the read path's fail-closed rule (D5/D6): "scope absent" is
        never silently "scope open." A `found == false` is logged as a security warning.

**Controlled vocabulary enforcement:**
`isClassificationTag()` MUST reject unknown tags against a deployment-time controlled vocabulary
(config map or database table, not hardcoded). If any tag in `req.Tags` is not in the vocabulary,
the RPC returns `InvalidArgument` *before* scope checks. This prevents tag coinage — an agent
cannot invent a new classification like `superuser_bypass` and write under it. Vocabulary updates
are operator actions, not agent actions.

> **Zero-Hardcode pre-emption:** The controlled vocabulary table is a deterministic safety
> exception (same class as Reflexive Path / Omurilik routing). The *contents* of the vocabulary
> are data-driven (operator-configured), but the *existence* of the validation gate is a core
> invariant enforced by deterministic code, not by the Awareness layer.

**3rd-party agent safety:** A malicious or compromised agent cannot escape its scope by forging
tags, because classification tags are validated server-side against its genotype `ScopeProfile`,
and provenance tags are stamped by the kernel from the authenticated boot token — not from agent
input.

**Write-side enforcement boundary — `ScopedStoreWriter` decorator (R3):** There is **no
"trusted in-process" carve-out.** The earlier design validated only at the gRPC handler and trusted
anything calling `store.Save` directly "by process membership" — but the `ConsolidatorAgent` is
`TraitCognitive` (it runs an LLM), and D11 itself insists the LLM is *never* a security boundary. A
jailbroken or hallucinating Consolidator calling `store.Save(Tags:["company_wide","secrets"])` would
have escaped with zero validation. Process membership does not constrain a model's output.

Instead, **every write — RPC and in-process — passes through a `ScopedStoreWriter` decorator**, the
write-side twin of `ScopedVectorStore` (D5). The decorator:
1. **Validates classification** against the writer's effective scope: rejects any tag in
   `effective.ForbiddenTags`, and any tag outside the controlled vocabulary (`InvalidArgument`).
   The Consolidator writing `["secrets"]` is now **rejected** — `secrets ∈ ScopeConsolidator.ForbiddenTags`
   — because the check actually runs on its path too.
2. **Stamps provenance tags itself** (`provenance:source=<id>` / `provenance:connector=<name>`) from
   the authenticated execution context — never copied from caller/agent input. Provenance is the
   decorator's responsibility, not the writer's.

The invariant mirrors D5 exactly: **no principal — cognitive, system, or 3rd-party — ever holds a
raw, unvalidated store reference.** They are wired with the `ScopedStoreWriter` only. This is what
makes R1 and R3 consistent: once all writes go through a validating gate, the "trusted in-process"
surface that the cache-revocation and LLM-write risks both depended on simply ceases to exist.

### D9 — `AgentDefinition.ScopeProfile` (genotype-level intrinsic scope)

The agent's intrinsic scope is a **static property of its genotype**, set by the operator at
registration time. It is not in the agent's Python source code, not in its manifest, and not
self-declared.

```go
type AgentDefinition struct {
    ID              string
    Name            string
    Description     string
    Trait           AgentTrait
    Runtime         AgentRuntime
    ExecPath        string
    Capabilities    []string
    ScopeProfile    ScopeConfig  // NEW — intrinsic read/write boundary
}
```

- Set via API at registration: `POST /v1/admin/agents/{id}/scope` (core mechanism, no GUI
  required for v1).
- Defaults to `ScopeConfig{}` (empty = unrestricted read/write) for backward compatibility.
- **Validated at registration (R5):** `POST /v1/admin/agents/{id}/scope` runs `ScopeConfig.Validate()`
  and rejects unsatisfiable / out-of-vocabulary profiles before persisting (see R5).
- **Storage split (R1):** the `AgentDefinition` genotype stays in the BBolt `agents` bucket, but the
  `ScopeProfile` itself is persisted in the PostgreSQL `agent_scopes` table (shared, replica-visible).
- **Resolution:** `ScopeResolver` (D8) warms from `agent_scopes` at boot, caches in-memory (no hot
  path), and invalidates via `LISTEN/NOTIFY` on scope writes — cross-replica revocation within notify
  latency, no process restart required. See D8 for the full contract.

### D10 — Scope is genotype-static; phenotype inherits (no per-instance overrides)

**Different instances of the same genotype carry identical intrinsic scope.**

- `AgentDefinition` (genotype) carries `ScopeProfile`.
- `Instance` (phenotype) has no scope of its own and no `Instance.ScopeOverride` field. Effective
  agent scope is resolved per-RPC from the genotype via `ScopeResolver` (D8), keyed by agent ID —
  not snapshotted into the instance. All instances of a genotype therefore share one boundary.
- **Why:** Auditability. "What can the support agent access?" has one answer: the genotype's
  `ScopeProfile`. If Instance A could read `secrets` and Instance B could not, security reviews
  become impossible. The phenotype is the running process; its access boundary is fixed by design.
- Per-turn variation comes **only** from `caller_scope` intersection, which is dynamic and
  caller-controlled. A daemon handling conversations for different users will see different
  `caller_scope` on each signal, producing different `effective` scopes — but its intrinsic
  boundary never changes.

### D11 — Scope Promotion via Consolidation: Raw → Derived → General (grilled 2026-06-03)

Tag isolation creates silos. A Company Brain that learns from its data **must** bridge silos.
The bridge is not a hole in the wall — it is a **trusted, audited pipeline** that reads raw data
in narrow scope and writes *derived, anonymized knowledge* to broader scope.

> **Grilling outcome (D11-Q1..Q4):** promotion safety is **deterministic** — the LLM only
> *generalizes*, it is never the security boundary. Promotion is a **watermark-driven cross-session
> batch** (not per-session), made idempotent by an **out-of-band ledger** that keeps raw docs
> immutable, and the GDPR story is closed: **raw PII lives only in narrow Tier-0 scopes; everything
> broadly readable is provably non-personal.** Details below.

**Data tiers and their natural scope boundaries:**

| Tier | Example | Scope | Access |
|---|---|---|---|
| **Tier 0 — Raw/Specific** | Customer conversation text, order details | `customer_789`, `chat_raw` | Support agent + that customer only |
| **Tier 1 — Derived/Anonymized** | "Checkout slowness is the #1 complaint in June" | `derived`, `analytics` | Analyst agents + company operators |
| **Tier 2 — General/Public** | "Return policy: 30 days" | `public_kb`, `company_wide` | All agents + public chatbot |

**Promotion is one-way, append-only, and batched.** Raw Tier-0 docs are **never modified** (D11-Q3):
promotion state lives in an **out-of-band ledger** (`source_hash → promotion_batch_id`, a BBolt
bucket), not as a flag on the raw row. A new derived document is created at a broader scope with a
full provenance chain. Promotion runs as a **cross-session batch** (D11-Q2), never per-session —
because the anonymity guarantee (below) requires aggregating across many sessions.

**The ConsolidatorAgent (ADR-0029) is the only authorized bridge.** It does NOT use
`ScopeSystem` (which bypasses all filtering). Instead it carries a dedicated genotype profile:

```go
var ScopeConsolidator = ScopeConfig{
    RequiredTags:  nil,
    AnyOfTags:     []string{"chat_raw", "invoice_raw", "feedback_raw"},
    ForbiddenTags: []string{"secrets", "internal_only", "PII"},
}
```

> **Deterministic-safety exception:** `ScopeConsolidator` is a kernel-defined profile, not
> operator-registered. It is part of the deterministic safety surface (same exception class as
> Reflexive Path / Omurilik routing and the controlled-vocabulary gate). Operator-registered
> `ScopeProfile`s apply to 3rd-party agents; kernel-defined profiles apply to system agents.

This is least-privilege: the Consolidator can read raw customer data, but it **cannot** read
`secrets` or `internal_only`. It writes to broader scopes (e.g., `company_wide`) because its
`ScopeProfile` does not forbid them.

**Trigger & batch model (D11-Q2).** Promotion reuses the ADR-0030 lifecycle: it fires on
`MemoryPressureEvent` / a circadian pass (**not** `SessionDormant` — one session can never satisfy
the anonymity floor). `session_completion` only marks a session *promotion-eligible*; it does not
trigger a run. The unit of work is a **theme cluster** over Tier-0 docs within a rolling
**lookback window** (`ConsolidationScope.Since`), not a single session.

```go
func (c *ConsolidatorAgent) promoteBatch(ctx context.Context, scope domain.ConsolidationScope) error {
    // 1. READ: scope-filtered Tier-0 docs in the lookback window, minus already-promoted
    //    source_hashes (consulted from the out-of-band ledger — raw docs are never flagged).
    docs := c.store.Search(ctx, nil, domain.SearchOptions{
        Scope:  domain.ScopeConsolidator,         // tag filtering still enforced (no secrets/PII)
        Since:  scope.Since,                       // lookback floor, NOT an advancing cursor
    })
    fresh := c.ledger.Unpromoted(docs)             // exclude already-promoted source_hashes

    // 2. CLUSTER by theme. Only clusters spanning >= K distinct source SESSIONS are eligible;
    //    sub-K clusters are left for a later run (they may accumulate more sources).
    for _, cluster := range c.cluster(fresh) {
        if cluster.DistinctSessions() < KAnonymityFloor { // e.g. K = 5
            continue
        }

        // 3. GENERALIZE (LLM is the generalizer, NOT the security boundary).
        insight := c.llm.ExtractInsight(cluster.Docs)

        // 4. DETERMINISTIC SCRUB (backstop): regex-mask any stray pattern PII the model echoed.
        insight = c.piiMasker.Mask(insight)        // domain.RegexPIIMasker

        // 5. WRITE (idempotent): dedup key = hash of the sorted contributing source_hashes.
        //    A re-run on the same cluster upserts; a GROWN cluster supersedes the prior insight.
        key := dedupKey(cluster.SourceHashes())
        c.store.UpsertByKey(ctx, key, &domain.Document{
            Text: insight,
            Metadata: map[string]interface{}{
                "tier":          "derived",
                "source_hashes": cluster.SourceHashes(), // irreversible hashes — audit, not re-id
                "session_count": cluster.DistinctSessions(),
                "supersedes":    c.ledger.PriorKeyFor(cluster), // mark older narrower insight stale
                "tags":          []string{"company_wide", "analytics", "derived"},
            },
        })
        c.ledger.Record(cluster.SourceHashes(), key) // out-of-band; raw docs untouched
    }
    return nil
}
```

**Why this is safe — three deterministic layers (D11-Q1):**
1. **Input gate (scope):** `ScopeConsolidator.ForbiddenTags` keeps tagged `secrets`/`PII`/
   `internal_only` out of the read set entirely — those docs are silently skipped at query time.
2. **Aggregation floor (k-anonymity):** a derived doc may cross into a broader scope **only** if it
   aggregates **≥ K distinct source sessions**, enforced by counting distinct `source_session`s.
   **Per-record promotion is forbidden** — a single customer's data can never become a Tier-2 doc,
   so the output is structurally non-re-identifiable.
3. **Output scrub (deterministic backstop):** a **mandatory** `RegexPIIMasker.Mask()` pass on the
   LLM output before write catches stray pattern PII (emails, phones, numeric IDs). A net, not the
   primary defense.

The LLM is confined to *generalization*; promotion safety rests entirely on deterministic,
auditable checks (scope membership + a counted k-floor + a regex pass), never on model behavior.

**Idempotency & supersede (D11-Q3):**
- The **watermark is a lookback floor**, not an advancing cursor — sub-K clusters stay in-window and
  are reconsidered next run, so deferred insights are never stranded. Docs that age out of the
  window without reaching K are abandoned (stale, low-signal).
- Idempotency is the **out-of-band ledger** (`source_hash → promotion_batch_id`); raw Tier-0 docs
  are never mutated, preserving the append-only audit guarantee.
- Dedup key = hash of the sorted contributing `source_hashes` → re-runs upsert. When a cluster
  **grows** (5 → 7 sources) the key changes and the new insight **supersedes** the old (prior marked
  stale), keeping Tier-2 free of near-duplicate aggregates.

**GDPR / right-to-be-forgotten (D11-Q4):**
- **Tier-0 retains raw PII at rest** — the support agent needs the real email/order number, and is
  protected by *scope*, not by masking. Masking at ingest would break support; promotion is the
  *sole* masking gate crossing into broader scope.
- **Deletion is a narrow-scope purge** of that customer's Tier-0 docs (the Company-owns-PII story,
  intra-deployment).
- **Derived Tier-1/2 docs survive deletion** — by construction (k-anonymized + regex-scrubbed) they
  contain no personal data, so they fall outside GDPR's "personal data" scope. The ledger's
  irreversible `source_hash` pointing at a deleted doc is not personal data; **no cascade or
  dangling-reference cleanup is required.** An aggregate that retroactively drops below K after a
  deletion is still safe (it was never re-identifiable) and is **not** revisited.

**Agents cannot self-promote:**
A support agent whose `ScopeProfile` says `ForbiddenTags:["company_wide"]` cannot call
`memory.remember(..., tags:["company_wide"])` — the RPC rejects it (D8). Only the ConsolidatorAgent
(or other kernel-managed system processes with dedicated elevated profiles) can write to broader
scopes. The Consolidator is, by necessity, the **broadest-read principal** in the system (it reads
all `chat_raw` across customers); this is mitigated by it being in-process/kernel-defined, blocked
from `secrets`/`PII` by `ForbiddenTags`, and only ever *emitting* k-aggregated, scrubbed output.

**Tag naming convention (advisory) vs vocabulary membership (enforced):**
The suffix *semantics* below are an **advisory** convention to keep tag names legible — Cambrian
does not interpret suffixes. What **is** enforced (D8) is that every tag is a member of the
deployment's controlled vocabulary; unknown tags are rejected with `InvalidArgument`.

| Tag suffix | Meaning (advisory) | Example |
|---|---|---|
| `_raw` | Unprocessed, narrow scope | `chat_raw`, `invoice_raw` |
| `_derived` | Anonymized/aggregated from raw | `analytics_derived` |
| `_public` | Company-wide, safe for all agents | `kb_public`, `policy_public` |

The Company operator decides which tags exist (the vocabulary). Cambrian enforces vocabulary
membership on write (D8) and that the agent's `ScopeProfile` permits what it tries to read/write.

### D12 — `ScopeConfig` intersection (least-privilege combination)

The effective scope is the **intersection** of `caller_scope` and `agent_scope`. The set operation
is defined field-by-field:

| Field | Combination Rule | Rationale |
|---|---|---|
| `RequiredTags` | Union: `caller.Required ∪ agent.Required` | More required boundaries ⇒ fewer docs pass (narrower) ✓ |
| `AnyOfTags` | **Conjunction of OR-clauses (CNF):** each side's `AnyOfTags` becomes one clause; the doc must satisfy **ALL** clauses | Each side's whitelist is an OR-condition; intersecting two OR-conditions means the doc must satisfy **both** (AND of ORs) |
| `ForbiddenTags` | Union: `caller.Forbidden ∪ agent.Forbidden` | More forbidden tags ⇒ fewer docs pass (narrower) ✓ |

**Why AnyOfTags cannot be a flat Union:**
`AnyOfTags` is an OR-gate (whitelist): "doc must carry at least one of these tags." If caller says
`AnyOf:["published"]` and agent says `AnyOf:["support"]`, a doc tagged `["support"]` satisfies the
agent's OR but **not** the caller's. A Union `AnyOf:["published","support"]` would incorrectly
allow it — the caller never authorized `support`. The correct intersection is CNF:
`(doc matches caller.AnyOf) AND (doc matches agent.AnyOf)`.

**EffectiveScope representation:**
```go
type EffectiveScope struct {
    RequiredTags  []string     // Union of caller + agent RequiredTags
    AnyOfClauses  [][]string   // CNF: each inner slice is one side's AnyOfTags (OR-set);
                               // all clauses must be satisfied (AND-composed)
    ForbiddenTags []string     // Union of caller + agent ForbiddenTags
}
```

For the common case where only one side sets `AnyOfTags`, `AnyOfClauses` has one element and
behaves identically to the original flat list. When both sides set `AnyOfTags`, it carries two
clauses.

**SQL predicate (updated for CNF).** Illustrative shape below; the real implementation builds
parameterized `metadata @> '{"tags":["x"]}'::jsonb` containment terms (the form the existing
`jsonb_path_ops` GIN index serves), not bare tag-string arrays:
```sql
-- Required: must carry ALL required tags  → AND of per-tag containment
  metadata @> '{"tags":["r1"]}'::jsonb AND metadata @> '{"tags":["r2"]}'::jsonb

-- AnyOfClauses (CNF): each clause is an OR of per-tag containment; all clauses ANDed
AND (metadata @> '{"tags":["a1"]}'::jsonb OR metadata @> '{"tags":["a2"]}'::jsonb)  -- clause_1
AND (metadata @> '{"tags":["b1"]}'::jsonb)                                          -- clause_2 (if both sides set AnyOf)

-- Forbidden: must not carry ANY forbidden tag
AND NOT (metadata @> '{"tags":["f1"]}'::jsonb OR metadata @> '{"tags":["f2"]}'::jsonb)
```

**Precedence:** `ForbiddenTags` > `AnyOfClauses` > `RequiredTags`. A forbidden tag on a document
immediately disqualifies it, regardless of any other matching tags.

**Example:**
- `caller`: `Required:["customer_789"], AnyOf:["published"], Forbidden:["secrets"]`
- `agent`: `Required:nil, AnyOf:["support"], Forbidden:["internal_only"]`
- `effective`: `Required:["customer_789"], AnyOfClauses:[["published"],["support"]], Forbidden:["secrets","internal_only"]`

A document tagged `["customer_789", "published", "internal_only"]` → excluded by `ForbiddenTags`.
A document tagged `["customer_789", "support"]` → **excluded** (matches Required and agent.AnyOf,
but fails caller.AnyOf — `"published"` is not present).
A document tagged `["customer_789", "published", "support"]` → passes (satisfies Required,
matches both AnyOf clauses, no Forbidden).

**Unsatisfiable scopes — `ScopeConfig.Validate()` (R5).** A scope like
`Required:["secrets"], Forbidden:["secrets"]` is unsatisfiable: the read path returns zero rows and
the write path rejects everything. Both are *safe* (fail-closed), but the operator gets no signal
they created a **zombie agent** that silently does nothing. Two layers close this:

- **Static (registration time):** `POST /v1/admin/agents/{id}/scope` runs `ScopeConfig.Validate()`
  and **rejects** before persisting (D9): `RequiredTags ∩ ForbiddenTags ≠ ∅`, every `AnyOfTags`
  element also in `ForbiddenTags` (whitelist fully denied), or any tag outside the controlled
  vocabulary. The operator gets a `400` with the specific conflict — not a silent zombie.
- **Dynamic (intersection time):** D12 intersection can produce an unsatisfiable `effective` from two
  *individually valid* scopes (`caller.Required=["secrets"]` ∩ `agent.Forbidden=["secrets"]`).
  Returning zero rows is the correct, safe result — so this does **not** error — but the chokepoint
  **emits a warning log** (`"unsatisfiable effective scope: Required∩Forbidden={secrets}"`, the R4
  core audit line). This makes "why is this agent blind?" diagnosable instead of a black box.

### D13 — `caller_scope` transport via Handoff.Context

`caller_scope` is serialized into `Handoff.Context` (and from there into `AgentTask`) as three
string-slice keys:
- `_required_tags` — JSON array of strings
- `_any_of_tags` — JSON array of strings
- `_forbidden_tags` — JSON array of strings

The SDK's `AgentTask` constructor parses these into a `ScopeConfig` object before invoking
`run()`. If the keys are absent, `caller_scope` is treated as `ScopeConfig{}` (unrestricted),
but the `agent_scope` still applies (D9).

**Example Handoff.Context:**
```json
{
  "_required_tags": "[\"customer_789\"]",
  "_any_of_tags": "[\"published\",\"support\"]",
  "_forbidden_tags": "[\"secrets\",\"internal_only\"]",
  "_conversation_id": "conv-123"
}
```

**Forgeability is fatal to caller_scope — so caller_scope enforcement ships in PHASE 2 only (R2).**
`Handoff.Context` is plaintext JSON held by the agent process. A compromised intermediate agent (A)
can strip `_forbidden_tags` before forwarding to agent (B), widening B's effective scope. Therefore
**caller-supplied scope cannot be a security boundary until it is transported non-forgeably.** The
previous draft shipped this as a "known gap" under an `Accepted` status — that was an exploitable
hole advertised as protection. Corrected via explicit phasing:

**Phase 1 (shippable now) — `agent_scope`-only enforcement.**
- `agent_scope` comes from `ScopeResolver` (server-side, Postgres-backed, never from `Handoff.Context`)
  — it is **non-forgeable**. Phase 1 enforces *only* this.
- The `_required_tags` / `_any_of_tags` / `_forbidden_tags` keys are **ignored for enforcement** in
  Phase 1. The Substrate **does not** rely on caller-supplied forbidden tags, and the ADR makes **no
  protection promise** that depends on them. An agent stripping them changes nothing, because nothing
  trusts them.

**Phase 2 (BLOCKED on `domain.Session.CallerScope`) — caller_scope enforcement.**
- Requires `domain.Session` to gain a `CallerScope ScopeConfig` field, persisted server-side at
  `StartConversation` / dispatch, looked up via `session_token_id`.
- The Substrate then **re-derives** `effective = caller_scope ∩ agent_scope` per RPC from
  `ScopeResolver` (agent) + the session record (caller) — **never** from `Handoff.Context`.
- Only once this lands may the ADR claim caller-scoped protection. Until then the `Handoff.Context`
  keys remain **advisory** (SDK convenience for constructing an initial object) and carry **no
  security weight**.

> **Hard rule:** enforcement may not advertise a guarantee whose transport is forgeable. Phase 1 is
> honest because it promises only `agent_scope`. Phase 2 unlocks `caller_scope` *and* the promise
> together, atomically with `Session.CallerScope`.

## Blocking Risks & Resolutions (raised 2026-06-03)

Five architectural risks were raised against the otherwise-complete D1–D13. All five blocked
acceptance; each is resolved in-place in the decisions above. Summary:

| # | Risk | Resolution | Touches |
|---|------|-----------|---------|
| **R1** | `ScopeResolver` cache invalidation was single-process fiction (BBolt-local; warm replicas serve stale scope indefinitely). | Scope profiles move to a **PostgreSQL `agent_scopes` table**; `ScopeResolver` warms at boot and invalidates via **`LISTEN/NOTIFY`** across all replicas (+ 60s safety TTL). "Immediate" → "within notify latency, cross-replica." BBolt keeps only the genotype. | D8, D9 |
| **R2** | D13 forgeability was an *accepted* CVE: a compromised agent strips `_forbidden_tags`; no server-side defense. | **Phased.** Phase 1 ships `agent_scope`-only (non-forgeable, from `ScopeResolver`) and makes **no promise** depending on caller tags. `caller_scope` enforcement is **disabled until `Session.CallerScope` lands** (Phase 2). Status reverted to Proposed. | D13, Status |
| **R3** | "Trusted in-process" was fiction for a **cognitive** (LLM) system agent — the Consolidator had unvalidated `store.Save`. | **`ScopedStoreWriter` decorator** validates `ForbiddenTags` + vocabulary on **every** write (incl. Consolidator) and stamps provenance itself. The in-process carve-out is **deleted**. | D8, D11 |
| **R4** | Audit logging was fully paywalled → OSS enforcement is a silent-failure black box. | **Basic access-decision logging is CORE** (one `slog` line per deny / `ScopeSystem` use / promotion / unsatisfiable scope). The premium product is the *queryable, retained, tamper-evident* audit store — not the existence of logs. | D1 |
| **R5** | Unsatisfiable scopes (`Required ∩ Forbidden ≠ ∅`) created silent zombie agents with no operator feedback. | **`ScopeConfig.Validate()`** rejects at registration (static); intersection-time unsatisfiability is safe (zero rows) but **logged as a warning** (dynamic, via R4). | D3, D9, D12 |

**R1 + R3 reinforce each other:** once every write goes through a validating `ScopedStoreWriter`
(R3), the "trusted in-process" surface that made both the stale-cache (R1) and LLM-write (R3) risks
dangerous ceases to exist — no principal ever holds a raw, unvalidated store reference, read or write.

## Consequences

### Good
- Access scoping is a **core security primitive**, secure-by-default in OSS builds.
- Basic access-decision audit logging is **core** — OSS operators can investigate suspected leaks.
- Single canonical `ScopeConfig`; no premium/OSS type divergence.
- Three-set model expresses real access policies without the cross-leak / over-restriction failure
  modes of a single flat tag list.
- Fail-closed + explicit chokepoint + `ScopeSystem` make dropped-scope a loud failure, not a silent
  leak.
- Reuses existing infrastructure (GIN index, `SearchOptions.Filter` field).

### Bad / Cost
- Every internal/system retrieval call site must be audited once and explicitly tagged
  `ScopeSystem` (deliberate — produces the exact list a security reviewer wants).
- ctx-value propagation is implicit; safety depends on fail-closed catching any path that forgets to
  seed the scope.
- Write-side enforcement (D8) requires a controlled vocabulary table and RPC-level tag validation;
  this is additional wiring at every write entry point (`IngestMemory`, `UploadArtifact`).
- Genotype-static scope means operators must plan agent profiles carefully; a misconfigured
  `ScopeProfile` on a widely-used agent affects all its instances. Scope changes are visible
  immediately (cache invalidation), but there is no "undo" for writes already committed under
  the old scope.
- Promotion (D11) adds infrastructure: an out-of-band promotion ledger (BBolt bucket), a theme
  clustering pass, a configurable `KAnonymityFloor`, and a mandatory `RegexPIIMasker` on the write
  path. The k-anonymity floor means **low-volume insights are never promoted** — a genuinely rare
  but real pattern that spans fewer than K sessions stays siloed. This is the accepted cost of
  structural non-re-identifiability.

### Neutral
- Tag strings remain opaque to Cambrian; their meaning is the integrating application's convention.
- Genotype-static scope prevents per-instance capability variation (e.g., "this one support agent
  gets temporary admin access"). If that pattern is needed, register a second agent definition
  (`support_agent_admin`) with a broader `ScopeProfile`.

## Related
- REQ-CHATBOT-001 (full requirement document)
- ADR-0033 (Daemon Agent Architecture — `ConversationEngine`, conversation daemon)
- ADR-0031 (Universal Input Router — classification layer)
- ADR-0028 (External Ingestion — document tagging at ingestion time; controlled vocabulary D8)
- ADR-0015 (Engram Engine — LTM storage foundation)
- REQ-SDK-002 (SDK v2 — `ArtifactManager` tag isolation, `memory.remember()` scope validation)
