package sandbox

import (
	"path"
	"sort"
	"strings"
)

// The macOS Seatbelt agent runner (ADR-0157 D2.3, stage S5).
//
// macOS has no bubblewrap, but its kernel sandbox (Seatbelt, the profile
// language behind sandbox-exec and sandbox_init) can express the same D1 shape
// the Linux runner builds with mounts — shape A, the owner's 2026-11-17 choice
// (ENG-1973): the whole filesystem readable, the hide-list unreadable, the
// agent's own code and runtime inside the hide-list readable again, writes only
// to the instance's scratch, its socket directory and the declared writable
// paths, and no network for a kernel-only tier except the unix sockets in its
// socket directory.
//
// The profile is generated here, in a file without a build tag, so its text is
// unit-tested on every platform (seatbelt_test.go); start_unix.go runs it on
// darwin as `sandbox-exec -p <profile> /bin/sh -c 'ulimit …; exec "$@"' …`.
//
// SBPL evaluates every matching rule and the LAST match wins, so the order
// below is the policy: broad allow, then the write denial, then the
// hide-list denial, then the re-exposed and own paths allowed again.
//
// STATUS: built and unit-tested only. No macOS host was available for S5, so
// the profile has never run under sandbox-exec; the macOS live e2e is a
// follow-up (see the S5 ticket). Do not read the "seatbelt" mechanism in a
// report as a verified gate until that run exists.

// seatbeltInput is what one profile is rendered from (all paths absolute;
// canonicalised by the caller on the host — /var is /private/var on macOS and
// Seatbelt matches the resolved path).
type seatbeltInput struct {
	Workdir   string
	SocketDir string
	Hide      []string // the D1 hide-list (Policy.HidePaths)
	HideFiles map[string]bool
	Binds     []Bind // the agent's binds (code, runtime, declared paths)
	Root      string // an MCP server's declared writable root ("" for agents)
	// DenyNetwork: kernel-only — no network but the unix sockets under SocketDir.
	DenyNetwork bool
}

// seatbeltDevWrites are the device nodes any process writes to.
var seatbeltDevWrites = []string{
	`(literal "/dev/null")`, `(literal "/dev/zero")`, `(literal "/dev/dtracehelper")`,
	`(regex #"^/dev/tty")`, `(regex #"^/dev/fd/")`,
}

// seatbeltProfile renders the SBPL profile for one launch.
func seatbeltProfile(in seatbeltInput) string {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("; ADR-0157 D1 (shape A): everything readable but the kernel's state.\n")
	b.WriteString("(allow default)\n")

	// Writes: only the instance's own directories and declared writable paths.
	own := []string{in.Workdir}
	if in.SocketDir != "" {
		own = append(own, in.SocketDir)
	}
	if in.Root != "" {
		own = append(own, in.Root)
	}
	var writable []string
	for _, bd := range in.Binds {
		if bd.Write && bd.Path != "" {
			writable = append(writable, bd.Path)
		}
	}
	b.WriteString("(deny file-write*)\n")
	b.WriteString("(allow file-write*")
	for _, p := range append(append([]string{}, own...), writable...) {
		b.WriteString("\n    " + sbSubpath(p))
	}
	for _, d := range seatbeltDevWrites {
		b.WriteString("\n    " + d)
	}
	b.WriteString(")\n")

	// The hide-list: neither readable nor writable.
	hide := cleanSorted(in.Hide)
	if len(hide) > 0 {
		b.WriteString("; the kernel's state (configs/, the key file, data directories, the journal,\n")
		b.WriteString("; the vault, other instances' scratch): unreadable.\n")
		b.WriteString("(deny file-read* file-write*")
		for _, h := range hide {
			if in.HideFiles[h] {
				b.WriteString("\n    " + sbLiteral(h))
			} else {
				b.WriteString("\n    " + sbSubpath(h))
			}
		}
		b.WriteString(")\n")
	}

	// Re-exposed: a bind inside a hidden path is readable again (writable when
	// declared so), and the directories on the way down to it may be stat'ed —
	// not listed — so the path resolves.
	var reRead, reWrite []string
	traverse := map[string]bool{}
	for _, bd := range in.Binds {
		if bd.Path == "" {
			continue
		}
		h := containingHide(bd.Path, hide)
		if h == "" {
			continue
		}
		if bd.Write {
			reWrite = append(reWrite, bd.Path)
		} else {
			reRead = append(reRead, bd.Path)
		}
		for d := path.Dir(bd.Path); sbWithin(d, h); d = path.Dir(d) {
			traverse[d] = true
			if path.Clean(d) == path.Clean(h) || path.Dir(d) == d {
				break
			}
		}
	}
	// The instance's own directories live under the agent work root, which is
	// itself hidden (other instances' scratch): allowed again last.
	for _, p := range own {
		if h := containingHide(p, hide); h != "" {
			reWrite = append(reWrite, p)
			for d := path.Dir(p); sbWithin(d, h); d = path.Dir(d) {
				traverse[d] = true
				if path.Clean(d) == path.Clean(h) || path.Dir(d) == d {
					break
				}
			}
		}
	}
	if len(traverse) > 0 {
		b.WriteString("(allow file-read-metadata")
		for _, d := range sortedKeys(traverse) {
			b.WriteString("\n    " + sbLiteral(d))
		}
		b.WriteString(")\n")
	}
	if len(reRead) > 0 {
		b.WriteString("(allow file-read*")
		for _, p := range cleanSorted(reRead) {
			b.WriteString("\n    " + sbSubpath(p))
		}
		b.WriteString(")\n")
	}
	if len(reWrite) > 0 {
		b.WriteString("(allow file-read* file-write*")
		for _, p := range cleanSorted(reWrite) {
			b.WriteString("\n    " + sbSubpath(p))
		}
		b.WriteString(")\n")
	}

	// Network (D3).
	if in.DenyNetwork {
		b.WriteString("; kernel-only (ADR-0157 D3): no network but the unix sockets in the socket directory.\n")
		b.WriteString("(deny network*)\n")
		if in.SocketDir != "" {
			b.WriteString("(allow network*\n    (local unix-socket " + sbSubpath(in.SocketDir) + ")\n    (remote unix-socket " + sbSubpath(in.SocketDir) + "))\n")
		}
	}
	return b.String()
}

// containingHide returns the hide path p lies inside ("" if none).
func containingHide(p string, hide []string) string {
	for _, h := range hide {
		if sbWithin(p, h) {
			return h
		}
	}
	return ""
}

// sbWithin reports whether p is d or lies under it (slash paths: macOS).
func sbWithin(p, d string) bool {
	p, d = path.Clean(p), path.Clean(d)
	return p == d || strings.HasPrefix(p, strings.TrimSuffix(d, "/")+"/")
}

func sbQuote(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(p) + `"`
}

func sbSubpath(p string) string { return "(subpath " + sbQuote(path.Clean(p)) + ")" }
func sbLiteral(p string) string { return "(literal " + sbQuote(path.Clean(p)) + ")" }

func cleanSorted(ps []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range ps {
		if p == "" {
			continue
		}
		p = path.Clean(p)
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
