package calc

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/ledger"
)

// ADR-0145 D4 (T-20261108-adr-0145-d4-kernel-calculation-tools-working): the
// calculation tools are exact, read their inputs by reference through the
// enforced ledger, and record every result as a computed entry with its inputs.

type memStore struct {
	mu   sync.Mutex
	rows map[string]domain.LedgerEntry
}

func (m *memStore) Put(_ context.Context, e domain.LedgerEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[e.ID]; !ok {
		m.rows[e.ID] = e
	}
	return nil
}
func (m *memStore) Get(_ context.Context, id string) (*domain.LedgerEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.rows[id]
	if !ok {
		return nil, nil
	}
	return &e, nil
}
func (m *memStore) Find(_ context.Context, q domain.LedgerQuery) ([]domain.LedgerEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.LedgerEntry
	for _, e := range m.rows {
		if (q.RunID != "" && e.RunID != q.RunID) || (q.Kind != "" && e.Kind != q.Kind) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}
func (m *memStore) DeleteScratch(context.Context, string) (int, error)   { return 0, nil }
func (m *memStore) PurgeExpired(context.Context, time.Time) (int, error) { return 0, nil }

// fixture: Friday 2026-11-06; Republic Day 10-29 recurring, 2026-11-10 dated.
func fixture(t *testing.T) (map[string]domain.KernelTool, *memStore, domain.LedgerWriter, context.Context) {
	t.Helper()
	work, err := NewWorkCalendar(nil, []string{"10-29", "2026-11-10"})
	if err != nil {
		t.Fatal(err)
	}
	store := &memStore{rows: map[string]domain.LedgerEntry{}}
	w := ledger.NewRecorder(store, nil, ledger.RecorderConfig{})
	r := authz.NewEnforcingLedger(store, nil)
	cal := domain.NewCalendar(time.UTC, func() time.Time { return time.Date(2026, 11, 6, 15, 0, 0, 0, time.UTC) })
	tools := map[string]domain.KernelTool{}
	for _, kt := range Tools(Deps{Calendar: cal, Work: work, Reader: r, Writer: w}) {
		tools[kt.Spec().Name] = kt
	}
	ctx := domain.WithLedgerProvenance(context.Background(), domain.LedgerProvenance{RunID: "run-1", StepIndex: 2, TurnID: "turn-1", ConversationID: "c1"})
	ctx = domain.WithScope(ctx, &domain.TagPredicate{Bypass: true})
	return tools, store, w, ctx
}

func call(t *testing.T, ctx context.Context, kt domain.KernelTool, args string) map[string]any {
	t.Helper()
	out, err := kt.Call(ctx, domain.KernelToolCall{AgentID: "worker", ArgsJSON: []byte(args)})
	if err != nil {
		t.Fatalf("%s(%s): %v", kt.Spec().Name, args, err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestWorkingDays_SkipWeekendsAndHolidays(t *testing.T) {
	tools, _, _, ctx := fixture(t)
	// Mon 2026-11-02 .. Fri 2026-11-13: 10 weekdays, 2026-11-10 is a holiday.
	m := call(t, ctx, tools[ToolWorkingDays], `{"start":"2026-11-02","end":"2026-11-13"}`)
	if m["working_days"].(float64) != 9 || m["calendar_days"].(float64) != 11 {
		t.Fatalf("working days = %v, calendar days = %v", m["working_days"], m["calendar_days"])
	}
	if hol := m["holidays"].([]any); len(hol) != 1 || hol[0] != "2026-11-10" {
		t.Fatalf("holidays = %v", m["holidays"])
	}
	// The recurring holiday (10-29, a Thursday in 2026) and excluded ends.
	m = call(t, ctx, tools[ToolWorkingDays], `{"start":"2026-10-28","end":"2026-10-30","include_start":false}`)
	if m["working_days"].(float64) != 1 {
		t.Fatalf("28 (excluded) .. 30 with 29 a holiday = %v", m["working_days"])
	}
	// Day.month.year as Turkish records write it; end defaults to today.
	m = call(t, ctx, tools[ToolWorkingDays], `{"start":"02.11.2026"}`)
	if m["working_days"].(float64) != 5 || m["end"] != "2026-11-06" {
		t.Fatalf("02.11.2026 .. today = %v (end %v)", m["working_days"], m["end"])
	}
}

func TestDateAdd_WorkingDaysAndCalendarUnits(t *testing.T) {
	tools, _, _, ctx := fixture(t)
	// Fri 2026-11-06 + 3 working days: Mon 9, (Tue 10 holiday), Wed 11, Thu 12.
	m := call(t, ctx, tools[ToolDateAdd], `{"working_days":3}`)
	if m["date"] != "2026-11-12" || m["weekday"] != "Thursday" {
		t.Fatalf("today + 3 working days = %v %v", m["date"], m["weekday"])
	}
	if hol := m["holidays_skipped"].([]any); len(hol) != 1 || hol[0] != "2026-11-10" {
		t.Fatalf("holidays_skipped = %v", m["holidays_skipped"])
	}
	m = call(t, ctx, tools[ToolDateAdd], `{"date":"2026-01-31","months":1}`)
	if m["date"] != "2026-03-03" { // Go AddDate normalisation, stated not hidden
		t.Fatalf("2026-01-31 + 1 month = %v", m["date"])
	}
	m = call(t, ctx, tools[ToolDateAdd], `{"date":"2026-11-12","working_days":-3}`)
	if m["date"] != "2026-11-06" {
		t.Fatalf("2026-11-12 - 3 working days = %v", m["date"])
	}
	if _, err := tools[ToolDateAdd].Call(ctx, domain.KernelToolCall{AgentID: "w", ArgsJSON: []byte(`{"working_days":1,"days":2}`)}); err == nil {
		t.Fatal("working_days with days must be refused")
	}
}

func TestDateWindow_FromTheKernelCalendar(t *testing.T) {
	tools, _, _, ctx := fixture(t)
	cases := []struct{ args, start, end string }{
		{`{"window":"last_week"}`, "2026-10-26", "2026-11-01"},
		{`{"window":"this_quarter"}`, "2026-10-01", "2026-12-31"},
		{`{"window":"last_month"}`, "2026-10-01", "2026-10-31"},
		{`{"window":"last_n_days","n":14}`, "2026-10-23", "2026-11-06"},
		{`{"window":"n_days_ago","n":30}`, "2026-10-07", "2026-10-07"},
		{`{"window":"this_week","anchor":"2026-11-10"}`, "2026-11-09", "2026-11-15"},
	}
	for _, c := range cases {
		m := call(t, ctx, tools[ToolDateWindow], c.args)
		if m["start"] != c.start || m["end"] != c.end {
			t.Errorf("%s = %v..%v, want %s..%s", c.args, m["start"], m["end"], c.start, c.end)
		}
	}
	m := call(t, ctx, tools[ToolDateWindow], `{"window":"this_month"}`)
	// November 2026: 21 weekdays, minus the 10th.
	if m["working_days"].(float64) != 20 {
		t.Fatalf("working days in November 2026 = %v", m["working_days"])
	}
	if _, err := tools[ToolDateWindow].Call(ctx, domain.KernelToolCall{AgentID: "w", ArgsJSON: []byte(`{"window":"last_n_days"}`)}); err == nil {
		t.Fatal("last_n_days without n must be refused")
	}
}

// recordInvoices puts an o3x-shaped tool result in the ledger: an MCP result
// whose text content is the JSON list of open invoices.
func recordInvoices(t *testing.T, ctx context.Context, w domain.LedgerWriter) string {
	t.Helper()
	inv := `[{"id":"INV-1","customer":"CUS-004","amount":"1.250,50 TL","due":"2026-10-01"},` +
		`{"id":"INV-2","customer":"CUS-007","amount":80,"due":"2026-11-20"},` +
		`{"id":"INV-3","customer":"CUS-004","amount":"300","due":"2026-09-15"},` +
		`{"id":"INV-4","customer":"CUS-009","amount":"n/a","due":"2026-10-20"}]`
	text, _ := json.Marshal(inv)
	body := `{"content":[{"type":"text","text":` + string(text) + `}]}`
	p := domain.LedgerProvenanceFromContext(ctx)
	p.StepIndex = 0
	e, err := w.Record(ctx, domain.LedgerRecord{Kind: domain.LedgerToolResult, Provenance: p, ToolName: "invoices_search",
		Writer: domain.AgentPrincipal("worker"), ContentType: "application/json", Body: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	return e.ID
}

func TestAggregate_OverLedgerRowsByReference_RecordsAComputedEntry(t *testing.T) {
	tools, store, w, ctx := fixture(t)
	ref := recordInvoices(t, ctx, w)
	args := `{"ref":"` + ref + `","path":"content[0].text","op":"sum","field":"amount",` +
		`"where":[{"field":"due","op":"before","value":"2026-11-06"}]}`
	m := call(t, ctx, tools[ToolAggregate], args)
	// Overdue: INV-1 (1250.50), INV-3 (300), INV-4 (not a number: skipped, listed).
	if m["result"].(float64) != 1550.5 || m["matched"].(float64) != 3 || m["used"].(float64) != 2 {
		t.Fatalf("sum = %v matched %v used %v", m["result"], m["matched"], m["used"])
	}
	if sk := m["skipped"].([]any); len(sk) != 1 {
		t.Fatalf("the non-numeric amount must be listed as skipped: %v", m["skipped"])
	}
	led, _ := m["ledger_ref"].(string)
	e, _ := store.Get(ctx, led)
	if e == nil || e.Kind != domain.LedgerComputed || e.ToolName != ToolAggregate || e.RunID != "run-1" || e.StepIndex != 2 {
		t.Fatalf("computed entry = %+v", e)
	}
	var body struct {
		Inputs    map[string]any `json:"inputs"`
		InputRefs []string       `json:"input_refs"`
		Result    map[string]any `json:"result"`
	}
	if err := json.Unmarshal(e.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Inputs["ref"] != ref || len(body.InputRefs) != 1 || body.InputRefs[0] != ref || body.Result["result"].(float64) != 1550.5 {
		t.Fatalf("the computed entry must carry its inputs and result: %s", e.Body)
	}
	// The deep evidence check accepts the figure against the computed entry.
	v := domain.EnvelopeValue{Name: "total", Type: domain.ValueMoney, Value: json.RawMessage(`1550.5`), EvidenceRef: led}
	if !domain.EvidenceContains(v, e.Body) {
		t.Fatal("a figure citing the computed entry must pass the deep check")
	}

	// group_by and count.
	m = call(t, ctx, tools[ToolAggregate], `{"ref":"`+ref+`","path":"content[0].text","op":"count","group_by":"customer"}`)
	groups := m["groups"].([]any)
	if len(groups) != 3 || groups[0].(map[string]any)["key"] != "CUS-004" || groups[0].(map[string]any)["result"].(float64) != 2 {
		t.Fatalf("groups = %v", groups)
	}
	// An identifier never compares as a number ("CUS-004" is not 4).
	m = call(t, ctx, tools[ToolAggregate], `{"ref":"`+ref+`","path":"content[0].text","op":"list","where":[{"field":"customer","op":"eq","value":"INV-004"}]}`)
	if m["matched"].(float64) != 0 {
		t.Fatalf("CUS-004 must not equal INV-004: %v", m)
	}
}

func TestAggregate_RefusesWhatTheCallerCannotRead(t *testing.T) {
	tools, _, w, ctx := fixture(t)
	ref := recordInvoices(t, ctx, w)
	// A predicate that admits nothing the entry carries: the reference reads as
	// absent, exactly like ledger_get.
	deny := domain.WithScope(ctx, &domain.TagPredicate{RequiredTags: []string{"hr-only"}})
	_, err := tools[ToolAggregate].Call(deny, domain.KernelToolCall{AgentID: "w", ArgsJSON: []byte(`{"ref":"` + ref + `","path":"content[0].text","op":"count"}`)})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("an unreadable reference must read as absent, got %v", err)
	}
}

func TestCompare_ThresholdsAndRules(t *testing.T) {
	tools, _, w, ctx := fixture(t)
	ref := recordInvoices(t, ctx, w)
	m := call(t, ctx, tools[ToolCompare], `{"comparisons":[`+
		`{"name":"over limit","left":{"ref":"`+ref+`","path":"content[0].text[0].amount"},"op":"gt","right":1000},`+
		`{"name":"in band","left":"80","op":"between","right":50,"right_max":100},`+
		`{"name":"overdue","left":{"ref":"`+ref+`","path":"content[0].text[1].due"},"op":"before","right":"2026-11-06"}]}`)
	cs := m["comparisons"].([]any)
	if cs[0].(map[string]any)["holds"] != true || cs[0].(map[string]any)["difference"].(float64) != 250.5 {
		t.Fatalf("1.250,50 TL > 1000: %v", cs[0])
	}
	if cs[1].(map[string]any)["holds"] != true {
		t.Fatalf("80 between 50 and 100: %v", cs[1])
	}
	if cs[2].(map[string]any)["holds"] != false || m["all_hold"] != false || m["any_hold"] != true {
		t.Fatalf("2026-11-20 is not before 2026-11-06: %v", m)
	}
	if m["ledger_ref"] == "" {
		t.Fatal("the comparison must be recorded")
	}
}

func TestTools_WithoutLedger_ComputeButDoNotRecord(t *testing.T) {
	var kt domain.KernelTool
	for _, x := range Tools(Deps{}) {
		if x.Spec().Name == ToolAggregate {
			kt = x
		}
	}
	m := call(t, context.Background(), kt, `{"values":[1,2,"3,5"],"op":"sum"}`)
	if m["result"].(float64) != 6.5 {
		t.Fatalf("sum = %v", m["result"])
	}
	if _, has := m["ledger_ref"]; has {
		t.Fatal("no writer: nothing recorded, no ledger_ref")
	}
	if _, err := kt.Call(context.Background(), domain.KernelToolCall{ArgsJSON: []byte(`{"ref":"led_x","op":"sum"}`)}); err == nil {
		t.Fatal("a reference without the ledger must be refused")
	}
}
