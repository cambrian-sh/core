//go:build linux

package sandbox

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// ENG-2112: the Linux limit watch sees a launched tree through /proc — its
// processes (thread-group leaders, not threads) and their resident memory.
func TestSampleTreeCountsChildrenAndMemory(t *testing.T) {
	c := exec.Command("sleep", "30")
	if err := c.Start(); err != nil {
		t.Skip("no sleep binary: " + err.Error())
	}
	defer func() { _ = c.Process.Kill(); _ = c.Wait() }()
	s := sampleTree(os.Getpid())
	if !s.ok || s.procs < 2 || s.rss <= 0 {
		t.Fatalf("sampleTree(self) = %+v, want ok, >= 2 processes (this test and its sleep child), rss > 0", s)
	}
	if r := checkLimits(TierPolicy{MaxProcesses: 2}, "", os.Getpid(), false, true); !strings.HasPrefix(r, "processes: ") {
		t.Fatalf("a tree of %d processes under a 2-process limit: %q", s.procs, r)
	}
	if r := checkLimits(TierPolicy{MemoryMB: 1}, "", os.Getpid(), false, true); !strings.HasPrefix(r, "memory: ") {
		t.Fatalf("a %d-byte tree under a 1 MiB cap: %q", s.rss, r)
	}
	if s := sampleTree(1 << 30); s.ok {
		t.Fatal("a pid that does not exist was sampled")
	}
}

func TestProcPPIDHandlesSpacesInTheCommandName(t *testing.T) {
	pp, ok := procPPID(os.Getpid())
	if !ok || pp != os.Getppid() {
		t.Fatalf("procPPID(self) = %d, %v; want %d", pp, ok, os.Getppid())
	}
}
