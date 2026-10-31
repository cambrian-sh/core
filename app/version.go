package app

// KernelVersion is the kernel version reported on every identity surface: the
// operator-plane handshake (SetHandshake) and the MCP endpoint's ServerVersion.
// One constant so the two can never report different kernels — they were two
// hand-kept literals, and both lagged the documented version (0.6.9-alpha vs
// v0.6.10, owner ruling 2026-10-31: 0.6.10).
//
// Bump it here, in cambrian-core/CONTEXT.md, together. The binary's build-time
// `main.version` (cmd/orchestrator, set by -ldflags from `git describe`) is a
// separate build identifier and is not read by these surfaces.
const KernelVersion = "0.6.10-alpha"
