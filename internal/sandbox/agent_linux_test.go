//go:build linux

package sandbox

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ADR-0157 D1 on Linux: bubblewrap hides the kernel's state from an agent.

// The bwrap argv puts binds outside the hide-list before the hides, binds
// inside a hidden path after them, the socket directory at its own path, and
// leaves the network shared for a tier that does not deny it (first-party:
// normal network, ADR-0157 D3).
func TestBwrapArgsForAnAgent(t *testing.T) {
	root := t.TempDir()
	kernel := filepath.Join(root, "kernel")
	code := filepath.Join(kernel, "agents")
	cache := filepath.Join(root, "cache")
	sock := filepath.Join(root, "sock")
	for _, d := range []string{code, cache, sock} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	p := agentPolicy(t, kernel)
	spec := agentSpec(p, TierFirstParty, sock, Bind{Path: cache, Write: true}, Bind{Path: code})
	a := bwrapArgs("bwrap", p, spec, p.Tiers[TierFirstParty], filepath.Join(root, "work"), code, true)
	idx := func(seq ...string) int {
		for i := 0; i+len(seq) <= len(a); i++ {
			if slices.Equal(a[i:i+len(seq)], seq) {
				return i
			}
		}
		t.Fatalf("argv lacks %v: %v", seq, a)
		return -1
	}
	cacheAt, hideAt, codeAt := idx("--bind", cache, cache), idx("--tmpfs", kernel), idx("--ro-bind", code, code)
	if !(cacheAt < hideAt && hideAt < codeAt) {
		t.Fatalf("order cache=%d hide=%d code=%d: %v", cacheAt, hideAt, codeAt, a)
	}
	if ro := idx("--remount-ro", kernel); ro < codeAt {
		t.Fatalf("hidden dirs must be remounted read-only after the re-exposing binds: %v", a)
	}
	idx("--bind", sock, sock)
	idx("--chdir", code)
	if slices.Contains(a, "--unshare-net") {
		t.Fatalf("a first-party agent keeps the normal network: %v", a)
	}
	// ADR-0157 S2: a kernel-only tier gets its own (empty) network namespace;
	// its socket directory — where the kernel socket lives — is still bound.
	kp := p.Tiers[TierFirstParty]
	kp.DenyNetwork = true
	a = bwrapArgs("bwrap", p, spec, kp, filepath.Join(root, "work"), code, true)
	if !slices.Contains(a, "--unshare-net") {
		t.Fatalf("a kernel-only agent must get --unshare-net: %v", a)
	}
	idx("--bind", sock, sock)
}

// ENG-1969: bwrapArgs never lays a bind over the agent's private /tmp — not
// /tmp, not / (beyond the base read-only root), not the /tmp/work mount point —
// while a directory below /tmp (an interpreter in /tmp/bin) is still bound.
func TestBwrapArgsNeverBindOverThePrivateTmp(t *testing.T) {
	root := t.TempDir()
	interp := filepath.Join(root, "bin")
	if err := os.MkdirAll(interp, 0o700); err != nil {
		t.Fatal(err)
	}
	p := agentPolicy(t, filepath.Join(root, "kernel"))
	spec := agentSpec(p, TierFirstParty, "", Bind{Path: "/tmp"}, Bind{Path: "/"}, Bind{Path: "/tmp/work", Write: true},
		Bind{Path: interp})
	a := bwrapArgs("bwrap", p, spec, p.Tiers[TierFirstParty], filepath.Join(root, "work"), "", true)
	roots := 0
	for i := 0; i+2 < len(a); i++ {
		if a[i] != "--ro-bind" && a[i] != "--bind" {
			continue
		}
		switch a[i+2] {
		case "/":
			roots++
		case "/tmp":
			t.Fatalf("argv binds over the private /tmp: %v", a[i:i+3])
		case "/tmp/work":
			if a[i+1] != filepath.Join(root, "work") {
				t.Fatalf("argv binds %s at the workdir mount point: %v", a[i+1], a)
			}
		}
	}
	if roots != 1 || a[1] != "--ro-bind" || a[2] != "/" {
		t.Fatalf("only the base read-only root may be bound at /: %v", a)
	}
	if !slices.Contains(a, interp) {
		t.Fatalf("a directory below /tmp must still be bound: %v", a)
	}
}

// End to end through StartCmd under bubblewrap: the agent cannot read the
// kernel's config.db, its key file, the journal or the vault, nor another
// instance's scratch; a path outside the hide-list stays readable.
func TestBwrapAgentCannotReadKernelState(t *testing.T) {
	if bw, why := bwrapUsable(""); bw == "" {
		t.Skip("bubblewrap not usable here: " + why)
	}
	root := e2eRoot(t)
	kernel := filepath.Join(root, "kernel")
	files := map[string]string{
		"config.db":  filepath.Join(kernel, "configs", "config.db"),
		"secret.key": filepath.Join(kernel, "configs", "secret.key"),
		"journal":    filepath.Join(root, "data", "agents.db"),
		"vault":      filepath.Join(root, "data", "vault", "blob"),
		"visible":    filepath.Join(root, "public", "readme.txt"),
	}
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := agentPolicy(t, kernel, filepath.Join(root, "data"))
	p.WorkRoot = filepath.Join(root, "agents-work")
	other := filepath.Join(p.WorkRoot, string(TierFirstParty), "other-11112222", "secret.txt")
	if err := os.MkdirAll(filepath.Dir(other), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.HidePaths = append(p.HidePaths, p.WorkRoot)
	files["other-agent"] = other
	sock := filepath.Join(p.WorkRoot, "sock", "0000aaaa")
	if err := os.MkdirAll(sock, 0o700); err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	var paths []string
	for _, f := range files {
		paths = append(paths, f)
	}
	c := fixture(t, "read", paths...)
	if CoversPrivateTmp(filepath.Dir(exe)) {
		exe = copyExecutable(t, exe) // ENG-1969: never bound over the private /tmp
		c.Path = exe
	}
	spec := agentSpec(p, TierFirstParty, sock, Bind{Path: filepath.Dir(exe)})
	var stdout io.ReadCloser
	l, rep, err := StartCmd(context.Background(), spec, c, func(cmd *exec.Cmd) error {
		var perr error
		stdout, perr = cmd.StdoutPipe()
		return perr
	})
	if err != nil {
		t.Fatalf("start: %v (%s)", err, rep.Summary())
	}
	out, _ := io.ReadAll(stdout)
	_ = l.Cmd.Wait()
	l.Release()
	t.Log(rep.Summary())
	if !slices.Contains(rep.Enforced, ControlHideState) || !slices.Contains(rep.Enforced, ControlFSConfine) {
		t.Fatalf("hide_state/fs_confine not enforced: %s", rep.Summary())
	}
	for name, f := range files {
		want := "denied"
		if name == "visible" {
			want = "ok"
		}
		if !strings.Contains(string(out), f+"="+want) {
			t.Errorf("%s (%s): want %s, got:\n%s", name, f, want, out)
		}
	}
}

// e2eRoot is a directory outside /tmp (bubblewrap gives the agent a private
// /tmp, which would hide a /tmp fixture for the wrong reason): $SANDBOX_E2E_ROOT
// when set, else a directory under the user cache.
func e2eRoot(t *testing.T) string {
	t.Helper()
	base := os.Getenv("SANDBOX_E2E_ROOT")
	if base == "" {
		c, err := os.UserCacheDir()
		if err != nil {
			t.Skip("no user cache dir for an out-of-/tmp fixture")
		}
		base = c
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := os.MkdirTemp(base, "sbx-e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}
