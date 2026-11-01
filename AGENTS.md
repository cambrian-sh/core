Sub-repo: cambrian-core
Language: Go
Top-level context: docs/ARCHITECTURE.md + docs/adr/0001..0058
Authoritative ADR: 0057-open-core-boundary.md

# AGENTS.md — Guide for AI Coding Agents & Contributors

Conventions for working in this repository (read alongside `CONTRIBUTING.md` and
`docs/ARCHITECTURE.md`). Tool-specific instruction files (`CLAUDE.md`, `GEMINI.md`) are
local-only; this is the shared, published guide.

## Knowledge substrate & tickets (MANDATORY — every task)

The full protocol is root [`../AGENTS.md` §0](../AGENTS.md) — read it. In short, for every
change in this repo, however small:

1. **Before editing:** `list_tickets` (`repo: "cambrian-core"`) → `create_ticket` if the work is not
   ticketed → `update_ticket` action `claim`. Query the graph (`search_graph`, `query_graph`)
   for the ADRs and decisions that govern the code you will touch; if one is stale, read why
   (knowledge-board → Freshness) and verify it before relying on it.
2. **While working:** `log` findings, dead ends and `decision:` lines on the ticket. A bug or
   follow-up you will not fix now gets its own `create_ticket`, immediately.
3. **Decisions:** architectural → `cambrian-knowledge/adrs/NNNN-slug.md` (`status: proposed`,
   per `cambrian-knowledge/meta/SCHEMA.md`); operating rule → `cambrian-knowledge/decisions/`;
   tradeoff → the ADR's `tradeoffs:`. **Never write ADRs to `docs/adr/`** — it holds redirect
   stubs only.
4. **Before reporting done:** benchmark runs → a run record each (`record_benchmark_run`; the
   procedure is root `../AGENTS.md` §0.5); other numbers you produced → the ADR's `measurements:`;
   gotchas → `observations:`; re-verified ADRs →
   `last_verified` + `verified_at_rev`; `CONTEXT.md` if architecture or gaps changed. Then
   `close` the ticket with a resolution (files changed, ADRs updated, tests run and result) —
   or, if unfinished, `log` where you stopped and `release`.

Pass `by: "agent:<name>"` on ticket calls. No MCP ticket tools? Use
`python ../cambrian-knowledge/bin/ticket … --json` — same store. Never hand-edit
`cambrian-knowledge/tickets/`; tickets never close from commit messages.

## Orientation
- Read **`docs/ARCHITECTURE.md`** first for the layering and domain terms (Substrate,
  Gatekeeper, Dispatcher, capability-typed dispatch, the Zero-Hardcode Rule).

## Cross-repo pointer
This file is a sub-repo guide. The monorepo-level map, entry points, and the open-core
boundary ADR live one level up.
- Monorepo context: [../../CONTEXT.md](../../CONTEXT.md)
- Monorepo agent guide: [../../AGENTS.md](../../AGENTS.md)
- Open-core boundary ADR: [../../0057-open-core-boundary.md](../../0057-open-core-boundary.md)

## Hard rules
- **Strict hexagonal separation** — keep ports/adapters boundaries clean.
- **Zero-Hardcode Rule** — agent-to-task routing must live in the Awareness (LLM) layer,
  never as Go `if/else`/`switch`. (Exception: system-shell and reflexive-path logic are
  deterministic for safety/latency.)
- **Open-core boundary** — the OSS module must never import premium code. The premium layer
  lives in a separate module and plugs in via `app.Options` (ADR-0057). CI enforces this
  (`scripts/check-no-premium.sh`).
- **Stable contracts (v0.x):** the gRPC/proto surface and the config schema are held stable;
  the Go package API is explicitly unstable.

## Build & verify
```sh
go build ./... && go vet ./... && go test ./...
make separability            # OSS/premium import boundary
bash scripts/check-no-premium.sh  # premium-leak audit (script here; authority per ADR-0057 D13 is in cambrian-premium/)
```
For cross-module (premium) work, use a Go workspace (`go.work`) — see `CONTRIBUTING.md`.

## Change discipline
- Match surrounding code style; add tests; keep PRs focused.
- After architectural changes, update `docs/ARCHITECTURE.md` and the relevant ADR.
- Never commit secrets — use `.env` / `CAMBRIAN_*`; the real `configs/config.json` is gitignored.
- **Keep the sub-repo's `CONTEXT.md` in sync.** When you add, remove, or change code, architecture, status, modules, domain terms, or known gaps, update the sub-repo's `CONTEXT.md` to reflect the change. The context is the source of truth for AI agents (and humans) navigating the sub-repo; a stale context is worse than no context. Update the relevant section: `Module Breakdown` (paths), `Implementation Status` (areas with ADR + status), `Terminology Glossary` (new terms), `Known Gaps` (new deferred work), `Core Philosophy` (principle changes).
- **Data-driven development.** Cambrian's product is developed through the benchmark harness in `../cambrian-benchmarks/`. When you add, remove, or change a feature, you MUST measure the impact via the benchmarks that exercise the affected part. If no existing benchmark covers the affected part, you MUST propose a new benchmark. See `CONTEXT.md` (this sub-repo's manual) for the DDD workflow detail.
