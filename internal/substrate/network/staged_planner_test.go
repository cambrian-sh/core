package network

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/calc"
	"github.com/cambrian-sh/core/internal/ledger"
)

// ADR-0145 D11 / D5a at the server (T-20261108-adr-0145-d11-staged-planner-returns-next-stage):
// the staged planner is re-called per stage with a fresh context, figures come
// from the calculation tools, a stage that changes nothing stalls, "nothing more"
// closes the open items as gaps, and a question reaches the caller as a
// structured InputRequired.

// stagedPlanner answers each call from a script and records what each call saw.
type stagedPlanner struct {
	mu      sync.Mutex
	replies []func(ctx context.Context, input string) (*domain.ExecutionPlan, error)
	seen    []seenStage
	inputs  []string
	stages  []*domain.CoverageStage
}

func (p *stagedPlanner) GetExecutionPlan(ctx context.Context, input string) (*domain.ExecutionPlan, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := len(p.inputs)
	p.inputs = append(p.inputs, input)
	if sp := domain.StagePlanningFrom(ctx); sp != nil {
		p.seen = append(p.seen, seenStage{Number: sp.Number, LedgerIndex: sp.LedgerIndex,
			EarlierStages: sp.EarlierStages, CanAsk: sp.CanAsk, Lookup: sp.Lookup})
		p.stages = append(p.stages, domain.CoverageStageFrom(ctx))
	}
	if i >= len(p.replies) {
		panic("stagedPlanner: more calls than replies")
	}
	return p.replies[i](ctx, input)
}

// seenStage is what one planner call was told (StagePlanning without its lock).
type seenStage struct {
	Number        int
	LedgerIndex   string
	EarlierStages string
	CanAsk        bool
	Lookup        domain.LedgerLookupFunc
}

func (p *stagedPlanner) Generate(context.Context, string) (string, error) { return "", nil }

func planOf(steps ...domain.Step) func(context.Context, string) (*domain.ExecutionPlan, error) {
	return func(context.Context, string) (*domain.ExecutionPlan, error) {
		return &domain.ExecutionPlan{Subject: "s", Steps: steps}, nil
	}
}

// stagedServer is checklistServer with the staged planner on, the calculation
// tools served over the same ledger, and the ledger tools for the planner's reads.
func stagedServer(t *testing.T, items []domain.RequirementItem, replies ...func(context.Context, string) (*domain.ExecutionPlan, error)) (*Server, *stagedPlanner, *envelopeDispatcher, map[string]domain.KernelTool) {
	t.Helper()
	s, _, disp := checklistServer(t, nil, items)
	sp := &stagedPlanner{replies: replies}
	s.Planner = sp
	s.ExecCfg.Plan.StagedPlanner = true
	work, _ := calc.NewWorkCalendar(nil, []string{"2026-11-10"})
	cal := domain.NewCalendar(time.UTC, func() time.Time { return time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC) })
	tools := map[string]domain.KernelTool{}
	kts := append(ledger.Tools(s.Ledger.Reader, 0, nil), calc.Tools(calc.Deps{Calendar: cal, Work: work, Reader: s.Ledger.Reader, Writer: s.Ledger.Writer})...)
	for _, kt := range kts {
		tools[kt.Spec().Name] = kt
	}
	s.ToolExecutor = &domain.ToolExecutor{KernelTools: kts}
	return s, sp, disp, tools
}

// callCalc is a worker calling a calculation tool from inside its step: the
// kernel serves it under the step's provenance (run, step) and the caller's
// predicate, exactly as runKernelTool does.
func callCalc(t *testing.T, ctx context.Context, step int, kt domain.KernelTool, args string) map[string]any {
	t.Helper()
	p := domain.LedgerProvenanceFromContext(ctx)
	p.StepIndex = step
	cctx := domain.WithScope(domain.WithLedgerProvenance(ctx, p), &domain.TagPredicate{Bypass: true})
	out, err := kt.Call(cctx, domain.KernelToolCall{AgentID: "worker", ArgsJSON: []byte(args)})
	if err != nil {
		t.Fatalf("%s: %v", kt.Spec().Name, err)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	return m
}

func moneyValue(name string, amount float64, ref string) domain.EnvelopeValue {
	raw, _ := json.Marshal(amount)
	return domain.EnvelopeValue{Name: name, Type: domain.ValueMoney, Value: raw, EvidenceRef: ref}
}

func dateValue(name, d, ref string) domain.EnvelopeValue {
	raw, _ := json.Marshal(d)
	return domain.EnvelopeValue{Name: name, Type: domain.ValueDate, Value: raw, EvidenceRef: ref}
}

// The o3x shape: "which customers are overdue, the total overdue amount, the
// reminder deadline (5 working days from today), draft a reminder for each, do
// not send". Stage 1 finds the overdue invoices and computes the figures with the
// calculation tools; stage 2, planned from the ledger index and stage 1's
// results, drafts the reminders. Every item settles in two stages, and the
// figures cite computed entries, never model text.
func TestStagedPlanner_O3xReachesEveryItemInTwoStagesWithCalcFigures(t *testing.T) {
	items := []domain.RequirementItem{
		{ID: "R1", Type: domain.RequirementAnswer, Text: "Which customers are overdue", Quote: "overdue customers"},
		{ID: "R2", Type: domain.RequirementFigure, Text: "The total overdue amount", Quote: "total overdue"},
		{ID: "R3", Type: domain.RequirementFigure, Text: "The reminder deadline, 5 working days from today", Quote: "5 working days"},
		{ID: "R4", Type: domain.RequirementAnswer, Text: "A reminder draft for each overdue customer", Quote: "draft a reminder"},
		{ID: "R5", Type: domain.RequirementConstraint, Text: "Do not send", Quote: "do not send", Forbids: []string{"send"}},
	}
	s, sp, disp, tools := stagedServer(t, items,
		planOf(
			domain.Step{Query: "Use invoices_search to list the overdue invoices", RequiredEffects: []string{"read"}, Answers: []string{"R1"}},
			domain.Step{Query: "Use calc_aggregate to sum the overdue amounts of step 0 and calc_date_add for the deadline",
				DependsOn: []int{0}, RequiredEffects: []string{}, Answers: []string{"R2", "R3"}, Inputs: []string{"s0"}}),
		planOf(
			domain.Step{Query: "Draft a reminder for each overdue customer", RequiredEffects: []string{"write"}, Answers: []string{"R4"}}),
	)
	var invoicesRef string
	disp.answer = func(ctx context.Context, q string, record func(string, string) string) []byte {
		switch {
		case strings.HasPrefix(q, "Use invoices_search"):
			invoicesRef = record("invoices_search", `{"invoices":[{"id":"INV-1","customer":"CUS-004","amount":"1.250,50 TL"},`+
				`{"id":"INV-3","customer":"CUS-007","amount":300}]}`)
			return envelopeJSON(domain.ItemResult{Item: "R1", Status: domain.RequirementSatisfied, Values: []domain.EnvelopeValue{
				idValue("customer", "CUS-004", invoicesRef), idValue("customer_2", "CUS-007", invoicesRef)}})
		case strings.HasPrefix(q, "Use calc_aggregate"):
			sum := callCalc(t, ctx, 1, tools[calc.ToolAggregate], `{"ref":"`+invoicesRef+`","path":"invoices","op":"sum","field":"amount"}`)
			due := callCalc(t, ctx, 1, tools[calc.ToolDateAdd], `{"working_days":5}`)
			return envelopeJSON(
				domain.ItemResult{Item: "R2", Status: domain.RequirementSatisfied, Values: []domain.EnvelopeValue{
					moneyValue("total", sum["result"].(float64), sum["ledger_ref"].(string))}},
				domain.ItemResult{Item: "R3", Status: domain.RequirementSatisfied, Values: []domain.EnvelopeValue{
					dateValue("deadline", due["date"].(string), due["ledger_ref"].(string))}})
		default:
			ref := record("mail_draft", `{"drafts":[{"id":"DRF-1","to":"CUS-004"},{"id":"DRF-2","to":"CUS-007"}]}`)
			return envelopeJSON(domain.ItemResult{Item: "R4", Status: domain.RequirementSatisfied, Values: []domain.EnvelopeValue{
				idValue("draft", "DRF-1", ref), idValue("draft_2", "DRF-2", ref)}})
		}
	}
	out, err := s.Execute(context.Background(), &pb.Handoff{Id: "x", Payload: &pb.Object{Data: []byte(
		"overdue customers, total overdue, the deadline 5 working days from today; draft a reminder for each but do not send")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sp.inputs) != 2 {
		t.Fatalf("two stages: want 2 planner calls, got %d", len(sp.inputs))
	}
	if sp.seen[0].Number != 1 || sp.seen[1].Number != 2 {
		t.Fatalf("stage numbers = %d, %d", sp.seen[0].Number, sp.seen[1].Number)
	}
	if sp.seen[0].CanAsk || sp.seen[1].Lookup == nil {
		t.Fatal("no conversation: the person cannot be asked; the ledger lookup is served")
	}
	st2 := sp.seen[1]
	for _, want := range []string{"computed calc_aggregate", "computed calc_date_add", "tool_result invoices_search", "requirement_checklist"} {
		if !strings.Contains(st2.LedgerIndex, want) {
			t.Errorf("stage 2's ledger index lacks %q:\n%s", want, st2.LedgerIndex)
		}
	}
	if !strings.Contains(st2.EarlierStages, "Results of stage 1") || !strings.Contains(st2.EarlierStages, "led_") {
		t.Fatalf("stage 2 must see stage 1's results by reference: %s", st2.EarlierStages)
	}
	if cs := sp.stages[1]; cs == nil || len(cs.Open) != 1 || cs.Open[0].ID != "R4" || cs.SettledAs["R2"] != domain.RequirementSatisfied {
		t.Fatalf("stage 2's checklist statuses = %+v", cs)
	}
	// The planner reads the ledger through the kernel tools under the run's scope.
	got := st2.Lookup(context.Background(), domain.LedgerLookup{Tool: "ledger_get", Arguments: json.RawMessage(`{"id":"` + invoicesRef + `","path":"invoices[0].id"}`)})
	if !strings.Contains(got, `"INV-1"`) {
		t.Fatalf("planner ledger_get = %s", got)
	}
	if got := st2.Lookup(context.Background(), domain.LedgerLookup{Tool: "calc_aggregate"}); !strings.HasPrefix(got, "refused") {
		t.Fatalf("the planner may only read: %s", got)
	}

	rep := requirementsOf(t, out)
	for _, id := range []string{"R1", "R2", "R3", "R4"} {
		if rep[id]["status"] != "satisfied" {
			t.Fatalf("%s = %v (%v)", id, rep[id], rep["_"])
		}
	}
	if rep["_"]["stages"].(int) > 2 || rep["_"]["stalled"] == true {
		t.Fatalf("stages = %v", rep["_"])
	}
	// The figures cite computed entries carrying their inputs.
	var res domain.ExecuteResult
	_ = json.Unmarshal([]byte(out.GetMetadata()[domain.HandoffKeyExecuteResult]), &res)
	var cov struct {
		Items []struct {
			Item   domain.RequirementItem `json:"item"`
			Values []struct {
				Name        string          `json:"name"`
				Value       json.RawMessage `json:"value"`
				EvidenceRef string          `json:"evidence_ref"`
			} `json:"values"`
		} `json:"items"`
	}
	_ = json.Unmarshal(res.Requirements, &cov)
	for _, it := range cov.Items {
		if it.Item.Type != domain.RequirementFigure {
			continue
		}
		e, _ := s.Ledger.Reader.Get(domain.WithScope(context.Background(), &domain.TagPredicate{Bypass: true}), it.Values[0].EvidenceRef)
		if e == nil || e.Kind != domain.LedgerComputed || !strings.Contains(string(e.Body), `"inputs"`) {
			t.Fatalf("%s's figure must cite a computed entry with its inputs: %+v", it.Item.ID, e)
		}
		if it.Item.ID == "R2" && string(it.Values[0].Value) != "1550.5" {
			t.Fatalf("total = %s", it.Values[0].Value)
		}
		if it.Item.ID == "R3" && string(it.Values[0].Value) != `"2026-11-16"` { // Fri 6 + 5 working days, 10th a holiday
			t.Fatalf("deadline = %s", it.Values[0].Value)
		}
	}
}

// A stage that changes no item stalls: the run ends without another planner call.
func TestStagedPlanner_AStallTerminates(t *testing.T) {
	s, sp, disp, _ := stagedServer(t, []domain.RequirementItem{{ID: "R1", Type: domain.RequirementAnswer, Text: "the permit status"}},
		planOf(domain.Step{Query: "find the permit", RequiredEffects: []string{"read"}, Answers: []string{"R1"}}))
	disp.answer = func(context.Context, string, func(string, string) string) []byte { return []byte("no idea") }
	out, err := s.Execute(context.Background(), &pb.Handoff{Id: "x", Payload: &pb.Object{Data: []byte("permit?")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sp.inputs) != 1 {
		t.Fatalf("a stall plans no further stage: %d planner calls", len(sp.inputs))
	}
	rep := requirementsOf(t, out)
	if rep["R1"]["status"] != "failed" || !strings.Contains(rep["R1"]["reason"].(string), "stalled") {
		t.Fatalf("R1 = %v", rep["R1"])
	}
}

// "Nothing more" is not "done": the open item closes as a gap with the reason.
func TestStagedPlanner_NothingMoreClosesTheOpenItemsAsGaps(t *testing.T) {
	s, sp, disp, _ := stagedServer(t, []domain.RequirementItem{
		{ID: "R1", Type: domain.RequirementAnswer, Text: "the customer"},
		{ID: "R2", Type: domain.RequirementAnswer, Text: "the contract"}},
		planOf(domain.Step{Query: "find the customer", RequiredEffects: []string{"read"}, Answers: []string{"R1"}}),
		func(context.Context, string) (*domain.ExecutionPlan, error) { return nil, domain.ErrNoFurtherStage },
	)
	disp.answer = func(_ context.Context, _ string, record func(string, string) string) []byte {
		ref := record("crm_get", `{"id":"CUS-004"}`)
		return envelopeJSON(domain.ItemResult{Item: "R1", Status: domain.RequirementSatisfied, Values: []domain.EnvelopeValue{idValue("customer", "CUS-004", ref)}})
	}
	out, err := s.Execute(context.Background(), &pb.Handoff{Id: "x", Payload: &pb.Object{Data: []byte("customer and contract")}})
	if err != nil {
		t.Fatal(err)
	}
	rep := requirementsOf(t, out)
	if len(sp.inputs) != 2 || rep["R1"]["status"] != "satisfied" || rep["R2"]["status"] != "failed" ||
		!strings.Contains(rep["R2"]["reason"].(string), "nothing more") {
		t.Fatalf("calls %d, coverage %v", len(sp.inputs), rep)
	}
}

// A contradictory request ("send them now" and "do not send"): the planner asks.
// On a path that reports questions the run stops before any step with a
// structured InputRequired; on the default path the planner is told the person
// cannot be asked and plans with stated assumptions.
func TestStagedPlanner_ContradictoryRequestProducesInputRequired(t *testing.T) {
	items := []domain.RequirementItem{
		{ID: "R1", Type: domain.RequirementWrite, Text: "Send the reminders now", Quote: "send the reminders now"},
		{ID: "R2", Type: domain.RequirementConstraint, Text: "Do not send anything", Quote: "do not send anything", Forbids: []string{"send"}},
	}
	ask := func(context.Context, string) (*domain.ExecutionPlan, error) {
		return nil, &domain.InputRequiredError{Reason: "R1 and R2 contradict", Questions: []domain.InputQuestion{{ID: "Q1",
			Question: "Should the reminders be sent now, or only drafted?", Kind: domain.QuestionConflict, Items: []string{"R1", "R2"},
			Options: []string{"send now", "draft only"}}}}
	}
	s, sp, disp, _ := stagedServer(t, items, ask)
	s.ExecCfg.Plan.AskBackOtherPaths = "report"
	out, err := s.Execute(context.Background(), &pb.Handoff{Id: "x", Payload: &pb.Object{Data: []byte("send the reminders now, but do not send anything")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(disp.calls) != 0 || len(sp.inputs) != 1 {
		t.Fatalf("nothing runs before the question: %d dispatches, %d planner calls", len(disp.calls), len(sp.inputs))
	}
	var ir domain.InputRequired
	if err := json.Unmarshal([]byte(out.GetMetadata()[domain.HandoffKeyInputRequired]), &ir); err != nil {
		t.Fatalf("no input-required metadata: %v", err)
	}
	if ir.Stage != 1 || ir.Round != 1 || len(ir.Questions) != 1 || ir.Questions[0].Kind != domain.QuestionConflict || ir.LedgerRef == "" || len(ir.Requirements) == 0 {
		t.Fatalf("input required = %+v", ir)
	}
	var res domain.ExecuteResult
	if json.Unmarshal([]byte(out.GetMetadata()[domain.HandoffKeyExecuteResult]), &res) != nil || res.InputRequired == nil || len(res.Requirements) == 0 {
		t.Fatalf("the structured result must carry the question and the coverage: %+v", res)
	}
	if !strings.Contains(string(out.Payload.Data), "INPUT REQUIRED") || !strings.Contains(string(out.Payload.Data), "only drafted") {
		t.Fatalf("payload = %s", out.Payload.Data)
	}
	if rep := requirementsOf(t, out); rep["R1"]["status"] != "open" {
		t.Fatalf("an asked item stays open for the answer: %v", rep["R1"])
	}

	// Default path (proceed): planned again, told the person cannot be asked.
	s2, sp2, disp2, _ := stagedServer(t, items, ask,
		func(ctx context.Context, _ string) (*domain.ExecutionPlan, error) {
			domain.StagePlanningFrom(ctx).AddAssumptions([]string{"draft only, because sending is forbidden"})
			return &domain.ExecutionPlan{Subject: "s", Steps: []domain.Step{{Query: "draft the reminders", RequiredEffects: []string{"write"}, Answers: []string{"R1"}}}}, nil
		})
	disp2.answer = func(context.Context, string, func(string, string) string) []byte { return []byte("drafted") }
	out2, err := s2.Execute(context.Background(), &pb.Handoff{Id: "y", Payload: &pb.Object{Data: []byte("send the reminders now, but do not send anything")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sp2.inputs) != 2 || !strings.Contains(sp2.inputs[1], "The person cannot be asked") || sp2.seen[1].CanAsk {
		t.Fatalf("the re-plan must be told the person cannot be asked: %q", sp2.inputs)
	}
	if out2.GetMetadata()[domain.HandoffKeyInputRequired] != "" {
		t.Fatal("the default path proceeds; it never returns a question")
	}
	var res2 domain.ExecuteResult
	_ = json.Unmarshal([]byte(out2.GetMetadata()[domain.HandoffKeyExecuteResult]), &res2)
	if !strings.Contains(string(res2.Requirements), "draft only, because sending is forbidden") {
		t.Fatalf("the assumption must be written into the checklist: %s", res2.Requirements)
	}
}

// Ask-back reaches the person only on the conversation path, with the flag on
// and rounds left (D5a.2, D5a.4).
func TestStagedPlanner_CanAskOnlyInAConversationWithRoundsLeft(t *testing.T) {
	s := minimalServer(t)
	conv := &stagedRun{conversation: true}
	if s.canAsk(conv) {
		t.Fatal("ask-back is off by default")
	}
	s.ExecCfg.Plan.AskBackEnabled = true
	if !s.canAsk(conv) || s.canAsk(&stagedRun{}) {
		t.Fatal("only the conversation path asks")
	}
	s.ExecCfg.Plan.AskBackMaxRounds = 2
	if s.canAsk(&stagedRun{conversation: true, round: askBackRound(map[string]string{domain.HandoffKeyAskBackRound: "2"})}) {
		t.Fatal("no questions once the rounds are spent")
	}
}

// The step records coverage reads are the step's own, of its plan generation:
// side-effecting calls and refused reads (T-20261108-coverage-cannot-accept-not-permitted-for-a,
// T-20261108-ledger-step-indices-collide-across-replans-in).
func TestStepActionsOf_DenialsAndGenerations(t *testing.T) {
	s := minimalServer(t)
	run := domain.RunID("run-1")
	s.planActions.begin(run)
	s.planActions.record(run, domain.PlanAction{Step: 0, Tool: "mail_draft", Status: domain.PlanActionDone})
	s.planActions.record(run, domain.PlanAction{Step: 0, Generation: 1, Tool: "mail_draft", Status: domain.PlanActionDone})
	s.planActions.recordDenial(run, domain.PlanAction{Step: 0, Generation: 1, Tool: "hr_get", Status: domain.PlanActionRefused})
	s.planActions.recordDenial("not-begun", domain.PlanAction{Step: 0, Tool: "hr_get"})
	recs := s.stepActionsOf(&run)(1, 0)
	if len(recs.Actions) != 1 || recs.Actions[0].Generation != 1 || len(recs.Denials) != 1 || recs.Denials[0].Tool != "hr_get" {
		t.Fatalf("generation 1 step 0 = %+v", recs)
	}
	if recs := s.stepActionsOf(&run)(0, 0); len(recs.Actions) != 1 || len(recs.Denials) != 0 {
		t.Fatalf("generation 0 step 0 = %+v", recs)
	}
	if got := s.planActions.deniedReads("not-begun"); len(got) != 0 {
		t.Fatal("a run not begun records no denial")
	}
	s.planActions.end(run)
	if got := s.planActions.deniedReads(run); len(got) != 0 {
		t.Fatal("end forgets the run's denials")
	}
}

// failingDispatcher fails every step whose query contains "fail".
type failingDispatcher struct{ *envelopeDispatcher }

func (d failingDispatcher) Execute(ctx context.Context, task *domain.DispatchTask, h *domain.Handoff) (*domain.DispatchResult, error) {
	if strings.Contains(task.Description, "fail") {
		return nil, errors.New("the invoice system timed out")
	}
	return d.envelopeDispatcher.Execute(ctx, task, h)
}

// T-20261108-replanner-replan-cap-does-not-emit-answers: under the checklist a
// failed step is not replanned through replan_cap (whose steps answer no item);
// the stage ends, what it settled stands, and the next coverage stage plans the
// open item with answers.
func TestCoverageStages_AFailedStepGoesToTheNextStageNotToReplanCap(t *testing.T) {
	plan1 := &domain.ExecutionPlan{Subject: "s", Steps: []domain.Step{
		{Query: "find the customer", Answers: []string{"R1"}},
		{Query: "fail to find the invoice", Answers: []string{"R2"}}}}
	plan2 := &domain.ExecutionPlan{Subject: "s", Steps: []domain.Step{{Query: "find the invoice", Answers: []string{"R2"}}}}
	s, mp, disp := checklistServer(t, []*domain.ExecutionPlan{plan1, plan2}, []domain.RequirementItem{
		{ID: "R1", Type: domain.RequirementAnswer, Text: "the customer"},
		{ID: "R2", Type: domain.RequirementAnswer, Text: "the invoice"}})
	s.ExecCfg.Plan.MaxReplanAttempts = 3 // would replan through replan_cap on the legacy path
	s.Dispatcher = failingDispatcher{disp}
	disp.answer = func(_ context.Context, q string, record func(string, string) string) []byte {
		if strings.Contains(q, "customer") {
			ref := record("crm_get", `{"id":"CUS-004"}`)
			return envelopeJSON(domain.ItemResult{Item: "R1", Status: domain.RequirementSatisfied, Values: []domain.EnvelopeValue{idValue("customer", "CUS-004", ref)}})
		}
		ref := record("ar_get", `{"invoice":"INV-77"}`)
		return envelopeJSON(domain.ItemResult{Item: "R2", Status: domain.RequirementSatisfied, Values: []domain.EnvelopeValue{idValue("invoice", "INV-77", ref)}})
	}
	out, err := s.Execute(context.Background(), &pb.Handoff{Id: "x", Payload: &pb.Object{Data: []byte("customer and invoice")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(mp.genCalls) != 0 {
		t.Fatalf("replan_cap must not run under the checklist: %d replan calls", len(mp.genCalls))
	}
	if len(mp.calls) != 2 {
		t.Fatalf("the first plan and one coverage stage: %d planner calls", len(mp.calls))
	}
	rep := requirementsOf(t, out)
	if rep["R1"]["status"] != "satisfied" || rep["R2"]["status"] != "satisfied" {
		t.Fatalf("coverage = %v", rep)
	}
}

// D5a.2 resume: the answer to an earlier question set carries the pending
// InputRequired's ledger id (kernel-stamped from the turn lease by Execute, which
// drops a caller-sent one); the first stage is told which questions it answers
// and where the earlier state is.
func TestStagedPlanner_AnswerToAnEarlierQuestionIsShownToTheFirstStage(t *testing.T) {
	s, _, _, _ := stagedServer(t, []domain.RequirementItem{{ID: "R1", Type: domain.RequirementAnswer, Text: "draft the reminders"}})
	body, _ := json.Marshal(domain.InputRequired{Version: 1, Stage: 1, Round: 1, Questions: []domain.InputQuestion{
		{ID: "Q1", Question: "Send now or only draft?", Kind: domain.QuestionConflict}}})
	ent, err := s.Ledger.Writer.Record(context.Background(), domain.LedgerRecord{Kind: domain.LedgerComputed,
		Provenance: domain.LedgerProvenance{RunID: "old", StepIndex: -1}, ToolName: "input_required",
		Writer: domain.SystemPrincipal, ContentType: "application/json", Body: body})
	if err != nil {
		t.Fatal(err)
	}
	r := s.newStagedRun(context.Background(), map[string]string{domain.HandoffKeyAskBackPendingRef: ent.ID,
		domain.HandoffKeyAskBackRound: "1"}, "run-2", "")
	sp := s.stagePlanning(context.Background(), r, 1)
	if r.round != 1 || !strings.Contains(sp.EarlierStages, "Send now or only draft?") || !strings.Contains(sp.EarlierStages, ent.ID) {
		t.Fatalf("stage 1 must be told which questions it answers (round %d): %q", r.round, sp.EarlierStages)
	}
	// A caller-sent pending ref never reaches the planner through Execute: the
	// kernel stamps it from the lease (ask_back_stamp.go).
	in := &pb.Handoff{Metadata: map[string]string{domain.HandoffKeyAskBackPendingRef: ent.ID}}
	stampAskBack(in, func(map[string]string) (domain.LeaseBinding, bool) { return domain.LeaseBinding{}, false })
	if in.Metadata[domain.HandoffKeyAskBackPendingRef] != "" {
		t.Fatal("a caller-sent pending ref must be dropped")
	}
}
