package agentmgr

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/mapper"
	"github.com/cambrian-sh/core/internal/metabolism/compose"
	"github.com/cambrian-sh/core/internal/metabolism/dispatch"
	"github.com/cambrian-sh/core/internal/sandbox"
	"github.com/cambrian-sh/core/internal/storage"
)

// ENG-2083 end to end (owner decision 2026-11-18, option a): a third-party
// profile's work runs in a SEPARATE composed worker launched at the
// third-party tier, never in the shared system-tier composed_worker.
//
// The REAL chain from the profile files to the launched process: agents/profiles
// → storage.DiscoverFilesystemAgentsWith (both composed runtimes registered) →
// bbolt (UpsertDiscoveredAgent) → the registry reads the kernel makes (agent
// record + manifest through mapper, as kernel.AgentRepoDecorator does) →
//   - plan step: dispatch.Dispatcher.Execute with the gatekeeper's hard-pin
//     slate (the pinned profession alone) → compose.Composer.Compose →
//     the dispatcher's AgentCaller;
//   - single-agent lane: compose.Composer.ComposeRequest → the runtime id the
//     lane calls — the shared one, the third-party profession left out of the
//     request (ENG-2096);
//
// → InstanceManager.GetOrBootInstance under the default execution.agents.sandbox
// → sandbox.StartCmd. The caller is the AgentManager's lookup + boot (resolve the
// definition by id, boot its instance); the process is the probe agent
// (TestMain) in place of worker_agent.py --composed, which needs a live kernel —
// the definition, its tier and its launch are the registered runtime's.

const composedTierProfile = `name: %s
description: Does %s things.
role: "You do things."
capabilities:
  - analysis
skills:
  - s1
tools:
  - mcp:filesystem/read_*
trust_tier: %s
`

// composedRegistry is the registry the kernel's composer and agent manager read,
// over a bbolt store filled by discovery.
type composedRegistry struct {
	store *storage.BBoltAdapter
	am    *mapper.AgentMapper
}

func (r composedRegistry) GetAgentByName(_ context.Context, name string) (*domain.AgentDefinition, error) {
	rec, err := r.store.GetAgentRecord(name)
	if err != nil {
		return nil, err
	}
	d := r.am.ToDomain(*rec)
	return &d, nil
}

func (r composedRegistry) GetManifest(_ context.Context, id string) (*domain.AgentManifest, error) {
	mr, err := r.store.GetManifestRecord(id)
	if err != nil || mr == nil {
		return nil, err
	}
	m := r.am.ManifestToDomain(*mr)
	return &m, nil
}

func (r composedRegistry) GetAllAgents(_ context.Context) ([]domain.AgentDefinition, error) {
	recs, err := r.store.GetAllAgentRecords()
	if err != nil {
		return nil, err
	}
	out := make([]domain.AgentDefinition, 0, len(recs))
	for _, rec := range recs {
		out = append(out, r.am.ToDomain(rec))
	}
	return out, nil
}

// pinGatekeeper is the gatekeeper's answer to a hard-pinned step: the pinned
// agent is the whole slate.
type pinGatekeeper struct{ pinned domain.AgentDefinition }

func (g pinGatekeeper) FindCandidates(context.Context, *domain.DispatchTask) ([]domain.ScoredCandidate, error) {
	return []domain.ScoredCandidate{{Agent: g.pinned, Score: 1}}, nil
}
func (g pinGatekeeper) FindModelCandidates(context.Context, []string) ([]domain.ScoredCandidate, error) {
	return nil, nil
}

// bootingCaller resolves the called agent from the registry and boots it, as
// the AgentManager does, recording each agent's probe report.
type bootingCaller struct {
	t       *testing.T
	root    string
	reg     composedRegistry
	im      *InstanceManager
	mu      sync.Mutex
	reports map[string]*probeReport
	insts   []*domain.Instance
}

func (c *bootingCaller) CallAgent(ctx context.Context, agentID string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
	def, err := c.reg.GetAgentByName(ctx, agentID)
	if err != nil {
		return nil, err
	}
	exe := probeExe(c.t, c.root)
	def.Runtime, def.ExecPath, def.Dir = domain.RuntimeBinary, exe, filepath.Dir(exe)
	inst, err := c.im.GetOrBootInstance(ctx, def, "")
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.insts = append(c.insts, inst)
	c.mu.Unlock()
	if err := waitHealthy(ctx, inst.SocketPath, 15*time.Second); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(inst.SocketPath), "probe.json"))
	if err != nil {
		return nil, err
	}
	c.t.Logf("%s probe: %s", agentID, raw)
	var rep probeReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.reports[agentID] = &rep
	c.mu.Unlock()
	return &domain.Handoff{FromAgent: agentID, Payload: &domain.Payload{Type: "text", Data: []byte("ok")}, Context: map[string]string{}}, nil
}

func (c *bootingCaller) stop() {
	for _, inst := range c.insts {
		c.im.mu.Lock()
		pe := c.im.exits[inst.ID]
		c.im.mu.Unlock()
		c.im.EvictInstance(inst.ID)
		if pe != nil {
			select {
			case <-pe.done:
			case <-time.After(30 * time.Second):
				c.t.Error("an evicted runtime did not exit")
			}
		}
	}
}

// teeLog captures slog output for the duration of the test, still writing it to
// stderr.
func teeLog(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(buf, os.Stderr), nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestE2E_ThirdPartyProfileRunsInAThirdPartyComposedWorker(t *testing.T) {
	if testing.Short() {
		t.Skip("launches real subprocesses; skipped in -short")
	}
	logs := teeLog(t)
	root := e2eRoot(t)
	agents := filepath.Join(root, "agents")
	if err := os.MkdirAll(filepath.Join(agents, storage.ProfilesDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agents, storage.ProfileRuntimeFile), []byte("# runtime\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, tier := range map[string]string{"outsider": "third-party", "insider": "first-party"} {
		text := strings.NewReplacer("name: %s", "name: "+name, "Does %s", "Does "+name, "trust_tier: %s", "trust_tier: "+tier).Replace(composedTierProfile)
		if err := os.WriteFile(filepath.Join(agents, storage.ProfilesDirName, name+".yaml"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	workers := config.DefaultConfig().Execution.Workers
	if !workers.Composed {
		t.Fatal("composed workers are on by default (ADR-0150)")
	}
	found, err := storage.DiscoverFilesystemAgentsWith(agents, domain.IsSystemAgent, storage.DiscoverOptions{
		ComposedRuntimeID: workers.RuntimeID(), ComposedThirdPartyRuntimeID: workers.ThirdPartyRuntimeID()})
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewBBoltAdapterNoScan(filepath.Join(root, "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, da := range found {
		if err := store.UpsertDiscoveredAgent(da); err != nil {
			t.Fatal(err)
		}
	}
	reg := composedRegistry{store: store, am: mapper.NewAgentMapper()}

	sbc := config.DefaultConfig().Execution.Agents.Sandbox
	sb := NewAgentSandbox(AgentSandboxOptions{
		WorkRoot: filepath.Join(root, "w"), KernelDir: filepath.Join(root, "kernel"),
		KernelStatePaths: []string{filepath.Join(root, "data")},
		FirstPartyRefuse: sbc.FirstParty.Refuses(), ThirdPartyRefuse: sbc.ThirdParty.Refuses(),
		KernelOnlyAll: sbc.KernelOnlyAll(),
	})
	targets := localTargets(t)
	if os.Getenv("AGENTMGR_E2E_OFFLINE") == "" {
		targets = append(targets, "1.1.1.1:443")
	}
	im := NewInstanceManager("127.0.0.1:1")
	im.SetAgentContainment(AgentContainment{Sandbox: sb})
	im.SetAgentTransport(AgentTransport{KernelSocket: listenKernelSocketForTest})
	im.SetEnvExtras([]string{"AGENTMGR_CHAOS_HELPER=probe", "AGENTMGR_PROBE_DIAL=" + strings.Join(targets, ",")})
	caller := &bootingCaller{t: t, root: root, reg: reg, im: im, reports: map[string]*probeReport{}}
	t.Cleanup(caller.stop)

	comp := &compose.Composer{RuntimeID: workers.RuntimeID(), ThirdPartyRuntimeID: workers.ThirdPartyRuntimeID(),
		Agents: reg, Manifests: reg, Fleet: reg, Leases: domain.NewComposedLeases(), Overlay: domain.NewRunGrantOverlay()}
	plan := sandbox.Plan(context.Background(), sandbox.Spec{ServerID: "plan", Tier: sandbox.TierThirdParty,
		Policy: sb.Policy(), Agent: &sandbox.AgentSpec{}}, sandbox.Command{Path: "x"})
	t.Logf("third-party plan here: %s", plan.Summary())

	pinnedStep := func(profession, lease string) (*domain.DispatchResult, error) {
		pinned, err := reg.GetAgentByName(context.Background(), profession)
		if err != nil {
			t.Fatal(err)
		}
		d := &dispatch.Dispatcher{Gatekeeper: pinGatekeeper{pinned: *pinned}, Caller: caller,
			ExecCfg: config.DefaultConfig().Execution, Composer: comp}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		return d.Execute(ctx, &domain.DispatchTask{ID: "step-" + profession, Description: "do it"},
			&domain.Handoff{Context: map[string]string{"_step_index": "0", compose.HandoffLeaseKey: lease}})
	}

	// 1. A plan step hard-pinned to the third-party profession.
	_, err = pinnedStep("outsider", "lease-third")
	if plan.Refused {
		if err == nil || !strings.Contains(err.Error(), "third-party") {
			t.Fatalf("this host cannot contain third-party: the step must be refused naming the tier, got %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("third-party pinned step: %v", err)
	}
	tpID := workers.ThirdPartyRuntimeID()
	if c, ok := comp.Leases.For("lease-third"); !ok || c.Runtime != tpID {
		t.Fatalf("the step's composition runs on %q, want %q", c.Runtime, tpID)
	}
	assertThirdPartyWorker(t, caller.reports[tpID], targets, logs.String(), tpID)
	if _, ran := caller.reports[workers.RuntimeID()]; ran {
		t.Fatal("the third-party step reached the shared composed_worker")
	}

	// 2. A first-party profession still runs in the shared worker.
	if _, err := pinnedStep("insider", "lease-first"); err != nil {
		t.Fatalf("first-party pinned step: %v", err)
	}
	shared := caller.reports[workers.RuntimeID()]
	if shared == nil || !strings.Contains(filepath.ToSlash(shared.Temp), "/first-party/") {
		t.Fatalf("a first-party profession must run in the shared worker (first-party sandbox tier): %+v", shared)
	}
	if !strings.Contains(logs.String(), "agent_id="+workers.RuntimeID()+" tier=system") {
		t.Error("the shared worker's containment report must say tier=system")
	}

	// 3. The single-agent lane (ENG-2096, owner decision 2026-11-18): with the
	// third-party profile installed, a normal request leaves it OUT of its
	// composition and runs on the shared composed_worker, which reaches what the
	// third-party worker could not.
	tpRep := caller.reports[tpID]
	runtime, rc, err := comp.ComposeRequest(context.Background(), "lease-lane", "do it", &domain.Handoff{Context: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if runtime != workers.RuntimeID() || rc.Runtime != runtime {
		t.Fatalf("single-agent lane runtime = %q (composition %q), want the shared %q", runtime, rc.Runtime, workers.RuntimeID())
	}
	for _, p := range rc.Professions {
		if p == "outsider" {
			t.Fatalf("the request composition holds the third-party profession: %v", rc.Professions)
		}
	}
	if !strings.Contains(logs.String(), "third-party professions left out") {
		t.Error("the lane must log the third-party professions it left out")
	}
	delete(caller.reports, workers.RuntimeID())
	delete(caller.reports, tpID)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := caller.CallAgent(ctx, runtime, &domain.Handoff{Context: map[string]string{}}, ""); err != nil {
		t.Fatalf("single-agent lane call: %v", err)
	}
	if _, ran := caller.reports[tpID]; ran {
		t.Fatal("the single-agent lane request reached the third-party composed worker")
	}
	// The shared worker process already booted for step 2 serves the request
	// (GetOrBootInstance reuses the running instance), as in the kernel.
	lane := caller.reports[workers.RuntimeID()]
	if lane == nil || !strings.Contains(filepath.ToSlash(lane.Temp), "/first-party/") {
		t.Fatalf("the lane's request must run in the shared worker (first-party sandbox tier): %+v", lane)
	}
	// The first-party-only resource: a target the lane's worker reached and the
	// third-party worker was denied, where this host routes one at all.
	firstPartyOnly, sharedReached := 0, 0
	for _, a := range targets {
		if shared.Dial[a] == "ok" {
			sharedReached++
		}
		if lane.Dial[a] == "ok" && tpRep.Dial[a] != "ok" {
			firstPartyOnly++
		}
	}
	switch {
	case firstPartyOnly > 0:
	case sharedReached > 0:
		t.Fatalf("the lane's request reached no target the third-party worker was denied: lane %v, third-party %v", lane.Dial, tpRep.Dial)
	default:
		t.Logf("the shared worker reached none of %v here; the third-party denials are not discriminating on this host", targets)
	}
}

func assertThirdPartyWorker(t *testing.T, rep *probeReport, targets []string, logs, id string) {
	t.Helper()
	if rep == nil {
		t.Fatalf("the third-party composed worker %s never ran", id)
	}
	if !strings.Contains(filepath.ToSlash(rep.Temp), "/third-party/") {
		t.Errorf("the worker's scratch %q is not under the third-party tier", rep.Temp)
	}
	if !strings.Contains(logs, "agent_id="+id+" tier=third-party") {
		t.Errorf("no containment report with tier=third-party for %s", id)
	}
	for _, a := range targets {
		if rep.Dial[a] == "ok" {
			t.Errorf("the third-party composed worker reached %s (want kernel-only): %v", a, rep.Dial)
		}
	}
}
