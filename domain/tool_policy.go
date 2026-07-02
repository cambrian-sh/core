package domain

import (
	"net"
	"net/url"
	"path/filepath"
	"strings"
)

// ToolResourcePolicy is the per-grant system-resource authorization regime
// (ADR-0039 D8 Regime 2): the deterministic, fail-closed guard that bounds the
// filesystem paths, network egress, and shell commands a system tool may touch.
// It is distinct from EffectiveScope (which authorizes tagged documents) and is
// enforced kernel-side in ToolExecutor BEFORE the tool runs. No LLM in this
// boundary. Empty sub-policies deny everything.
type ToolResourcePolicy struct {
	Filesystem FilesystemPolicy `json:"filesystem,omitempty"`
	Network    NetworkPolicy    `json:"network,omitempty"`
	Command    CommandPolicy    `json:"command,omitempty"`
	// AllowAll is the permissive bypass used only in unrestricted mode
	// (ExecutionConfig.ToolsUnrestricted): the operator has chosen to trust all
	// agents with all tools. It allows every path/URL/command. Never set this on
	// an operator-issued grant — it defeats the resource regime by design.
	AllowAll bool `json:"allow_all,omitempty"`
}

// FilesystemPolicy bounds filesystem access to a set of allowed roots.
type FilesystemPolicy struct {
	AllowRoots []string `json:"allow_roots,omitempty"`
}

// NetworkPolicy bounds egress to an allowlist of domains. Private/loopback/
// link-local/metadata addresses are denied regardless (anti-SSRF).
type NetworkPolicy struct {
	AllowDomains []string `json:"allow_domains,omitempty"`
}

// CommandPolicy bounds shell execution to an allowlist of leading commands with
// a substring blocklist for shell metacharacters.
type CommandPolicy struct {
	AllowCommands   []string `json:"allow_commands,omitempty"`
	BlockSubstrings []string `json:"block_substrings,omitempty"`
}

// deviceBlocklist is a portable set of device/special path fragments that are
// never readable, regardless of root containment.
// Ported from hermes-agent tools/file_tools.py _BLOCKED_DEVICE_PATHS (MIT, © 2025 Nous Research).
var deviceBlocklist = []string{"/dev/", "/proc/", "/sys/", `\\.\`, `\\?\`}

// AllowsPath reports whether an absolute, **already symlink-resolved** path is
// permitted: it must sit under an allowed root and must not be a device path.
// The caller is responsible for resolving symlinks (filepath.EvalSymlinks) and
// re-checking, since symlink escape cannot be detected from the string alone.
func (p ToolResourcePolicy) AllowsPath(path string) bool {
	if p.AllowAll {
		return path != ""
	}
	if path == "" || len(p.Filesystem.AllowRoots) == 0 {
		return false
	}
	lower := strings.ToLower(filepath.ToSlash(path))
	for _, dev := range deviceBlocklist {
		if strings.Contains(lower, strings.ToLower(filepath.ToSlash(dev))) {
			return false
		}
	}
	clean := filepath.Clean(path)
	for _, root := range p.Filesystem.AllowRoots {
		if isUnderRoot(clean, filepath.Clean(root)) {
			return true
		}
	}
	return false
}

// isUnderRoot reports whether clean is the root or nested beneath it (no escape).
func isUnderRoot(clean, root string) bool {
	if clean == root {
		return true
	}
	rel, err := filepath.Rel(root, clean)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	return true
}

// AllowsURL reports whether a URL may be fetched: its host must be an
// allowlisted domain, and any IP (literal or allowlisted-domain host that is an
// IP) must not be private/loopback/link-local (anti-SSRF: cloud metadata
// 169.254.169.254 is link-local). Malformed URLs are denied.
func (p ToolResourcePolicy) AllowsURL(raw string) bool {
	if p.AllowAll {
		return raw != ""
	}
	if len(p.Network.AllowDomains) == 0 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := u.Hostname()

	// If the host is an IP literal, apply the private/loopback/link-local denial.
	if ip := net.ParseIP(host); ip != nil {
		return !isDeniedIP(ip)
	}

	// Domain host: must be in the allowlist.
	for _, d := range p.Network.AllowDomains {
		if strings.EqualFold(host, d) {
			return true
		}
	}
	return false
}

// isDeniedIP reports whether an IP is in a range tools must never reach.
func isDeniedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// AllowsCommand reports whether a shell command may run: its leading token must
// be allowlisted and it must contain no blocklisted substring.
func (p ToolResourcePolicy) AllowsCommand(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if p.AllowAll {
		return cmd != ""
	}
	if cmd == "" || len(p.Command.AllowCommands) == 0 {
		return false
	}
	for _, bad := range p.Command.BlockSubstrings {
		if strings.Contains(cmd, bad) {
			return false
		}
	}
	first := strings.Fields(cmd)[0]
	for _, ok := range p.Command.AllowCommands {
		if first == ok {
			return true
		}
	}
	return false
}
