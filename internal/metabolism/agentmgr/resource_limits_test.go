package agentmgr

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/sandbox"
)

// ENG-2112 (owner decision 2026-11-18): sandbox resource limits for every agent
// tier — a memory cap for first-party agents too, a process count and a
// scratch quota — and exceeding one kills the instance into the existing
// restart / quarantine policy with a clear reason.

// With the agent sandbox on, a first-party (or system) agent gets the
// first-party tier cap; a smaller SEC-01 limit still applies, a larger manifest
// limit cannot lift it. Without the sandbox the launch is unchanged.
func TestAdmitLaunch_FirstPartyMemoryCapUnderTheSandbox(t *testing.T) {
	sb, _ := testAgentSandbox(t, nil)
	im := withPython(NewInstanceManager(""), "python")
	im.SetManifestResolver(func(_ context.Context, id string) *domain.AgentManifest {
		return &domain.AgentManifest{MemoryLimitMB: map[string]int{"small": 300, "huge": 99999}[id]}
	})
	first := &domain.AgentDefinition{ID: "worker", Runtime: domain.RuntimePython, TrustTier: "first-party"}

	// Unsandboxed: the SEC-01 behaviour exactly (no global default ⇒ uncapped).
	if mb, err := im.admitLaunch(context.Background(), first); err != nil || mb != 0 {
		t.Fatalf("unsandboxed first-party: mb=%d err=%v, want 0 (unchanged)", mb, err)
	}
	im.SetAgentContainment(AgentContainment{Sandbox: sb, FirstPartyMemoryMB: 4096})
	if mb, _ := im.admitLaunch(context.Background(), first); mb != 4096 {
		t.Fatalf("sandboxed first-party: %d, want the tier cap 4096", mb)
	}
	// A system organ was never capped by the fleet-wide default; the tier cap
	// holds it now.
	organ := &domain.AgentDefinition{ID: "reranker_agent", Runtime: domain.RuntimePython}
	if !domain.IsSystemAgent(organ.ID) {
		t.Fatalf("%s is not a system agent id", organ.ID)
	}
	if mb, _ := im.admitLaunch(context.Background(), organ); mb != 4096 {
		t.Fatalf("system organ: %d, want the first-party tier cap 4096", mb)
	}
	small := &domain.AgentDefinition{ID: "small", Runtime: domain.RuntimePython}
	if mb, _ := im.admitLaunch(context.Background(), small); mb != 300 {
		t.Fatalf("smaller manifest limit: %d, want 300", mb)
	}
	huge := &domain.AgentDefinition{ID: "huge", Runtime: domain.RuntimePython}
	if mb, _ := im.admitLaunch(context.Background(), huge); mb != 4096 {
		t.Fatalf("a larger manifest limit lifted the tier cap: %d, want 4096", mb)
	}
	// The zero value is the documented default, never "uncapped".
	im.SetAgentContainment(AgentContainment{Sandbox: sb})
	if mb, _ := im.admitLaunch(context.Background(), first); mb != 8192 {
		t.Fatalf("zero-value first-party cap: %d, want 8192", mb)
	}
}

// The tiers' process, task and scratch limits reach the sandbox policy, so the
// plan wants (and reports) them.
func TestNewAgentSandbox_TierLimitsReachThePolicy(t *testing.T) {
	sb, _ := testAgentSandbox(t, func(o *AgentSandboxOptions) {
		o.FirstPartyLimits = AgentTierLimits{MaxProcesses: 32, MaxTasks: 1024, ScratchMB: 4096}
		o.ThirdPartyLimits = AgentTierLimits{MaxProcesses: 16, MaxTasks: 512, ScratchMB: 1024}
	})
	fp, tp := sb.Policy().Tiers[sandbox.TierFirstParty], sb.Policy().Tiers[sandbox.TierThirdParty]
	if fp.MaxProcesses != 32 || fp.MaxTasks != 1024 || fp.ScratchMB != 4096 {
		t.Fatalf("first-party limits: %+v", fp)
	}
	if tp.MaxProcesses != 16 || tp.MaxTasks != 512 || tp.ScratchMB != 1024 {
		t.Fatalf("third-party limits: %+v", tp)
	}
	rep := planAgent(sb)
	all := append(controlNames(rep.Enforced), rep.Missing...)
	for _, c := range []sandbox.Control{sandbox.ControlProcesses, sandbox.ControlScratch} {
		found := false
		for _, s := range all {
			if strings.HasPrefix(s, string(c)) {
				found = true
			}
		}
		if !found {
			t.Errorf("the report names neither an enforced nor a missing %s: %s", c, rep.Summary())
		}
	}
}

// eventBus records the events a test's manager publishes.
type eventBus struct {
	mu     sync.Mutex
	events []domain.DomainEvent
}

func (b *eventBus) Publish(_ context.Context, e domain.DomainEvent) error {
	b.mu.Lock()
	b.events = append(b.events, e)
	b.mu.Unlock()
	return nil
}

func (b *eventBus) quarantined() []domain.DaemonQuarantinedEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []domain.DaemonQuarantinedEvent
	for _, e := range b.events {
		if q, ok := e.(domain.DaemonQuarantinedEvent); ok {
			out = append(out, q)
		}
	}
	return out
}

func (b *eventBus) crashed() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, e := range b.events {
		if _, ok := e.(domain.DaemonCrashedEvent); ok {
			n++
		}
	}
	return n
}

// A limit kill noted by the sandboxed cleanup lands on the instance's exit,
// and the daemon restart policy carries it as the reason.
func TestLimitKill_ReachesTheExitAndTheRestartReason(t *testing.T) {
	m := newTestManager()
	bus := &eventBus{}
	m.EventBus = bus
	m.RestartPolicy = NewDaemonRestartPolicy(0, time.Minute, time.Millisecond, time.Millisecond) // quarantine at once
	defer m.StopAllDaemons()

	cmd := exec.Command(os.Args[0], "-test.run=^$") // exits at once
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	const reason = "scratch_quota: the instance's scratch reached 9 MiB (limit 8 MiB)"
	pe := m.watchExitThen("abuser", "inst-1", cmd, func() { m.noteLimitKill("inst-1", reason) })
	<-pe.done
	if got := m.limitKillOf("inst-1"); got != reason {
		t.Fatalf("limitKillOf = %q, want %q", got, reason)
	}
	if len(m.limitKills) != 0 {
		t.Fatalf("the hand-off map kept an entry: %v", m.limitKills)
	}
	m.handleDaemonExit(&domain.Instance{AgentID: "abuser", ID: "inst-1"}, "stream-a", true)
	q := bus.quarantined()
	if len(q) != 1 || q[0].Reason != "sandbox limit: "+reason {
		t.Fatalf("quarantine events = %+v, want one with the limit as its reason", q)
	}
	// An ordinary crash keeps the old reason.
	m.handleDaemonExit(&domain.Instance{AgentID: "other", ID: "inst-2"}, "stream-b", true)
	if q := bus.quarantined(); len(q) != 2 || q[1].Reason != "crash-loop" {
		t.Fatalf("plain crash reason: %+v", q)
	}
}
