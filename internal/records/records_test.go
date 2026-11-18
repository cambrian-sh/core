package records

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ── fakes: a plane that records the scope every read was given ───────────────

type fakeCatalog struct {
	p     *fakePlane
	scope *domain.TagPredicate
}

func (c fakeCatalog) Overview(context.Context) (domain.SubstrateOverview, error) {
	return domain.SubstrateOverview{}, nil
}
func (c fakeCatalog) Entities(_ context.Context, f domain.EntityFilter) ([]domain.CatalogEntity, error) {
	c.p.scopes = append(c.p.scopes, c.scope)
	c.p.filters = append(c.p.filters, f)
	var out []domain.CatalogEntity
	for _, e := range c.p.entities {
		if len(f.IDs) > 0 && e.ID != f.IDs[0] {
			continue
		}
		if len(f.Substrings) > 0 && !strings.Contains(strings.ToLower(e.ID), strings.ToLower(f.Substrings[0])) {
			continue
		}
		out = append(out, e)
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}
func (c fakeCatalog) Predicates(context.Context, int) ([]string, error) {
	c.p.scopes = append(c.p.scopes, c.scope)
	return []string{"status", "promised_delivery_date"}, nil
}
func (c fakeCatalog) EventTypes(context.Context, int) ([]string, error) {
	c.p.scopes = append(c.p.scopes, c.scope)
	return []string{"order_shipped"}, nil
}
func (c fakeCatalog) ItemKinds(context.Context, int) ([]string, error) { return nil, nil }
func (c fakeCatalog) Actors(context.Context, []string, int) ([]string, error) {
	return nil, nil
}
func (c fakeCatalog) Links(context.Context, domain.LinkListFilter) (domain.LinkPage, error) {
	return domain.LinkPage{}, nil
}
func (c fakeCatalog) Events(context.Context, domain.EventListFilter) (domain.EventPage, error) {
	return domain.EventPage{}, nil
}
func (c fakeCatalog) EntityHandles(context.Context, domain.EntityHandleFilter) (domain.EntityHandlePage, error) {
	return domain.EntityHandlePage{}, nil
}

type fakePlane struct {
	entities []domain.CatalogEntity
	points   map[string]domain.QueryRow // entity|predicate → row
	events   []domain.QueryRow
	err      error
	scopes   []*domain.TagPredicate
	queries  []domain.KnowledgeQuery
	filters  []domain.EntityFilter
}

func (p *fakePlane) Query(_ context.Context, q domain.KnowledgeQuery, scope *domain.TagPredicate) (domain.QueryResult, error) {
	p.scopes = append(p.scopes, scope)
	p.queries = append(p.queries, q)
	if p.err != nil {
		return domain.QueryResult{}, p.err
	}
	if err := q.Validate(); err != nil {
		return domain.QueryResult{}, err
	}
	switch q.Kind {
	case domain.QueryPoint:
		if r, ok := p.points[q.EntityID+"|"+q.Predicate]; ok {
			return domain.QueryResult{Guarantee: q.Guarantee(), Rows: []domain.QueryRow{r}}, nil
		}
	case domain.QueryEvents:
		return domain.QueryResult{Guarantee: q.Guarantee(), Rows: p.events}, nil
	}
	return domain.QueryResult{Guarantee: q.Guarantee()}, nil
}

func (p *fakePlane) Catalog(scope *domain.TagPredicate) domain.SubstrateCatalog {
	return fakeCatalog{p: p, scope: scope}
}

type fakeObjects struct {
	listed []domain.ObjectsQuery
	scopes []*domain.TagPredicate
	view   *domain.ObjectView
}

func (o *fakeObjects) List(_ context.Context, scope *domain.TagPredicate, q domain.ObjectsQuery) (domain.ObjectsResult, error) {
	o.scopes = append(o.scopes, scope)
	o.listed = append(o.listed, q)
	return domain.ObjectsResult{Kind: q.Kind, Rows: []domain.ObjectRow{{EntityID: "DEV-7",
		Values: []domain.ObjectValue{{Property: "severity", Value: domain.StatementValue{Type: "text", Text: "major"}}}}}}, nil
}

func (o *fakeObjects) Get(_ context.Context, scope *domain.TagPredicate, _, id string) (domain.ObjectView, error) {
	o.scopes = append(o.scopes, scope)
	if o.view == nil || o.view.EntityID != id {
		return domain.ObjectView{}, domain.ErrObjectNotFound
	}
	return *o.view, nil
}

var shipped = time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)

func orderPlane() *fakePlane {
	return &fakePlane{
		entities: []domain.CatalogEntity{{ID: "ORD-1042", Predicates: []string{"status", "promised_delivery_date"}, LastSeen: shipped}},
		points: map[string]domain.QueryRow{
			"ORD-1042|status": {"entity_id": "ORD-1042", "predicate": "status", "value_type": "text",
				"value_text": "shipped", "occurred_at": shipped, "evidence_id": "ev-1"},
			"ORD-1042|promised_delivery_date": {"entity_id": "ORD-1042", "predicate": "promised_delivery_date",
				"value_type": "date", "value_date": time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC), "occurred_at": shipped},
		},
		events: []domain.QueryRow{
			{"event_id": "e1", "event_type": "order_placed", "occurred_at": shipped.Add(-48 * time.Hour), "roles": map[string]string{"order": "ORD-1042", "customer": "CUS-001"}},
			{"event_id": "e2", "event_type": "order_shipped", "occurred_at": shipped, "roles": map[string]string{"order": "ORD-1042"}},
		},
	}
}

func call(t *testing.T, kt domain.KernelTool, ctx context.Context, args string) (map[string]any, error) {
	t.Helper()
	out, err := kt.Call(ctx, domain.KernelToolCall{AgentID: "chat_agent", ArgsJSON: []byte(args)})
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("result is not JSON: %v: %s", err, out)
	}
	return m, nil
}

func mustNew(t *testing.T, d Deps) domain.KernelTool {
	t.Helper()
	kt, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	return kt
}

var rep = &domain.TagPredicate{AnyOfClauses: [][]string{{"scope-orders"}}, PartyScopedTags: []string{"scope-orders"}, PartyIdentities: []string{"emp:EMP-001"}}

// ── tests ────────────────────────────────────────────────────────────────────

func TestNew_RequiresThePlane(t *testing.T) {
	if _, err := New(Deps{}); err == nil {
		t.Fatal("a tool without the query plane must not be built")
	}
}

func TestSpec_IsAReadToolWithAValidSchema(t *testing.T) {
	s := mustNew(t, Deps{Plane: &fakePlane{}}).Spec()
	if s.Name != ToolName || len(s.Effects) != 1 || s.Effects[0] != domain.EffectRead {
		t.Fatalf("spec = %s %v, want company_records [read]", s.Name, s.Effects)
	}
	if s.SideEffecting() {
		t.Fatal("company_records must not be side-effecting")
	}
	var schema map[string]any
	if err := json.Unmarshal(s.Schema, &schema); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	if !strings.Contains(s.Description, "where is order") {
		t.Error("the description must tell the model to use it for record questions")
	}
}

// No predicate on ctx: refused before anything is read (fail closed).
func TestCall_RefusesWithoutAScopeOnCtx(t *testing.T) {
	p := orderPlane()
	kt := mustNew(t, Deps{Plane: p})
	if _, err := call(t, kt, context.Background(), `{"action":"get","id":"ORD-1042"}`); err == nil {
		t.Fatal("a call with no read scope must be refused")
	}
	if len(p.scopes) != 0 {
		t.Fatalf("the plane was read %d times without a scope", len(p.scopes))
	}
	nilScope := domain.WithScope(context.Background(), nil)
	if _, err := call(t, kt, nilScope, `{"action":"get","id":"ORD-1042"}`); err == nil {
		t.Fatal("a present-but-nil scope must be refused")
	}
}

// get: the latest value of each attribute plus the events, every read under the
// exact predicate that was on ctx (AGENTS.md §2.4).
func TestGet_ReturnsAttributesAndEventsUnderTheCallersScope(t *testing.T) {
	p := orderPlane()
	objs := &fakeObjects{}
	kt := mustNew(t, Deps{Plane: p, Objects: objs})
	m, err := call(t, kt, domain.WithScope(context.Background(), rep), `{"action":"get","id":"ORD-1042"}`)
	if err != nil {
		t.Fatal(err)
	}
	if m["found"] != true {
		t.Fatalf("found = %v: %v", m["found"], m)
	}
	body, _ := json.Marshal(m)
	for _, want := range []string{`"value":"shipped"`, `"value":"2026-11-05T00:00:00Z"`, `"type":"order_shipped"`, `"as_of":"2026-11-02T09:30:00Z"`, `"guarantee"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("answer lacks %s: %s", want, body)
		}
	}
	for i, s := range append(p.scopes, objs.scopes...) {
		if s != rep {
			t.Fatalf("read %d ran under %+v, not the caller's predicate", i, s)
		}
	}
	for _, q := range p.queries {
		if q.IncludeRetired {
			t.Fatal("company_records must never ask for retired rows")
		}
	}
}

// A record the caller cannot see (the fenced plane returns nothing) reads as not
// found, with only near ids the caller CAN read.
func TestGet_HiddenReadsAsNotFound(t *testing.T) {
	p := orderPlane()
	p.events = nil
	kt := mustNew(t, Deps{Plane: p})
	m, err := call(t, kt, domain.WithScope(context.Background(), rep), `{"action":"get","id":"ORD-9999"}`)
	if err != nil {
		t.Fatal(err)
	}
	if m["found"] != false || m["record"] != nil {
		t.Fatalf("hidden record answered: %v", m)
	}
	m, err = call(t, kt, domain.WithScope(context.Background(), rep), `{"action":"get","id":"1042"}`)
	if err != nil {
		t.Fatal(err)
	}
	if ids, _ := m["similar_ids"].([]any); len(ids) != 1 || ids[0] != "ORD-1042" {
		t.Fatalf("a near id the caller can read should be offered: %v", m)
	}
}

func TestGet_IncludesTheObjectPageWhenTheRecordIsAnObject(t *testing.T) {
	p := orderPlane()
	objs := &fakeObjects{view: &domain.ObjectView{EntityID: "ORD-1042", Kind: "order",
		Properties: []domain.ObjectProperty{{Property: "carrier", Value: domain.StatementValue{Type: "text", Text: "DHL"}}},
		Lifecycle:  &domain.ObjectLifecycle{State: "in_transit"}}}
	kt := mustNew(t, Deps{Plane: p, Objects: objs})
	m, err := call(t, kt, domain.WithScope(context.Background(), rep), `{"action":"get","id":"ORD-1042"}`)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(m)
	if !strings.Contains(string(body), `"carrier":"DHL"`) || !strings.Contains(string(body), `"state":"in_transit"`) {
		t.Fatalf("object page missing: %s", body)
	}
}

func TestFind_ByFragmentAttributeAndEventType(t *testing.T) {
	p := orderPlane()
	kt := mustNew(t, Deps{Plane: p, MaxRows: 5})
	ctx := domain.WithScope(context.Background(), rep)
	m, err := call(t, kt, ctx, `{"action":"find","id":"ord-10"}`)
	if err != nil {
		t.Fatal(err)
	}
	if m["count"].(float64) != 1 {
		t.Fatalf("find by fragment: %v", m)
	}
	if _, err := call(t, kt, ctx, `{"action":"find","event_type":"order_shipped","limit":99}`); err != nil {
		t.Fatal(err)
	}
	last := p.filters[len(p.filters)-1]
	if last.EventType != "order_shipped" || last.Limit != 6 {
		t.Fatalf("filter = %+v, want event type and limit bounded to max_rows+1", last)
	}
	if _, err := call(t, kt, ctx, `{"action":"find"}`); err == nil {
		t.Fatal("find with no filter must be refused (no unbounded dump)")
	}
	if _, err := call(t, kt, ctx, `{"action":"find","attribute":"status","event_type":"x"}`); err == nil {
		t.Fatal("attribute and event_type together cannot be expressed")
	}
}

func TestFind_TruncatesAtMaxRows(t *testing.T) {
	p := &fakePlane{}
	for i := 0; i < 4; i++ {
		p.entities = append(p.entities, domain.CatalogEntity{ID: fmt.Sprintf("ORD-%d", i)})
	}
	kt := mustNew(t, Deps{Plane: p, MaxRows: 3})
	m, err := call(t, kt, domain.WithScope(context.Background(), rep), `{"action":"find","id":"ORD"}`)
	if err != nil {
		t.Fatal(err)
	}
	if m["count"].(float64) != 3 || m["truncated"] != true {
		t.Fatalf("want 3 rows truncated: %v", m)
	}
}

func TestFind_ObjectsOfAKindWithTypedWhere(t *testing.T) {
	objs := &fakeObjects{}
	kt := mustNew(t, Deps{Plane: &fakePlane{}, Objects: objs})
	ctx := domain.WithScope(context.Background(), rep)
	m, err := call(t, kt, ctx, `{"action":"find","kind":"deviation","where":[`+
		`{"property":"severity","op":"eq","value":"major"},{"property":"cost","op":"gt","value":100},`+
		`{"property":"opened","op":"ge","value":"2026-11-01T00:00:00Z","value_type":"date"},{"property":"closed","op":"is_null"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	q := objs.listed[0]
	if q.Kind != "deviation" || len(q.Where) != 4 || q.Where[0].Values[0].Type != "text" ||
		q.Where[1].Values[0].Type != "number" || q.Where[2].Values[0].Type != "date" || len(q.Where[3].Values) != 0 {
		t.Fatalf("objects query = %+v", q)
	}
	if objs.scopes[0] != rep || m["count"].(float64) != 1 {
		t.Fatalf("objects listed under %+v: %v", objs.scopes[0], m)
	}
	if _, err := call(t, mustNew(t, Deps{Plane: &fakePlane{}}), ctx, `{"action":"find","kind":"deviation"}`); err == nil {
		t.Fatal("find with kind and no objects reader must say so")
	}
}

func TestEvents_FiltersByTypeAndKeepsTheNewest(t *testing.T) {
	p := orderPlane()
	p.events = append(p.events, domain.QueryRow{"event_id": "e3", "event_type": "order_shipped", "occurred_at": shipped.Add(time.Hour)})
	kt := mustNew(t, Deps{Plane: p})
	ctx := domain.WithScope(context.Background(), rep)
	m, err := call(t, kt, ctx, `{"action":"events","id":"ORD-1042","event_type":"order_shipped","limit":1}`)
	if err != nil {
		t.Fatal(err)
	}
	evs := m["events"].([]any)
	if len(evs) != 1 || evs[0].(map[string]any)["event_id"] != "e3" || m["truncated"] != true {
		t.Fatalf("want only the newest shipped event, truncated: %v", m)
	}
	if _, err := call(t, kt, ctx, `{"action":"events","id":"ORD-1042","from":"yesterday"}`); err == nil {
		t.Fatal("an unparseable from must be refused, not dropped")
	}
	q := p.queries[0]
	if q.Kind != domain.QueryEvents || q.From.IsZero() || q.To.IsZero() {
		t.Fatalf("events query = %+v", q)
	}
}

func TestTypes_ListsNamesUnderTheScope(t *testing.T) {
	p := orderPlane()
	m, err := call(t, mustNew(t, Deps{Plane: p}), domain.WithScope(context.Background(), rep), `{"action":"types"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(m["attributes"].([]any)) != 2 || len(m["event_types"].([]any)) != 1 {
		t.Fatalf("types: %v", m)
	}
	for _, s := range p.scopes {
		if s != rep {
			t.Fatal("catalog read under another predicate")
		}
	}
}

func TestCall_DenialIsWordedWithoutDetail(t *testing.T) {
	p := orderPlane()
	p.err = fmt.Errorf("%w: no policy", domain.ErrQueryDenied)
	p.entities = nil
	_, err := call(t, mustNew(t, Deps{Plane: p}), domain.WithScope(context.Background(), rep), `{"action":"get","id":"ORD-1042"}`)
	if err == nil || !strings.Contains(err.Error(), "may not read") || strings.Contains(err.Error(), "no policy") {
		t.Fatalf("denial = %v", err)
	}
	_, err = call(t, mustNew(t, Deps{Plane: orderPlane()}), domain.WithScope(context.Background(), rep), `{"action":"drop"}`)
	if err == nil || errors.Is(err, domain.ErrQueryDenied) {
		t.Fatalf("unknown action: %v", err)
	}
}
