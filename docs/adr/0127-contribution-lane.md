---
id: 0127
title: The Contribution Lane — principal-scoped local capability workers
status: Proposed
date: 2026-08-14
supersedes: []
superseded_by: []
depends_on:
  - 0043-mcp-tool-provider
  - 0063-reactive-llm-condition-injection-hardening
  - 0067-capability-vocabulary-canonicalization
  - 0090-ingress-and-surface-identity
  - 0098-conversation-progress-channel
  - 0101-config-and-secret-store
  - 0103-evidence-receipts
  - 0113-reactive-pipeline-graphs
  - 0114-pipeline-runtime-semantics
  - 0121-row-level-entitlement
  - 0126-published-tool-surface-and-mcp-endpoint
---

# ADR-0127: The Contribution Lane — principal-scoped local capability workers

## Status

Proposed — design only, no code written. Elaborates ADR-0126 D13 (which remains
the decision seed; this ADR is its implementable form). **Builds after ADR-0126
phases 1–2 and the phase-4 task lane** — the broker's upstream connection *is*
the ADR-0126 endpoint, and parked steps need the task lane to park in.

**Date:** 2026-08-14

---

## Context

ADR-0126 lets external agents **consume** every capability Cambrian's agents
hold (the parity directive). This ADR is the reverse half: an external party
**contributes** capabilities — MCP servers running on their own machines —
to the Cambrian agents attending *their* tasks.

The motivating scenario (owner, 2026-08-14): a consumer talks to Cambrian from
Telegram and asks for work that needs a file operation on their laptop. Cambrian
cannot reach the laptop, must not hold credentials to it, and must still let the
attending agent use the laptop's capabilities — with consent, attribution, and
receipts.

Three facts shape everything:

1. **MCP forbids server→client tool calls.** Spec 2026-07-28 permits
   server-initiated requests only while actively processing a client request,
   and deprecated sampling/roots. The kernel can never push work to a machine;
   machines **pull**.
2. **Laptops are unreachable and intermittent.** NAT, firewalls, sleep. The
   only viable transport is a connection the consumer's machine holds
   *outbound* to the kernel.
3. **The conversation surface, the principal, and the execution locus are
   three different things.** Claude Code collapses them into one connection;
   Telegram separates them. Any design keyed on "the session" is wrong the
   day a second surface appears.

### The safety inversion that makes this lawful

ADR-0126's parity directive permanently excludes republishing Cambrian's own
dialled tools (open proxy — Cambrian's credentials exercised for strangers).
The contribution lane is the inverse and passes the test the proxy failed:

> **A foreign capability may be attached to exactly the principal that
> supplied it.** Authority owner == task beneficiary, per call, structurally.

Cambrian never lends its keys out, and never borrows a consumer's keys for
anyone but that consumer.

### Direction of consumption — read this twice

Contributed tools are consumed by **Cambrian's internal agents** (the agent
attending the consumer's task), not by external callers. They therefore enter
the **inward-facing** tool plane — the `domain.SystemTool` side that agents'
tool menus resolve from — as a new *scoped* source, never the outward Published
Tool Surface. ADR-0126 D1's "the registries face opposite directions" is
preserved: the Published Tool Surface stays outward-only; the contribution lane
extends the inward registry with per-principal entries. Today that registry is
fed by exactly two sources (`app/app.go:1410-1464`: filesystem discovery and
the ADR-0043 MCP connector); this ADR adds the third, and it is the first
source whose entries are **not global**.

---

## Decisions

### D1 — Machines are named workers owned by a party

A consumer's machine registers as `machine:<name>` (e.g. `machine:afsin-laptop`),
**owned by** a party. Ownership is bound at token issuance:

```
cambrian mcp token create <machine-name> --owner <party>
```

(the ADR-0126 issuance subcommand, grown one flag). The token resolves through
`domain.IdentityResolver` (`domain/identity_binding.go:124`) to a principal
whose owner is the party; `StrangerPolicyFor` governs unbound tokens. The
consumer's machines form a stable **fleet** keyed by party — not a session
artifact. Any conversation surface (Telegram, UI, MCP, chat console) that
resolves to the same party sees the same fleet.

### D2 — The broker is the D9 bridge grown two-way

`cambrian mcp` (the ADR-0126 stdio bridge, in `cmd/orchestrator/main.go`'s
subcommand switch) gains the contributor role:

- **Downstream (existing):** relays the kernel's MCP endpoint to a local MCP
  client over stdio.
- **Upstream (new):** dials local MCP servers as an SDK *client* (the
  `internal/infrastructure/mcp/connector.go` machinery is the in-tree
  exemplar), derives a manifest from their `tools/list`, and offers it to the
  kernel over its own outbound connection to the ADR-0126 endpoint.

The broker is a **dumb pipe** both ways: no tool logic, no schema rewriting
beyond namespacing (D4), no caching of results.

### D3 — Pull is the only transport: `poll_step` / `report_step`

The kernel never pushes. Two published tools on the ADR-0126 surface, callable
only by `machine:*` principals:

- `poll_step` — long-poll; returns the next step targeted at this worker (or
  times out empty). Holding the poll open *is* the liveness signal.
- `report_step` — returns a step result (or a refusal/consent-denied), which
  flows into the receipt chain attributed to the worker's principal.

Between polls the worker is *live* for `liveness_window` (config; default a
small multiple of the poll timeout). No open poll ⇒ not live ⇒ D6 parking.

### D4 — Attachment is a per-party scoped registry source, namespaced per machine

A new inward registry source resolves, **per task**, the beneficiary party's
live fleet into tool entries named:

```
local:<machine>/<tool>        e.g.  local:afsin-laptop/read_file
```

Rules, each load-bearing:

- **Never through `Registry.AddMCPServer`** — that is operator-level,
  kernel-global config; it would show party A's filesystem to party B's agents.
- Resolution happens at **menu-build time** from (task → beneficiary party →
  live fleet), so scoping is structural — there is no global list to filter.
- Two machines' same-named tools cannot shadow each other or kernel tools; the
  `local:` prefix and machine segment make every name unique.
- Contributed tool *descriptions* are prefixed with a locality note ("runs on
  the requester's machine <name>") so agents can weigh cost and effect domain.

### D5 — Dispatch is a relay with receipts

When an attending agent calls `local:<machine>/<tool>`, the executor enqueues a
step for that worker; the worker's `poll_step` collects it, executes against
the local MCP server under the consumer's own OS authority, and `report_step`
returns the result. The step, the consent outcome (D7), and the result land in
the decision journal / receipt chain (ADR-0103) attributed to the worker
principal and correlated with the task (ADR-0126 D6 prefix scheme).

### D6 — Liveness, the selection ladder, and parking

- **Live-only advertisement** (the REC-02 lesson): a machine appears in menus
  only while live (D3). "No fleet here" must stay distinguishable from "this
  kernel is broken."
- **Machine selection is a ladder, never a guess:**
  1. explicitly named in the request;
  2. sole capable live machine of the party;
  3. the party's configured default machine;
  4. otherwise **ask through the initiating conversation surface** — the
     ADR-0098 progress channel makes a button prompt native on Telegram.
  Effectful steps never fall through to a guess.
- **Parking:** a step targeting a non-live machine parks with a deadline and
  notifies the surface; it dispatches on reconnect or expires visibly. Parking
  rides the phase-4 task lane's state, not a new queue.

### D7 — Consent follows the principal, not the machine

Per-machine policy knob, stored with the worker registration:

- `auto` — no prompt (read-only tools default here);
- `any-surface` — approval prompt routed to the **initiating conversation**
  ("approve write on afsin-laptop? ✓/✗" in Telegram); the default for
  effectful tools;
- `on-machine-only` — the broker prompts locally; the strictest setting.

The consent decision is recorded in the receipt for the step.

### D8 — Contributed results are fenced before any LLM sees them

The reverse-injection guard: a hostile or compromised local server returns
*results* that flow up into a kernel agent holding kernel capabilities. Every
`report_step` payload is wrapped with the ADR-0063 (REACT-03) nonce-fenced
payload-as-data hardening before it reaches an agent's context. This is not
optional and not deferrable; it ships in the first slice that relays a result.

### D9 — Manifests enter the ROUTE-03 capability vocabulary

The broker-derived manifest normalizes through `NormalizeCapability`
(ADR-0067) into the same capability vocabulary internal agents use, tagged
with the owner party. Routing/auction may target a step at a worker **only
when the task's beneficiary is the worker's owner** (the D1 invariant applied
at selection time — defense in depth on top of D4's structural scoping).

### D10 — Out of scope, permanently or for now

- **Permanently:** re-publishing contributed tools outward (they are inward
  only, per Context); Cambrian storing or proxying consumer machine
  credentials of any kind.
- **For now:** worker-initiated tasks; multi-party fleets (a machine owned by
  a team); contributed *resources/prompts* (tools only in v1); Windows
  service/daemonized broker (a foreground process is v1).

---

## Consequences

- Cambrian agents can act on a consumer's machine with per-step consent, full
  receipts, and zero standing credentials — from any conversation surface.
- The inward tool registry gains its first non-global source; menu building
  becomes principal-dependent. Tests that assume one global menu need the
  task-scoped resolution seam.
- A new class of principal (`machine:*`) appears in policy, receipts and the
  journal; policy authors can target it via the ADR-0126 three-tier model.
- The broker becomes a consumer-visible program with a lifecycle of its own;
  its UX (first-run, reconnect, consent prompts) is product surface, not
  plumbing.

---

## Build order (slices; each lands wired or does not land)

| Slice | Contents | Gate |
|---|---|---|
| CL-0 | Contracts: worker registration (`--owner`), fleet model, `local:` naming, attachment-source seam in the inward registry (menu-build resolution), THE parity-ledger entry | A party's fleet resolves into a task menu in tests; party B's task provably cannot list party A's tools |
| CL-1 | Broker upstream half + `poll_step`/`report_step` + relay dispatch + D8 fencing, against `cmd/reference-fs-mcp` as the local server, single machine, `auto` consent | E2E on one machine: agent calls `local:<m>/read_file`, result relayed, fenced, receipted; kill the broker mid-step → step parks, reconnect → completes |
| CL-2 | Selection ladder + parking deadlines + consent knob incl. `any-surface` prompts through the conversation lane | Telegram (or chat-console stand-in) receives and answers a "which machine / approve?" prompt; effectful step without consent is refused with a receipt |
| CL-3 | ROUTE-03 manifest normalization + auction targeting restricted to owner fleet + default-machine preference | Routing selects the sole capable live machine without a prompt; a crafted cross-party step is refused at selection AND at attachment (both layers hold) |

---

## Open questions for the owner

1. Broker distribution: inside the kernel binary (one artifact, ADR-0122
   precedent) vs a separate small binary (no kernel bits on consumer
   machines). One-artifact is proposed.
2. Default consent for read-only contributed tools: `auto` (proposed) or
   `any-surface` for the first release.
3. Fleet visibility in the operator UI (phase-3 console work) — which release
   carries it.
