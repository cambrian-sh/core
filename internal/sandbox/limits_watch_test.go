package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// ENG-2112: an agent that exceeds a resource limit is KILLED (owner decision
// 2026-11-18), with a reason, and its report names every limit it wanted.

// limitPolicy is the agent policy with first-party limits and a fast watch.
func limitPolicy(t *testing.T, mut func(*TierPolicy)) *Policy {
	t.Helper()
	p := agentPolicy(t)
	tp := p.Tiers[TierFirstParty]
	mut(&tp)
	p.Tiers[TierFirstParty] = tp
	p.LimitPoll = 50 * time.Millisecond
	return p
}

// startLimited launches a fixture as a first-party agent under p.
func startLimited(t *testing.T, p *Policy, mode string, args ...string) (*Launched, Report) {
	t.Helper()
	sockDir := filepath.Join(p.WorkRoot, "sock", "0000aaaa")
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	c := fixture(t, mode, args...)
	if CoversPrivateTmp(filepath.Dir(c.Path)) {
		c.Path = copyExecutable(t, c.Path)
	}
	c.Dir = filepath.Dir(c.Path)
	spec := agentSpec(p, TierFirstParty, sockDir, Bind{Path: filepath.Dir(c.Path)})
	l, rep, err := StartCmd(context.Background(), spec, c, nil)
	if err != nil {
		t.Fatalf("start: %v (report %s)", err, rep.Summary())
	}
	t.Cleanup(l.Release)
	return l, rep
}

// waitExit waits for the launched process to exit.
func waitExit(t *testing.T, l *Launched, within time.Duration) bool {
	t.Helper()
	done := make(chan struct{})
	go func() { _ = l.Cmd.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(within):
		_ = l.Kill()
		<-done
		return false
	}
}

func TestScratchQuotaIsWantedAndEnforcedForAgentsOnly(t *testing.T) {
	p := limitPolicy(t, func(tp *TierPolicy) { tp.ScratchMB = 8 })
	rep := Plan(context.Background(), agentSpec(p, TierFirstParty, ""), Command{Path: "x"})
	if !slices.Contains(rep.Enforced, ControlScratch) || !slices.Contains(rep.Mechanisms, "scratch_watch") {
		t.Fatalf("an agent's scratch quota is not enforced: %s", rep.Summary())
	}
	// An MCP launch has no limit watch: a scratch quota set on its tier is
	// reported missing, never silently not enforced.
	mp := testPolicy(t)
	tp := mp.Tiers[TierFirstParty]
	tp.ScratchMB = 8
	mp.Tiers[TierFirstParty] = tp
	mrep := Plan(context.Background(), mp.Resolve("srv", "first-party", "", false, ServerSpec{}), Command{Path: "x"})
	if slices.Contains(mrep.Enforced, ControlScratch) ||
		!slices.ContainsFunc(mrep.Missing, func(s string) bool { return strings.HasPrefix(s, string(ControlScratch)+":") }) {
		t.Fatalf("an MCP scratch quota must be reported missing: %s", mrep.Summary())
	}
	// No quota, no control: the MCP reports are unchanged by ENG-2112.
	mrep = Plan(context.Background(), testPolicy(t).Resolve("srv", "third-party", "", false, ServerSpec{}), Command{Path: "x"})
	for _, s := range append(controlStrings(mrep.Enforced), mrep.Missing...) {
		if strings.HasPrefix(s, string(ControlScratch)) {
			t.Fatalf("an MCP report gained scratch_quota: %s", mrep.Summary())
		}
	}
}

func TestTaskCeilingFallsBackToMaxProcesses(t *testing.T) {
	if n := (TierPolicy{MaxProcesses: 64}).taskCeiling(); n != 64 {
		t.Fatalf("MCP tier (no MaxTasks): %d, want 64", n)
	}
	if n := (TierPolicy{MaxProcesses: 32, MaxTasks: 1024}).taskCeiling(); n != 1024 {
		t.Fatalf("agent tier: %d, want MaxTasks 1024", n)
	}
}

func TestCheckLimitsScratch(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "home", "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "home", "deep", "a.bin"), make([]byte, 3<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := dirSize(dir); n != 3<<20 {
		t.Fatalf("dirSize = %d, want %d", n, 3<<20)
	}
	if r := checkLimits(TierPolicy{ScratchMB: 4}, dir, 0, true, false); r != "" {
		t.Fatalf("3 MiB under a 4 MiB quota killed: %q", r)
	}
	if r := checkLimits(TierPolicy{ScratchMB: 2}, dir, 0, true, false); !strings.HasPrefix(r, "scratch_quota: ") || !strings.Contains(r, "limit 2 MiB") {
		t.Fatalf("3 MiB over a 2 MiB quota: %q", r)
	}
}

// Nothing to watch, no goroutine (goleak in TestMain checks the others stop).
func TestLimitWatchNotStartedWithoutLimits(t *testing.T) {
	if w := startLimitWatch(TierPolicy{}, t.TempDir(), os.Getpid(), 0, func() error { return nil }); w != nil {
		w.close()
		t.Fatal("a watch started with no limit to watch")
	}
	var w *limitWatch
	w.close() // nil-safe
	if w.killReason() != "" {
		t.Fatal("nil watch has a reason")
	}
}

// A real agent fills its scratch past the quota: the watch kills the tree and
// says why. Every OS (size accounting).
func TestStartCmd_ScratchQuotaKillsTheAgent(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a real subprocess")
	}
	p := limitPolicy(t, func(tp *TierPolicy) { tp.ScratchMB = 8 })
	l, rep := startLimited(t, p, "fill", "64")
	if !slices.Contains(rep.Enforced, ControlScratch) {
		t.Fatalf("scratch quota not enforced: %s", rep.Summary())
	}
	if !waitExit(t, l, 30*time.Second) {
		t.Fatal("an agent writing 64 MiB survived an 8 MiB scratch quota")
	}
	if r := l.KillReason(); !strings.HasPrefix(r, "scratch_quota: ") {
		t.Fatalf("kill reason = %q, want scratch_quota", r)
	}
}

// A real agent forks past its process limit: the tree is killed with the
// reason. Windows: the job's active-process limit message; Linux: the watch.
func TestStartCmd_ProcessLimitKillsTheAgent(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a real subprocess")
	}
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("no process-limit killer on " + runtime.GOOS + " (the report names processes missing)")
	}
	p := limitPolicy(t, func(tp *TierPolicy) { tp.MaxProcesses = 4; tp.MaxTasks = 256 })
	l, rep := startLimited(t, p, "forkhold")
	if !waitExit(t, l, 30*time.Second) {
		t.Fatalf("a fork-bombing agent survived a 4-process limit (report %s)", rep.Summary())
	}
	if r := l.KillReason(); !strings.HasPrefix(r, "processes: ") {
		t.Fatalf("kill reason = %q, want processes", r)
	}
}

// The first-party memory cap reaches an agent launch and kills it (Windows:
// the job's memory message; the watch elsewhere where the tree is visible).
func TestStartCmd_MemoryCapKillsTheAgent(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a real subprocess")
	}
	if runtime.GOOS != "windows" {
		t.Skip("on Unix the per-process data limit makes the allocation fail (the Go runtime then aborts) before any tree total is reached")
	}
	p := limitPolicy(t, func(*TierPolicy) {})
	sockDir := filepath.Join(p.WorkRoot, "sock", "0000aaaa")
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	c := fixture(t, "hog")
	c.Dir = filepath.Dir(c.Path)
	spec := agentSpec(p, TierFirstParty, sockDir, Bind{Path: filepath.Dir(c.Path)})
	spec.Server.MemoryMB = 128 // admitLaunch's cap travels as ServerSpec.MemoryMB
	l, rep, err := StartCmd(context.Background(), spec, c, func(cmd *exec.Cmd) error { return nil })
	if err != nil {
		t.Fatalf("start: %v (%s)", err, rep.Summary())
	}
	t.Cleanup(l.Release)
	if !slices.Contains(rep.Enforced, ControlMemory) {
		t.Fatalf("memory not enforced: %s", rep.Summary())
	}
	if !waitExit(t, l, 60*time.Second) {
		t.Fatal("a 4 GiB allocation loop survived a 128 MiB agent memory cap")
	}
	if r := l.KillReason(); !strings.HasPrefix(r, "memory: ") {
		t.Fatalf("kill reason = %q, want memory", r)
	}
}
