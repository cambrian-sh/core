# Architecture Decision Records (ADRs)

ADRs record the *why* behind significant decisions. They are the project's design history
and, collectively, its roadmap (Proposed/Accepted ADRs = what's planned).

## Status vocabulary (controlled — one token, greppable)

| Status | Meaning |
|---|---|
| `Proposed` | Designed, not yet accepted/built. |
| `Accepted` | Decision approved; implementation may be pending. |
| `Implemented` | Built and live in the codebase. |
| `Amended-by-ADR-NNNN` | Still live, but partially revised by a later ADR. |
| `Superseded-by-ADR-NNNN` | Fully replaced by a later **shipped** ADR (no longer how the system works). |
| `Deprecated` | On the way out; avoid relying on it. |
| `Rejected` | Considered and declined (kept for the record). |
| `Cancelled` | Abandoned before/after partial work (kept for the record). |

**Rule — only flip to `Superseded` when the replacement actually ships.** A *Proposed*
superseder leaves the original `Implemented` with a `Superseded-by (pending): ADR-NNNN`
note (it's still what runs). Supersede the ADR that *introduced* the mechanism, not every
ADR that ever touched it. Never rewrite an original decision's text — add a top banner +
the frontmatter links below.

## Frontmatter (machine-readable)

New ADRs use YAML frontmatter so status and the supersession graph are derivable
(`grep`/scripts), which lets the "current architecture" view be generated rather than
hand-maintained:

```yaml
---
id: 0057
title: Open-Core Boundary, Licensing & OSS Release Model
status: Accepted
date: 2026-06-27
supersedes: []
superseded_by: []
---
```

`docs/adr/0057-open-core-boundary.md` is the reference example. Legacy ADRs use a
`**Status:** <token>` line; both are acceptable during the reconciliation, but the token
must come from the vocabulary above.

## Conventions

- Filename: `NNNN-kebab-title.md`; numbers are **unique and immutable** once published.
  (ADR-0001 = DAG Parallel Execution; the former duplicate is now ADR-0058.)
- PRDs are optional context (`docs/prd/`); the ADR is the canonical decision unit.
- Implementation slices live under `docs/issues/adrNNNN/`.
