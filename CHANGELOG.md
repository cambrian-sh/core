# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project follows
[Semantic Versioning](https://semver.org/) (pre-1.0: the public Go API is unstable;
only the proto + config surfaces are held stable — see `CONTRIBUTING.md`).

## [Unreleased]

### Added
- Open-core split (ADR-0057): importable `app` package (`app.Run` + `app.Options`) so a
  downstream premium binary reuses the OSS composition root and injects proprietary
  components (langfuse tracing, reactive rule engine) via hooks.
- Source-available licensing (BSL 1.1, see `LICENSE`); `CONTRIBUTING`, `SECURITY`,
  `CODE_OF_CONDUCT`, `SUPPORT`, `MAINTAINERS`, `CODEOWNERS`.
- `configs/config.example.json` user-config template; three-tier config loading
  (built-in defaults → user config → `CAMBRIAN_*` env), tolerant of a missing base file.

### Changed
- Module path → `github.com/cambrian-sh/cambrian-runtime`.
- `internal/domain` promoted to the importable top-level `domain/` package.
- Build tags removed from the OSS/premium boundary; separation is now physical
  (premium is a separate module) + dependency injection.
- **Config files split** (ADR-0024 amended): `LoadConfig` now composes an
  **11-layer pipeline** (Go defaults → `tuning.json` → `tuning.local.json`
  → `config.json` → `config.local.json` → `embedder.json` →
  `embedder.local.json` → `providers.json` → `providers.local.json` →
  `mcp.json` → `CAMBRIAN_*` env vars). The committed `configs/tuning.json`
  is a **curated power-user starter** (13 hand-picked hyperparameters) — not
  a full mirror of `DefaultConfig()` — so new hyperparameters fall through
  cleanly. `configs/embedder.json` and `configs/providers.json` are
  gitignored; their committed `.example` files are templates. The
  `CAMBRIAN_*` env-var convention is **unchanged** and now applies to
  fields in every layer. **Non-breaking schema change**: existing
  `config.json` files continue to load, and `embedder` / `llm_provider`
  blocks inside `config.json` still work when the corresponding
  `*.json` file is absent.

### Removed
- All premium source (langfuse, reactive engine) extracted out of the OSS repo to the
  separate premium module; premium config fields removed from the OSS config schema.

### Security
- Untracked `configs/config.json` / `config.dev.json` (they contained live secrets);
  only the sanitized `config.example.json` is published.

<!-- Release sections (newest first) go below once tagged, e.g.:
## [v0.1.0-alpha.1] - 2026-xx-xx
-->
