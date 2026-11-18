package agentmgr

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ENG-2088: the agent work root is owned per kernel, and a boot sweeps the
// instance directories no live kernel owns.

func mkInstanceDir(t *testing.T, root, rel string, files ...string) string {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestMarkerNameRoundTrips(t *testing.T) {
	for _, rel := range []string{"first-party/chat_agent-1234abcd", "third-party/vendor.x-0000aaaa", "sock/1234abcd"} {
		if got := markerRel(markerName(rel)); got != rel {
			t.Errorf("%s → %s → %s", rel, markerName(rel), got)
		}
		if strings.ContainsAny(markerName(rel), `/\`) {
			t.Errorf("marker %q is not one path segment", markerName(rel))
		}
	}
}

// Owned vs orphaned vs another live kernel's: the boot sweep removes the
// leftovers of a kernel that died without records (pre-ENG-2088) and those a
// dead owner recorded, and leaves every directory a live owner claims.
func TestOwnWorkRoot_SweepsOrphansKeepsLiveOwners(t *testing.T) {
	root := t.TempDir()
	legacy := mkInstanceDir(t, root, "first-party/chat_agent-aaaa1111", "out/a.txt", "out/sub/b.txt", "home/x")
	legacySock := mkInstanceDir(t, root, "sock/aaaa1111", "kernel.sock")
	legacyThird := mkInstanceDir(t, root, "third-party/vendor-bbbb2222")
	// A dead owner: a lock file nobody holds, and its record of one directory.
	deadDir := mkInstanceDir(t, root, "first-party/old-cccc3333", "out/c.txt")
	owners := filepath.Join(root, ownersDirName)
	mkInstanceDir(t, owners, "dead00")
	if err := os.WriteFile(filepath.Join(owners, "dead00"+lockSuffix), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owners, "dead00", markerName("first-party/old-cccc3333")), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A markers directory whose lock file is gone: also dead.
	mkInstanceDir(t, owners, "nolock")
	// Another LIVE kernel sharing the work root, claiming two directories.
	live, err := acquireWorkRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(live.close)
	liveScratch := filepath.Join(root, "first-party", "live-dddd4444")
	liveSock := filepath.Join(root, "sock", "dddd4444")
	if err := live.claim(liveScratch, liveSock); err != nil {
		t.Fatal(err)
	}
	mkInstanceDir(t, root, "first-party/live-dddd4444", "out/keep.txt")
	mkInstanceDir(t, root, "sock/dddd4444")
	// Not instance directories: never touched.
	stray := filepath.Join(root, "first-party", "note.txt")
	if err := os.WriteFile(stray, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	sb := NewAgentSandbox(AgentSandboxOptions{WorkRoot: root})
	t.Cleanup(sb.ReleaseWorkRoot)
	rep, err := sb.OwnWorkRoot()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"first-party/chat_agent-aaaa1111", "first-party/old-cccc3333", "sock/aaaa1111", "third-party/vendor-bbbb2222"}
	if !slices.Equal(rep.Removed, want) {
		t.Errorf("removed %v, want %v", rep.Removed, want)
	}
	if rep.OutFiles != 3 || rep.LiveOwners != 1 || rep.DeadOwners != 2 || len(rep.Failed) != 0 || rep.Skipped != "" {
		t.Errorf("report %+v: want 3 out files, 1 live owner, 2 dead owners, no failure", rep)
	}
	for _, d := range []string{legacy, legacySock, legacyThird, deadDir} {
		if exists(d) {
			t.Errorf("orphan %s survived the sweep", d)
		}
	}
	for _, d := range []string{liveScratch, filepath.Join(liveScratch, "out", "keep.txt"), liveSock, stray} {
		if !exists(d) {
			t.Errorf("%s (a live kernel's, or not an instance dir) was removed", d)
		}
	}
	for _, p := range []string{filepath.Join(owners, "dead00"), filepath.Join(owners, "dead00"+lockSuffix), filepath.Join(owners, "nolock")} {
		if exists(p) {
			t.Errorf("dead owner record %s was not cleared", p)
		}
	}
	// A second call does not sweep again.
	if again, _ := sb.OwnWorkRoot(); again.Skipped != "already owned" {
		t.Errorf("second OwnWorkRoot: %+v", again)
	}
}

// The live owner's claims are what protects it: once it gives the root up
// with a claim left (an instance whose cleanup never ran), the next boot
// treats it as dead and sweeps that directory.
func TestOwnWorkRoot_ReleasedOwnerWithClaimsIsSwept(t *testing.T) {
	root := t.TempDir()
	a, err := acquireWorkRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(root, "first-party", "busy-eeee5555")
	gone := filepath.Join(root, "sock", "eeee5555")
	if err := a.claim(kept, gone); err != nil {
		t.Fatal(err)
	}
	mkInstanceDir(t, root, "first-party/busy-eeee5555")
	a.release(gone) // a cleanup that ran
	a.close()
	if exists(a.lock0()) {
		t.Errorf("close left the lock file")
	}
	if !exists(a.markers) {
		t.Fatalf("close removed the markers of an unfinished instance")
	}
	sb := NewAgentSandbox(AgentSandboxOptions{WorkRoot: root})
	t.Cleanup(sb.ReleaseWorkRoot)
	rep, err := sb.OwnWorkRoot()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Removed, []string{"first-party/busy-eeee5555"}) || exists(kept) || exists(a.markers) {
		t.Errorf("released owner's leftover not swept: %+v", rep)
	}
	// A clean release leaves nothing behind.
	sb.ReleaseWorkRoot()
	if ents, _ := os.ReadDir(filepath.Join(root, ownersDirName)); len(ents) != 0 {
		t.Errorf("owners/ not empty after a clean release: %v", ents)
	}
}

// When the MCP sandbox's work root overlaps the agent work root, unclaimed
// directories may be MCP workdirs: only what a dead agent kernel recorded goes.
func TestOwnWorkRoot_OverlappingKeepRootSweepsOnlyDeadRecords(t *testing.T) {
	root := t.TempDir()
	mcp := mkInstanceDir(t, root, "first-party/github")
	deadDir := mkInstanceDir(t, root, "first-party/old-ffff6666")
	owners := filepath.Join(root, ownersDirName)
	mkInstanceDir(t, owners, "dead01")
	if err := os.WriteFile(filepath.Join(owners, "dead01", markerName("first-party/old-ffff6666")), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sb := NewAgentSandbox(AgentSandboxOptions{WorkRoot: root})
	t.Cleanup(sb.ReleaseWorkRoot)
	rep, err := sb.OwnWorkRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if !exists(mcp) || exists(deadDir) || rep.Skipped == "" {
		t.Errorf("overlap: mcp kept=%v dead removed=%v report %+v", exists(mcp), !exists(deadDir), rep)
	}
}

// A launch claims its directories through the owner; without OwnWorkRoot it
// claims nothing (nil-safe), and a directory outside the root is refused.
func TestWorkRootOwner_ClaimRelease(t *testing.T) {
	var none *workRootOwner
	if err := none.claim("/x"); err != nil {
		t.Fatal(err)
	}
	none.release("/x")
	root := t.TempDir()
	o, err := acquireWorkRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.close)
	d := filepath.Join(root, "third-party", "v-12345678")
	if err := o.claim(d); err != nil {
		t.Fatal(err)
	}
	if got := readMarkers(o.markers); !slices.Equal(got, []string{"third-party/v-12345678"}) {
		t.Errorf("markers %v", got)
	}
	o.release(d)
	if got := readMarkers(o.markers); len(got) != 0 {
		t.Errorf("markers after release %v", got)
	}
	if err := o.claim(filepath.Join(t.TempDir(), "elsewhere")); err == nil {
		t.Errorf("a directory outside the work root was claimed")
	}
}

// lock0 is the owner's lock file path (tests).
func (o *workRootOwner) lock0() string {
	return filepath.Join(o.root, ownersDirName, o.id+lockSuffix)
}

// runWorkRootKernel is the "kernel" of the e2e below (TestMain,
// AGENTMGR_CHAOS_HELPER=wrkernel): it owns AGENTMGR_WR_ROOT the way a booting
// kernel does, launches one sandboxed probe agent through the real path,
// prints "READY <socket> <agent pid>" once the agent has written out/, and on
// stdin EOF evicts it and gives the root up ("BYE").
func runWorkRootKernel() {
	root, kernelDir, exe := os.Getenv("AGENTMGR_WR_ROOT"), os.Getenv("AGENTMGR_WR_KERNEL"), os.Getenv("AGENTMGR_WR_EXE")
	fail := func(err error) { fmt.Println("ERR", err); os.Exit(5) }
	sb := NewAgentSandbox(AgentSandboxOptions{WorkRoot: root, KernelDir: kernelDir})
	if _, err := sb.OwnWorkRoot(); err != nil {
		fail(err)
	}
	im := NewInstanceManager("127.0.0.1:1")
	im.SetAgentContainment(AgentContainment{Sandbox: sb})
	im.SetAgentTransport(AgentTransport{KernelSocket: listenKernelSocketForTest})
	im.SetEnvExtras([]string{"AGENTMGR_CHAOS_HELPER=probe"})
	def := &domain.AgentDefinition{ID: os.Getenv("AGENTMGR_WR_ID"), Runtime: domain.RuntimeBinary, ExecPath: exe,
		Dir: filepath.Dir(exe), TrustTier: "first-party"}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	inst, err := im.GetOrBootInstance(ctx, def, "")
	if err != nil {
		fail(err)
	}
	var rep probeReport
	for deadline := time.Now().Add(30 * time.Second); ; {
		raw, rerr := os.ReadFile(filepath.Join(filepath.Dir(inst.SocketPath), "probe.json"))
		if rerr == nil && json.Unmarshal(raw, &rep) == nil {
			break
		}
		if time.Now().After(deadline) {
			fail(fmt.Errorf("no probe report"))
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Printf("READY %s %d\n", inst.SocketPath, rep.Pid)
	_, _ = io.Copy(io.Discard, os.Stdin)
	im.mu.Lock()
	pe := im.exits[inst.ID]
	im.mu.Unlock()
	im.EvictInstance(inst.ID)
	if pe != nil {
		<-pe.done
	}
	sb.ReleaseWorkRoot()
	fmt.Println("BYE")
	os.Exit(0)
}

type wrKernel struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  *bufio.Scanner
	socket string
	pid    int
}

func startWRKernel(t *testing.T, root, kernelDir, exe, id string) *wrKernel {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^$")
	cmd.Env = append(os.Environ(), "AGENTMGR_CHAOS_HELPER=wrkernel", "AGENTMGR_WR_ROOT="+root,
		"AGENTMGR_WR_KERNEL="+kernelDir, "AGENTMGR_WR_EXE="+exe, "AGENTMGR_WR_ID="+id)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	k := &wrKernel{cmd: cmd, stdin: stdin, lines: bufio.NewScanner(out)}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	got := make(chan string, 1)
	go func() {
		for k.lines.Scan() {
			if l := k.lines.Text(); strings.HasPrefix(l, "READY ") || strings.HasPrefix(l, "ERR ") {
				got <- l
				return
			}
		}
		got <- "EOF"
	}()
	select {
	case l := <-got:
		f := strings.Fields(l)
		if f[0] != "READY" || len(f) != 3 {
			t.Fatalf("kernel %s: %s", id, l)
		}
		k.socket = f[1]
		k.pid, _ = strconv.Atoi(f[2])
	case <-time.After(90 * time.Second):
		t.Fatalf("kernel %s never got its agent ready", id)
	}
	return k
}

// agentDead waits until nothing answers on the agent's socket.
func agentDead(socket string, within time.Duration) bool {
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		c, err := net.DialTimeout("unix", socket, 500*time.Millisecond)
		if err != nil {
			return true
		}
		_ = c.Close()
	}
	return false
}

// ENG-2088 end to end, through the real launch path: two "kernels" share a
// work root and each runs a sandboxed agent (scratch with an out/ file, socket
// directory). One is KILLED (no cleanup runs), as the S7 live run did ten
// times. A third kernel then boots over the work root: the killed kernel's
// directories and a pre-ENG-2088 leftover are gone; the live kernel's agent
// still answers and its directories are untouched; then the live kernel shuts
// down cleanly and leaves nothing.
func TestE2E_BootSweepsAKilledKernelsInstanceDirs(t *testing.T) {
	if testing.Short() {
		t.Skip("launches real subprocesses; skipped in -short")
	}
	root := e2eRoot(t)
	kernelDir := filepath.Join(root, "kernel")
	if err := os.MkdirAll(kernelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	workRoot := filepath.Join(root, "w")
	exe := probeExe(t, root)
	killed := startWRKernel(t, workRoot, kernelDir, exe, "victim")
	alive := startWRKernel(t, workRoot, kernelDir, exe, "survivor")
	// A leftover with no owner record at all (a kernel from before ENG-2088),
	// made after both kernels' own boot sweeps.
	legacy := mkInstanceDir(t, workRoot, "first-party/chat_agent-0badf00d", "out/old.txt")
	killedScratch, _ := filepath.Glob(filepath.Join(workRoot, "first-party", "victim-*"))
	aliveScratch, _ := filepath.Glob(filepath.Join(workRoot, "first-party", "survivor-*"))
	if len(killedScratch) != 1 || len(aliveScratch) != 1 {
		t.Fatalf("scratch dirs: victim %v survivor %v", killedScratch, aliveScratch)
	}
	if outs := countFiles(filepath.Join(killedScratch[0], AgentOutDir)); outs == 0 {
		t.Fatalf("the victim's agent left no out/ file to lose")
	}

	// Kill the victim kernel outright, then its agent (Windows: the Job
	// Object already did; Unix: an orphaned native-runner agent survives its
	// parent, and the S7 run saw its scratch, not its process, outlive it).
	if err := killed.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = killed.cmd.Wait()
	if p, err := os.FindProcess(killed.pid); err == nil {
		_ = p.Kill()
	}
	if !agentDead(killed.socket, 15*time.Second) {
		t.Fatal("the killed kernel's agent still answers")
	}
	if !exists(killedScratch[0]) || !exists(filepath.Dir(killed.socket)) {
		t.Fatal("the killed kernel's directories are already gone; nothing to sweep")
	}

	// A kernel boots over the shared work root.
	sb := NewAgentSandbox(AgentSandboxOptions{WorkRoot: workRoot, KernelDir: kernelDir})
	t.Cleanup(sb.ReleaseWorkRoot)
	rep, err := sb.OwnWorkRoot()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sweep: %+v", rep)
	for _, d := range []string{killedScratch[0], filepath.Dir(killed.socket), legacy} {
		if exists(d) {
			t.Errorf("%s survived the boot sweep", d)
		}
	}
	if rep.OutFiles < 2 || rep.DeadOwners != 1 || rep.LiveOwners != 1 || len(rep.Failed) != 0 {
		t.Errorf("sweep report %+v: want the victim's and the legacy out/ files, 1 dead and 1 live owner", rep)
	}
	for _, d := range []string{aliveScratch[0], filepath.Dir(alive.socket)} {
		if !exists(d) {
			t.Errorf("the live kernel's %s was swept", d)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := waitHealthy(ctx, alive.socket, 10*time.Second); err != nil {
		t.Errorf("the live kernel's agent stopped answering after the sweep: %v", err)
	}

	// The live kernel shuts down cleanly: its directories and records go.
	_ = alive.stdin.Close()
	bye := make(chan bool, 1)
	go func() {
		for alive.lines.Scan() {
			if alive.lines.Text() == "BYE" {
				bye <- true
				return
			}
		}
		bye <- false
	}()
	select {
	case ok := <-bye:
		if !ok {
			t.Fatal("the live kernel did not shut down cleanly")
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the live kernel did not shut down")
	}
	_ = alive.cmd.Wait()
	for _, d := range []string{aliveScratch[0], filepath.Dir(alive.socket)} {
		if exists(d) {
			t.Errorf("%s survived its kernel's clean shutdown", d)
		}
	}
	sb.ReleaseWorkRoot()
	if ents, _ := os.ReadDir(filepath.Join(workRoot, ownersDirName)); len(ents) != 0 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("owners/ not empty after every kernel left: %v", names)
	}
}
