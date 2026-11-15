Sub-repo: cambrian-core
Language: Go
Top-level context: docs/ARCHITECTURE.md + knowledge cards in HQ (`brief <path>`) + ADRs in HQ (`adrs/`)
Authoritative ADR: HQ document `adrs/0057-open-core-boundary-licensing-oss-release-model.md`

# AGENTS.md — Guide for AI Coding Agents & Contributors

Conventions for working in this repository (read alongside `docs/ARCHITECTURE.md`; this repo has
no `CONTRIBUTING.md`). Tool-specific instruction files (`CLAUDE.md`, `GEMINI.md`) are local-only;
this is the shared, published guide.

## Knowledge & tickets in HQ (MANDATORY — every task)

The full protocol is root [`../AGENTS.md` §0](../AGENTS.md) — read it. **All project knowledge and
all live status live in HQ**: ADRs, decisions, investigations, research and knowledge cards are
versioned HQ documents (the `cambrian-knowledge/` repo is read-only until it is archived — never edit
it), and tickets, ADR status, card freshness and benchmark runs are HQ records. Use the `hq` MCP
tools (`ready`, `create_ticket`, `update_ticket`, `list_tickets`, `brief`, `read_doc`, `search_docs`,
`write_doc`, `record_finding`, `verify_card`, `list_decisions` / `set_decision_status`, the benchmark
tools) or `C:/Users/afsin/Dev/hq/bin/hq.exe … --config ../hq.yaml`. Every call carries your HQ token
(MCP header; the CLI and hooks read `~/.hq/token-cambrian`) — never print it. In short, for every
change in this repo, however small:

1. **Before editing:** `ready` / `list_tickets` (`repo: "cambrian-core"`) → `create_ticket` if the work is
   not ticketed → `update_ticket` action `claim`. `brief <path>` (or the card that loads when you
   open a file) for the cards, ADRs, decisions and open tickets that govern the code you will
   touch; a drifted card or stale ADR is a claim to re-verify against code before relying on it.
2. **While working:** `log` findings, dead ends and `decision:` lines on the ticket. A bug or
   follow-up you will not fix now gets its own `create_ticket` (`triage=true`), immediately.
3. **Decisions:** architectural → document `adrs/NNNN-slug.md` via `write_doc` (`status: proposed`,
   template `meta/SCHEMA.md`; its lifecycle status is set in HQ, not the frontmatter); operating rule
   → document `decisions/<slug>.md`; tradeoff → the ADR's `tradeoffs:`. ADR and decision writes are
   **drafts** only the owner approves — never approve one yourself. **Never write ADRs to
   `docs/adr/`** — no sub-repo has that directory.
4. **Before reporting done:** benchmark runs → a run record each (`record_benchmark_run`; the
   procedure is root `../AGENTS.md` §0.5); other numbers you produced → the ADR's `measurements:`;
   gotchas → `observations:` (ADR drafts) or `record_finding` on the card; re-verified ADRs →
   `last_verified` + `verified_at_rev`; cards you re-read against code → `verify_card` (the Stop hook
   blocks while a card your edit drifted is unverified); architecture, behaviour or gaps changed →
   the area's card via `read_doc` → `write_doc` (with `base_rev` and a reason; never an empty or
   truncated text). `CONTEXT.md` here is a short stub — never add facts to it. Then `close` the
   ticket with a resolution (files changed, documents written with their revisions, drafts left for
   the owner, tests run and result) — or, if unfinished, `log` where you stopped and `release`.

Pass `by: "agent:<name>"` on HQ calls (you are recorded as `agent:<name>@<person>`). Tickets never
close from commit messages.

**Default execution lane (ADR-0150, owner decision 2026-11-13).** A request runs in the
**single-agent lane** by default: one agent holds the person's tools and answers itself, fanning
out read-only work through the kernel-run `run_subtasks` tool (`execution.lane.default:
single_agent`, `cambrian-core/internal/config/lane.go`). The ADR-0145 **multi-agent research lane** (planner →
Gatekeeper → Dispatcher → DAG, coverage, verifier) runs only under `execution.lane.default:
planner` and is kept for research — so plan views, dispatch events and Gatekeeper scoring appear
only in that lane. See document `decisions/default-lane-single-agent-with-fan-out.md`.

## Orientation
- Read **`docs/ARCHITECTURE.md`** first for the layering, the two execution lanes (single-agent
  default; multi-agent research lane behind config) and domain terms. Gatekeeper, Dispatcher and
  capability-typed dispatch belong to the multi-agent research lane only.
- Per-area facts are in the knowledge cards in HQ: `brief <path>` for any file or folder (a card also loads when you open a matching file); `CONTEXT.md` is only a stub.

## Cross-repo pointer
This file is a sub-repo guide. The monorepo-level map, entry points, and the open-core
boundary ADR live one level up.
- Monorepo context: [../CONTEXT.md](../CONTEXT.md)
- Monorepo agent guide: [../AGENTS.md](../AGENTS.md)
- Open-core boundary ADR: document `adrs/0057-open-core-boundary-licensing-oss-release-model.md`

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
- **Keep the knowledge cards true.** When you add, remove, or change code, architecture, behaviour, modules, domain terms, or known gaps, update the card(s) for that area in HQ (`brief <path>` names them; `read_doc` → edit → `write_doc` with a reason and `base_rev`, then `verify_card`). A new area gets a new card (`read_doc cards/README.md` for the format); new terms go into `card.concept.glossary`; known gaps are tickets, not prose. The cards are the source of truth for agents and humans navigating the repo; a stale card is worse than none. `CONTEXT.md` is a short stub and holds no facts.
- **Data-driven development.** Cambrian's product is developed through the benchmark harness in `../cambrian-benchmarks/`. When you add, remove, or change a feature, you MUST measure the impact via the benchmarks that exercise the affected part. If no existing benchmark covers the affected part, you MUST propose a new benchmark. The procedure is root [`../AGENTS.md` §0.5](../AGENTS.md); the harness side is [`../cambrian-benchmarks/AGENTS.md`](../cambrian-benchmarks/AGENTS.md) and `brief card.subsystem.benchmarks-eval`.
