# AGENTS.md — Guide for AI Coding Agents & Contributors

Conventions for working in this repository (read alongside `CONTRIBUTING.md` and
`docs/ARCHITECTURE.md`). Tool-specific instruction files (`CLAUDE.md`, `GEMINI.md`) are
local-only; this is the shared, published guide.

## Orientation
- Read **`docs/ARCHITECTURE.md`** first for the layering and domain terms (Substrate,
  Gatekeeper, Auctioneer, the Auction model, the Zero-Hardcode Rule).
- Significant decisions live as ADRs in **`docs/adr/`** (see `docs/adr/README.md` for the
  status model). ADRs are the source of truth for *why*.

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
bash scripts/check-no-premium.sh
```
For cross-module (premium) work, use a Go workspace (`go.work`) — see `CONTRIBUTING.md`.

## Change discipline
- Match surrounding code style; add tests; keep PRs focused.
- After architectural changes, update `docs/ARCHITECTURE.md` and the relevant ADR.
- Never commit secrets — use `.env` / `CAMBRIAN_*`; the real `configs/config.json` is gitignored.
