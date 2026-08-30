# The Published-Tool Parity Ledger (ADR-0126 D11)

**The ruling principle (owner directive, 2026-08-14):** any capability
Cambrian's own SDK agents hold must be reachable by external agents over the
Published Tool Surface. This ledger is that principle made checkable: every
agent-plane capability maps to its published counterpart, or to a written
exclusion with a reason. `scripts/check-parity.sh` (and `.ps1`) enforce the
mechanical half — a published tool that is not in this ledger, or a named
exclusion that goes missing, fails the build.

Parity covers Cambrian's **product surfaces** — memory, knowledge, policy,
receipts, tasks, conversations. It does not cover the kernel's own authority
(the exclusions below).

## Capability map

| Agent-plane capability | Published counterpart | Status |
| --- | --- | --- |
| Memory search (scoped recall) | `search_memory` | **live** (phase 1) |
| Grounded answer (ADR-0081 lane) | `ask_memory` | **live** (phase 1); **caller-scoped since E6** — it now backs onto `QueryService.Answer(ctx, query, callerID)`, the scoped counterpart of the system-scoped `AnswerSystem`, so a premium PDP narrows the answer lane exactly as it narrows search |
| Document read by id (principal-scoped) | `get_document` | **live** (phase 1) |
| Document enumeration + labels | `list_documents` | **live** (phase 1) |
| Memory write | `remember` | **deferred by owner ruling (2026-08-14)** — phase 1 is read-only; ships no earlier than per-client identity work, carrying the ADR-0126 D5 hardening (provenance triple + idempotency) as its contract |
| Typed knowledge query (ADR-0111/0118) | `query_knowledge` | **live** (phase 2, premium `substrateplugin`) — the ADR-0118 D1 scoped seam, run as the calling principal |
| NL knowledge ask | `ask_knowledge` | **live** (phase 2, premium `substrateplugin`) — the same `astFormer` the operator lane uses: LLM-drafted AST, name-verified against the real catalog, executed scoped |
| Policy check (deterministic) | `check_policy` | **live** (phase 2, premium `substrateplugin`) — `policycheck.Check` against the current bounds; no LLM |
| Access explanation | `explain_access` | **live** (phase 2, premium `authzplugin`) — `ExplainAccess` asked about the CALLER only; the "sees why" half of the week-6 test |
| Receipt fetch / chain verify | `get_receipt` | **live** (phase 2, premium `receiptsplugin`) — reads by the D6 correlation PREFIX so a multi-hop call returns as one chain, and reports integrity/signatures/completeness separately |
| Task hand-off | `submit_task` | **live (phase 4)** — enters the kernel's EXISTING session + Execute lane exactly as an operator-submitted task (no second engine, no parallel queue); the beneficiary owner principal is derived from the AUTHENTICATED caller (machine → its owner; everyone else → itself, never from arguments) and recorded in the task index, which is what seeds `WithTaskBeneficiary` for menu-build (ADR-0127 D4). Effects write+spend. The official Tasks-extension spelling of the handle is deferred to a later slice; the ADR-0126 phase-4 implementation record carries the deviation from the earlier pipeline-head sketch (owner course-correction 2026-08-21) |
| Task status polling | `get_task_status` | **live (phase 4)** — status polling ONLY (streaming/cancel/conversations are phase 5). A caller sees its OWN tasks (a machine principal, its owner's); unknown and foreign ids answer byte-identically not-found, so task ids cannot be enumerated. Task visibility and status are memory-resident and do not survive a kernel restart — the same lifetime as the execution itself |
| Conversations (ADR-0084 lane) | `run_turn`-backed tool | phase 5 (ADR-0126 D10) |
| Capability contribution (local machines) | contribution lane | ADR-0127 — **CL-0 implemented 2026-08-20; CL-1 kernel half implemented 2026-08-21** (worker identity + owner-scoped fleet + `local:` attachment + dispatch scoping + worker hub + relay + D8 fencing). The broker (separate premium binary, owner ruling) and the E2E gate remain. Contributed tools themselves are NEVER published outward (exclusion 3) |
| Worker step transport (pull-only) | `poll_step` | **live (CL-1 kernel half)** — `machine:*` principals ONLY: hidden from every other caller's `tools/list`, refused at the call side, and re-checked in the handler. Long-poll; holding it open is the liveness signal; the manifest rides every poll (declarative registration). NOT an agent capability and NOT an ingest lane — it moves steps, nothing it carries reaches the corpus |
| Worker step results (pull-only) | `report_step` | **live (CL-1 kernel half)** — `machine:*` principals ONLY, same three locks. Idempotent on the step id; every payload (results AND worker-reported errors) is ADR-0063 nonce-fenced before any agent context (D8, no exceptions) |

## Exclusions — permanent, with reasons

These are not agent capabilities; they are **the kernel's own authority that
agents borrow under grants**, and they never appear on the Published Tool
Surface:

1. **Foreign MCP tools** (`mcp:<server>/<tool>`, dialled by the ADR-0043
   connector). Republishing them outward makes Cambrian an open proxy — its
   credentials on those servers exercised on behalf of arbitrary token
   holders, a confused deputy. External agents can dial those servers
   themselves; they lack no capability, only a credential they should not
   borrow.
2. **Filesystem-discovered subprocess tools** (the `tools/` registry).
   Publishing them hands external callers execution with the kernel's ambient
   authority — the exact thing ADR-0043's Option C rejection was protecting.
3. **Contributed worker tools** (`local:<machine>/<tool>`, the ADR-0127
   contribution lane). They face INWARD only: a consumer's machine supplies
   them for exactly that consumer's tasks (authority owner == task
   beneficiary, structurally), so republishing them outward would exercise one
   consumer's machine authority on behalf of arbitrary token holders — the
   same confused deputy as exclusion 1, with someone else's laptop as the
   victim. ADR-0127 D10 marks this permanent.

## Maintenance

- Adding a published tool? Add its row here in the same commit slice, or the
  parity check fails.
- Adding an agent-plane capability? Its row lands here too — with a published
  counterpart, a phase, or a written exclusion. "We forgot" is the failure
  mode this file exists to prevent.
- The tool contract itself is frozen by
  `internal/infrastructure/mcpserve/testdata/tools_list.golden.json`; this
  ledger tracks coverage, the golden tracks shape.
- The golden freezes the FOUR CORE tools only — it builds its surface from
  `CoreTools` alone, so premium additions cannot break it and must not be added
  to it. The task pair (`submit_task`/`get_task_status`, in
  `internal/infrastructure/mcpserve/tasktools.go`) and the worker transport
  (`poll_step`/`report_step`) compose BESIDE CoreTools at the root and are
  frozen by their own declaration tests, not by the golden. The five premium declarations are frozen by their own tests inside
  `cambrian-premium` (`plugins/{substrateplugin,authzplugin,receiptsplugin}/
  publishedtools_test.go`), because a check that lives in OSS cannot see them
  and one that lives in the golden would make an OSS build depend on a premium
  module.
- `scripts/check-parity.sh` checks the core half mechanically. The premium half
  is checked by the same rule applied by hand at review: a plugin that calls
  `Registry.PublishTool` and has no row here is the drift this file exists to
  prevent.
