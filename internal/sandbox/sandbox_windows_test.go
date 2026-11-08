//go:build windows

package sandbox

import (
	"bufio"
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// Windows native containment against real runaway processes: the Job Object
// must stop a memory hog, a fork bomb and a CPU spinner, kill the whole tree
// on Close, and the low-integrity token must keep writes inside the workdir.

func limitedPolicy(t *testing.T, mut func(*TierPolicy)) *Policy {
	p := testPolicy(t)
	tp := p.Tiers[TierFirstParty]
	mut(&tp)
	p.Tiers[TierFirstParty] = tp
	return p
}

func TestWindowsJobKillsMemoryHog(t *testing.T) {
	p := limitedPolicy(t, func(tp *TierPolicy) { tp.MemoryMB = 128 })
	proc, rep, err := Start(context.Background(), p.Resolve("hog", "first-party", "", false, ServerSpec{}), fixture(t, "hog"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proc.Close() }()
	if !waitDone(t, proc, 60*time.Second) {
		t.Fatal("a 4 GiB allocation loop survived a 128 MiB job limit")
	}
	if !strings.HasPrefix(proc.KillReason(), "memory:") {
		t.Fatalf("kill reason: %q", proc.KillReason())
	}
	if !strings.Contains(strings.Join(rep.Mechanisms, ","), "job_object") {
		t.Fatalf("report: %+v", rep)
	}
}

func TestWindowsJobCapsForkBombAndKillsTree(t *testing.T) {
	p := limitedPolicy(t, func(tp *TierPolicy) { tp.MaxProcesses = 4 })
	proc, _, err := Start(context.Background(), p.Resolve("bomb", "first-party", "", false, ServerSpec{}), fixture(t, "forkbomb"))
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(proc).ReadString('\n')
	if err != nil {
		_ = proc.Close()
		t.Fatalf("read: %v", err)
	}
	var started int
	var pids []uint32
	for _, f := range strings.Fields(line) {
		if v, ok := strings.CutPrefix(f, "started="); ok {
			started, _ = strconv.Atoi(v)
		}
		if v, ok := strings.CutPrefix(f, "pids="); ok && v != "" {
			for _, s := range strings.Split(v, ",") {
				n, _ := strconv.Atoi(s)
				pids = append(pids, uint32(n))
			}
		}
	}
	if started > 3 || started == 0 {
		t.Fatalf("a 4-process job let the fixture start %d children (want 1..3): %q", started, line)
	}
	// Grab handles before Close so a reused pid cannot fool the check.
	var handles []windows.Handle
	for _, pid := range pids {
		if h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid); err == nil {
			handles = append(handles, h)
		}
	}
	_ = proc.Close()
	for _, h := range handles {
		ev, _ := windows.WaitForSingleObject(h, 5000)
		_ = windows.CloseHandle(h)
		if ev != windows.WAIT_OBJECT_0 {
			t.Fatal("a child outlived Close: the tree was not killed")
		}
	}
}

func TestWindowsJobKillsCPUSpinner(t *testing.T) {
	p := limitedPolicy(t, func(tp *TierPolicy) { tp.CPUTimeSec = 1 })
	proc, _, err := Start(context.Background(), p.Resolve("spin", "first-party", "", false, ServerSpec{}), fixture(t, "spin"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proc.Close() }()
	if !waitDone(t, proc, 30*time.Second) {
		t.Fatal("a CPU spinner survived a 1 s job CPU-time limit")
	}
	if !strings.HasPrefix(proc.KillReason(), "cpu_time:") {
		t.Fatalf("kill reason: %q", proc.KillReason())
	}
}

func TestWindowsLowIntegrityConfinesWrites(t *testing.T) {
	p := testPolicy(t)
	outside := filepath.Join(t.TempDir(), "escaped.txt")
	// Third-party with declared egress: everything else it wants (job,
	// low integrity) is native on Windows, so it starts.
	spec := p.Resolve("conf", "third-party", "", false, ServerSpec{Network: NetworkEgress})
	proc, rep, err := Start(context.Background(), spec, fixture(t, "write", outside))
	if err != nil {
		t.Fatalf("start: %v (report %+v)", err, rep)
	}
	defer func() { _ = proc.Close() }()
	line, err := bufio.NewReader(proc).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(line) != "inside=true outside=false" {
		t.Fatalf("low integrity did not confine writes: %q", line)
	}
	for _, c := range []Control{ControlFSConfine, ControlLowPrivilege, ControlMemory, ControlProcesses} {
		if !strings.Contains(rep.Summary(), string(c)) {
			t.Fatalf("summary lacks %s: %s", c, rep.Summary())
		}
	}
	t.Log(rep.Summary())
}

func TestWindowsThirdPartyWithoutEgressRefuses(t *testing.T) {
	p := testPolicy(t)
	_, rep, err := Start(context.Background(), p.Resolve("custom", "", "", false, ServerSpec{}), fixture(t, "echo"))
	if err == nil || !rep.Refused || !strings.Contains(strings.Join(rep.Missing, ";"), "network") {
		t.Fatalf("want a network refusal: %+v %v", rep, err)
	}
}
