package domain

import (
	"path/filepath"
	"testing"
)

// ToolResourcePolicy is the system-resource authorization regime (ADR-0039 D8
// Regime 2) — the deterministic, fail-closed guard that bounds the OS/network
// resources a tool may touch. It is the security heart; these are its worked
// examples.

func TestToolResourcePolicy_AllowsPath(t *testing.T) {
	root := t.TempDir() // an absolute, real root

	pol := ToolResourcePolicy{Filesystem: FilesystemPolicy{AllowRoots: []string{root}}}

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"in-root file allowed", filepath.Join(root, "notes.txt"), true},
		{"nested in-root allowed", filepath.Join(root, "sub", "a.go"), true},
		{"the root itself allowed", root, true},
		{"parent-escape denied", filepath.Join(root, "..", "secret.txt"), false},
		{"sibling outside root denied", filepath.Join(filepath.Dir(root), "other", "x"), false},
		{"device path denied even if under root-ish", "/dev/mem", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pol.AllowsPath(tt.path); got != tt.want {
				t.Errorf("AllowsPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}

	// Fail-closed: a policy with no roots allows nothing.
	empty := ToolResourcePolicy{}
	if empty.AllowsPath(filepath.Join(root, "x")) {
		t.Error("empty policy must deny all paths (fail-closed)")
	}
}

func TestToolResourcePolicy_AllowsURL(t *testing.T) {
	pol := ToolResourcePolicy{Network: NetworkPolicy{AllowDomains: []string{"example.com", "api.github.com"}}}

	tests := []struct {
		name string
		url  string
		want bool
	}{
		{"allowlisted domain", "https://example.com/page", true},
		{"allowlisted subdomain host", "https://api.github.com/repos", true},
		{"non-allowlisted domain denied", "https://evil.com/x", false},
		{"loopback denied (SSRF)", "http://127.0.0.1/x", false},
		{"rfc1918 denied (SSRF)", "http://10.0.0.5/x", false},
		{"link-local cloud-metadata denied (SSRF)", "http://169.254.169.254/latest/meta-data", false},
		{"malformed denied", "not a url", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pol.AllowsURL(tt.url); got != tt.want {
				t.Errorf("AllowsURL(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}

	if (ToolResourcePolicy{}).AllowsURL("https://example.com") {
		t.Error("empty policy must deny all URLs (fail-closed)")
	}
}

func TestToolResourcePolicy_AllowsCommand(t *testing.T) {
	pol := ToolResourcePolicy{Command: CommandPolicy{
		AllowCommands:   []string{"ls", "cat", "echo"},
		BlockSubstrings: []string{"|", ">", "&&", "$(", "`"},
	}}

	tests := []struct {
		name string
		cmd  string
		want bool
	}{
		{"allowlisted command", "ls -la /data", true},
		{"allowlisted cat", "cat file.txt", true},
		{"non-allowlisted denied", "rm -rf /", false},
		{"pipe blocked", "ls | sh", false},
		{"redirect blocked", "echo x > /etc/passwd", false},
		{"command-substitution blocked", "echo $(whoami)", false},
		{"empty denied", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pol.AllowsCommand(tt.cmd); got != tt.want {
				t.Errorf("AllowsCommand(%q) = %v, want %v", tt.cmd, got, tt.want)
			}
		})
	}

	if (ToolResourcePolicy{}).AllowsCommand("ls") {
		t.Error("empty policy must deny all commands (fail-closed)")
	}
}

// AllowAll is the permissive policy used in unrestricted mode (operator opts the
// whole deployment into trusting all agents with all tools). It allows every
// path / URL / command — a deliberate, operator-chosen bypass of the resource
// regime, distinct from the fail-closed empty policy.
func TestToolResourcePolicy_AllowAll(t *testing.T) {
	pol := ToolResourcePolicy{AllowAll: true}
	if !pol.AllowsPath("/anything/at/all") {
		t.Error("AllowAll should permit any path")
	}
	if !pol.AllowsURL("http://169.254.169.254/") {
		t.Error("AllowAll should permit any URL (operator-chosen bypass)")
	}
	if !pol.AllowsCommand("rm -rf /") {
		t.Error("AllowAll should permit any command")
	}
}
