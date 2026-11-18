package agentmgr

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// ENG-2112 end to end through the REAL daemon path, with the readiness probe's
// abuse patterns (ENG-1995 phase B block 6): AgentManager.SpawnDaemon →
// bootDaemonAgent → admitLaunch (first-party memory cap) → sandbox.StartCmd
// (Windows: Job Object + AppContainer; Linux: bubblewrap/ulimit) → the agent
// serves its socket, then abuses — a fork bomb, a scratch fill, a memory
// balloon — and must be KILLED by its sandbox limit, the reason logged and
// carried into the ADR-0070 restart policy: restarted, and quarantined when it
// keeps doing it. The kernel (this test process) stays healthy: a well-behaved
// agent still boots and serves afterwards.

const abuseEnv = "AGENTMGR_ABUSE"

// runAbuseAgent is the abusive agent (TestMain, AGENTMGR_CHAOS_HELPER=abuse):
// it serves grpc.health.v1 on its socket so the boot succeeds, then abuses.
func runAbuseAgent() {
	var sock string
	for i, a := range os.Args {
		if a == "--socket" && i+1 < len(os.Args) {
			sock = os.Args[i+1]
		}
	}
	lis, err := net.Listen("unix", sock)
	if err != nil {
		os.Exit(3)
	}
	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go func() { _ = srv.Serve(lis) }()
	time.Sleep(300 * time.Millisecond)
	switch os.Getenv(abuseEnv) {
	case "fork": // 60 children, as the readiness probe did
		for i := 0; i < 60; i++ {
			c := exec.Command(os.Args[0])
			c.Env = append(os.Environ(), "AGENTMGR_CHAOS_HELPER=sleep")
			_ = c.Start()
			time.Sleep(10 * time.Millisecond)
		}
	case "fill": // up to 512 MiB into the scratch
		f, err := os.Create(filepath.Join(os.TempDir(), "fill.bin"))
		if err == nil {
			chunk := make([]byte, 1<<20)
			for i := 0; i < 512; i++ {
				if _, err := f.Write(chunk); err != nil {
					break
				}
				time.Sleep(2 * time.Millisecond)
			}
			_ = f.Close()
		}
	case "balloon": // up to 4 GiB, touched
		var keep [][]byte
		for i := 0; i < 256; i++ {
			b := make([]byte, 16<<20)
			for j := range b {
				b[j] = 1
			}
			keep = append(keep, b)
		}
		_ = keep
	}
	time.Sleep(2 * time.Minute) // survived: the test fails on its deadline
}

func TestE2E_SandboxLimitsKillTheAgentIntoTheRestartPolicy(t *testing.T) {
	if testing.Short() {
		t.Skip("launches real subprocesses; skipped in -short")
	}
	cases := []struct {
		abuse, reason string
		linuxReason   bool // the reason is asserted on Linux too
		memMB         int  // the first-party memory cap for this case
	}{
		// The fork and fill cases get a cap the tree cannot reach first (a
		// -race test binary is ~40 MiB committed per child on Windows).
		{"fork", "processes: ", true, 4096},
		{"fill", "scratch_quota: ", true, 4096},
		// Linux: the per-process data limit fails the allocation and the Go
		// runtime aborts before the tree total is reached — killed, restarted
		// and quarantined all the same, but as a crash without a limit reason.
		{"balloon", "memory: ", false, 256},
	}
	for _, tc := range cases {
		t.Run(tc.abuse, func(t *testing.T) {
			if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
				t.Skip("no process-limit killer on " + runtime.GOOS)
			}
			root := e2eRoot(t)
			sb := NewAgentSandbox(AgentSandboxOptions{
				WorkRoot: filepath.Join(root, "w"), KernelDir: filepath.Join(root, "kernel"),
				FirstPartyLimits: AgentTierLimits{MaxProcesses: 8, MaxTasks: 512, ScratchMB: 32},
			})
			sb.policy.LimitPoll = 100 * time.Millisecond
			if rep := planAgent(sb); rep.Refused {
				t.Skipf("the first-party agent sandbox cannot run here: %s", rep.Summary())
			}
			t.Logf("containment: %s", planAgent(sb).Summary())
			exe := probeExe(t, root)
			reg := newTestRegistry()
			reg.agents["abuser"] = domain.AgentDefinition{ID: "abuser", Runtime: domain.RuntimeBinary, ExecPath: exe,
				Dir: filepath.Dir(exe), TrustTier: "first-party"}
			reg.agents["probe"] = domain.AgentDefinition{ID: "probe", Runtime: domain.RuntimeBinary, ExecPath: exe,
				Dir: filepath.Dir(exe), TrustTier: "first-party"}
			m := NewAgentManager(reg, "127.0.0.1:1", nil)
			m.SetAgentContainment(AgentContainment{Sandbox: sb, FirstPartyMemoryMB: tc.memMB})
			m.SetAgentTransport(AgentTransport{KernelSocket: listenKernelSocketForTest})
			m.SetEnvExtras([]string{"AGENTMGR_CHAOS_HELPER=abuse", abuseEnv + "=" + tc.abuse})
			bus := &eventBus{}
			m.EventBus = bus
			// Two restarts in the window, then quarantine (ADR-0070 D2).
			m.RestartPolicy = NewDaemonRestartPolicy(2, time.Minute, 10*time.Millisecond, 50*time.Millisecond)
			defer func() { _ = m.KillAllAgents(context.Background()) }()
			defer m.StopAllDaemons()

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			first, err := m.SpawnDaemon(ctx, "abuser", "stream-"+tc.abuse, nil)
			if err != nil {
				t.Fatalf("spawn: %v", err)
			}
			reason := ""
			deadline := time.Now().Add(2 * time.Minute)
			for len(bus.quarantined()) == 0 {
				if time.Now().After(deadline) {
					t.Fatalf("the %s agent was not killed into quarantine (status %q, first instance limit %q)",
						tc.abuse, m.GetDaemonStatus("stream-"+tc.abuse), m.limitKillOf(first))
				}
				if r := m.limitKillOf(first); r != "" && reason == "" {
					reason = r
				}
				time.Sleep(50 * time.Millisecond)
			}
			q := bus.quarantined()[0]
			t.Logf("first instance limit: %q; quarantine reason: %q", reason, q.Reason)
			if crashes := bus.crashed(); crashes < 3 {
				t.Errorf("crashes = %d, want >= 3 (the first instance and its two restarts, each killed)", crashes)
			}
			if runtime.GOOS == "windows" || tc.linuxReason {
				if !strings.HasPrefix(q.Reason, "sandbox limit: "+tc.reason) {
					t.Errorf("quarantine reason = %q, want %q", q.Reason, "sandbox limit: "+tc.reason+"…")
				}
			}

			// The kernel is healthy: a well-behaved agent boots and serves.
			m.SetEnvExtras([]string{"AGENTMGR_CHAOS_HELPER=probe"})
			def, _ := reg.GetAgentByName(ctx, "probe")
			inst, err := m.GetOrBootInstance(ctx, def, "")
			if err != nil {
				t.Fatalf("a well-behaved agent no longer boots: %v", err)
			}
			if err := waitHealthy(ctx, inst.SocketPath, 10*time.Second); err != nil {
				t.Fatalf("a well-behaved agent does not serve: %v", err)
			}
		})
	}
}
