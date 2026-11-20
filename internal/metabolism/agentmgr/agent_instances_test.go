package agentmgr

import (
	"os"
	"os/exec"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ENG-1634: ListAgentInstances reports each live instance with the daemon stream it
// serves and the OS pid of the process the kernel started — the pid an external
// SIGKILL needs (OperatorConsole/ListAgentInstances).
func TestListAgentInstances_ReportsStreamAndThePidOfTheRealProcess(t *testing.T) {
	m := NewAgentManager(nil, "", nil)
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperDaemonProcess$")
	cmd.Env = append(os.Environ(), "AGENTMGR_CHAOS_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	im := m.InstanceManager
	im.mu.Lock()
	im.instances["inst-daemon"] = &domain.Instance{ID: "inst-daemon", AgentID: "chat_agent", Mode: domain.ModeDaemon}
	im.cmds["inst-daemon"] = cmd
	im.instances["inst-jit"] = &domain.Instance{ID: "inst-jit", AgentID: "a_planner", Mode: domain.ModeJIT}
	im.mu.Unlock()
	m.daemons.mu.Lock()
	m.daemons.byStream["chat-0"] = &daemonState{refCount: 1, agentID: "chat_agent", instanceID: "inst-daemon", status: "running"}
	m.daemons.mu.Unlock()

	got := m.ListAgentInstances()
	if len(got) != 2 {
		t.Fatalf("instances = %+v", got)
	}
	if got[0].AgentID != "a_planner" || got[0].PID != 0 || got[0].StreamID != "" || got[0].Mode != domain.ModeJIT {
		t.Errorf("JIT instance without a process handle = %+v", got[0])
	}
	d := got[1]
	if d.AgentID != "chat_agent" || d.StreamID != "chat-0" || d.Mode != domain.ModeDaemon || d.PID != cmd.Process.Pid || d.PID == 0 {
		t.Fatalf("daemon instance = %+v, want stream chat-0 and pid %d", d, cmd.Process.Pid)
	}
}
