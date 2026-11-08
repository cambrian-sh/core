//go:build linux

package sandbox

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Linux native containment against real runaway processes: the sh ulimit
// wrapper must stop a memory hog and a CPU spinner, Close must kill the whole
// process group, and bubblewrap (where it is usable) must confine writes and
// cut the network.

func linuxPolicy(t *testing.T, mut func(*TierPolicy)) *Policy {
	p := testPolicy(t)
	tp := p.Tiers[TierFirstParty]
	mut(&tp)
	p.Tiers[TierFirstParty] = tp
	return p
}

func TestLinuxUlimitKillsMemoryHog(t *testing.T) {
	p := linuxPolicy(t, func(tp *TierPolicy) { tp.MemoryMB = 256 })
	proc, rep, err := Start(context.Background(), p.Resolve("hog", "first-party", "", false, ServerSpec{}), fixture(t, "hog"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proc.Close() }()
	if !waitDone(t, proc, 60*time.Second) {
		t.Fatal("a 4 GiB allocation loop survived a 256 MiB data limit")
	}
	t.Log(rep.Summary())
}

func TestLinuxUlimitKillsCPUSpinner(t *testing.T) {
	p := linuxPolicy(t, func(tp *TierPolicy) { tp.CPUTimeSec = 1 })
	proc, _, err := Start(context.Background(), p.Resolve("spin", "first-party", "", false, ServerSpec{}), fixture(t, "spin"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proc.Close() }()
	if !waitDone(t, proc, 30*time.Second) {
		t.Fatal("a CPU spinner survived ulimit -t 1")
	}
}

func TestLinuxCloseKillsTheProcessGroup(t *testing.T) {
	p := testPolicy(t)
	proc, _, err := Start(context.Background(), p.Resolve("bomb", "first-party", "", false, ServerSpec{}), fixture(t, "forkbomb"))
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(proc).ReadString('\n')
	if err != nil {
		_ = proc.Close()
		t.Fatal(err)
	}
	var pids []int
	for _, f := range strings.Fields(line) {
		if v, ok := strings.CutPrefix(f, "pids="); ok && v != "" {
			for _, s := range strings.Split(v, ",") {
				n, _ := strconv.Atoi(s)
				pids = append(pids, n)
			}
		}
	}
	if len(pids) == 0 {
		_ = proc.Close()
		t.Fatalf("no children started: %q", line)
	}
	_ = proc.Close()
	deadline := time.Now().Add(5 * time.Second)
	for _, pid := range pids {
		for {
			// The orphaned children are reaped by init; signal 0 fails once gone
			// (a zombie still answers, so also accept a zombie state).
			if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) || isZombie(pid) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("child %d outlived Close: the group was not killed", pid)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func isZombie(pid int) bool {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return true
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i > 0 && i+2 < len(s) && s[i+2] == 'Z'
}

func TestLinuxBubblewrapConfinesWrites(t *testing.T) {
	p := testPolicy(t)
	if bw, why := bwrapUsable(p.BwrapPath); bw == "" {
		t.Skip("bubblewrap not usable here: " + why)
	}
	// The package source directory is outside the workdir and writable by
	// the test user, so only the sandbox stands between it and the write.
	wd, _ := os.Getwd()
	outside := filepath.Join(wd, "zz_bwrap_escaped.txt")
	t.Cleanup(func() { _ = os.Remove(outside) })
	// first-party with fs confinement switched on: the low_privilege control
	// (root-only natively) is not wanted, so the test runs as any user.
	tp := p.Tiers[TierFirstParty]
	tp.ConfineFS, tp.DenyNetwork = true, true
	p.Tiers[TierFirstParty] = tp
	spec := p.Resolve("conf", "first-party", "", false, ServerSpec{})
	// The test binary lives under /tmp, which bwrap replaces with a private
	// tmpfs: run a copy from the (bound) workdir instead.
	c := fixture(t, "write", outside)
	c.Path = copyInto(t, c.Path, WorkdirFor(p, spec))
	proc, rep, err := Start(context.Background(), spec, c)
	if err != nil {
		t.Fatalf("start: %v (%+v)", err, rep)
	}
	defer func() { _ = proc.Close() }()
	line, err := bufio.NewReader(proc).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(line) != "inside=true outside=false" {
		t.Fatalf("bubblewrap did not confine writes: %q", line)
	}
	if !strings.Contains(rep.Summary(), "bwrap") {
		t.Fatalf("summary: %s", rep.Summary())
	}
}

func copyInto(t *testing.T, src, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	dst := filepath.Join(dir, "fixture.bin")
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return dst
}

// TestLinuxThirdPartyFullContainmentAsRoot runs a third-party server with
// every control a root kernel with bubblewrap can give it: uid nobody, a
// read-only root, no network. It needs root and usable bubblewrap (the CI
// container runs it privileged); elsewhere it skips.
func TestLinuxThirdPartyFullContainmentAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs a root kernel")
	}
	p := DefaultPolicy()
	if bw, why := bwrapUsable(p.BwrapPath); bw == "" {
		t.Skip("bubblewrap not usable here: " + why)
	}
	root, err := os.MkdirTemp("/var/tmp", "cambrian-sbx-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0o711); err != nil {
		t.Fatal(err)
	}
	p.WorkRoot = root
	spec := p.Resolve("custom", "", "", false, ServerSpec{})
	for _, mode := range []string{"net", "env"} {
		c := fixture(t, mode)
		c.Path = copyInto(t, c.Path, WorkdirFor(p, spec))
		proc, rep, err := Start(context.Background(), spec, c)
		if err != nil {
			t.Fatalf("%s: start: %v (%s)", mode, err, rep.Summary())
		}
		// One line: the net fixture then waits on stdin until Close.
		out, _ := bufio.NewReader(proc).ReadString('\n')
		_ = proc.Close()
		switch mode {
		case "net":
			if strings.TrimSpace(string(out)) != "net=false" {
				t.Fatalf("network not denied: %q", out)
			}
			if rep.Refused || len(rep.Missing) != 0 {
				t.Fatalf("want full containment: %s", rep.Summary())
			}
			t.Log(rep.Summary())
		}
	}
}
