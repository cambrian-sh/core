package executer

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/requirements"
)

// ADR-0145 D5/D6 at the DAG executor (T-20261108-adr-0145-phase-2-requirement-checklist-with).

// runLedger is an in-memory ledger: Record stores (step outputs and, via
// addToolResult, a worker's tool results) and Get/Find read them back with bodies.
type runLedger struct {
	mu   sync.Mutex
	rows map[string]domain.LedgerEntry
	seq  int
}

func newRunLedger() *runLedger { return &runLedger{rows: map[string]domain.LedgerEntry{}} }

func (l *runLedger) put(e domain.LedgerEntry) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	if e.ID == "" {
		e.ID = domain.LedgerEntryID(domain.LedgerProvenance{RunID: e.RunID, StepIndex: e.StepIndex},
			domain.LedgerCallKey(e.Kind, e.ToolName, e.Args), string(e.Body)+string(rune(l.seq)))
	}
	l.rows[e.ID] = e
	return e.ID
}

func (l *runLedger) Record(_ context.Context, rec domain.LedgerRecord) (domain.LedgerEntry, error) {
	id := l.put(domain.LedgerEntry{Kind: rec.Kind, RunID: rec.Provenance.RunID, StepIndex: rec.Provenance.StepIndex,
		ToolName: rec.ToolName, Body: rec.Body})
	return domain.LedgerEntry{ID: id}, nil
}

func (l *runLedger) addToolResult(run string, step int, tool, body string) string {
	return l.put(domain.LedgerEntry{Kind: domain.LedgerToolResult, RunID: run, StepIndex: step, ToolName: tool, Body: []byte(body)})
}

func (l *runLedger) Get(_ context.Context, id string) (*domain.LedgerEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.rows[id]
	if !ok {
		return nil, nil
	}
	return &e, nil
}

func (l *runLedger) Find(_ context.Context, q domain.LedgerQuery) ([]domain.LedgerEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []domain.LedgerEntry
	for _, e := range l.rows {
		if (q.RunID != "" && e.RunID != q.RunID) || (q.HasStep && e.StepIndex != q.StepIndex) || (q.Kind != "" && e.Kind != q.Kind) {
			continue
		}
		e.Body = nil // Find returns metadata only, like the real store
		out = append(out, e)
	}
	return out, nil
}

func envelope(t *testing.T, text string, results ...domain.ItemResult) []byte {
	t.Helper()
	b, err := json.Marshal(domain.AnswerEnvelope{AnswerText: text, ItemResults: results})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func value(name, typ string, v any, ref string) domain.EnvelopeValue {
	raw, _ := json.Marshal(v)
	return domain.EnvelopeValue{Name: name, Type: typ, Value: raw, EvidenceRef: ref}
}

func stateOf(cov *requirements.Coverage, id string) requirements.ItemState {
	for _, it := range cov.Snapshot().Items {
		if it.Item.ID == id {
			return it
		}
	}
	return requirements.ItemState{}
}

// o3x-shaped: step 0 fetches the overdue invoices with a tool (its figures live
// in that tool result); step 1, which answers "which customers" and "the total",
// cites step 0's tool result. The figures reach the item results with their
// evidence, and the items are satisfied in code.
func TestEnvelope_O3xFiguresFromStep0ReachTheItemResults(t *testing.T) {
	led := newRunLedger()
	cl := &domain.RequirementChecklist{Items: []domain.RequirementItem{
		{ID: "R1", Type: domain.RequirementAnswer, Text: "which customers are more than 60 days overdue"},
		{ID: "R2", Type: domain.RequirementFigure, Text: "the total outstanding"},
	}}
	cov := requirements.NewCoverage(*cl)
	d := &DAGExecutor{Ledger: led, LedgerReader: led, CurrentRunID: "run-1",
		Requirements: NewStepRequirements(cl, cov, nil)}
	var aging string
	var seen1 *domain.Handoff
	stepFn := func(_ context.Context, i int, h *domain.Handoff) (*domain.Handoff, error) {
		if i == 0 {
			if _, has := h.Context[domain.HandoffKeyAnswerItems]; has {
				t.Error("a gathering step that answers no item must not be asked for the envelope")
			}
			aging = led.addToolResult("run-1", 0, "ar_aging",
				`{"invoices":[{"customer_id":"CUS-004","amount":"1.250,50","days":75},{"customer_id":"CUS-007","amount":80,"days":64}],"total":1330.5}`)
			return &domain.Handoff{FromAgent: "finance", Payload: &domain.Payload{Data: []byte(`CUS-004 1.250,50; CUS-007 80`)}}, nil
		}
		seen1 = h
		return &domain.Handoff{FromAgent: "writer", Payload: &domain.Payload{Data: envelope(t, "CUS-004 and CUS-007 owe 1330.50 in total",
			domain.ItemResult{Item: "R1", Status: domain.RequirementSatisfied, Values: []domain.EnvelopeValue{
				value("customer", domain.ValueID, "CUS-004", aging), value("customer", domain.ValueID, "CUS-007", aging)}},
			domain.ItemResult{Item: "R2", Status: domain.RequirementSatisfied, Values: []domain.EnvelopeValue{
				value("total", domain.ValueMoney, "1330.50", aging), value("amount_cus004", domain.ValueMoney, 1250.5, aging)}},
		)}}, nil
	}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "use ar_aging to list invoices more than 60 days overdue", RequiredEffects: []string{"read"}},
		{Query: "state the customers and the total", DependsOn: []int{0}, Answers: []string{"R1", "R2"}, Inputs: []string{"s0"}},
	}}
	if _, err := d.Execute(context.Background(), plan, nil, stepFn); err != nil {
		t.Fatal(err)
	}
	var items []domain.RequirementItem
	if err := json.Unmarshal([]byte(seen1.Context[domain.HandoffKeyAnswerItems]), &items); err != nil || len(items) != 2 {
		t.Fatalf("step 1 must be told its items: %q (%v)", seen1.Context[domain.HandoffKeyAnswerItems], err)
	}
	if !strings.Contains(seen1.Context[domain.HandoffKeyStepInputs], `"binding":"s0"`) {
		t.Fatalf("step 1 must receive its bound input: %q", seen1.Context[domain.HandoffKeyStepInputs])
	}
	r1, r2 := stateOf(cov, "R1"), stateOf(cov, "R2")
	if r1.Status != domain.RequirementSatisfied || len(r1.Values) != 2 {
		t.Fatalf("R1 = %+v", r1)
	}
	if r2.Status != domain.RequirementSatisfied || len(r2.Values) != 2 || r2.Values[0].EvidenceRef != aging {
		t.Fatalf("R2 must carry step 0's figures with their evidence: %+v", r2)
	}
}

// An invalid envelope gets exactly one repair turn, told the violations; a
// figure that is not in its evidence never reaches the coverage.
func TestEnvelope_OneRepairTurnThenTheItemStaysOpenWithTheReason(t *testing.T) {
	led := newRunLedger()
	cl := &domain.RequirementChecklist{Items: []domain.RequirementItem{{ID: "R1", Type: domain.RequirementFigure, Text: "the total"}}}
	cov := requirements.NewCoverage(*cl)
	d := &DAGExecutor{Ledger: led, LedgerReader: led, CurrentRunID: "run-1", Requirements: NewStepRequirements(cl, cov, nil)}
	calls := 0
	var repairNote string
	stepFn := func(_ context.Context, i int, h *domain.Handoff) (*domain.Handoff, error) {
		calls++
		ref := led.addToolResult("run-1", 0, "ar_total", `{"total": 1330.5}`)
		repairNote = h.Context[domain.HandoffKeyEnvelopeViolations]
		// Both attempts state a total the tool never returned (model arithmetic).
		return &domain.Handoff{Payload: &domain.Payload{Data: envelope(t, "1400",
			domain.ItemResult{Item: "R1", Status: domain.RequirementSatisfied, Values: []domain.EnvelopeValue{
				value("total", domain.ValueMoney, "1400.00", ref)}})}}, nil
	}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "total", Answers: []string{"R1"}, RequiredEffects: []string{"read"}}}}
	if _, err := d.Execute(context.Background(), plan, nil, stepFn); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("want the step and exactly one repair turn, got %d calls", calls)
	}
	if !strings.Contains(repairNote, domain.ViolationNotInEvidence) {
		t.Fatalf("the repair turn must name the violation, got %q", repairNote)
	}
	if st := stateOf(cov, "R1"); st.Status != domain.RequirementOpen || len(st.Values) != 0 || !strings.Contains(st.Reason, "does not appear") {
		t.Fatalf("R1 = %+v", st)
	}
}

// for_each through a fan-out: each child answers for its member; a member whose
// child produced no accepted result keeps the item open, naming the member.
func TestEnvelope_ForEachWithAMissingMemberKeepsTheItemOpen(t *testing.T) {
	led := newRunLedger()
	cl := &domain.RequirementChecklist{Items: []domain.RequirementItem{{ID: "R1", Type: domain.RequirementAnswer, Text: "a reminder per customer"}}}
	cov := requirements.NewCoverage(*cl)
	d := &DAGExecutor{Ledger: led, LedgerReader: led, CurrentRunID: "run-1", Requirements: NewStepRequirements(cl, cov, nil)}
	var mu sync.Mutex
	stepFn := func(_ context.Context, i int, h *domain.Handoff) (*domain.Handoff, error) {
		q := string(h.Payload.Data)
		if strings.HasPrefix(q, "list") {
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte(`["CUS-004","CUS-007"]`)}}, nil
		}
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(q, "CUS-007") {
			// This child never produced evidence: a prose answer.
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("could not reach the mail system")}}, nil
		}
		ref := led.addToolResult("run-1", i, "mail_draft", `{"id":"MSG-41","to":"CUS-004"}`)
		return &domain.Handoff{Payload: &domain.Payload{Data: envelope(t, "drafted",
			domain.ItemResult{Item: "R1", Status: domain.RequirementSatisfied, Values: []domain.EnvelopeValue{
				value("draft", domain.ValueID, "MSG-41", ref)}})}}, nil
	}
	src := 0
	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "list overdue customers", RequiredEffects: []string{"read"}},
		{Query: "draft a reminder for {item}", DependsOn: []int{0}, FanOutOver: &src, ForEach: "s0",
			Answers: []string{"R1"}, RequiredEffects: []string{"write"}},
	}}
	if _, err := d.Execute(context.Background(), plan, nil, stepFn); err != nil {
		t.Fatal(err)
	}
	st := stateOf(cov, "R1")
	if st.Status != domain.RequirementOpen || !strings.Contains(st.Reason, "CUS-007") || !st.Covered["CUS-004"] {
		t.Fatalf("R1 = %+v", st)
	}
	next := cov.NextStage(2)
	if len(next.Open) != 1 || len(next.Open[0].Missing) != 1 || next.Open[0].Missing[0] != "CUS-007" {
		t.Fatalf("the next stage covers exactly the missing member: %+v", next)
	}
}

// Legacy path: without Requirements no item key is stamped and a prose answer
// passes untouched.
func TestEnvelope_LegacyPathIsUntouched(t *testing.T) {
	var seen *domain.Handoff
	d := &DAGExecutor{}
	stepFn := func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
		seen = h
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("prose")}}, nil
	}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "q", Answers: []string{"R1"}}}}
	master, err := d.Execute(context.Background(), plan, map[string]string{domain.HandoffKeyAnswerItems: "spoof"}, stepFn)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := seen.Context[domain.HandoffKeyAnswerItems]; has {
		t.Fatal("a caller-supplied item key must not reach the step")
	}
	if master["step_0_result"] != "prose" {
		t.Fatalf("result = %q", master["step_0_result"])
	}
}
