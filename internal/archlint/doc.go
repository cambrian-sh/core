// Package archlint holds the architecture guards of AGENTS.md §1.3, §2.1, §2.2
// and §2.7 as tests (ENG-1484): import layering, the restricted memory-stack
// constructors, ctx-first I/O methods on domain ports, and context.Background
// outside an explicit allowlist. It has no production code; `make archlint`
// runs its tests, in `make per-pr` and the CI verify job.
package archlint
