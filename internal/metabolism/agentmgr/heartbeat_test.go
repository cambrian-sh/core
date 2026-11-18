package agentmgr

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/cambrian-sh/core/domain"
)

// fakeClock is a settable clock for the heartbeat tracker.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func heartbeatManager(interval time.Duration, missed int) (*AgentManager, *fakeClock) {
	m := NewAgentManager(nil, "", nil)
	m.ConfigureHeartbeat(interval, missed)
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	m.heartbeats.now = clk.now
	return m, clk
}

func registerDaemon(m *AgentManager, streamID, agentID, instanceID string) {
	m.daemons.mu.Lock()
	m.daemons.byStream[streamID] = &daemonState{refCount: 1, agentID: agentID, instanceID: instanceID, status: "running"}
	m.daemons.mu.Unlock()
}

// A beat is attributed by the token-stamped instance, or — without one — by a stream
// that belongs to the sender. Anything else is dropped, and liveness off drops all.
func TestHeartbeat_Attribution(t *testing.T) {
	m, _ := heartbeatManager(time.Second, 3)
	registerDaemon(m, "chat-chat_agent-0", "chat_agent", "i1")

	if !m.RecordDaemonHeartbeat("i1", "", "chat_agent", false) {
		t.Fatal("a beat from the registered instance was not attributed")
	}
	if m.RecordDaemonHeartbeat("i-unknown", "chat-chat_agent-0", "chat_agent", false) {
		t.Fatal("a beat naming an unregistered instance was attributed (the token is authoritative)")
	}
	if !m.RecordDaemonHeartbeat("", "chat-chat_agent-0", "chat_agent", false) {
		t.Fatal("an unauthenticated beat for the sender's own stream was not attributed")
	}
	if m.RecordDaemonHeartbeat("", "chat-chat_agent-0", "someone_else", false) {
		t.Fatal("a beat for another agent's stream was attributed")
	}

	off, _ := heartbeatManager(0, 3)
	registerDaemon(off, "s", "a", "i1")
	if off.RecordDaemonHeartbeat("i1", "", "a", false) {
		t.Fatal("liveness off must ignore beats")
	}
}

// DaemonServing: untracked until the first beat; idle only after a not-busy beat later
// than the time asked about.
func TestHeartbeat_DaemonServing(t *testing.T) {
	m, clk := heartbeatManager(time.Second, 3)
	registerDaemon(m, "s", "a", "i1")
	if tracked, _ := m.DaemonServing("s", clk.now()); tracked {
		t.Fatal("tracked before any beat")
	}
	m.RecordDaemonHeartbeat("i1", "", "a", true)
	abandoned := clk.now()
	if tracked, idle := m.DaemonServing("s", abandoned); !tracked || idle {
		t.Fatalf("busy worker: tracked=%v idle=%v, want true/false", tracked, idle)
	}
	clk.add(time.Second)
	m.RecordDaemonHeartbeat("i1", "", "a", false)
	if _, idle := m.DaemonServing("s", abandoned); !idle {
		t.Fatal("a not-busy beat after the abandonment did not read as idle")
	}
}

// helperDaemon boots the chaos helper process (sleeps until killed) through
// DaemonBootHook and registers it with the InstanceManager like the real boot path
// does, so EvictInstance can kill it.
func helperDaemon(t *testing.T, m *AgentManager) (boots func() int) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	var procs []*exec.Cmd
	m.DaemonBootHook = func(agentID, streamID string, _ map[string]any) (string, *exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperDaemonProcess$")
		cmd.Env = append(os.Environ(), "AGENTMGR_CHAOS_HELPER=1")
		if err := cmd.Start(); err != nil {
			return "", nil, err
		}
		mu.Lock()
		n++
		id := "hb-inst-" + string(rune('0'+n))
		procs = append(procs, cmd)
		mu.Unlock()
		inst := domain.NewInstance(agentID)
		inst.ID = id
		inst.Mode = domain.ModeDaemon
		m.InstanceManager.mu.Lock()
		m.InstanceManager.instances[id] = inst
		m.InstanceManager.cmds[id] = cmd
		m.InstanceManager.mu.Unlock()
		return id, cmd, nil
	}
	t.Cleanup(func() {
		m.StopAllDaemons()
		mu.Lock()
		defer mu.Unlock()
		for _, p := range procs {
			_ = p.Process.Kill()
		}
	})
	return func() int { mu.Lock(); defer mu.Unlock(); return n }
}

// LIVE: an instance that beat and then went silent past missed × interval is killed and
// the restart policy brings up a fresh one; the operator feed sees the crash and the
// recovery. An instance that never beat (an agent on an older SDK) is never touched.
func TestHeartbeat_SilentInstanceIsKilledAndRestarted(t *testing.T) {
	if testing.Short() {
		t.Skip("launches real subprocesses")
	}
	m, clk := heartbeatManager(time.Second, 3)
	m.RestartPolicy = NewDaemonRestartPolicy(5, time.Minute, 5*time.Millisecond, 10*time.Millisecond)
	bus := newCountingBus()
	m.EventBus = bus
	boots := helperDaemon(t, m)

	ctx := context.Background()
	first, err := m.SpawnDaemon(ctx, "chat_agent", "chat-chat_agent-0", nil)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := m.SpawnDaemon(ctx, "old_sdk_agent", "old-stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !m.RecordDaemonHeartbeat(first, "", "chat_agent", false) {
		t.Fatal("first beat not attributed")
	}

	// Within budget: nothing happens.
	clk.add(2 * time.Second)
	m.reclaimSilentDaemons()
	if id, _ := m.DaemonInstanceID("chat-chat_agent-0"); id != first {
		t.Fatal("an instance within its beat budget was reclaimed")
	}

	// Past 3 missed beats: killed, restarted as a new instance.
	clk.add(2 * time.Second)
	m.reclaimSilentDaemons()
	waitUntil(t, "restart after missed heartbeats", func() bool {
		id, ok := m.DaemonInstanceID("chat-chat_agent-0")
		return ok && id != first
	})
	waitUntil(t, "recovery event", func() bool { return bus.count(domain.EventTypeDaemonRecovered) >= 1 })
	if bus.count(domain.EventTypeDaemonCrashed) < 1 {
		t.Fatal("the reclaim was not reported as a daemon crash")
	}
	if got := m.GetDaemonStatus("chat-chat_agent-0"); got != "running" {
		t.Fatalf("status after restart = %q, want running", got)
	}
	if _, ok := m.InstanceManager.GetInstanceByID(first); ok {
		t.Fatal("the hung instance is still registered")
	}
	// The replacement is not tracked until IT beats.
	if tracked, _ := m.DaemonServing("chat-chat_agent-0", time.Time{}); tracked {
		t.Fatal("the replacement inherited the old instance's tracking")
	}

	// The never-beating legacy daemon survived all of it.
	clk.add(time.Hour)
	m.reclaimSilentDaemons()
	time.Sleep(50 * time.Millisecond)
	if id, ok := m.DaemonInstanceID("old-stream"); !ok || id != legacy {
		t.Fatal("a daemon that never sent a heartbeat was killed")
	}
	// Exactly two boots for the chat stream (first + restart) plus the legacy one: the
	// crash watcher did not schedule a second restart for the expected kill.
	time.Sleep(100 * time.Millisecond)
	if got := boots(); got != 3 {
		t.Fatalf("boots = %d, want 3 (no double restart)", got)
	}
}

// The watcher goroutine starts only when liveness is on and stops cleanly.
func TestHeartbeat_WatchLifecycle(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	off, _ := heartbeatManager(0, 3)
	off.StartHeartbeatWatch(context.Background())
	off.StopHeartbeatWatch()

	m, _ := heartbeatManager(20*time.Millisecond, 3)
	m.StartHeartbeatWatch(context.Background())
	m.StartHeartbeatWatch(context.Background()) // idempotent
	time.Sleep(50 * time.Millisecond)
	m.StopHeartbeatWatch()
	m.StopHeartbeatWatch()
}

// The heartbeat flag rides every long-lived launch (any trait — the chat pool worker
// is cognitive) and only when liveness is on.
func TestBuildAgentCmd_HeartbeatFlag(t *testing.T) {
	im := withPython(NewInstanceManager("localhost:50051"), "python")
	cognitive := &domain.AgentDefinition{ID: "chat_agent", Runtime: domain.RuntimePython, ExecPath: "chat_agent.py", Dir: "/agents"}
	daemon := &domain.AgentDefinition{ID: "tg", Runtime: domain.RuntimePython, ExecPath: "tg.py", Dir: "/agents", Trait: domain.TraitDaemon}

	cmd, _ := im.buildAgentCmd(cognitive, &domain.Instance{ID: "i"}, "chat-chat_agent-0")
	if slices.Contains(cmd.Args, "--heartbeat-interval-ms") {
		t.Fatalf("flag passed with liveness off: %v", cmd.Args)
	}

	im.heartbeatInterval = 5 * time.Second
	cmd, _ = im.buildAgentCmd(cognitive, &domain.Instance{ID: "i"}, "chat-chat_agent-0")
	if i := slices.Index(cmd.Args, "--heartbeat-interval-ms"); i < 0 || cmd.Args[i+1] != "5000" {
		t.Fatalf("pool worker launch lacks --heartbeat-interval-ms 5000: %v", cmd.Args)
	}
	if i := slices.Index(cmd.Args, "--stream-id"); i < 0 || cmd.Args[i+1] != "chat-chat_agent-0" {
		t.Fatalf("pool worker launch lacks its stream id: %v", cmd.Args)
	}
	cmd, _ = im.buildAgentCmd(daemon, &domain.Instance{ID: "i"}, "tg-stream")
	if n := countOf(cmd.Args, "--stream-id"); n != 1 {
		t.Fatalf("daemon launch has %d --stream-id flags, want 1: %v", n, cmd.Args)
	}
	cmd, _ = im.buildAgentCmd(cognitive, &domain.Instance{ID: "i"}, "")
	if slices.Contains(cmd.Args, "--heartbeat-interval-ms") {
		t.Fatalf("a short-lived (non-daemon) launch was asked to heartbeat: %v", cmd.Args)
	}
}

func countOf(xs []string, x string) int {
	n := 0
	for _, v := range xs {
		if v == x {
			n++
		}
	}
	return n
}
