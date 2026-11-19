package app

// KernelVersion is the kernel version reported on every identity surface: the
// operator-plane handshake (operator.Deps.KernelVersion) and the MCP endpoint's ServerVersion.
// One constant so the two can never report different kernels — they were two
// hand-kept literals, and both lagged the documented version (0.6.9-alpha vs
// v0.6.10, owner ruling 2026-10-31: 0.6.10).
//
// Bump it here, and only here: this constant is the one place the kernel
// version is recorded (CONTEXT.md no longer carries one since it became an HQ
// stub, ENG-1668), and the release tag names the same version. Premium reads
// it too — its accounts plugin release-gates the legacy-credential cleanup on
// it (ADR-0141 D11, cambrian-premium/plugins/accounts) — so a bump is a release
// boundary. The binary's build-time `main.version` (cmd/orchestrator, set by
// -ldflags from `git describe`) is a separate build identifier and is not read
// by these surfaces.
const KernelVersion = "0.6.10-alpha"
