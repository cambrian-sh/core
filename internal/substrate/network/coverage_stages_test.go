package network

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/ledger"
)

// ADR-0145 D5.5 at the server (T-20261108-adr-0145-phase-2-requirement-checklist-with):
// the run continues in coverage stages while items change, stops when every item
// is final, and a stage that changes nothing is a stall that closes the open items
// as gaps.

// memLedgerStore is an in-memory domain.LedgerStore for server tests.
type memLedgerStore struct {
	mu   sync.Mutex
	rows map[string]domain.LedgerEntry
}

func (m *memLedgerStore) Put(_ context.Context, e domain.LedgerEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[e.ID]; !ok {
		m.rows[e.ID] = e
	}
	return nil
}
func (m *memLedgerStore) Get(_ context.Context, id string) (*domain.LedgerEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.rows[id]
	if !ok {
		return nil, nil
	}
	return &e, nil
}
func (m *memLedgerStore) Find(_ context.Context, q domain.LedgerQuery) ([]domain.LedgerEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.LedgerEntry
	for _, e := range m.rows {
		if (q.RunID != "" && e.RunID != q.RunID) || (q.HasStep && e.StepIndex != q.StepIndex) ||
			(q.Kind != "" && e.Kind != q.Kind) || (q.ToolName != "" && e.ToolName != q.ToolName) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}
func (m *memLedgerStore) DeleteScratch(context.Context, string) (int, error)   { return 0, nil }
func (m *memLedgerStore) PurgeExpired(context.Context, time.Time) (int, error) { return 0, nil }

type fixedChecklist struct{ cl domain.RequirementChecklist }

func (f fixedChecklist) Derive(context.Context, string) domain.RequirementChecklist { return f.cl }

// envelopeDispatcher answers each step from a script keyed by the step's query.
type envelopeDispatcher struct {
	mu     sync.Mutex
	writer domain.LedgerWriter
	answer func(ctx context.Context, query string, record func(tool, body string) string) []byte
	calls  []string
}

func (d *envelopeDispatcher) Execute(ctx context.Context, task *domain.DispatchTask, h *domain.Handoff) (*domain.DispatchResult, error) {
	d.mu.Lock()
	d.calls = append(d.calls, task.Description)
	d.mu.Unlock()
	record := func(tool, body string) string {
		p := domain.LedgerProvenanceFromContext(ctx)
		p.StepIndex = atoiOr(h.Context["_step_index"], -1)
		e, err := d.writer.Record(ctx, domain.LedgerRecord{Kind: domain.LedgerToolResult, Provenance: p, ToolName: tool,
			Writer: domain.AgentPrincipal("worker"), ContentType: "application/json", Body: []byte(body)})
		if err != nil {
			panic(err)
		}
		return e.ID
	}
	return &domain.DispatchResult{Handoff: &domain.Handoff{FromAgent: "worker",
		Payload: &domain.Payload{Data: d.answer(ctx, task.Description, record)}}, Confidence: 0.9}, nil
}

func (d *envelopeDispatcher) CallAgent(context.Context, string, *domain.Handoff, string) (*domain.Handoff, error) {
	return nil, nil
}

func atoiOr(s string, def int) int {
	n := 0
	if s == "" {
		return def
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func checklistServer(t *testing.T, plans []*domain.ExecutionPlan, items []domain.RequirementItem) (*Server, *mockPlanner, *envelopeDispatcher) {
	t.Helper()
	s := minimalServer(t)
	resp := make([]plannerResponse, 0, len(plans))
	for _, p := range plans {
		resp = append(resp, plannerResponse{plan: p})
	}
	mp := &mockPlanner{responses: resp}
	s.Planner = mp
	store := &memLedgerStore{rows: map[string]domain.LedgerEntry{}}
	w := ledger.NewRecorder(store, nil, ledger.RecorderConfig{})
	s.Ledger = &LedgerAccess{Writer: w, Reader: authz.NewEnforcingLedger(store, nil)}
	s.ExecCfg.Plan.ChecklistEnabled = true
	s.Checklist = fixedChecklist{cl: domain.RequirementChecklist{Request: "r", Items: items}}
	disp := &envelopeDispatcher{writer: w}
	s.Dispatcher = disp
	return s, mp, disp
}

func envelopeJSON(results ...domain.ItemResult) []byte {
	b, _ := json.Marshal(domain.AnswerEnvelope{AnswerText: "done", ItemResults: results})
	return b
}

func idValue(name, v, ref string) domain.EnvelopeValue {
	raw, _ := json.Marshal(v)
	return domain.EnvelopeValue{Name: name, Type: domain.ValueID, Value: raw, EvidenceRef: ref}
}

func requirementsOf(t *testing.T, h *pb.Handoff) map[string]map[string]any {
	t.Helper()
	var res domain.ExecuteResult
	if err := json.Unmarshal([]byte(h.GetMetadata()[domain.HandoffKeyExecuteResult]), &res); err != nil {
		t.Fatalf("no structured result: %v", err)
	}
	var rep struct {
		Items []struct {
			Item   domain.RequirementItem `json:"item"`
			Status string                 `json:"status"`
			Reason string                 `json:"reason"`
		} `json:"items"`
		Stages  int  `json:"stages"`
		Settled bool `json:"settled"`
		Stalled bool `json:"stalled"`
	}
	if err := json.Unmarshal(res.Requirements, &rep); err != nil {
		t.Fatalf("no requirements in the structured result: %v (%s)", err, res.Requirements)
	}
	out := map[string]map[string]any{"_": {"stages": rep.Stages, "stalled": rep.Stalled}}
	for _, it := range rep.Items {
		out[it.Item.ID] = map[string]any{"status": it.Status, "reason": it.Reason}
	}
	return out
}

// A stage that changes no item is a stall: the run ends (one planner call, no
// further stage) and the open item closes as a gap with the reason.
func TestCoverageStages_AStallTerminates(t *testing.T) {
	plan := &domain.ExecutionPlan{Subject: "s", Steps: []domain.Step{{Query: "answer it", Answers: []string{"R1"}}}}
	s, mp, disp := checklistServer(t, []*domain.ExecutionPlan{plan},
		[]domain.RequirementItem{{ID: "R1", Type: domain.RequirementAnswer, Text: "the permit status"}})
	disp.answer = func(context.Context, string, func(string, string) string) []byte {
		return []byte("I could not find it.") // prose, never an envelope
	}
	out, err := s.Execute(context.Background(), &pb.Handoff{Id: "x", Payload: &pb.Object{Data: []byte("what is the permit status?")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(mp.calls) != 1 {
		t.Fatalf("a stall must not plan another stage; planner called %d times", len(mp.calls))
	}
	if len(disp.calls) != 2 {
		t.Fatalf("the step and its one repair turn, got %d dispatches", len(disp.calls))
	}
	rep := requirementsOf(t, out)
	if rep["R1"]["status"] != "failed" || !strings.Contains(rep["R1"]["reason"].(string), "stalled") || rep["_"]["stalled"] != true {
		t.Fatalf("R1 = %v (%v)", rep["R1"], rep["_"])
	}
	if !strings.Contains(string(out.Payload.Data), "REQUIREMENT COVERAGE") {
		t.Fatalf("the text payload must report the coverage: %s", out.Payload.Data)
	}
}

// A stage that settles one item but not the other makes progress: the next stage
// is planned for the open item only, and the run ends when every item is final.
func TestCoverageStages_ProgressPlansTheNextStageUntilSettled(t *testing.T) {
	plan1 := &domain.ExecutionPlan{Subject: "s", Steps: []domain.Step{{Query: "find the customer", Answers: []string{"R1", "R2"}}}}
	plan2 := &domain.ExecutionPlan{Subject: "s", Steps: []domain.Step{{Query: "find the invoice", Answers: []string{"R2"}}}}
	s, mp, disp := checklistServer(t, []*domain.ExecutionPlan{plan1, plan2}, []domain.RequirementItem{
		{ID: "R1", Type: domain.RequirementAnswer, Text: "the customer"},
		{ID: "R2", Type: domain.RequirementAnswer, Text: "the invoice"}})
	disp.answer = func(_ context.Context, q string, record func(string, string) string) []byte {
		if strings.Contains(q, "customer") {
			ref := record("crm_get", `{"id":"CUS-004"}`)
			return envelopeJSON(domain.ItemResult{Item: "R1", Status: domain.RequirementSatisfied,
				Values: []domain.EnvelopeValue{idValue("customer", "CUS-004", ref)}},
				domain.ItemResult{Item: "R2", Status: domain.RequirementFailed, Reason: "the invoice tool timed out"})
		}
		ref := record("ar_get", `{"invoice":"INV-77"}`)
		return envelopeJSON(domain.ItemResult{Item: "R2", Status: domain.RequirementSatisfied,
			Values: []domain.EnvelopeValue{idValue("invoice", "INV-77", ref)}})
	}
	out, err := s.Execute(context.Background(), &pb.Handoff{Id: "x", Payload: &pb.Object{Data: []byte("customer and invoice?")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(mp.calls) != 2 {
		t.Fatalf("want the first plan and one coverage stage, got %d planner calls", len(mp.calls))
	}
	rep := requirementsOf(t, out)
	if rep["R1"]["status"] != "satisfied" || rep["R2"]["status"] != "satisfied" || rep["_"]["stalled"] == true {
		t.Fatalf("coverage = %v", rep)
	}
}

// ADR-0145 D7: with execution.plan.plan_validator on, a plan the validator rejects
// goes back to the planner with the structured violations, and the repaired plan
// runs.
func TestPlanChecked_ValidatorViolationsGoBackToThePlanner(t *testing.T) {
	bad := &domain.ExecutionPlan{Subject: "s", Steps: []domain.Step{
		{Query: "list invoices due 2031-02-30"}, {Query: "b", Inputs: []string{"s0.list"}}}}
	good := &domain.ExecutionPlan{Subject: "s", Steps: []domain.Step{{Query: "list invoices"}}}
	s := minimalServer(t)
	mp := &mockPlanner{responses: []plannerResponse{{plan: bad}, {plan: good}}}
	s.ExecCfg.Plan.PlanValidator = true
	got, err := s.planChecked(context.Background(), mp, "list my invoices")
	if err != nil || got != good {
		t.Fatalf("got %+v %v", got, err)
	}
	if len(mp.calls) != 2 || !strings.Contains(mp.calls[1], "<plan_violations>") ||
		!strings.Contains(mp.calls[1], "structure.binding") || !strings.Contains(mp.calls[1], "dates.unsourced_date") {
		t.Fatalf("the repair call must carry the structured violations: %q", mp.calls)
	}
	// Flag off: the legacy DAG-only check accepts the same plan as it is.
	s.ExecCfg.Plan.PlanValidator = false
	mp2 := &mockPlanner{responses: []plannerResponse{{plan: bad}}}
	if got, err := s.planChecked(context.Background(), mp2, "x"); err != nil || got != bad || len(mp2.calls) != 1 {
		t.Fatalf("legacy path: %+v %v", got, err)
	}
}
