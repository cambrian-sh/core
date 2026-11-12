package awareness

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0145 D11 (T-20261108-adr-0145-d11-staged-planner-returns-next-stage): the
// staged arm (planner_cap v8 + planner_input v4) is used only when the server
// put a StagePlanning on ctx; it returns the next stage, serves the planner's
// ledger reads between its calls, and turns a question or "nothing more" into
// the typed errors the server decides on.

// scriptedGenerator answers each call with the next reply.
type scriptedGenerator struct {
	replies  []string
	captured []string
}

func (g *scriptedGenerator) Generate(_ context.Context, prompt string) (string, error) {
	g.captured = append(g.captured, prompt)
	if len(g.captured) > len(g.replies) {
		return "", errors.New("scriptedGenerator: no reply left")
	}
	return g.replies[len(g.captured)-1], nil
}

func stagedCtx(sp *domain.StagePlanning, stage *domain.CoverageStage) context.Context {
	cl := &domain.RequirementChecklist{Items: []domain.RequirementItem{
		{ID: "R1", Type: domain.RequirementAnswer, Text: "Which customers are overdue", Quote: "vadesi geçen müşteriler"},
		{ID: "R2", Type: domain.RequirementFigure, Text: "The total overdue amount", Quote: "toplam tutar"},
		{ID: "R3", Type: domain.RequirementConstraint, Text: "Do not send", Quote: "gönderme", Forbids: []string{"send"}},
	}}
	ctx := domain.WithRequirementChecklist(context.Background(), cl)
	if stage != nil {
		ctx = domain.WithCoverageStage(ctx, stage)
	}
	return domain.WithStagePlanning(ctx, sp)
}

const stagedStageReply = `{"reasoning":"r","outcome":"stage","subject":"s","cache_policy":"default","steps":[` +
	`{"query":"Use invoices_search to list the overdue invoices","depends_on":[],"required_capabilities":[],"required_effects":["read"],"answers":["R1"]},` +
	`{"query":"Use calc_aggregate to sum the amount of the invoices in the result of step 0","depends_on":[0],"required_capabilities":[],"required_effects":[],"answers":["R2"],"inputs":["s0"],"is_thought":true}]}`

func TestStagedPlanner_PinsAndStagePrompt(t *testing.T) {
	if plannerChecklistPrompts.System.Version != 10 || plannerChecklistPrompts.Input.Version != 3 {
		t.Fatal("the checklist arm must stay pinned to v10 + v3 while the staged flag is off")
	}
	if plannerStagedPrompts.System.Version != 11 || plannerStagedPrompts.Input.Version != 4 {
		t.Fatalf("staged arm = v%d + v%d", plannerStagedPrompts.System.Version, plannerStagedPrompts.Input.Version)
	}
	gen := &scriptedGenerator{replies: []string{stagedStageReply}}
	p := NewPlanner(gen, &mockAgentProvider{}, nil)
	sp := &domain.StagePlanning{Number: 1, CanAsk: true, MaxLookupRounds: 3}
	plan, err := p.GetExecutionPlan(stagedCtx(sp, nil), "vadesi geçen müşteriler ve toplam tutar; gönderme")
	if err != nil {
		t.Fatal(err)
	}
	if plan.PlannerPromptVersion != plannerPromptHashStaged || len(plan.Steps) != 2 {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.Steps[1].IsThought {
		t.Fatal("a thought step must be retired on the staged arm")
	}
	sent := gen.captured[0]
	for _, want := range []string{"<stage>", "stage: 1", "the person can be asked: yes", "<ledger_reads>",
		"R1 [answer] Which customers are overdue", "— status: open", "Stages:", "Exact work:", "Decomposition:", "calc_aggregate"} {
		if !strings.Contains(sent, want) {
			t.Errorf("the staged request lacks %q", want)
		}
	}
}

func TestStagedPlanner_LaterStageIsFreshAndShowsStatuses(t *testing.T) {
	gen := &scriptedGenerator{replies: []string{stagedStageReply}}
	p := NewPlanner(gen, &mockAgentProvider{}, nil)
	sp := &domain.StagePlanning{Number: 2, CanAsk: false, LedgerIndex: "led_a tool_result invoices_search step 0: [INV-1...]",
		EarlierStages: "- stage 1 step 0 \"list\" -> led_s0: [INV-1]"}
	stage := &domain.CoverageStage{Number: 2, Settled: []string{"R1"}, SettledAs: map[string]domain.RequirementStatus{"R1": domain.RequirementSatisfied},
		Open: []domain.OpenItem{{ID: "R2", Reason: "no number, money amount or date from a tool result or calculation"}}}
	if _, err := p.GetExecutionPlan(stagedCtx(sp, stage), "r"); err != nil {
		t.Fatal(err)
	}
	sent := gen.captured[0]
	for _, want := range []string{"stage: 2", "the person can be asked: no", "R1 [answer] Which customers are overdue (from the request: \"vadesi geçen müşteriler\") — status: satisfied (settled",
		"R2 [figure] The total overdue amount (from the request: \"toplam tutar\") — status: open; no number", "led_a tool_result", "led_s0",
		"ledger reads: not available"} {
		if !strings.Contains(sent, want) {
			t.Errorf("stage 2 request lacks %q", want)
		}
	}
}

func TestStagedPlanner_ServesLedgerReadsBetweenCalls(t *testing.T) {
	gen := &scriptedGenerator{replies: []string{
		`{"reasoning":"look first","outcome":"read","subject":"s","cache_policy":"default","steps":[],` +
			`"ledger_reads":[{"tool":"ledger_get","arguments":{"id":"led_a","path":"content[0].text"}},{"tool":"mail_send","arguments":{}}]}`,
		stagedStageReply,
	}}
	var served []domain.LedgerLookup
	sp := &domain.StagePlanning{Number: 2, MaxLookupRounds: 2, Lookup: func(_ context.Context, l domain.LedgerLookup) string {
		served = append(served, l)
		return `{"found":true,"id":"led_a","value":[{"id":"INV-1"}]}`
	}}
	p := NewPlanner(gen, &mockAgentProvider{}, nil)
	plan, err := p.GetExecutionPlan(stagedCtx(sp, nil), "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 2 || len(gen.captured) != 2 {
		t.Fatalf("one read round then the stage: %d calls", len(gen.captured))
	}
	if len(served) != 1 || served[0].Tool != "ledger_get" {
		t.Fatalf("only ledger_get / ledger_find are served: %+v", served)
	}
	second := gen.captured[1]
	if !strings.Contains(second, `"id":"INV-1"`) || !strings.Contains(second, `refused: only ledger_get and ledger_find`) {
		t.Fatalf("the second call must carry the read results:\n%s", second)
	}
}

func TestStagedPlanner_ReadRoundsAreBounded(t *testing.T) {
	read := `{"reasoning":"r","outcome":"read","subject":"s","cache_policy":"default","steps":[],"ledger_reads":[{"tool":"ledger_find","arguments":{"scope":"turn"}}]}`
	gen := &scriptedGenerator{replies: []string{read, read, read}}
	sp := &domain.StagePlanning{Number: 1, MaxLookupRounds: 2, Lookup: func(context.Context, domain.LedgerLookup) string { return "{}" }}
	_, err := NewPlanner(gen, &mockAgentProvider{}, nil).GetExecutionPlan(stagedCtx(sp, nil), "r")
	if err == nil || !strings.Contains(err.Error(), "stage_ledger_read_rounds") {
		t.Fatalf("a planner that only reads must stop at the bound: %v", err)
	}
}

func TestStagedPlanner_QuestionAndNothingMoreAreTyped(t *testing.T) {
	gen := &scriptedGenerator{replies: []string{
		`{"reasoning":"the request asks to send and not to send","outcome":"input_required","subject":"s","cache_policy":"default","steps":[],` +
			`"questions":[{"question":"Should the reminders be sent now or only drafted?","kind":"conflict","items":["R3"],"quote":"gönderme","options":["send","draft only"]},{"question":"  ","kind":"x"}],` +
			`"assumptions":["the overdue cut-off is today"]}`,
	}}
	sp := &domain.StagePlanning{Number: 1, CanAsk: true}
	_, err := NewPlanner(gen, &mockAgentProvider{}, nil).GetExecutionPlan(stagedCtx(sp, nil), "r")
	var ir *domain.InputRequiredError
	if !errors.As(err, &ir) || len(ir.Questions) != 1 || ir.Questions[0].ID != "Q1" || ir.Questions[0].Kind != domain.QuestionConflict {
		t.Fatalf("input_required = %v", err)
	}
	if got := sp.Assumptions(); len(got) != 1 {
		t.Fatalf("assumptions = %v", got)
	}

	gen2 := &scriptedGenerator{replies: []string{`{"reasoning":"nothing can help","outcome":"nothing_more","subject":"s","cache_policy":"default","steps":[]}`}}
	_, err = NewPlanner(gen2, &mockAgentProvider{}, nil).GetExecutionPlan(stagedCtx(&domain.StagePlanning{Number: 3}, nil), "r")
	if !errors.Is(err, domain.ErrNoFurtherStage) {
		t.Fatalf("nothing_more = %v", err)
	}
}
