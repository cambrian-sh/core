package sandbox

import (
	"strings"
	"testing"
)

// ADR-0157 S5 (D2.3): the macOS Seatbelt profile, as text. It runs on every
// platform; the live macOS e2e is a follow-up (no macOS host for S5).

func seatbeltFixture() seatbeltInput {
	return seatbeltInput{
		Workdir:   "/Users/k/Library/Caches/cambrian/agents/first-party/chat-1a2b3c4d",
		SocketDir: "/Users/k/Library/Caches/cambrian/agents/sock/1a2b3c4d",
		Hide: []string{"/srv/cambrian", "/srv/cambrian/configs", "/srv/cambrian/configs/secret.key",
			"/Users/k/Library/Caches/cambrian/agents", "/var/lib/pg"},
		HideFiles: map[string]bool{"/srv/cambrian/configs/secret.key": true},
		Binds: []Bind{
			{Path: "/srv/cambrian/agents"},
			{Path: "/opt/venv"},
			{Path: "/Users/k/models/hf", Write: true, Private: true},
		},
		DenyNetwork: true,
	}
}

const seatbeltGolden = `(version 1)
; ADR-0157 D1 (shape A): everything readable but the kernel's state.
(allow default)
(deny file-write*)
(allow file-write*
    (subpath "/Users/k/Library/Caches/cambrian/agents/first-party/chat-1a2b3c4d")
    (subpath "/Users/k/Library/Caches/cambrian/agents/sock/1a2b3c4d")
    (subpath "/Users/k/models/hf")
    (literal "/dev/null")
    (literal "/dev/zero")
    (literal "/dev/dtracehelper")
    (regex #"^/dev/tty")
    (regex #"^/dev/fd/"))
; the kernel's state (configs/, the key file, data directories, the journal,
; the vault, other instances' scratch): unreadable.
(deny file-read* file-write*
    (subpath "/Users/k/Library/Caches/cambrian/agents")
    (subpath "/srv/cambrian")
    (subpath "/srv/cambrian/configs")
    (literal "/srv/cambrian/configs/secret.key")
    (subpath "/var/lib/pg"))
(allow file-read-metadata
    (literal "/Users/k/Library/Caches/cambrian/agents")
    (literal "/Users/k/Library/Caches/cambrian/agents/first-party")
    (literal "/Users/k/Library/Caches/cambrian/agents/sock")
    (literal "/srv/cambrian"))
(allow file-read*
    (subpath "/srv/cambrian/agents"))
(allow file-read* file-write*
    (subpath "/Users/k/Library/Caches/cambrian/agents/first-party/chat-1a2b3c4d")
    (subpath "/Users/k/Library/Caches/cambrian/agents/sock/1a2b3c4d"))
; kernel-only (ADR-0157 D3): no network but the unix sockets in the socket directory.
(deny network*)
(allow network*
    (local unix-socket (subpath "/Users/k/Library/Caches/cambrian/agents/sock/1a2b3c4d"))
    (remote unix-socket (subpath "/Users/k/Library/Caches/cambrian/agents/sock/1a2b3c4d")))
`

func TestSeatbeltProfileGolden(t *testing.T) {
	got := seatbeltProfile(seatbeltFixture())
	if got != seatbeltGolden {
		t.Fatalf("profile differs from the golden text\n--- got ---\n%s\n--- want ---\n%s", got, seatbeltGolden)
	}
}

// SBPL: the last matching rule wins, so the order IS the policy.
func TestSeatbeltProfileOrder(t *testing.T) {
	prof := seatbeltProfile(seatbeltFixture())
	idx := func(s string) int {
		i := strings.Index(prof, s)
		if i < 0 {
			t.Fatalf("profile lacks %q:\n%s", s, prof)
		}
		return i
	}
	allowDefault, denyWrite, allowWrite := idx("(allow default)"), idx("(deny file-write*)"), idx("(allow file-write*")
	denyHidden, reRead, reOwn := idx("(deny file-read* file-write*"), idx("(allow file-read*\n"), idx("(allow file-read* file-write*")
	denyNet, allowSock := idx("(deny network*)"), idx("(allow network*")
	if !(allowDefault < denyWrite && denyWrite < allowWrite && allowWrite < denyHidden &&
		denyHidden < reRead && reRead < reOwn && denyNet < allowSock) {
		t.Fatalf("rule order breaks last-match-wins:\n%s", prof)
	}
	// A path outside the hide-list is never re-allowed explicitly: it is
	// readable through (allow default) and stays write-denied.
	if strings.Contains(prof, `(subpath "/opt/venv")`) {
		t.Fatalf("a bind outside the hide-list needs no rule:\n%s", prof)
	}
}

// The normal network (first-party under per_tier): no network rules at all.
func TestSeatbeltProfileNormalNetwork(t *testing.T) {
	in := seatbeltFixture()
	in.DenyNetwork = false
	if prof := seatbeltProfile(in); strings.Contains(prof, "network") {
		t.Fatalf("a tier with the normal network got network rules:\n%s", prof)
	}
}

// A writable declared path inside the hide-list is re-allowed read-write; a
// hidden file is a literal, a hidden directory a subpath; quotes are escaped.
func TestSeatbeltProfileReexposedWritableAndQuoting(t *testing.T) {
	in := seatbeltInput{
		Workdir: "/w/x", Hide: []string{"/k", `/k/we"ird`}, HideFiles: map[string]bool{`/k/we"ird`: true},
		Binds: []Bind{{Path: "/k/cache", Write: true}},
	}
	prof := seatbeltProfile(in)
	for _, want := range []string{
		`(literal "/k/we\"ird")`,
		"(allow file-read* file-write*\n    (subpath \"/k/cache\"))",
		"(allow file-read-metadata\n    (literal \"/k\"))",
	} {
		if !strings.Contains(prof, want) {
			t.Errorf("profile lacks %q:\n%s", want, prof)
		}
	}
	if strings.Contains(prof, "network") {
		t.Errorf("no DenyNetwork, no network rules:\n%s", prof)
	}
}

// An MCP-shaped input (no socket dir, a declared root): the root is writable.
func TestSeatbeltProfileRoot(t *testing.T) {
	prof := seatbeltProfile(seatbeltInput{Workdir: "/w", Root: "/data/out", DenyNetwork: true})
	if !strings.Contains(prof, `(subpath "/data/out")`) || !strings.Contains(prof, "(deny network*)") ||
		strings.Contains(prof, "unix-socket") {
		t.Fatalf("root/no-socket profile:\n%s", prof)
	}
}
