package network

import (
	"context"
	"slices"
	"strings"
	"testing"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/requirements"
	supgk "github.com/cambrian-sh/core/internal/supervision/gatekeeper"
)

// ENG-1426 (owner decision 2026-11-19): one definition of "sending" — the
// declared class egress_send wins. End to end through the production paths,
// with the shipped defaults (config.DefaultConfig) and each tool held by its own
// agent:
//
//   - planner/dispatch: the effect vocabulary through gatekeeper.ToolEffectSource
//     over ToolExecutor.AvailableTools (what L1 routes on);
//   - D14 constraint: Server.stepGuard resolves "do not send anything" through
//     execution.tools.constraint_classes, the lease carries it, and ExecuteTool
//     refuses or runs the call;
//   - Rule of Two: the default gate as an executor constructor control, in a
//     tainted session, decides which calls go to approval.
//
// For tools that declare the split, all three agree tool by tool. For a tool
// that declares plain egress the guards fail closed (it may send) and the
// vocabulary needs the name glob as positive evidence that the agent can send.

type sendingCase struct {
	agent string
	tool  domain.SystemTool
	// vocabSend: the planner vocabulary says this agent can "send".
	vocabSend bool
	// guardSend: the guards (constraint, Rule of Two) treat the call as a send.
	guardSend bool
}

var sendingCases = []sendingCase{
	// Declared egress_read, named like a send: never a send.
	{"a-preview", domain.SystemTool{Name: "mcp:mail/send_preview", Effects: []domain.ToolEffect{domain.EffectRead, domain.EffectEgressRead}}, false, false},
	// Declared egress_send, no matching name: a send.
	{"a-notify", domain.SystemTool{Name: "mcp:hooks/notify_webhook", Effects: []domain.ToolEffect{domain.EffectEgressSend}}, true, true},
	// Plain egress, matching name: a send everywhere.
	{"a-gmail", domain.SystemTool{Name: "mcp:gmail/send_email", Effects: []domain.ToolEffect{domain.EffectEgress}}, true, true},
	// Plain egress, no matching name: the guards fail closed, the vocabulary has
	// no positive evidence.
	{"a-fetch", domain.SystemTool{Name: "mcp:web/fetch_page", Effects: []domain.ToolEffect{domain.EffectEgress}}, false, true},
}

type sendingFixture struct {
	srv  *Server
	gw   *SubstrateLLMGateway
	appr *rotApprovals
}

func newSendingFixture(t *testing.T, controls func(*domain.ToolExecutorControls)) *sendingFixture {
	t.Helper()
	s, gw := newLeaseServer(t)
	reg := domain.NewInMemoryToolRegistry()
	grants := domain.NewInMemoryGrantsStore()
	reg.Register(domain.SystemTool{Name: "mcp:files/read", Effects: []domain.ToolEffect{domain.EffectRead}})
	for _, c := range sendingCases {
		reg.Register(c.tool)
		grants.Set(c.agent, []domain.ToolGrant{
			{Tool: c.tool.Name, Policy: domain.ToolResourcePolicy{AllowAll: true}},
			{Tool: "mcp:files/read", Policy: domain.ToolResourcePolicy{AllowAll: true}},
		})
	}
	appr := &rotApprovals{}
	h := &staticCountingHandler{}
	c := domain.ToolExecutorControls{Registry: reg, Grants: grants, Approval: appr}
	if controls != nil {
		controls(&c)
	}
	s.ToolExecutor = newTestToolExecutor(t, c, domain.ToolExecutor{Handler: h, MCPHandler: h})
	s.ExecCfg = config.DefaultConfig().Execution
	return &sendingFixture{srv: s, gw: gw, appr: appr}
}

func (f *sendingFixture) call(t *testing.T, agent, tool string, lease domain.LeaseID) (denied bool, reason string) {
	t.Helper()
	resp, err := f.srv.ExecuteTool(agentPlaneCtx(agent, "x-agent-id", agent), &pb.ExecuteToolRequest{
		ToolName: tool, ArgsJson: `{"to":"someone@example.org","text":"hello"}`, LeaseId: string(lease),
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp.Denied, resp.DenyReason
}

func TestSendingDefinitionE2E_PlannerConstraintAndRuleOfTwoAgree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// ── planner / dispatch: the effect vocabulary over each agent's callable tools.
	f := newSendingFixture(t, nil)
	vocab, err := f.srv.ExecCfg.Capability.StepEffectVocabulary()
	if err != nil {
		t.Fatal(err)
	}
	src := supgk.NewToolEffectSource(vocab, f.srv.ToolExecutor)
	vocabSend := map[string]bool{}
	for _, c := range sendingCases {
		effs := src.AgentEffects(ctx, c.agent)
		vocabSend[c.agent] = slices.Contains(effs, "send")
		if vocabSend[c.agent] != c.vocabSend {
			t.Errorf("planner: %s (%s %v) effects %v: send=%v, want %v", c.agent, c.tool.Name, c.tool.Effects, effs, vocabSend[c.agent], c.vocabSend)
		}
		if !slices.Contains(effs, "egress") {
			t.Errorf("planner: %s reaches outside, effects %v lack egress", c.agent, effs)
		}
	}

	// ── D14 constraint: "do not send anything" through the default constraint vocabulary.
	f = newSendingFixture(t, func(c *domain.ToolExecutorControls) { c.Guards = &domain.RequestGuards{} })
	f.srv.ExecCfg.Tools.RequestGuards = true
	cl := domain.RequirementChecklist{Items: []domain.RequirementItem{
		{ID: "R1", Type: domain.RequirementAnswer, Text: "Summarise the thread"},
		{ID: "R2", Type: domain.RequirementConstraint, Quote: "do not send anything", Forbids: []string{"send"}},
	}}
	reqs := &requestRequirements{checklist: &cl, coverage: requirements.NewCoverage(cl)}
	guard := f.srv.stepGuard(ctx, domain.Step{Answers: []string{"R1"}}, reqs, "run-c")
	if guard == nil || len(guard.Constraints) != 1 || !slices.Equal(guard.Constraints[0].Classes, []domain.ToolEffect{domain.EffectEgressSend}) {
		t.Fatalf("forbids send must resolve to egress_send: %+v", guard)
	}
	constraintSend := map[string]bool{}
	for _, c := range sendingCases {
		lease := bindNotesLease(t, f.gw, domain.LeaseBinding{SessionID: domain.SessionID("ses-c-" + c.agent), RunID: "run-c", StepIndex: 0, AgentID: c.agent, Guard: guard})
		denied, why := f.call(t, c.agent, c.tool.Name, lease)
		constraintSend[c.agent] = denied && strings.Contains(why, "do not send anything")
		if denied && !constraintSend[c.agent] {
			t.Errorf("constraint: %s refused for another reason: %s", c.agent, why)
		}
	}

	// ── Rule of Two: a tainted session (a tool result is untrusted content) with
	// sensitive access (no sensitivity vocabulary ⇒ every session) calls each tool.
	rot := config.DefaultConfig().Execution.Tools.RuleOfTwo
	gate, err := domain.NewRuleOfTwoGate(domain.RuleOfTwoPolicy{
		UntrustedSources: rot.Sources(), SensitiveTags: rot.SensitiveTags, GatedEffects: rot.Effects(),
	}, domain.NewProvenanceTracker(0, nil))
	if err != nil {
		t.Fatal(err)
	}
	f = newSendingFixture(t, func(c *domain.ToolExecutorControls) { c.RuleOfTwo = gate })
	rotSend := map[string]bool{}
	for _, c := range sendingCases {
		lease := bindNotesLease(t, f.gw, domain.LeaseBinding{SessionID: domain.SessionID("ses-r-" + c.agent), RunID: domain.RunID("run-r-" + c.agent), StepIndex: 0, AgentID: c.agent})
		if denied, why := f.call(t, c.agent, "mcp:files/read", lease); denied {
			t.Fatalf("taint read refused: %s", why)
		}
		before := f.appr.count()
		if denied, why := f.call(t, c.agent, c.tool.Name, lease); denied {
			t.Fatalf("rule of two: %s denied (approvals approve): %s", c.agent, why)
		}
		rotSend[c.agent] = f.appr.count() > before
	}

	for _, c := range sendingCases {
		if constraintSend[c.agent] != c.guardSend || rotSend[c.agent] != c.guardSend {
			t.Errorf("%s (%s %v): constraint send=%v, rule of two send=%v, want %v",
				c.agent, c.tool.Name, c.tool.Effects, constraintSend[c.agent], rotSend[c.agent], c.guardSend)
		}
		// One definition: where the tool declares the split, the planner and both
		// guards say the same thing.
		if declaresSplit(c.tool) && (vocabSend[c.agent] != constraintSend[c.agent] || vocabSend[c.agent] != rotSend[c.agent]) {
			t.Errorf("%s declares the split but the paths disagree: planner=%v constraint=%v rule-of-two=%v",
				c.agent, vocabSend[c.agent], constraintSend[c.agent], rotSend[c.agent])
		}
	}
}

func declaresSplit(t domain.SystemTool) bool {
	return t.HasEffect(domain.EffectEgressRead) || t.HasEffect(domain.EffectEgressSend)
}
