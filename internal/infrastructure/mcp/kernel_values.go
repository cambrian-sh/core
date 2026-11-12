package mcp

import (
	"path/filepath"
	"sync"

	"github.com/cambrian-sh/core/internal/config"
)

// KernelDirPlaceholder names the kernel's own bundle directory (the directory
// holding configs/, prompts/, tools/; config.ResolveBaseDir, made absolute) as an
// args placeholder "${CAMBRIAN_KERNEL_DIR}". It lets a catalog entry launch a
// script the kernel ships, such as the code sandbox's server
// (tools/code_sandbox/server.py, launched with `uv run --directory
// ${CAMBRIAN_KERNEL_DIR} ... python tools/code_sandbox/server.py`), from a stdio
// server whose working directory is its own sandbox workdir. Kernel-provided:
// env_values refuses CAMBRIAN_* names, so no setting can redirect it.
const KernelDirPlaceholder = "CAMBRIAN_KERNEL_DIR"

var (
	kernelDirOnce sync.Once
	kernelDir     string
)

// KernelLaunchValue returns the value of a kernel-provided launch placeholder,
// and whether name is one.
func KernelLaunchValue(name string) (string, bool) {
	if name != KernelDirPlaceholder {
		return "", false
	}
	kernelDirOnce.Do(func() {
		dir := config.ResolveBaseDir()
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		kernelDir = filepath.ToSlash(dir)
	})
	return kernelDir, true
}

// IsKernelPlaceholder reports whether name is a kernel-provided launch value.
func IsKernelPlaceholder(name string) bool { return name == KernelDirPlaceholder }
