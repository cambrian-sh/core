---
id: 0126
title: The Published Tool Surface and the Cambrian MCP Endpoint
status: Implemented
date: 2026-08-13
supersedes: []
superseded_by: []
depends_on:
  - 0043-mcp-tool-provider
  - 0057-open-core-boundary
  - 0074-plugin-architecture
  - 0082-additive-licensed-plugins
  - 0085-access-policy-port-and-extraction
  - 0086-tool-effect-classes
  - 0101-config-and-secret-store
  - 0103-evidence-receipts
  - 0111-typed-query-plane
  - 0118-substrate-read-scope-and-agent-retrieval
  - 0122-self-installing-kernel-setup
---

# ADR-0126: The Published Tool Surface and the Cambrian MCP Endpoint

## Status

Implemented — phases 1 and 2.
Amended 2026-08-14: the owner's capability-parity directive folded in
(Context §"The parity directive", D5, D10–D12, build order phases 4–5).

**Status corrected 2026-08-20.** This record read *"Proposed — design only, no code
written"* after the endpoint had shipped. What exists: the MCP server at
`internal/infrastructure/mcpserve/` (`mcpserve.go`, `coretools.go`, `middleware.go`, with
`endpoint_test.go`, `contract_test.go` and a `tools_list.golden.json` fixture), the wiring
at `app/mcp_endpoint.go` and `app/mcp_runtime.go`, `domain.SystemTool` in
`domain/published_tool.go`, `IdentityResolver` and `StrangerPolicyFor` in
`domain/identity_binding.go`, the token CLI and bridge in `app/mcp_cli.go`, the premium
published tools (`get_receipt`, `check_policy`, `explain_access`) in their respective
plugins, and the parity checker (`scripts/check-parity.sh`, `docs/published-tool-parity.md`).
Outstanding: the live rig run, and ADR-0127's contribution lane, which remains genuinely
unbuilt. See ADR-0128 §8.

**Date:** 2026-08-13
**Relates to:** ADR-0043 (the MCP *client*, whose Option C rejection this must
reconcile), ADR-0074/0082 (the plugin registry and entitlement chokepoint this
extends), ADR-0073/0118 (the operator and agent transport planes this sits
beside), ADR-0085/0086/0087 (the Authorizer port, tool effects, and PDP this
reuses), ADR-0103 (the receipts this must be able to hand back), ADR-0111/0118
(the query plane and scoped seam the tools call), ADR-0101 (the secret store
that holds the endpoint credential), SEC-03 / `app/listener_security.go` (the
bind-time TLS discipline a new listener must inherit)

---

## Context

`docs/research/substrate-position-and-roadmap.md` F5 (Wave 1) names a Cambrian
MCP server as *"the single highest-leverage item after Wave 0."* The 7 August
product review ranks it #1 of nine by leverage-per-engineering-day and makes it
load-bearing for the week-6 exit test:

> Someone who is not you pulls the image, runs it on a machine you have never
> touched, **points Claude Code at the MCP server, asks a question, gets a cited
> answer, is denied by a policy, sees why, and verifies the receipt chain.**
> Twice in a row, without you touching a config file.

That sentence is the acceptance criterion for this ADR. Note what it demands
beyond "expose retrieval": a *scoped* principal (so a policy can deny), an
*explanation* (so the denial is legible), and a *receipt handle* (so the chain
can be verified). A tool surface that only answers questions passes none of it.

### What the tree actually holds today

Verified by reading the working tree on 2026-08-13:

| Fact | Evidence |
| --- | --- |
| `github.com/modelcontextprotocol/go-sdk v1.6.1` is already a **direct** dependency of core | `cambrian-core/go.mod:13` |
| The SDK's **server** half is already exercised in-tree | `cmd/reference-fs-mcp/main.go:118-131` (`NewServer` + `AddTool` + `StdioTransport`) |
| A streamable-HTTP server is already stood up in tests | `internal/infrastructure/mcp/connector_test.go:27` (`mcpsdk.NewStreamableHTTPHandler`) |
| Cambrian is otherwise **client-only** | `internal/infrastructure/mcp/{connector,handler,secrets}.go` |
| The SDK negotiates protocol versions itself | SDK advertises `2025-11-25`, accepts down to `2024-11-05` |
| There is **no** `Registry.AddTool` seam | the tool registry is fed by exactly two sources, `app/app.go:1410-1464`: filesystem discovery and the MCP connector |
| No query RPC returns a receipt handle | `QueryID` is minted inside `internal/memory/query.go:508 emitDecision` and handed only to in-process `DecisionObserver`s; `grep query_id api/proto/*.proto` is empty |

**Correction to a widely-repeated claim in this repo:** the doc comment at
`internal/infrastructure/mcp/handler.go:3` says the SDK "is imported ONLY here …
(separability rule)". That is a *convention stated in prose*, not a mechanized
gate — `scripts/check-separability.{sh,ps1}` enforce only (1) no premium imports
in core and (2) OTel confined to `internal/telemetry`. Neither mentions the MCP
SDK. The rule the comment is really protecting is **"`domain/` stays
protocol-agnostic."** This ADR honours that rule and needs no gate change.

### The protocol moved under this ADR (checked 2026-08-14)

Spec **2026-07-28** is final — the largest revision since MCP launched. What
matters here: a **stateless core** (no `initialize` handshake, no session ids;
version/capabilities travel in `_meta` per request, plus a mandatory
`server/discover` RPC), an **official Tasks extension** (tool calls answer with
task handles; clients drive `tasks/get`/`update`/`cancel`), full **JSON Schema
2020-12** in tool schemas, a formal **extensions framework** (reverse-DNS ids),
**MCP Apps** (sandboxed server-rendered HTML panels) — and **sampling, roots and
logging are deprecated** (annotation-only, ≥12 months before removal).

`go-sdk v1.7.0+` ships it; the streamable HTTP transport accepts 2026-07-28 only
with `StreamableHTTPOptions.Stateless = true`, otherwise clients negotiate down
to 2025-11-25. **Decision: build phase 1 against v1.7.0+, serve stateless, and
let the SDK negotiate down for older clients** — zero compatibility risk, and
statelessness matches D4's per-request middleware exactly (nothing about a
caller lives in transport state; a hosted deployment load-balances without
sticky sessions). Threaded consequences: D10 phase 4 (Tasks extension), D12
(sampling deprecation), D13 (pull confirmed as the only lawful direction).

### Reconciling ADR-0043 D10

ADR-0043 explicitly rejected Option C: *"consume external MCP servers + retire
native tools, **not** 'wrap our own `tools/` as an MCP server'."* That rejection
stands and this ADR does not disturb it. The rejected proposal was to re-export
the **Python `tools/` subprocess registry** outward — which buys no interop and
loses subprocess confinement. This ADR proposes something categorically
different: publishing the **memory, knowledge, policy and receipt surfaces** —
Cambrian's product, not its tool plumbing. D2 below makes the two directions
structurally incapable of leaking into each other.

### Naming hazard, stated up front

In this codebase "MCP server" already means *a foreign server Cambrian dials as
a client*: `config.MCPConfig.Servers`, `Registry.AddMCPServer`, the RPCs
`SaveMCPServer`/`RemoveMCPServer`/`TestMCPServer`, and the entire `/mcp` operator
console. Reusing the phrase for the inbound direction would make every existing
comment, RPC name and console label ambiguous.

Vocabulary fixed by this ADR:

- **MCP connector / MCP servers** — outbound, unchanged, ADR-0043.
- **The Cambrian MCP endpoint** — inbound. Served from the **Published Tool
  Surface**, a transport-agnostic registry of tools Cambrian offers outward.

### The parity directive (owner, 2026-08-14)

**Any capability Cambrian's own SDK agents hold must be reachable by external
agents over this endpoint.** An external coding agent is not a second-class
consumer of a curated demo subset; it is another agent, with another transport.
The tool list in D5 is therefore not a product choice to be revisited per tool —
it is the *current projection* of the agent plane, and it grows whenever the
agent plane grows (mechanized in D11).

Two things are permanently excluded from parity, because they are not agent
capabilities at all but **the kernel's own authority that agents borrow under
grants**:

1. **Foreign MCP tools** (`mcp:<server>/<tool>`, dialled by the ADR-0043
   connector). Republishing them outward makes Cambrian an open proxy: its
   credentials on those servers exercised on behalf of arbitrary token holders —
   a confused deputy. External agents can dial those servers themselves; they
   lack no capability, only a credential they should not borrow.
2. **Filesystem-discovered subprocess tools** (the `tools/` registry). Publishing
   them hands external callers execution with the kernel's ambient authority —
   the exact thing ADR-0043's Option C rejection was protecting.

Both exclusions are written into the parity ledger (D11) with these reasons.
Parity covers Cambrian's *product surfaces*: memory, knowledge, policy,
receipts, tasks, conversations.

**The corollary on identity (owner, same date):** every external agent that
registers via the MCP endpoint is **a principal in its own right**, exactly as
internal agents are — not an anonymous holder of a shared surface credential.
D4 is written to that end state.

---

## Decisions

### D1 — The port is a *published tool*, not an *MCP tool*; MCP is one renderer

`domain/published_tool.go` (new) declares protocol-agnostic data plus a
one-method handler:

```go
// PublishedTool is a tool Cambrian OFFERS to external callers (ADR-0126).
// It is the outbound counterpart of SystemTool, which is a tool Cambrian's own
// agents may CALL. The two never mix — see D2.
type PublishedTool struct {
    Name        string        // ^[a-z][a-z0-9_]{0,47}$ — see D7
    Title       string        // human label for a tool picker
    Description string        // written for an LLM caller, not an operator
    InputSchema []byte        // JSON Schema, object-typed at the top level
    Effects     []ToolEffect  // the ADR-0086 CLOSED set, reused verbatim
    ReadOnly    bool          // maps to the MCP readOnlyHint annotation
    Capability  string        // operator capability this tool rides on ("" = always)
}

type PublishedToolHandler interface {
    // Invoke runs the tool for the principal already established on ctx.
    // It must never read a principal from its arguments (D4).
    Invoke(ctx context.Context, args json.RawMessage) (PublishedToolResult, error)
}

type PublishedToolResult struct {
    Structured any      // marshalled to MCP structuredContent
    Text       string   // human/LLM-readable rendering
    ReceiptRef string   // D6 — the correlation handle, "" when no receipt lane
}
```

`domain/` gains no MCP import; the SDK stays in `internal/`. The registry is a
port so a second renderer (an HTTP/OpenAPI surface, an ACP adapter per roadmap
F6) is additive rather than a rewrite.

**Rejected: reuse `domain.SystemTool` + `InMemoryToolRegistry`.** Tempting —
AGENTS.md §4.3 forbids second pathways, `SystemTool` is already pure data with a
JSON schema and effects, and `InMemoryToolRegistry.Register` is already the
single validation chokepoint. But the registries face opposite directions, and
fusing them has two concrete failure modes, not stylistic ones: (a) every
published tool would appear in *internal agents'* tool menus, so `remember`
becomes an agent-callable action with no tool grant behind it; (b) every foreign
MCP tool already in the registry (`mcp:<server>/<tool>`) would be re-published
outward, turning Cambrian into an open proxy for whatever the operator dialled.
The effect vocabulary *is* shared (`domain.ToolEffect`) so policy stays one
thing; the registries are not.

### D2 — `Registry.PublishTool` is Tier-2 add-many, duplicate names are fatal

```go
// PublishTool contributes a tool to the OUTBOUND Published Tool Surface
// (ADR-0126). Distinct from AddMCPServer, which CONSUMES a foreign MCP server.
// Tier-2 add-many. A duplicate name is an error: two handlers for one tool name
// is two answers to one question with no way to say which held.
func (r *Registry) PublishTool(owner string, t domain.PublishedTool, h domain.PublishedToolHandler) error
```

Folded in `applyPlugins` alongside the other add-many points, keyed by name with
the owner recorded for attribution — the `AddAgentGRPCService` shape
(`app/plugin.go:313`), which already rejects duplicate keys for exactly this
reason, rather than the silent-append shape used by observers.

**Entitlement needs nothing new.** The chokepoint at `app/plugin.go:589-613`
runs before `Register`, so an unentitled plugin never publishes a tool — it
cannot be listed and cannot be called, by construction. ADR-0082 D6 explicitly
rejects per-capability gating; do not add a per-tool entitlement check.

**Register vs Build.** Declarations (name, schema, effects) are Register-time
data. Handlers needing `KernelServices` — SQL, the query plane, the LLM — are
constructed in `Build`, following the `substrateplugin` stable-value-then-populate
pattern and its `var _ app.Builder = (*Plugin)(nil)` compile guard.

### D3 — The endpoint gets its own listener, built through `transportCredentials`

`internal/infrastructure/mcpserve/` (new) holds the SDK-facing adapter: it reads
the composed Published Tool Surface, builds one `mcpsdk.Server`, and serves it
over `mcpsdk.NewStreamableHTTPHandler`.

The listener is **its own**, on `server.mcp.port`, but constructed through the
existing `transportCredentials(cfg)` + `secureListener(...)` path from
`app/listener_security.go`, so SEC-03's property holds: *it refuses to bind a
routable address without either a cert or an explicit `server.insecure_localhost`.*

**Rejected: mount on the ADR-0028 ingestion HTTP server.** That mux is the
obvious host — it already exists and already takes a bearer token. But it is
built as `&http.Server{Addr: fmt.Sprintf(":%d", port)}` (`app/app.go:2758`):
plaintext, all interfaces, no TLS decision, no bind-address honoured. Hanging the
product's public front door on it would silently un-do SEC-03 for the one surface
strangers are meant to reach.

**Rejected: multiplex onto the main gRPC listener.** The `demuxListener` already
peeks a byte to split TLS from plaintext h2c, so a third branch is conceivable —
but gRPC and plain HTTP/1.1 on one port is a debugging tax paid forever for one
saved port number.

**Boot-order constraint (do not skip this).** `Connector.ConnectAll` is
synchronous during boot; the debugging playbook records that gRPC is not really
serving until MCP *client* init finishes, ~40-45 s after the port opens. The MCP
endpoint must be started **before**, or independently of, that work — otherwise
`claude mcp add` times out against a kernel that looks up.

### D4 — Authentication: a bearer token that names an external sender on a new `mcp` surface

Neither existing identity fits:

- **Operator bearer** (`OperatorConsole.Login`) mints a token into a plain
  in-process map with no TTL (`internal/substrate/operator/authz.go:104-109`), so
  it dies on every kernel restart. A coding agent cannot re-login; the UI only
  copes by storing the password in a keychain. It is also role-scoped
  (operator/viewer), not data-scoped.
- **`x-agent-id`** is an unauthenticated designation, and AGENTS.md §5 forbids
  giving a human principal an `x-agent-id` path.

So: a new `SurfaceRef` kind `domain.SurfaceMCP = "mcp"`, and a bearer credential
resolved to a principal through the **existing** `domain.IdentityResolver` —
the port whose charter is already *"who is this external sender?"*, with
`StrangerPolicyFor` already deciding what an unbound sender may do. Two existing
mechanisms, no fifth pathway.

Per request, one middleware establishes everything before any handler runs:
constant-time bearer compare → external id → `IdentityResolver` → principal →
`domain.WithPrincipal` + `domain.WithSurface(SurfaceMCP)` → authorize the tool's
declared `Effects` via `Authorizer.Authorize` with `ResourceKind "tool"`. Every
downstream read is then scoped by machinery that already exists, because the
scoped seams take a principal and resolve the predicate themselves
(`internal/authz/query_knowledge.go:24`).

**Every registered external agent is a principal (owner directive, 2026-08-14).**
The bearer token is not a door key to an anonymous surface — each token *names a
client*, and each named client resolves to its **own principal**, scoped by
`agent_scopes` like any internal agent, distinguishable in policy, in receipts,
and in the decision journal. Concretely: `cambrian mcp token create <client-name>`
(a kernel subcommand beside ADR-0122's `setup`/`status`/`stop` — no operator
console needed) mints a token bound to external id `mcp:<client-name>`, stores it
in the ADR-0101 secret store, and registers the external id so `IdentityResolver`
can bind it. Presenting the token establishes that principal; `StrangerPolicyFor`
governs a token whose external id was never bound. Unlike operator tokens, which
die with the kernel by design, **these tokens survive restarts** — the client's
MCP config is static, and a token that rotates on restart makes every coding
agent's config silently stale.

**Identity is two tags plus membership (owner, 2026-08-14).** The context
established by the middleware carries *both* the individual principal
(`mcp:<client-name>`) *and* the surface (`SurfaceMCP`) — neither substitutes for
the other. And a registered client can be added to the **same groups/roles
internal agents belong to**, through the existing live role-rebinding surface
(contract 0096) — no parallel grouping system for external principals. Policy
authors therefore get three targeting granularities, the same three they already
have internally:

| Target | Means | Example rule |
| --- | --- | --- |
| everything arriving from outside | surface `mcp` | "external agents never read `finance/*`" |
| a class of clients | group/role | "`coding-agents` may `remember` into `eng/*`" |
| one client | principal `mcp:<client-name>` | "`mcp:ci-bot` is read-only" |

**The token is never optional.** TLS may be waived on loopback; authentication
may not. The reason is asymmetric risk: OSS ships `AllowAllAuthorizer`, which
**fails open** by design (`domain/authz.go:386`). An unauthenticated endpoint on
top of a fail-open authorizer is an anonymous read of the entire corpus.

**Honest open-core statement, to be repeated in the README:** in an OSS build
with no authz plugin there is no `IdentityResolver` and no PDP, so the endpoint's
reach is "the surface is the identity" — every holder of the token has identical,
unscoped reach. Per-caller scoping, denial explanations and receipts arrive with
the premium plugins. This is the same boundary ADR-0118 already drew for agent
retrieval; it is not a new one.

### D5 — Which tools, and who owns them

The split falls out of the existing module boundary rather than being imposed:

| Tool | Owner | Backing seam |
| --- | --- | --- |
| `search_memory` | **core** | `QueryService.Search` → doc_id, text, section_path, score, tags |
| `ask_memory` | **core** | `AnswerMemory` — grounded prose with inline `[n]` markers + citations. *The demo tool.* |
| `get_document` | **core** | `domain.DocumentGetter` / `GetDocument` |
| `list_documents` | **core** | the ADR-0099 `ListDocuments` seam (contract 0070) — browse + filter by labels; without it an external agent can search but never navigate |
| `remember` | **core** — *deferred, not in phase 1 (owner, 2026-08-14)* | `KernelServices.Ingestor` → `IngestionManager.ProcessSync`, the one canonical document write path |
| `query_knowledge` | premium `substrateplugin` | typed query plane; refusal-only failure with a guarantee label |
| `ask_knowledge` | premium `substrateplugin` | NL → LLM-drafted AST → name-verified against a real catalog → executed scoped |
| `check_policy` | premium `substrateplugin` | `policycheck.Check` — deterministic claim-vs-bound, no LLM |
| `explain_access` | premium `authzplugin` | `domain.Explainer.ExplainAccess` — **this is the "sees why" half of the week-6 test** |
| `get_receipt` | premium `receiptsplugin` | `ReceiptLane.GetReceipt` / `VerifyChain` |

Four read-only OSS tools (`search_memory`, `ask_memory`, `get_document`,
`list_documents`) make the endpoint genuinely useful standalone; five premium
tools demonstrate the seam this ADR exists to build. Nobody has to take the
premium build on faith — `plugins/langfuse/plugin.go` is 92 lines and shows the
shape.

**`remember` is deferred (owner ruling, 2026-08-14): phase 1 is read-only.**
The endpoint that strangers point coding agents at cannot corrupt a pilot's
corpus, and the security story for the exit test is trivial. The write tool
lands later — **no earlier than the per-client identity work (phase 2)**, so
that every write is party-attributable from birth; the hardening below is its
shipping contract, kept here so the deferral loses no design.

`remember` is the only tool declaring `EffectWrite`. It must go through
`ProcessSync` and nothing else: AGENTS.md §2 records four existing write pathways
into the vector store and calls a fifth *"a defect, not a feature."*

Two properties the `remember` handler must establish, both already paid for
elsewhere in this repo:

- **The surface establishes scope — with the full provenance triple (owner,
  2026-08-14).** The worldsim deployment demonstrated that unclassified writes
  cause leaks to strangers *and* over-refusals at once, from the same defect;
  the owner rule is that the world never supplies scope — the ingress does. The
  MCP endpoint is an ingress head, so every `remember` write auto-carries three
  attributions, all already present on the D4 context: the **surface** (`mcp`),
  the **client** (`mcp:<client-name>` — the connection that wrote it), and the
  **party** (the principal's owner — the person whose experience it is). The
  client and the party are not the same thing: after D13's surface≠machine
  split, `claude-code-laptop` is plumbing; *whose memory this is* is the party.
  Experiential memory written by many people's agents is multi-tenant by
  construction, and the party attribution is what keeps it partitionable. Set
  the ADR-0121 party column from it — the party-scoping mechanism shipped and
  sits inert for want of exactly this data; the MCP write path is its natural
  populator. Reads then need nothing new: the scoped seams already take a
  principal and resolve the predicate. In an OSS build the tags and party are
  **recorded regardless** — provenance is OSS mechanism; enforcement arrives
  with the premium PDP, per the honest open-core statement in D4.
- **Writes are idempotent.** Coding agents retry on timeout. `remember` accepts
  an optional caller-supplied idempotency key and otherwise dedups on content
  hash, so a retried call cannot mint a second document.

**Latency note.** `ask_memory` invokes an LLM under a 90 s budget. Its
description must say so, `search_memory` stays the cheap default, and the handler
should signal progress — `ServerSession.NotifyProgress` on 2025-11-25 sessions;
verify the stateless-mode progress mechanics against the final 2026-07-28 SDK
surface at build time (the multi-round-trip model reshapes server→client
signalling). If a long call cannot signal progress in stateless mode, answer it
as a Tasks-extension handle instead (D10 phase 4 machinery, reused).

### D6 — Plumb a caller-supplied correlation id into `emitDecision`

This is the one genuinely missing piece and the reason the week-6 test cannot be
passed by wiring alone. `QueryID` is minted at
`internal/memory/query.go:508` and handed only to in-process observers; no RPC
returns it. `get_receipt` would have nothing to look up.

The fix is small and belongs in OSS: `domain.WithQueryCorrelation(ctx, id)`, and
`emitDecision` prefers a correlation id present on the context over minting one.
The MCP middleware mints the id per tool call, puts it on the context, and
returns it as `PublishedToolResult.ReceiptRef`. Every caller benefits, and no
proto changes.

**Care required:** agentic retrieval emits **one decision per hop**, so a single
tool call legitimately produces several receipts. The correlation id must
therefore be a *prefix* the hops extend (`<corr>-h1`, `<corr>-h2`), and
`ListReceipts` must be able to filter on it — not a 1:1 id, or the second hop
overwrites the first and the chain reads as incomplete.

**Rejected: add `query_id` to `QueryMemoryResponse` / `AnswerMemoryResponse`.**
Correct eventually, and it forces an operator contract bump past 0097 plus
propagation to all four vendored proto copies (`ui/proto`, `cli/proto`, the
benchmark stubs — and per project memory, the premium protos are vendored too and
the contract sequence is global). Not on the critical path for the MCP endpoint;
do it when a gRPC consumer needs it.

### D7 — Tool names are `snake_case`, never dotted

The roadmap writes `memory.query`, `policy.check`. Do not ship those names. This
codebase already paid for the lesson: ADR-0097 D7 records that tool names must
match `^[A-Za-z0-9_-]{1,64}$` because `mcp:filesystem/write_file` was rejected
with a 400 by a provider, forcing a sanitizer and a reverse map in the SDK. A
dotted name is the same bet on every downstream client's tokenizer and
name-validator. Claude Code presents these as `mcp__cambrian__ask_memory`; the
server is named `cambrian`, so the tool name carries no namespace of its own.

### D8 — `tools/list` is filtered per caller, and reflects only what is live

Two filters compose:

1. **Registration** — an unentitled or dependency-unmet plugin never published,
   so its tools are absent. Free, from D2.
2. **Authorization** — a `ListTools` receiving-middleware drops tools whose
   declared `Effects` the caller may not exercise, so a read-only principal is
   not shown `remember`. Reuses `Authorizer.Authorize`; adds no new policy
   concept.

Where serving depends on infrastructure rather than on code being compiled in,
advertise through `LiveCapabilities()` not the manifest — the REC-02 lesson:
the record lane once advertised itself on a deployment with no Postgres, and
"no lane here" became indistinguishable from "this kernel is broken."

### D9 — A stdio bridge, because it is ~100 lines and removes all adoption friction

All three target clients speak streamable HTTP with an `Authorization: Bearer`
header — Claude Code (`--transport http --header`), Codex (`url` +
`bearer_token_env_var` in `~/.codex/config.toml`), Goose (Remote Extension,
headers). So HTTP is sufficient. But stdio is still the lowest-friction default
for a local install, and a bridge costs almost nothing: a `cambrian mcp`
subcommand on the kernel binary (where ADR-0122 already put `setup`/`status`/
`stop`) that runs an SDK client against the HTTP endpoint and relays.

It must be a **dumb pipe**. No tool logic in the bridge — one implementation of
each tool, in-kernel, or the two transports drift.

### D10 — Task hand-off and conversations are *sequenced*, not excluded; two things are excluded forever

An earlier draft of this decision excluded the planner from the endpoint's scope
outright. The parity directive re-frames it: the 7 August review's warning —
*"the moment a pilot rule calls `start_plan` you are on the partial subsystems"*
— is an argument about **which half of the kernel is hardened**, and therefore
about *sequencing*, not about what external agents may ultimately do.

- **Phase 4 — task hand-off, on the hardened half.** `submit_task` +
  `get_task_status`: submission drops into a **reactive pipeline head**
  (ADR-0113/0114 — the one engine, per the one-registry rule), returns a task
  id, and the caller polls to a terminal state. This matches the owner model
  ("ingress is an entry-organ daemon at a pipeline head" — the MCP endpoint *is*
  one) and rides the shipped engine rather than the partial planner. **Shape it
  as the official 2026-07-28 Tasks extension** — a tool call answered with a
  task handle, driven by `tasks/get`/`tasks/update`/`tasks/cancel` — so clients
  that speak the extension get native task UX; keep `get_task_status` as a plain
  tool alias for clients that don't. The handle is a task id either way; one
  implementation, two spellings.
- **Phase 5 — conversations.** A `run_turn`-backed tool over the ADR-0084
  conversation lane, which chat-on-pipelines already exercises live. An external
  agent opens and continues a Cambrian conversation like any other chat ingress.
- **The planner lane itself** follows when it is hardened — a successor decision,
  not a re-litigation of this one.
- **Permanently excluded, as parity-ledger entries with reasons (Context §parity
  directive):** re-publishing foreign MCP tools (open proxy / confused deputy)
  and re-publishing the `tools/` subprocess registry (kernel ambient authority
  handed outward — ADR-0043's rejection stands).
- **No OAuth in phase 1.** The SDK ships `auth.RequireBearerToken` and
  `ProtectedResourceMetadataHandler` (RFC 9728) when a hosted deployment needs
  it; a static bearer is correct for on-premise and is what all three clients
  support today.

### D11 — Parity is mechanized: a ledger and shared definitions

A parity principle stated once in prose decays the first time an agent-plane
capability ships without a published twin. Two mechanisms:

1. **A parity ledger, CI-enforced.** A checked file
   (`docs/published-tool-parity.md`) maps every agent-plane capability to either
   its published counterpart or a written exclusion with a reason, enforced by a
   script beside `check-separability.{sh,ps1}`. A new agent capability with
   neither entry is a red build, not a review comment.
2. **Define once, render twice.** Where one seam backs both directions, the tool
   *declaration* (name, schema, effects) is shared data with direction flags;
   the agent registry and the Published Tool Surface both render from it. D1's
   registry separation survives untouched — it is the **definitions** that
   unify, never the registries. This kills schema drift between what an internal
   agent and an external agent see of the same capability.

### D12 — The endpoint teaches its caller, and may borrow the caller's model

Tools alone do not make an external agent use memory — the universally observed
failure mode of memory MCP servers is tools that sit uncalled. Three MCP-native
features close the gap:

- **Server `instructions`** (returned at initialize; supported by the SDK) ship
  in **phase 1** — they tell the calling agent *when* to `search_memory` vs
  `ask_memory`, when to `remember`, and what the `[n]` citation markers mean.
  ~Twenty lines, and the single highest-leverage line item for the "works
  almost natively" goal.
- **Prompts and resources** (phase 3): documents exposed as MCP resources with
  stable URIs so clients link and read them natively; prompt templates as
  explicit entry points (`/cambrian:recall`-shaped).
- **No sampling fallback — the spec deprecated it.** An earlier draft proposed
  `ask_memory` borrowing the caller's LLM via MCP sampling when the kernel has
  none configured. Spec 2026-07-28 deprecates sampling (Context §protocol);
  building a feature on a deprecated capability is a debt with a due date. When
  the kernel has no LLM, `ask_memory` degrades to an **extractive answer**
  (top passages + citations, no drafting) and says so in its result — still a
  zero-LLM-config demo, just an honest one.

### D13 — The contribution lane: a principal's local MCP servers, attached per-principal (owner sketch, 2026-08-14)

> **Elaborated into ADR-0127** (`0127-contribution-lane.md`, D1–D10, slices
> CL-0…CL-3, with its implementation handoff at
> `docs/handoffs/HANDOFF-contribution-lane.md`). This D13 remains the decision
> seed; ADR-0127 is the implementable form and governs on any divergence.

Parity has two directions. D1–D12 let an external agent **consume** every
capability Cambrian's agents hold. The reverse — an external party
**contributing** capabilities to the agents attending their task — is designed
here and built as phase 6.

The consumer-side app (the operator UI, or the D9 `cambrian mcp` bridge grown
two-way) dials MCP servers on the consumer's own machine, connects **outbound**
to the kernel, and offers those servers' tools upstream over that held
connection. The kernel never dials into a consumer machine — laptops are behind
NAT and sleep; the broker's outbound connection is the only transport. An agent
attending that consumer's task sees a merged menu: Cambrian's tools plus the
consumer's contributed tools. At call time the registry dispatches as ever —
kernel-backed tools execute in-kernel; contributed tools are relayed down the
broker's live connection and execute on the consumer's machine, under the
consumer's own OS authority, with the result and receipt attributed to that
principal. Spec 2026-07-28 confirms **pull is the only lawful direction**:
server-initiated requests may occur only while actively processing a client
request, so the broker holds a long-poll (`poll_step`) rather than the kernel
ever pushing — which the stateless core also makes the idiomatic shape.

Rules that make this safe, each load-bearing:

1. **Per-principal attachment, never global.** Contributed tools do *not* go
   through `AddMCPServer` — that is operator-level, kernel-global
   infrastructure, and would show consumer A's filesystem to agents attending
   consumer B. The registry gains session-scoped attachment: A's tools exist
   only in the tool menus of agents attending A's tasks. Structural, not a
   filter.
2. **Authority owner == task beneficiary.** This is the test the excluded open
   proxy failed and this lane passes: Cambrian never lends its own credentials
   outward (Context §parity exclusions), and never borrows a consumer's
   authority for anyone but that consumer.
3. **Consent on effects.** The broker may require per-call user approval for
   effectful contributed tools — a guard against an agent steered by hostile
   corpus content misusing the consumer's machine. The machine's owner gets the
   last word on effects executed on it.
4. **Live-only advertisement.** Contributed tools are advertised only while the
   broker connection is live (the REC-02 / `LiveCapabilities` lesson); a step
   targeting a disconnected contribution parks and notifies rather than failing
   the plan.
5. **One vocabulary.** Contributed manifests are derived from the local servers'
   `tools/list` and enter the ROUTE-03 capability vocabulary, so routing and
   the auction can target them like any internal agent's capabilities.

**The Telegram test: surface ≠ machine (owner question, 2026-08-14).** The
Claude Code case collapses three roles into one connection — the conversation
surface, the principal, and the execution locus — and an earlier draft of this
decision inherited that collapse by scoping attachment to the *session*. A
Telegram conversation separates the roles: the person speaks through the
Telegram ingress, which is no machine at all, while owning several machines in
varying states of online. Therefore:

- **Contribution is principal-scoped, not conversation-scoped.** A machine's
  broker registers as a named worker (`machine:<name>`) *owned by* a party, on
  its own long-lived outbound connection independent of any conversation. The
  person's machines are a stable fleet, not a session artifact. Rule 1 above is
  restated accordingly: A's tools appear in menus of agents attending A's
  tasks — from *any* surface A speaks through.
- **Identity linking is the prerequisite.** The Telegram sender and each machine
  broker must resolve to the same party — bound at token issuance
  (`cambrian mcp token create --owner <party>`) through `IdentityResolver`,
  whose charter is already "who is this external sender?".
- **Per-machine namespacing.** Menus list `local:<machine>/<tool>`, so targeting
  is explicit and two machines' filesystems cannot shadow one another.
- **Machine selection is a ladder, never a guess:** (1) explicitly named in the
  request; (2) sole capable online machine; (3) the principal's configured
  default machine; (4) otherwise **ask through the conversation surface** — the
  ADR-0098 progress channel makes a "laptop or desktop?" button prompt native
  on Telegram. Effectful steps never fall through to a guess. An offline target
  parks the step (rule 4) and notifies the surface.
- **Consent follows the principal, not the machine.** The person on Telegram is
  not at the laptop, so approval prompts for effectful steps route to the
  initiating conversation, with a per-machine policy knob:
  auto / approve-from-any-surface / approve-on-machine-only.
- **Artifacts stay the default pressure valve.** Most conversational asks
  resolve as D10 artifacts returned into the chat; machine targeting fires only
  when a task genuinely needs local effects.

---

## Consequences

**Enables the week-6 exit test end to end.** Ask (`ask_memory`) → cited answer
(citations, `[n]` markers) → denial (scoped principal via D4) → why
(`explain_access`) → verify (`get_receipt`, reachable because of D6).

**A new public contract.** Tool names and input schemas become as load-bearing as
the proto surface, consumed by clients Cambrian does not ship. They need their own
stability statement — the operator contract number describes the gRPC plane and
must not be overloaded to describe this one. Mechanize it from day one: a golden
file snapshotting `tools/list` output (names, schemas, annotations), diffed in
CI — the tool plane's analogue of the contract number. Retrofitting pagination or
renaming a field later *is* a contract break; this is also why `get_document` and
`list_documents` take size/offset parameters from the first release (coding-agent
clients truncate tool results at roughly 25k tokens).

**Unmetered spend without limits.** `ask_memory` runs an LLM under a 90 s budget
and coding agents retry aggressively; REACT-02 gave the watch lane storm control
and this surface must not ship with none. Phase 1 carries at minimum a per-token
concurrency cap; per-principal rate limits follow with the identity work.

**A new externally-reachable attack surface**, which is the point and also the
risk. Mitigations are D3 (bind-time TLS discipline), D4 (mandatory auth,
principal established before any handler), D8 (effect-filtered listing), and the
SDK's built-in DNS-rebinding protection, which must be left on.

**Distribution without adoption.** A Claude Code, Codex or Goose user reaches
Cambrian without adopting its protobuf, planner, or UI — which is the whole
argument for ranking this first.

---

## Build order

Each phase lands wired or does not land (AGENTS.md §4.2).

**Phase 1 — the surface and the endpoint (OSS, read-only).**
`domain/published_tool.go` · `Registry.PublishTool` + fold in `applyPlugins` ·
`internal/infrastructure/mcpserve/` · the four read-only core tools
(`search_memory`, `ask_memory`, `get_document`, `list_documents`; `remember` is
deferred per D5) · server `instructions` (D12) · per-token concurrency cap ·
the `tools/list` golden snapshot test · the parity ledger with its two founding
exclusions · `cambrian mcp token create <client>` with the credential in the
ADR-0101 secret store · listener through `transportCredentials` · `cambrian
mcp` stdio bridge.
*Exit:* `claude mcp add --transport http cambrian …` lists the tools and
`ask_memory` returns a cited answer on a clean machine.

**Phase 2 — scope, explanation, receipts (premium).**
The five premium tools · `SurfaceMCP` bound through `IdentityResolver` so each
registered client is its own principal (D4) · D6's correlation id.
*Exit:* the week-6 sentence, twice in a row, no config file touched — and two
clients with different tokens receive *different* denials.

**Phase 3 — operations.**
An operator console for MCP *clients* (a surface distinct from `/mcp`, per the
naming hazard) · `tools/list` change notifications on plugin hot-attach · MCP
prompts + resources (D12) · OAuth for hosted deployments · a stability statement
for the tool contract.
*Cost:* an operator contract bump and propagation to every vendored proto copy.

**Phase 4 — task hand-off (D10).**
`submit_task` + `get_task_status` over a reactive pipeline head, with progress
notifications.
*Exit:* an external agent submits a task, polls it to a terminal state, and
retrieves the produced artifact with citations — without touching the planner.

**Phase 5 — conversations (D10).**
A `run_turn`-backed conversation tool on the ADR-0084 lane.
*Exit:* Claude Code holds a multi-turn conversation with a Cambrian session
entirely over MCP.

**Phase 6 — the contribution lane (D13).**
Two-way broker in `cambrian mcp` (and the UI) · session-scoped attachment in
the registry · relay dispatch with receipts · per-call consent for effects ·
parking on disconnect.
*Exit:* consumer A's attending agent lists both Cambrian's tools and A's local
filesystem tools; a web call executes in-kernel, a file op executes on A's
machine, both land in one receipt chain — and consumer B's agent can neither
see nor call any of A's tools.

---

## Open questions for the owner

None — all three original questions are resolved below.

## Resolved by the owner, 2026-08-14

- **`remember` is deferred; phase 1 ships read-only** (former question 1). The
  write tool lands no earlier than the per-client identity work, carrying the
  D5 hardening (provenance triple incl. party, idempotency) as its shipping
  contract.

- **Capability parity is the ruling principle**, amended into this ADR in place
  (Context §"The parity directive"). Any capability of Cambrian's own SDK
  agents is reachable externally; parity covers product surfaces only.
- **The `SystemTool` registry is excluded from parity** — both foreign MCP tools
  and filesystem/subprocess tools. Recorded as the parity ledger's two founding
  exclusions (D10, D11).
- **`SurfaceMCP` is a new surface** (former question 3), and — going further
  than the original question — **every external agent registered via MCP is a
  principal in its own right** (D4). This also resolves former question 2:
  per-client tokens are the requirement, issued by the `cambrian mcp token
  create` subcommand rather than waiting on a phase-3 console; a phase-1
  bootstrap may still use a single token, but the identity model is per-client
  from the start.
