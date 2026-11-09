package network

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// ADR-0145 D12, agent-plane enforcement of a composed step: the REAL gateway lease
// registry, ToolExecutor (under tools_unrestricted, the widest bypass) and ListSkills.

const composedRuntime = "composed_worker"

type composedFixture struct {
	s     *Server
	lease domain.LeaseID
	sa    *fakeSkillAccess
}

func newComposedFixture(t *testing.T) *composedFixture {
	t.Helper()
	s, gw := newLeaseServer(t)
	reg := domain.NewInMemoryToolRegistry()
	for _, n := range []string{"read_file", "write_file", "web_fetch"} {
		reg.Register(domain.SystemTool{Name: n})
	}
	leases := domain.NewComposedLeases()
	s.ToolExecutor = &domain.ToolExecutor{
		Registry: reg, Grants: domain.NewInMemoryGrantsStore(), Handler: okTool{},
		Overlay:      domain.NewRunGrantOverlay(),
		Unrestricted: true, // the ceiling must hold over the widest bypass
		LeaseCeiling: domain.ChainLeaseCeilings(nil, leases),
	}
	s.Compositions = leases
	s.ComposedRuntimeID = composedRuntime
	skills := domain.NewInMemorySkillRegistry()
	for _, n := range []string{"summarise", "web-research", "file-report"} {
		skills.Register(domain.Skill{Name: n, Description: "Does " + n + ".", Instructions: "BODY-" + n})
	}
	s.SkillRegistry = skills
	sa := &fakeSkillAccess{}
	s.SkillAccess = sa

	lease, err := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	gw.BindLease(lease, domain.LeaseBinding{SessionID: "sess-1", RunID: "run-1", StepIndex: 0, AgentID: composedRuntime})
	comp := domain.WorkerComposition{Runtime: composedRuntime, Tools: []string{"read_file"}, Skills: []string{"summarise"}}
	leases.Bind(lease, comp)
	s.ToolExecutor.Overlay.Activate(string(lease), comp.Tools)
	return &composedFixture{s: s, lease: lease, sa: sa}
}

func (f *composedFixture) call(tool string, lease domain.LeaseID) *pb.ExecuteToolResponse {
	resp, err := f.s.ExecuteTool(agentPlaneCtx(composedRuntime, "x-agent-id", composedRuntime),
		&pb.ExecuteToolRequest{ToolName: tool, LeaseId: string(lease), ArgsJson: "{}"})
	if err != nil {
		panic(err)
	}
	return resp
}

func TestComposedStep_CallsOnlyItsComposedTools_EvenUnrestricted(t *testing.T) {
	f := newComposedFixture(t)
	if r := f.call("read_file", f.lease); r.GetDenied() {
		t.Fatalf("composed tool refused: %s", r.GetDenyReason())
	}
	// A model request to widen: a registered tool the composition does not hold.
	for _, tool := range []string{"write_file", "web_fetch"} {
		if r := f.call(tool, f.lease); !r.GetDenied() {
			t.Fatalf("%s outside the composition was not refused", tool)
		}
	}
}

func TestComposedRuntime_WithoutAComposedLease_GetsNothing(t *testing.T) {
	f := newComposedFixture(t)
	r := f.call("read_file", "")
	if !r.GetDenied() {
		t.Fatal("the runtime called a tool with no composed lease")
	}
	resp, err := f.s.ListTools(agentPlaneCtx(composedRuntime, "x-agent-id", composedRuntime), &pb.ListToolsRequest{})
	if err != nil || len(resp.GetTools()) != 0 {
		t.Fatalf("runtime without a composed lease got a menu: %v %v", resp.GetTools(), err)
	}
	md := metadata.Pairs("x-agent-id", composedRuntime)
	sk, _ := f.s.ListSkills(skillAgentCtx(composedRuntime, md), &pb.ListSkillsRequest{})
	if len(sk.GetSkills()) != 0 {
		t.Fatalf("runtime without a composed lease got skills: %v", sk.GetSkills())
	}
}

func TestComposedStep_MenuIsTheComposition(t *testing.T) {
	for _, unrestricted := range []bool{true, false} {
		f := newComposedFixture(t)
		f.s.ToolExecutor.Unrestricted = unrestricted
		resp, err := f.s.ListTools(agentPlaneCtx(composedRuntime, "x-agent-id", composedRuntime, "x-lease-id", string(f.lease)),
			&pb.ListToolsRequest{})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tl := range resp.GetTools() {
			names = append(names, tl.GetName())
		}
		if len(names) != 1 || names[0] != "read_file" {
			t.Fatalf("unrestricted=%v: menu = %v, want exactly [read_file]", unrestricted, names)
		}
		// Without the bypass the composed tool still runs: the lease's run-scoped grant.
		if r := f.call("read_file", f.lease); r.GetDenied() {
			t.Fatalf("unrestricted=%v: composed tool refused: %s", unrestricted, r.GetDenyReason())
		}
		if r := f.call("write_file", f.lease); !r.GetDenied() {
			t.Fatalf("unrestricted=%v: widening allowed", unrestricted)
		}
	}
}

func TestComposedStep_SkillIndexIsTheComposition_AndNeverWidens(t *testing.T) {
	f := newComposedFixture(t)
	md := metadata.Pairs("x-agent-id", composedRuntime, "x-session-token", string(f.lease))
	menu, _ := f.s.ListSkills(skillAgentCtx(composedRuntime, md), &pb.ListSkillsRequest{})
	if got := menu.GetSkills(); len(got) != 1 || got[0].GetName() != "summarise" {
		t.Fatalf("skill menu = %v, want exactly [summarise]", got)
	}
	// use_skill on a skill outside the composition: denied, no body, and NO operator
	// access request (the model may narrow, never widen).
	md = metadata.Pairs("x-agent-id", composedRuntime, "x-session-token", string(f.lease),
		"x-skill-names", "web-research", "x-skill-full", "true")
	load, _ := f.s.ListSkills(skillAgentCtx(composedRuntime, md), &pb.ListSkillsRequest{})
	if got := load.GetSkills(); len(got) != 1 || got[0].GetAccess() != domain.SkillAccessDenied || got[0].GetInstructions() != "" {
		t.Fatalf("out-of-composition load = %v, want one denied descriptor without a body", got)
	}
	if len(f.sa.requests) != 0 {
		t.Fatalf("an access request was opened for a composed worker: %v", f.sa.requests)
	}
	// The composed skill loads with its body.
	md = metadata.Pairs("x-agent-id", composedRuntime, "x-session-token", string(f.lease),
		"x-skill-names", "summarise", "x-skill-full", "true")
	ok, _ := f.s.ListSkills(skillAgentCtx(composedRuntime, md), &pb.ListSkillsRequest{})
	if got := ok.GetSkills(); len(got) != 1 || got[0].GetInstructions() != "BODY-summarise" {
		t.Fatalf("composed skill load = %v", got)
	}
}

func TestComposedLease_IsForgottenWithTheLease(t *testing.T) {
	f := newComposedFixture(t)
	f.s.Compositions.Forget(f.lease)
	if r := f.call("read_file", f.lease); !r.GetDenied() {
		t.Fatal("after the lease ended the runtime could still call its composed tool")
	}
}

func TestComposedStep_SubagentChildInheritsTheNarrowedComposition(t *testing.T) {
	f := newComposedFixture(t)
	f.s.Compositions.Bind(f.lease, domain.WorkerComposition{Runtime: composedRuntime,
		Tools: []string{"read_file", "web_fetch"}, Skills: []string{"summarise"}})
	f.s.inheritComposition(f.lease, "child-1", map[string]bool{"web_fetch": true, "write_file": true})
	got, ok := f.s.Compositions.For("child-1")
	if !ok || len(got.Tools) != 1 || got.Tools[0] != "web_fetch" || !got.HasSkill("summarise") {
		t.Fatalf("child composition = %+v (bound=%v), want tools [web_fetch] and the parent's skills", got, ok)
	}
	f.s.inheritComposition("not-composed", "child-2", map[string]bool{"read_file": true})
	if _, ok := f.s.Compositions.For("child-2"); ok {
		t.Fatal("a child of an uncomposed parent got a composition")
	}
}

// okTool runs every tool successfully.
type okTool struct{}

func (okTool) Execute(context.Context, domain.ToolCall) ([]byte, error) {
	return []byte(`{"ok":true}`), nil
}
