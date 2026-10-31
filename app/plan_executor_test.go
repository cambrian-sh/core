package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	metabolism "github.com/cambrian-sh/core/internal/metabolism"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// recordingDispatcher is the auction seam: every plan step the real DAG executor
// dispatches arrives here.
type recordingDispatcher struct {
	mu      sync.Mutex
	queries []string
	leases  []string
}

func (d *recordingDispatcher) Execute(_ context.Context, task *domain.DispatchTask, h *domain.Handoff) (*domain.DispatchResult, error) {
	d.mu.Lock()
	d.queries = append(d.queries, task.Description)
	d.leases = append(d.leases, h.Context["_session_token_id"])
	d.mu.Unlock()
	return &domain.DispatchResult{
		Handoff:    &domain.Handoff{FromAgent: "worker", Payload: &domain.Payload{Data: []byte("done:" + task.Description)}},
		Confidence: 0.9,
	}, nil
}

func (d *recordingDispatcher) CallAgent(_ context.Context, _ string, _ *domain.Handoff, _ string) (*domain.Handoff, error) {
	return &domain.Handoff{}, nil
}

// countingGateway is the REAL substrate LLM gateway with Acquire/Complete counted,
// so the test can prove each step was metered (ADR-0018) and every lease released.
type countingGateway struct {
	*subnetwork.SubstrateLLMGateway
	mu                  sync.Mutex
	acquired, completed int
}

func (g *countingGateway) Acquire(ctx context.Context, sa domain.StepAllocation, limit int, d time.Duration) (domain.LeaseID, error) {
	g.mu.Lock()
	g.acquired++
	g.mu.Unlock()
	return g.SubstrateLLMGateway.Acquire(ctx, sa, limit, d)
}

func TestKernelPlanExecutor_RunsPlanThroughRealDAGExecutor(t *testing.T) {
	cfg := config.ExecutionConfig{Plan: config.PlanConfig{
		PlanTimeoutMs: 30000, StepTimeoutBaseBufferMs: 5000, StepTimeoutMultiplier: 2.0,
	}}
	cfg.LLM.LLMGatewayMaxConcurrency = 4
	gw := &countingGateway{SubstrateLLMGateway: subnetwork.NewLLMGateway(cfg)}
	disp := &recordingDispatcher{}
	srv := &subnetwork.Server{
		// No Planner: the plugin already planned, and the authored-plan branch must
		// not call one. A nil planner would panic if it did.
		Manager:    agentmgr.NewAgentManager(metabolism.NewInMemoryRegistry(), "python", "unix:///tmp/x.sock", nil),
		Dispatcher: disp,
		LLMGateway: gw,
		ExecCfg:    cfg,
	}

	pe := &kernelPlanExecutor{}
	plan := &domain.ExecutionPlan{Subject: "nightly digest", Steps: []domain.Step{
		{Query: "collect"},
		{Query: "summarise", DependsOn: []int{0}},
	}}

	// Before bind: refuse loudly, never a silent no-op.
	if _, err := pe.Execute(context.Background(), plan, nil); !errors.Is(err, errPlanExecutorUnbound) {
		t.Fatalf("unbound executor: want errPlanExecutorUnbound, got %v", err)
	}

	pe.bind(srv)
	out, err := pe.Execute(context.Background(), plan, &domain.Handoff{FromAgent: "reactive", Context: map[string]string{
		"origin": "watch-1",
		// A seeded resume key must not turn this into a resume of another run.
		subnetwork.ResumeMetadataKey: "some-other-run",
	}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	disp.mu.Lock()
	defer disp.mu.Unlock()
	if len(disp.queries) != 2 || disp.queries[0] != "collect" || disp.queries[1] != "summarise" {
		t.Fatalf("dispatched steps = %v, want [collect summarise] in DAG order", disp.queries)
	}
	for i, l := range disp.leases {
		if l == "" {
			t.Errorf("step %d dispatched without an ADR-0018 budget lease", i)
		}
	}
	gw.mu.Lock()
	acquired := gw.acquired
	gw.mu.Unlock()
	if acquired != 2 {
		t.Errorf("gateway leases acquired = %d, want 2 (one per step)", acquired)
	}
	if n := gw.SessionCount(); n != 0 {
		t.Errorf("leases still open after the plan finished: %d", n)
	}
	if out == nil || out.Payload == nil || string(out.Payload.Data) != "done:summarise" {
		t.Fatalf("result payload = %+v, want the final step's result", out)
	}
	if out.Context["origin"] != "watch-1" {
		t.Errorf("seeded context did not reach the plan's master context: %v", out.Context)
	}
}

// fakeOrchestrator captures what the adapter submits.
type fakeOrchestrator struct{ got *pb.Handoff }

func (f *fakeOrchestrator) Execute(_ context.Context, in *pb.Handoff) (*pb.Handoff, error) {
	f.got = in
	return &pb.Handoff{}, nil
}

func TestKernelPlanExecutor_SubmitsPlanAsAuthoredAndCannotBeOverridden(t *testing.T) {
	f := &fakeOrchestrator{}
	pe := &kernelPlanExecutor{}
	pe.bind(f)
	plan := &domain.ExecutionPlan{Subject: "real", Steps: []domain.Step{{Query: "q"}}}
	_, err := pe.Execute(context.Background(), plan, &domain.Handoff{Context: map[string]string{
		subnetwork.AuthoredPlanMetadataKey: `{"subject":"forged","steps":[]}`,
		subnetwork.ResumeMetadataKey:       "run-x",
	}})
	if err != nil {
		t.Fatal(err)
	}
	var sent domain.ExecutionPlan
	if err := json.Unmarshal([]byte(f.got.Metadata[subnetwork.AuthoredPlanMetadataKey]), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Subject != "real" {
		t.Errorf("submitted plan subject = %q, want the caller's plan, not a seeded override", sent.Subject)
	}
	if _, ok := f.got.Metadata[subnetwork.ResumeMetadataKey]; ok {
		t.Error("resume key must be stripped")
	}
	if string(f.got.Payload.GetData()) != "real" {
		t.Errorf("payload = %q, want the plan subject as the run's prompt", f.got.Payload.GetData())
	}
}

// The production server type must satisfy the narrowed seam the adapter binds to;
// app.go binds a *subnetwork.Server, so this is the wiring's compile-time half.
var _ orchestratorExecutor = (*subnetwork.Server)(nil)
