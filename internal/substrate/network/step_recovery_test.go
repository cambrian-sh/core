package network

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
	"github.com/cambrian-sh/core/internal/substrate/harness"
)

// Under the checklist the next stage's planner is the re-planner: a stage that
// stopped at a needs_replan step hands it the failure; any other failure does not.
func TestStageReplan(t *testing.T) {
	cm := &domain.CapabilityMissingError{Needed: []string{"run a PowerShell command"}, Intent: "write the CSV"}
	stopped := &executer.PartialPlanError{FailedStep: 2, LastError: &harness.HealingExhaustedError{
		StepIndex: 2, AttemptCount: 1, LastError: cm, Class: domain.StepFailureNeedsReplan}}
	note := stageReplan(stopped, 1)
	if !strings.Contains(note, "Stage 1 stopped at step 2") || !strings.Contains(note, "run a PowerShell command") {
		t.Fatalf("note = %q", note)
	}
	if stageReplan(&executer.PartialPlanError{FailedStep: 0, LastError: errors.New("agent failed")}, 1) != "" {
		t.Fatal("an ordinary failure is not a re-plan note")
	}
	if stageReplan(nil, 1) != "" {
		t.Fatal("a stage that did not stop has no note")
	}
}

// find_tools under a composed lease reports matching tools outside the step in a
// response header and never puts them on the menu
// (T-20261112-yield-sub-goal-on-a-composed-lease-runs-under, owner 2026-11-12).
type headerStream struct{ md metadata.MD }

func (h *headerStream) Method() string                  { return "/cambrian.Orchestrator/ListTools" }
func (h *headerStream) SetHeader(md metadata.MD) error  { h.md = metadata.Join(h.md, md); return nil }
func (h *headerStream) SendHeader(md metadata.MD) error { return h.SetHeader(md) }
func (h *headerStream) SetTrailer(metadata.MD) error    { return nil }

type rankAll struct{}

func (rankAll) Rank(_ context.Context, _ string, names []string, k int) ([]string, error) {
	if len(names) > k {
		names = names[:k]
	}
	return names, nil
}

func TestFindTools_ReportsToolsOutsideTheStepWithoutLoadingThem(t *testing.T) {
	f := newComposedFixture(t)
	f.s.ToolExecutor.Retriever = rankAll{}
	f.s.ToolsOutsideStep = func(_ context.Context, comp domain.WorkerComposition) []domain.SystemTool {
		return []domain.SystemTool{{Name: "web_fetch"}}
	}
	hs := &headerStream{}
	ctx := grpc.NewContextWithServerTransportStream(agentPlaneCtx(composedRuntime, "x-agent-id", composedRuntime,
		"x-lease-id", string(f.lease), "x-tool-query", "fetch a web page"), hs)
	resp, err := f.s.ListTools(ctx, &pb.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range resp.GetTools() {
		if tl.GetName() == "web_fetch" {
			t.Fatal("a tool outside the step was loaded onto the menu")
		}
	}
	if got := hs.md.Get(ToolOutsideStepHeader); len(got) != 1 || got[0] != "web_fetch" {
		t.Fatalf("%s = %v, want [web_fetch]", ToolOutsideStepHeader, got)
	}
	if r := f.call("web_fetch", f.lease); !r.GetDenied() {
		t.Fatal("reporting a tool must not make it callable")
	}
}
