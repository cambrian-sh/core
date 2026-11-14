Sub-repo: cambrian-core
Language: Go
Top-level context: docs/ARCHITECTURE.md + CONTEXT.md (index of knowledge cards) + cambrian-knowledge/adrs/
Authoritative ADR: cambrian-knowledge/adrs/0057-open-core-boundary-licensing-oss-release-model.md

# AGENTS.md — Guide for AI Coding Agents & Contributors

Conventions for working in this repository (read alongside `docs/ARCHITECTURE.md`; this repo has
no `CONTRIBUTING.md`). Tool-specific instruction files (`CLAUDE.md`, `GEMINI.md`) are local-only;
this is the shared, published guide.

## Knowledge substrate & tickets (MANDATORY — every task)

The full protocol is root [`../AGENTS.md` §0](../AGENTS.md) — read it. **Live status lives in HQ**
on a cut-over machine (`../cambrian-knowledge/.hq-cutover` exists — the owner's machine): use the
`hq` MCP tools (`ready`, `create_ticket`, `update_ticket`, `list_tickets`, `brief`, `record_finding`,
`verify_card`, `list_decisions` / `set_decision_status`, the benchmark tools) or
`C:/Users/afsin/Dev/hq/bin/hq.exe … --config ../hq.yaml`. There the old `bin/ticket` / `bin/bench`
scripts and the codebase-memory-mcp ticket tools **refuse writes**. Without the marker, the file
workflow of root §0.4/§0.5 applies. In short, for every change in this repo, however small:

1. **Before editing:** `ready` / `list_tickets` (`repo: "cambrian-core"`) → `create_ticket` if the work is
   not ticketed → `update_ticket` action `claim`. `brief <path>` (or the card that loads when you
   open a file) for the cards, ADRs, decisions and open tickets that govern the code you will
   touch; a drifted card or stale ADR is a claim to re-verify against code before relying on it.
2. **While working:** `log` findings, dead ends and `decision:` lines on the ticket. A bug or
   follow-up you will not fix now gets its own `create_ticket` (`triage=true`), immediately.
3. **Decisions:** architectural → `cambrian-knowledge/adrs/NNNN-slug.md` (proposed, per
   `cambrian-knowledge/meta/SCHEMA.md`; its lifecycle status is set in HQ, not the frontmatter);
   operating rule → `cambrian-knowledge/decisions/`; tradeoff → the ADR's `tradeoffs:`. **Never
   write ADRs to `docs/adr/`** — no sub-repo has that directory; ADRs live in `cambrian-knowledge/adrs/`.
4. **Before reporting done:** benchmark runs → a run record each (`record_benchmark_run`; the
   procedure is root `../AGENTS.md` §0.5); other numbers you produced → the ADR's `measurements:`;
   gotchas → `observations:`; re-verified ADRs → `last_verified` + `verified_at_rev`; cards you
   re-read against code → `verify_card` (the Stop hook blocks while a card your edit drifted is
   unverified); `CONTEXT.md` if architecture or gaps changed (never edit between the
   `<!-- hq:cards:start -->` / `<!-- hq:cards:end -->` markers — `hq context` generates that block).
   Then `close` the ticket with a resolution (files changed, ADRs updated, tests run and result) —
   or, if unfinished, `log` where you stopped and `release`.

Pass `by: "agent:<name>"` on ticket calls. Never hand-edit `cambrian-knowledge/tickets/`; tickets
never close from commit messages.

**Default execution lane (ADR-0150, owner decision 2026-11-13).** A request runs in the
**single-agent lane** by default: one agent holds the person's tools and answers itself, fanning
out read-only work through the kernel-run `run_subtasks` tool (`execution.lane.default:
single_agent`, `cambrian-core/internal/config/lane.go`). The ADR-0145 **multi-agent research lane** (planner →
Gatekeeper → Dispatcher → DAG, coverage, verifier) runs only under `execution.lane.default:
planner` and is kept for research — so plan views, dispatch events and Gatekeeper scoring appear
only in that lane. See [`decisions/default-lane-single-agent-with-fan-out.md`](../cambrian-knowledge/decisions/default-lane-single-agent-with-fan-out.md).

## Orientation
- Read **`docs/ARCHITECTURE.md`** first for the layering, the two execution lanes (single-agent
  default; multi-agent research lane behind config) and domain terms. Gatekeeper, Dispatcher and
  capability-typed dispatch belong to the multi-agent research lane only.
- Per-area facts are in the knowledge cards listed in `CONTEXT.md` (`brief <path>` in HQ).

## Cross-repo pointer
This file is a sub-repo guide. The monorepo-level map, entry points, and the open-core
boundary ADR live one level up.
- Monorepo context: [../CONTEXT.md](../CONTEXT.md)
- Monorepo agent guide: [../AGENTS.md](../AGENTS.md)
- Open-core boundary ADR: [../cambrian-knowledge/adrs/0057-open-core-boundary-licensing-oss-release-model.md](../cambrian-knowledge/adrs/0057-open-core-boundary-licensing-oss-release-model.md)

## Hard rules
- **Strict hexagonal separation** — keep ports/adapters boundaries clean.
- **Zero-Hardcode Rule** — policy, routing tables and thresholds are config or data, never a Go
  branch on an agent, tool or customer (root `AGENTS.md` §3.4). (Exception: system-shell and
  reflexive-path logic are deterministic for safety/latency.)
- **Open-core boundary** — core must never import premium code. The premium layer lives in a
  separate module and plugs in via `app.Options` (ADR-0057). CI enforces this with
  `make separability` (`scripts/check-separability.sh`, which greps for the premium module import
  path); `scripts/check-no-premium.sh` is an extra manual audit, not run by CI.
- **Licence** — this module is source-available under the Elastic License 2.0 (`LICENSE`,
  ADR-0057 D6), not OSI open source. Say "source-available" in outward-facing text.
- **Stable contracts (v0.x):** the gRPC/proto surface and the config schema are held stable;
  the Go package API is explicitly unstable.

## Build & verify
```sh
go build ./... && go vet ./... && go test ./...
make separability            # core/premium import boundary
make per-pr                  # proto-check proto-breaking vet-tagged test-race separability integration chaos
bash scripts/check-no-premium.sh  # premium-leak audit (script here; authority per ADR-0057 D13 is in cambrian-premium/)
```
For cross-module (premium) work, use the workspace `go.work` one level up (core + premium).

## Change discipline
- Match surrounding code style; add tests; keep PRs focused.
- After architectural changes, update `docs/ARCHITECTURE.md` and the relevant ADR.
- Never commit secrets — use `.env` / `CAMBRIAN_*`; the real `configs/config.json` is gitignored.
- **Keep the sub-repo's `CONTEXT.md` in sync.** When you add, remove, or change code, architecture, status, modules, domain terms, or known gaps, update the sub-repo's `CONTEXT.md` to reflect the change. The context is the source of truth for AI agents (and humans) navigating the sub-repo; a stale context is worse than no context. Update the relevant section: `Module Breakdown` (paths), `Implementation Status` (areas with ADR + status), `Terminology Glossary` (new terms), `Known Gaps` (new deferred work), `Core Philosophy` (principle changes).
- **Data-driven development.** Cambrian's product is developed through the benchmark harness in `../cambrian-benchmarks/`. When you add, remove, or change a feature, you MUST measure the impact via the benchmarks that exercise the affected part. If no existing benchmark covers the affected part, you MUST propose a new benchmark. See `CONTEXT.md` (this sub-repo's manual) for the DDD workflow detail.
