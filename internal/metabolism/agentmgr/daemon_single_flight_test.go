package agentmgr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ENG-353: one daemon binding must yield one process. SpawnDaemon used to check
// "running?" under the lock, release it for the multi-second boot, and only then
// register — so two callers on the same stream (the Telegram plugin restoring its
// bot at boot while a watch registration, or a pending auto-restart, spawns the
// same stream) both booted a process: two pollers on one bot token.

// gatedBootHook counts boots and holds every boot until the gate is closed, so a
// test can line concurrent callers up inside the boot window deterministically.
type gatedBootHook struct {
	gate    chan struct{}
	entered chan struct{} // receives once per boot that reached the hook
	boots   atomic.Int32
	failN   int32 // boots numbered <= failN fail
}

func newGatedBootHook() *gatedBootHook {
	return &gatedBootHook{gate: make(chan struct{}), entered: make(chan struct{}, 64)}
}

func (h *gatedBootHook) hook(agentID, streamID string, _ map[string]any) (string, *exec.Cmd, error) {
	n := h.boots.Add(1)
	h.entered <- struct{}{}
	<-h.gate
	if n <= h.failN {
		return "", nil, errors.New("boot failed")
	}
	return fmt.Sprintf("inst-%d", n), nil, nil
}

func TestSpawnDaemon_ConcurrentCallersOnOneStreamBootOneProcess(t *testing.T) {
	m := newTestManager()
	h := newGatedBootHook()
	m.DaemonBootHook = h.hook

	const callers = 8
	ids := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = m.SpawnDaemon(context.Background(),
				"telegram_ingress_agent", "telegram_ingress_agent", nil)
		}(i)
	}
	<-h.entered // one boot is in flight; the others must be waiting on it
	// Give the other callers time to reach SpawnDaemon. Before the fix each of
	// them would enter the hook here too.
	time.Sleep(50 * time.Millisecond)
	close(h.gate)
	wg.Wait()

	if got := h.boots.Load(); got != 1 {
		t.Fatalf("one stream, %d concurrent spawns: want 1 process booted, got %d", callers, got)
	}
	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if ids[i] != "inst-1" {
			t.Fatalf("caller %d got instance %q; every caller must share the one process", i, ids[i])
		}
	}
	if got := m.DaemonRefCount("telegram_ingress_agent"); got != callers {
		t.Fatalf("want ref-count %d (one per caller), got %d", callers, got)
	}
}

// A failed boot must release its waiters (no hang) and let one of them boot.
func TestSpawnDaemon_FailedBootReleasesWaiters(t *testing.T) {
	m := newTestManager()
	h := newGatedBootHook()
	h.failN = 1
	m.DaemonBootHook = h.hook

	errs := make(chan error, 2)
	go func() {
		_, err := m.SpawnDaemon(context.Background(), "a", "s", nil)
		errs <- err
	}()
	<-h.entered
	go func() {
		_, err := m.SpawnDaemon(context.Background(), "a", "s", nil)
		errs <- err
	}()
	time.Sleep(50 * time.Millisecond)
	close(h.gate)

	var failed, ok int
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				failed++
			} else {
				ok++
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a waiter hung after the boot it waited on failed")
		}
	}
	if failed != 1 || ok != 1 {
		t.Fatalf("want one failed and one successful spawn, got failed=%d ok=%d", failed, ok)
	}
	if got := h.boots.Load(); got != 2 {
		t.Fatalf("want 2 boot attempts (failed, then the waiter's own), got %d", got)
	}
	if got := m.DaemonRefCount("s"); got != 1 {
		t.Fatalf("want ref-count 1, got %d", got)
	}
}

// A waiter honours its own ctx: it is not stuck behind a slow boot.
func TestSpawnDaemon_WaiterHonoursItsContext(t *testing.T) {
	m := newTestManager()
	h := newGatedBootHook()
	m.DaemonBootHook = h.hook
	defer close(h.gate)

	go func() { _, _ = m.SpawnDaemon(context.Background(), "a", "s", nil) }()
	<-h.entered

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := m.SpawnDaemon(ctx, "a", "s", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the waiter's deadline, got %v", err)
	}
	if got := h.boots.Load(); got != 1 {
		t.Fatalf("the cancelled waiter must not boot: got %d boots", got)
	}
}

// LIVE (real subprocesses): a crashed daemon's auto-restart is mid-boot when the
// binding is started again (the operator toggling the bot, a watch registering
// the stream). The restart must not leave two processes behind — the second
// caller takes a reference on the restarted process.
func TestSpawnDaemon_RestartInFlightDoesNotAddASecondProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("launches real subprocesses; skipped in -short")
	}
	var mu sync.Mutex
	var procs []*exec.Cmd
	var boots int
	restartGate := make(chan struct{})
	restartEntered := make(chan struct{}, 1)

	m := withPythonM(NewAgentManager(nil, "", nil), "")
	m.RestartPolicy = NewDaemonRestartPolicy(5, time.Minute, 10*time.Millisecond, 10*time.Millisecond)
	m.DaemonBootHook = func(agentID, streamID string, _ map[string]any) (string, *exec.Cmd, error) {
		mu.Lock()
		boots++
		n := boots
		mu.Unlock()
		if n == 2 { // the auto-restart: hold it inside the boot window
			restartEntered <- struct{}{}
			<-restartGate
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperDaemonProcess$")
		cmd.Env = append(os.Environ(), "AGENTMGR_CHAOS_HELPER=1")
		if err := cmd.Start(); err != nil {
			return "", nil, err
		}
		mu.Lock()
		procs = append(procs, cmd)
		mu.Unlock()
		return fmt.Sprintf("inst-%d", n), cmd, nil
	}
	defer func() {
		// Shut the restart loop down first and mark the live stream's exit as
		// expected: killing the restarted helper otherwise reads as a crash and
		// schedules a third boot after this test has returned — a stray process
		// that outlives the test binary's run.
		m.StopAllDaemons()
		mu.Lock()
		defer mu.Unlock()
		for _, p := range procs {
			_ = p.Process.Kill()
		}
	}()
	getBoots := func() int { mu.Lock(); defer mu.Unlock(); return boots }

	const stream = "telegram_ingress_agent"
	if _, err := m.SpawnDaemon(context.Background(), stream, stream, nil); err != nil {
		t.Fatalf("SpawnDaemon: %v", err)
	}
	mu.Lock()
	first := procs[0]
	mu.Unlock()
	_ = first.Process.Kill() // crash → auto-restart (boot #2), held at the gate
	select {
	case <-restartEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("auto-restart never started")
	}

	// The binding is started again while the restart is still booting.
	done := make(chan error, 1)
	go func() {
		_, err := m.SpawnDaemon(context.Background(), stream, stream, nil)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	close(restartGate)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second start: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second start hung behind the restart")
	}

	if got := getBoots(); got != 2 {
		t.Fatalf("want 2 boots (original + one restart), got %d — a second live process", got)
	}
	if got := m.DaemonRefCount(stream); got != 2 {
		t.Fatalf("want the restarted process to carry both references, got ref-count %d", got)
	}
	if id, ok := m.DaemonInstanceID(stream); !ok || id != "inst-2" {
		t.Fatalf("want the stream on the restarted instance inst-2, got %q (%v)", id, ok)
	}
}
