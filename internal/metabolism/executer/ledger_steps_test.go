package executer

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0145 D1 at the DAG executor: every step output is recorded, step_N_cid is
// its ledger id, and a dependant's upstream content is READ FROM THE LEDGER (the
// masterContext string is a view, not the source of truth).

// fakeRunLedger records step outputs and serves them back marked, so a test can
// tell content read from the ledger from content taken from masterContext.
type fakeRunLedger struct {
	mu      sync.Mutex
	recs    []domain.LedgerRecord
	bodies  map[string][]byte
	scopes  []*domain.TagPredicate
	missing bool // Get answers absent (a forbidden or lost entry)
}

func (f *fakeRunLedger) Record(_ context.Context, rec domain.LedgerRecord) (domain.LedgerEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bodies == nil {
		f.bodies = map[string][]byte{}
	}
	f.recs = append(f.recs, rec)
	id := domain.LedgerEntryID(rec.Provenance, domain.LedgerCallKey(rec.Kind, rec.ToolName, nil), string(rec.Body))
	f.bodies[id] = rec.Body
	return domain.LedgerEntry{ID: id}, nil
}

func (f *fakeRunLedger) Get(ctx context.Context, id string) (*domain.LedgerEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	eff, _ := domain.ScopeFromContext(ctx)
	f.scopes = append(f.scopes, eff)
	b, ok := f.bodies[id]
	if !ok || f.missing {
		return nil, nil
	}
	return &domain.LedgerEntry{ID: id, Body: append([]byte("LEDGER:"), b...)}, nil
}

func (f *fakeRunLedger) Find(context.Context, domain.LedgerQuery) ([]domain.LedgerEntry, error) {
	return nil, nil
}

func TestLedger_StepOutputsRecordedAndDependantsReadFromIt(t *testing.T) {
	led := &fakeRunLedger{}
	owner := &domain.TagPredicate{Bypass: true, Owners: []string{"alice"}}
	d := &DAGExecutor{Ledger: led, LedgerReader: led, LedgerReadScope: owner, CurrentRunID: "run-1"}
	var mu sync.Mutex
	seen := map[string]*domain.Handoff{}
	stepFn := func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
		q := string(h.Payload.Data)
		mu.Lock()
		seen[q] = h
		mu.Unlock()
		return &domain.Handoff{FromAgent: "worker", Payload: &domain.Payload{Data: []byte("result of " + q)}}, nil
	}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "fetch invoices"},
		{Query: "total them", DependsOn: []int{0}},
	}}
	ctx := domain.WithLedgerProvenance(t.Context(), domain.LedgerProvenance{ConversationID: "c", TurnID: "m", Owner: "alice", StepIndex: -1})
	master, err := d.Execute(ctx, plan, nil, stepFn)
	if err != nil {
		t.Fatal(err)
	}
	if len(led.recs) != 2 {
		t.Fatalf("recorded %d step outputs, want 2", len(led.recs))
	}
	for i, r := range led.recs {
		if r.Kind != domain.LedgerStepOutput || r.Provenance.RunID != "run-1" || r.Provenance.TurnID != "m" ||
			r.Provenance.Owner != "alice" || r.Writer != domain.AgentPrincipal("worker") {
			t.Fatalf("record %d: %+v", i, r)
		}
	}
	ref0 := master["step_0_cid"]
	if !domain.IsLedgerID(ref0) {
		t.Fatalf("step_0_cid = %q, want its ledger id", ref0)
	}
	deps := decodeManifest(t, seen["total them"])
	if len(deps) != 1 || deps[0].CID != ref0 || deps[0].Content != "LEDGER:result of fetch invoices" {
		t.Fatalf("dependant manifest = %+v, want the ledger's content and reference", deps)
	}
	// T-20261108-ledger-step-outputs-do-not-inherit-the: the dependant's output is
	// recorded as derived from its upstream entry (the recorder inherits its tags).
	if len(led.recs[0].DerivedFrom) != 0 || len(led.recs[1].DerivedFrom) != 1 || led.recs[1].DerivedFrom[0] != ref0 {
		t.Fatalf("DerivedFrom = %v / %v, want none for the root and [%s] for the dependant",
			led.recs[0].DerivedFrom, led.recs[1].DerivedFrom, ref0)
	}
	if len(led.scopes) == 0 || led.scopes[0] != owner {
		t.Fatalf("the executor must read under LedgerReadScope, got %v", led.scopes)
	}
	// The structured result's step index carries the references.
	refs := PlanStepRefsFrom(master)
	if len(refs) != 2 || refs[0].Ref != ref0 || refs[1].Status != domain.ExecuteStepCompleted || refs[1].Ref == "" {
		t.Fatalf("step refs = %+v", refs)
	}
}

// A ledger entry that cannot be read is reported to the dependant as unavailable,
// with the reason — never replaced by the in-memory string.
func TestLedger_UnreadableUpstreamIsUnavailableNotTheMasterString(t *testing.T) {
	led := &fakeRunLedger{missing: true}
	d := &DAGExecutor{Ledger: led, LedgerReader: led}
	var got *domain.Handoff
	stepFn := func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
		if string(h.Payload.Data) == "b" {
			got = h
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("out")}}, nil
	}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "a"}, {Query: "b", DependsOn: []int{0}}}}
	if _, err := d.Execute(t.Context(), plan, nil, stepFn); err != nil {
		t.Fatal(err)
	}
	deps := decodeManifest(t, got)
	if deps[0].Status != domain.StepDependencyUnavailable || deps[0].Content != "" || !strings.Contains(deps[0].Error, "not readable") {
		t.Fatalf("dep = %+v", deps[0])
	}
}

// A fan-out child depends on its source and receives its whole item object on the
// handoff (ADR-0145 D6.3).
func TestFanOutChild_ReceivesItemObjectAndSourceResult(t *testing.T) {
	d := &DAGExecutor{}
	var mu sync.Mutex
	children := map[string]*domain.Handoff{}
	stepFn := func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
		q := string(h.Payload.Data)
		if strings.HasPrefix(q, "list overdue") {
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte(
				`[{"customer_id":"C-4","amount":1250.5},{"customer_id":"C-7","amount":80}]`)}}, nil
		}
		mu.Lock()
		children[q] = h
		mu.Unlock()
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")}}, nil
	}
	src := 0
	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "list overdue customers"},
		{Query: "remind {item}", FanOutOver: &src, RequiredEffects: []string{"read"}},
	}}
	if _, err := d.Execute(t.Context(), plan, nil, stepFn); err != nil {
		t.Fatal(err)
	}
	h := children["remind C-4"]
	if h == nil {
		t.Fatalf("children ran: %v", children)
	}
	var item map[string]any
	if err := json.Unmarshal([]byte(h.Context[domain.HandoffKeyFanOutItem]), &item); err != nil || item["amount"] != 1250.5 {
		t.Fatalf("child item = %q (%v)", h.Context[domain.HandoffKeyFanOutItem], err)
	}
	deps := decodeManifest(t, h)
	if len(deps) != 1 || deps[0].Step != 0 || !strings.Contains(deps[0].Content, "C-7") {
		t.Fatalf("child must depend on the source and receive its result, manifest = %+v", deps)
	}
}
