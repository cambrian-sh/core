package app

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

type countingWrite struct{ n atomic.Int32 }

func (h *countingWrite) Execute(context.Context, domain.ToolCall) ([]byte, error) {
	h.n.Add(1)
	return []byte(`{"moved":"Receipts/2026-10/a.pdf"}`), nil
}

// ADR-0145 D14 item 7 (owner 2026-11-09): the request guards are off by default
// and stay off in every run configuration until they are redesigned. With them
// off, an identical retry of a side-effecting call made by a plan step is still
// answered by the D9 idempotency cache (execution.tools.idempotent_writes): the
// write runs once, and a corrected retry (other arguments) runs, because there is
// no per-target write record to refuse it as a duplicate.
func TestGuardsOffIdempotentWritesStillAnswerRetries(t *testing.T) {
	cfg := config.DefaultConfig().Execution
	if cfg.Tools.RequestGuards {
		t.Fatal("execution.tools.request_guards must default to false")
	}
	cfg.Tools.IdempotentWrites = true
	reg := domain.NewInMemoryToolRegistry()
	reg.Register(domain.SystemTool{Name: "move_file", Effects: []domain.ToolEffect{domain.EffectWrite}})
	h := &countingWrite{}
	// The controls go in through the constructor, exactly as bootstrapKernel builds
	// the executor (AGENTS.md §2.3).
	controls := evidenceToolControls(cfg, nil, domain.ToolExecutorControls{
		Registry: reg, Grants: domain.NewInMemoryGrantsStore(), Authz: domain.AllowAllAuthorizer{}, Unrestricted: true})
	te, err := domain.NewToolExecutor(controls, domain.ToolExecutor{Handler: h, InlineThreshold: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	srv := &subnetwork.Server{ToolExecutor: te}
	wireEvidencePipeline(cfg, srv, nil, nil)
	if te.Guards() != nil {
		t.Fatal("request guards wired although the flag is off")
	}
	if te.Idempotency() == nil {
		t.Fatal("idempotent_writes not wired")
	}
	ctx := domain.WithStepCall(context.Background(), domain.StepCall{RunID: "run-1", StepIndex: 3})
	call := domain.ToolCallRequest{AgentID: "w", ToolName: "move_file", ArgsJSON: []byte(`{"source":"a.pdf","destination":"R/a.pdf"}`)}
	first := te.Execute(ctx, call)
	again := te.Execute(ctx, domain.ToolCallRequest{AgentID: "w", ToolName: "move_file",
		ArgsJSON: []byte(`{"destination":"R/a.pdf","source":"a.pdf"}`)})
	if first.Denied || again.Denied || h.n.Load() != 1 || string(again.ResultJSON) != string(first.ResultJSON) {
		t.Fatalf("identical retry: ran %d times, first %+v again %+v", h.n.Load(), first, again)
	}
	fixed := te.Execute(ctx, domain.ToolCallRequest{AgentID: "w", ToolName: "move_file",
		ArgsJSON: []byte(`{"source":"a.pdf","destination":"R/2026-10/a.pdf"}`)})
	if fixed.Denied || h.n.Load() != 2 {
		t.Fatalf("a corrected retry must run with the guards off: ran %d, %+v", h.n.Load(), fixed)
	}
}
