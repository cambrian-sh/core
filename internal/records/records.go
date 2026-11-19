// Package records is the agent-facing read tool over the company's typed records
// (ENG-2117, owner decision 2026-11-18): the observations, events and objects an
// ingress mapped from the company's own systems (orders, deviations, tickets),
// looked up and listed by id, type and simple filters.
//
// It is a kernel tool (domain.KernelTool, the ADR-0145 ledger_get / calc_*
// pattern): every agent reaches it through the one ExecuteTool path with its
// lease-resolved provenance; it needs no grant and is on every menu because it
// changes nothing and reads only what the caller may already read. Before this,
// the typed query plane was reachable only from the operator plane and the
// published MCP tools, so a person asking the chat agent "where is order X?"
// got nothing back however much they were allowed to see.
//
// ENFORCEMENT. The tool resolves no access itself. ToolExecutor.runKernelTool
// resolves the CALLER's read predicate from the decision point (ReadFilter for
// the agent principal on the turn's surface; for a first-party or system agent
// acting for a lease-resolved person that is the person's predicate, premium
// ADR-0091 option C, ENG-2107) and puts it on ctx. This tool takes that same
// predicate off ctx and hands it positionally to the fenced query plane, its
// catalog and the objects reader (AGENTS.md §2.4: scope travels both ways). Row
// filtering stays inside those readers (sqlfence / guardNodes, AGENTS.md §2.5);
// nothing here tests a row. No predicate on ctx refuses (fail closed). Hidden
// and absent read the same: a record the caller may not see is "not found".
//
// SCOPE. Look-ups and listings only: no aggregates, joins, comparisons or
// decision logic (those stay with the parked operations-brain layers L2–L6).
// Retired rows are never returned (IncludeRetired is operator-plane only).
//
// Wired by app/company_records.go under execution.tools.company_records.
package records

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ToolName is the tool's reserved name (kernel tools resolve before the registry).
const ToolName = "company_records"

// Actions.
const (
	ActionTypes  = "types"
	ActionFind   = "find"
	ActionGet    = "get"
	ActionEvents = "events"
)

// Defaults for Deps bounds (execution.tools.company_records; 0 ⇒ these).
const (
	DefaultMaxRows       = 50
	DefaultMaxEvents     = 50
	DefaultMaxAttributes = 40
)

// The events window when the caller gives none: every stored occurrence. The
// query plane's events shape needs a window; an unbounded ask is expressed as
// one wider than any company's records rather than as a special case.
var (
	allTimeFrom = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
	allTimeTo   = time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)
)

// TypedPlane is the slice of the fenced query plane the tool calls
// (postgres.PgQueryPlane): the closed AST and its names catalog, both taking the
// caller's predicate as a required parameter.
type TypedPlane interface {
	Query(ctx context.Context, q domain.KnowledgeQuery, scope *domain.TagPredicate) (domain.QueryResult, error)
	Catalog(scope *domain.TagPredicate) domain.SubstrateCatalog
}

// ObjectReader is the slice of the objects reader (internal/objects.Reader) the
// tool calls: ADR-0149 D6 list and D7 get, fenced per property by scope.
type ObjectReader interface {
	List(ctx context.Context, scope *domain.TagPredicate, q domain.ObjectsQuery) (domain.ObjectsResult, error)
	Get(ctx context.Context, scope *domain.TagPredicate, namespace, entityID string) (domain.ObjectView, error)
}

// Deps are what the tool reads.
type Deps struct {
	// Plane is the one fenced query plane. Required.
	Plane TypedPlane
	// Objects reads declared object kinds; nil ⇒ the object parts are skipped.
	Objects ObjectReader
	// MaxRows bounds a find listing, MaxEvents the events one answer returns,
	// MaxAttributes the attributes one get reads (0 ⇒ the defaults above).
	MaxRows       int
	MaxEvents     int
	MaxAttributes int
}

// New returns the tool, or an error when the query plane is missing (a tool on
// the menu that can only fail would teach the model it does not work).
func New(d Deps) (domain.KernelTool, error) {
	if d.Plane == nil {
		return nil, errors.New("company_records: the typed query plane is required")
	}
	if d.MaxRows <= 0 {
		d.MaxRows = DefaultMaxRows
	}
	if d.MaxRows > domain.MaxQueryLimit {
		d.MaxRows = domain.MaxQueryLimit
	}
	if d.MaxEvents <= 0 {
		d.MaxEvents = DefaultMaxEvents
	}
	if d.MaxEvents > domain.MaxQueryLimit {
		d.MaxEvents = domain.MaxQueryLimit
	}
	if d.MaxAttributes <= 0 {
		d.MaxAttributes = DefaultMaxAttributes
	}
	return &tool{d: d}, nil
}

type tool struct{ d Deps }

// Spec is the menu entry. The description is written for the calling model: it
// says WHEN to use the tool ("where is order X?") as much as how.
func (t *tool) Spec() domain.SystemTool {
	return domain.SystemTool{
		Name: ToolName,
		Description: "Look up the company's own operational records — orders, shipments, deviations, " +
			"complaints, tickets and other records the company's systems report — by id, type or simple " +
			"filters. Use it for questions about a specific record (\"where is order ORD-1042?\", \"what is the " +
			"status of deviation DEV-7?\", \"what happened to this shipment?\") and to list records of one type. " +
			"Actions: get (one record: the latest value of each attribute it has, e.g. status or promised date, " +
			"and the events it took part in, oldest first); find (record ids matching an id fragment, having an " +
			"attribute, or taking part in an event type; or objects of a declared kind with simple where " +
			"filters); events (one record's events, optionally of one type or within from/to); types (which " +
			"attributes and event types exist, to learn the names). You only ever see records the person you " +
			"are working for may see; a record that is hidden reads exactly like one that does not exist, so " +
			"say you found no such record rather than guessing. Values are the latest stored values, not " +
			"verified world truth; quote dates and statuses exactly as returned. No totals or comparisons: " +
			"use the calc tools on the returned rows for those.",
		Schema: []byte(`{"type":"object","properties":{` +
			`"action":{"type":"string","enum":["get","find","events","types"],"description":"get one record, find records, list one record's events, or list the types that exist"},` +
			`"id":{"type":"string","description":"get/events: the record id exactly as the company uses it (e.g. ORD-1042); find: a fragment of ids to match"},` +
			`"attribute":{"type":"string","description":"find: only records that have this attribute (e.g. status)"},` +
			`"event_type":{"type":"string","description":"find: only records that took part in an event of this type; events: only events of this type"},` +
			`"kind":{"type":"string","description":"find: list objects of this declared object kind instead (with where)"},` +
			`"where":{"type":"array","maxItems":8,"description":"find with kind: conditions on the kind's properties, ANDed","items":{"type":"object","properties":{` +
			`"property":{"type":"string"},"op":{"type":"string","enum":["eq","ne","lt","le","gt","ge","prefix","is_null"]},` +
			`"value":{"description":"a string, number or RFC3339 date"},"value_type":{"type":"string","enum":["text","number","date","entity"],"description":"the property's type when the value alone does not say it"}},` +
			`"required":["property","op"],"additionalProperties":false}},` +
			`"from":{"type":"string","description":"events: RFC3339 window start (inclusive)"},` +
			`"to":{"type":"string","description":"events: RFC3339 window end (exclusive)"},` +
			`"limit":{"type":"integer","minimum":1,"description":"maximum rows (bounded by the deployment)"}},` +
			`"required":["action"],"additionalProperties":false}`),
		Effects: []domain.ToolEffect{domain.EffectRead},
	}
}

type whereArg struct {
	Property  string          `json:"property"`
	Op        string          `json:"op"`
	Value     json.RawMessage `json:"value"`
	ValueType string          `json:"value_type"`
}

type args struct {
	Action    string     `json:"action"`
	ID        string     `json:"id"`
	Attribute string     `json:"attribute"`
	EventType string     `json:"event_type"`
	Kind      string     `json:"kind"`
	Where     []whereArg `json:"where"`
	From      string     `json:"from"`
	To        string     `json:"to"`
	Limit     int        `json:"limit"`
}

// Call runs one action under the caller's predicate.
func (t *tool) Call(ctx context.Context, call domain.KernelToolCall) ([]byte, error) {
	scope, ok := domain.ScopeFromContext(ctx)
	if !ok || scope == nil {
		// runKernelTool always puts the caller's predicate here; reaching this
		// without one is a wiring fault, and the safe answer is nothing.
		return nil, fmt.Errorf("%s: no read scope on this call; refusing rather than reading unscoped", ToolName)
	}
	var a args
	raw := call.ArgsJSON
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("%s: arguments are not a JSON object: %v", ToolName, err)
	}
	a.Action = strings.ToLower(strings.TrimSpace(a.Action))
	a.ID = strings.TrimSpace(a.ID)
	var out map[string]any
	var err error
	switch a.Action {
	case ActionTypes:
		out, err = t.types(ctx, scope, a)
	case ActionFind:
		out, err = t.find(ctx, scope, a)
	case ActionGet:
		out, err = t.get(ctx, scope, a)
	case ActionEvents:
		out, err = t.events(ctx, scope, a)
	default:
		return nil, fmt.Errorf("%s: action must be one of get, find, events, types", ToolName)
	}
	if err != nil {
		return nil, refusal(err)
	}
	out["action"] = a.Action
	return json.Marshal(out)
}

// refusal words a reader error for the calling model. A denial and an
// unexpressible request are told apart (retry differently vs give up); neither
// says anything about what exists.
func refusal(err error) error {
	switch {
	case errors.Is(err, domain.ErrQueryDenied), errors.Is(err, domain.ErrQueryScopeMissing):
		return fmt.Errorf("%s: you may not read company records for this person", ToolName)
	case errors.Is(err, domain.ErrCannotExpress), errors.Is(err, domain.ErrObjectsInvalid):
		return fmt.Errorf("%s: %v", ToolName, err)
	}
	if strings.HasPrefix(err.Error(), ToolName+":") {
		return err
	}
	return fmt.Errorf("%s: reading the records failed: %v", ToolName, err)
}

func (t *tool) limit(asked, bound int) int {
	if asked <= 0 || asked > bound {
		return bound
	}
	return asked
}

// ─── types ───────────────────────────────────────────────────────────────────

func (t *tool) types(ctx context.Context, scope *domain.TagPredicate, a args) (map[string]any, error) {
	cat := t.d.Plane.Catalog(scope)
	n := t.limit(a.Limit, t.d.MaxRows)
	preds, err := cat.Predicates(ctx, n)
	if err != nil {
		return nil, err
	}
	evTypes, err := cat.EventTypes(ctx, n)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"attributes":  nonNil(preds),
		"event_types": nonNil(evTypes),
		"note":        "names over records you may read; use find with attribute or event_type to list records of a type",
	}, nil
}

// ─── find ────────────────────────────────────────────────────────────────────

func (t *tool) find(ctx context.Context, scope *domain.TagPredicate, a args) (map[string]any, error) {
	if strings.TrimSpace(a.Kind) != "" {
		return t.findObjects(ctx, scope, a)
	}
	if len(a.Where) > 0 {
		return nil, fmt.Errorf("%s: where filters apply to objects of a declared kind; pass kind too", ToolName)
	}
	attr, evType := strings.TrimSpace(a.Attribute), strings.TrimSpace(a.EventType)
	if a.ID == "" && attr == "" && evType == "" {
		return nil, fmt.Errorf("%s: find needs id (a fragment), attribute, event_type or kind", ToolName)
	}
	if attr != "" && evType != "" {
		return nil, fmt.Errorf("%s: find takes attribute or event_type, not both", ToolName)
	}
	n := t.limit(a.Limit, t.d.MaxRows)
	f := domain.EntityFilter{Predicate: attr, EventType: evType, Limit: n + 1, PredicateLimit: t.d.MaxAttributes}
	if a.ID != "" {
		f.Substrings = []string{a.ID}
	}
	ents, err := t.d.Plane.Catalog(scope).Entities(ctx, f)
	if err != nil {
		return nil, err
	}
	truncated := len(ents) > n
	if truncated {
		ents = ents[:n]
	}
	rows := make([]map[string]any, 0, len(ents))
	for _, e := range ents {
		r := map[string]any{"id": e.ID, "attributes": nonNil(e.Predicates)}
		if !e.LastSeen.IsZero() {
			r["last_seen"] = e.LastSeen.UTC().Format(time.RFC3339)
		}
		rows = append(rows, r)
	}
	out := map[string]any{"records": rows, "count": len(rows), "truncated": truncated}
	if truncated {
		out["note"] = "more records match; narrow the id fragment or the type"
	}
	return out, nil
}

func (t *tool) findObjects(ctx context.Context, scope *domain.TagPredicate, a args) (map[string]any, error) {
	if t.d.Objects == nil {
		return nil, fmt.Errorf("%s: this deployment declares no object kinds; use find with id, attribute or event_type", ToolName)
	}
	q := domain.ObjectsQuery{Kind: strings.TrimSpace(a.Kind), PageSize: t.limit(a.Limit, min(t.d.MaxRows, domain.ObjectsMaxPageSize))}
	for _, w := range a.Where {
		c, err := condition(w)
		if err != nil {
			return nil, err
		}
		q.Where = append(q.Where, c)
	}
	if a.ID != "" {
		return nil, fmt.Errorf("%s: find with kind lists objects by property; use get with id for one object", ToolName)
	}
	res, err := t.d.Objects.List(ctx, scope, q)
	if err != nil {
		return nil, err
	}
	rows := make([]map[string]any, 0, len(res.Rows))
	for _, r := range res.Rows {
		vals := map[string]any{}
		for _, v := range r.Values {
			if v.Value.Type != "" {
				vals[v.Property] = value(v.Value)
			}
		}
		rows = append(rows, map[string]any{"id": r.EntityID, "properties": vals})
	}
	return map[string]any{"kind": res.Kind, "objects": rows, "count": len(rows), "truncated": res.NextPageToken != ""}, nil
}

// condition maps one where argument onto the closed objects condition. The
// kernel validates property, operator and type against the live declarations;
// here only the literal is typed.
func condition(w whereArg) (domain.ObjectCondition, error) {
	c := domain.ObjectCondition{Property: strings.TrimSpace(w.Property), Op: strings.ToLower(strings.TrimSpace(w.Op))}
	if c.Op == domain.ObjectOpIsNull {
		return c, nil
	}
	if len(w.Value) == 0 || string(w.Value) == "null" {
		return c, fmt.Errorf("%s: where %s %s needs a value", ToolName, c.Property, c.Op)
	}
	var num float64
	var str string
	vt := strings.ToLower(strings.TrimSpace(w.ValueType))
	switch {
	case json.Unmarshal(w.Value, &num) == nil && (vt == "" || vt == "number"):
		c.Values = []domain.StatementValue{{Type: "number", Number: num}}
	case json.Unmarshal(w.Value, &str) == nil:
		switch vt {
		case "", "text":
			c.Values = []domain.StatementValue{{Type: "text", Text: str}}
		case "date":
			d, err := time.Parse(time.RFC3339, str)
			if err != nil {
				return c, fmt.Errorf("%s: where %s: a date must be RFC3339 (2026-08-14T00:00:00Z)", ToolName, c.Property)
			}
			c.Values = []domain.StatementValue{{Type: "date", Date: d.UTC()}}
		case "entity":
			c.Values = []domain.StatementValue{{Type: "entity", EntityRef: str}}
		default:
			return c, fmt.Errorf("%s: where %s: value_type must be text, number, date or entity", ToolName, c.Property)
		}
	default:
		return c, fmt.Errorf("%s: where %s: value must be a string or a number", ToolName, c.Property)
	}
	return c, nil
}

// ─── get ─────────────────────────────────────────────────────────────────────

func (t *tool) get(ctx context.Context, scope *domain.TagPredicate, a args) (map[string]any, error) {
	if a.ID == "" {
		return nil, fmt.Errorf("%s: get needs id", ToolName)
	}
	cat := t.d.Plane.Catalog(scope)
	ents, err := cat.Entities(ctx, domain.EntityFilter{IDs: []string{a.ID}, Limit: 1, PredicateLimit: t.d.MaxAttributes})
	if err != nil {
		return nil, err
	}
	rec := map[string]any{"id": a.ID}
	found := false
	guarantee := ""
	if len(ents) == 1 {
		attrs := make([]map[string]any, 0, len(ents[0].Predicates))
		preds := append([]string(nil), ents[0].Predicates...)
		sort.Strings(preds)
		for _, p := range preds {
			q := domain.KnowledgeQuery{Kind: domain.QueryPoint, EntityID: a.ID, Predicate: p}
			res, err := t.d.Plane.Query(ctx, q, scope)
			if err != nil {
				return nil, err
			}
			guarantee = res.Guarantee
			for _, row := range res.Rows {
				attrs = append(attrs, attribute(row))
			}
		}
		if len(attrs) > 0 {
			rec["attributes"] = attrs
			found = true
		}
	}
	evs, truncated, err := t.eventRows(ctx, scope, a.ID, "", allTimeFrom, allTimeTo, t.d.MaxEvents)
	if err != nil {
		return nil, err
	}
	if len(evs) > 0 {
		rec["events"] = evs
		found = true
		if truncated {
			rec["events_note"] = "only the latest events are shown; call events with from/to for earlier ones"
		}
	}
	if t.d.Objects != nil {
		v, err := t.d.Objects.Get(ctx, scope, "", a.ID)
		switch {
		case err == nil:
			rec["object"] = objectView(v)
			found = true
		case errors.Is(err, domain.ErrObjectNotFound), errors.Is(err, domain.ErrObjectsInvalid):
			// Not an object (or one the caller may not know exists): nothing to add.
		default:
			return nil, err
		}
	}
	out := map[string]any{"found": found}
	if !found {
		// Hidden and absent read the same (ADR-0128 D12). Offer near ids the
		// caller CAN read, so a mistyped id is recoverable.
		near, err := cat.Entities(ctx, domain.EntityFilter{Substrings: []string{a.ID}, Limit: 5})
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(near))
		for _, e := range near {
			ids = append(ids, e.ID)
		}
		out["note"] = "no record with this id that you may see"
		if len(ids) > 0 {
			out["similar_ids"] = ids
		}
		return out, nil
	}
	if guarantee == "" {
		guarantee = domain.KnowledgeQuery{Kind: domain.QueryEvents}.Guarantee()
	}
	out["record"] = rec
	out["guarantee"] = guarantee
	return out, nil
}

// ─── events ──────────────────────────────────────────────────────────────────

func (t *tool) events(ctx context.Context, scope *domain.TagPredicate, a args) (map[string]any, error) {
	if a.ID == "" {
		return nil, fmt.Errorf("%s: events needs id", ToolName)
	}
	from, to := allTimeFrom, allTimeTo
	var err error
	if s := strings.TrimSpace(a.From); s != "" {
		if from, err = time.Parse(time.RFC3339, s); err != nil {
			return nil, fmt.Errorf("%s: from must be RFC3339 (2026-08-14T00:00:00Z)", ToolName)
		}
	}
	if s := strings.TrimSpace(a.To); s != "" {
		if to, err = time.Parse(time.RFC3339, s); err != nil {
			return nil, fmt.Errorf("%s: to must be RFC3339 (2026-08-14T00:00:00Z)", ToolName)
		}
	}
	if !to.After(from) {
		return nil, fmt.Errorf("%s: to must be after from", ToolName)
	}
	evs, truncated, err := t.eventRows(ctx, scope, a.ID, strings.TrimSpace(a.EventType), from, to, t.limit(a.Limit, t.d.MaxEvents))
	if err != nil {
		return nil, err
	}
	out := map[string]any{"id": a.ID, "events": evs, "count": len(evs), "truncated": truncated,
		"guarantee": domain.KnowledgeQuery{Kind: domain.QueryEvents}.Guarantee()}
	if truncated {
		out["note"] = "only the latest events are shown; narrow from/to for earlier ones"
	}
	return out, nil
}

// eventRows reads the entity's events through the plane's events shape (fenced
// in SQL, page = the first MaxQueryLimit events the caller may read, oldest
// first) and keeps the newest n, optionally of one type, oldest first.
func (t *tool) eventRows(ctx context.Context, scope *domain.TagPredicate, id, evType string, from, to time.Time, n int) ([]map[string]any, bool, error) {
	q := domain.KnowledgeQuery{Kind: domain.QueryEvents, EntityID: id, From: from, To: to, Limit: domain.MaxQueryLimit}
	res, err := t.d.Plane.Query(ctx, q, scope)
	if err != nil {
		return nil, false, err
	}
	rows := make([]map[string]any, 0, len(res.Rows))
	for _, r := range res.Rows {
		if evType != "" && !strings.EqualFold(fmt.Sprint(r["event_type"]), evType) {
			continue
		}
		rows = append(rows, event(r))
	}
	truncated := len(rows) > n || len(res.Rows) == domain.MaxQueryLimit
	if len(rows) > n {
		rows = rows[len(rows)-n:]
	}
	return rows, truncated, nil
}

// ─── rendering ───────────────────────────────────────────────────────────────

func attribute(row domain.QueryRow) map[string]any {
	a := map[string]any{"name": row["predicate"], "value_type": row["value_type"]}
	switch row["value_type"] {
	case "date":
		a["value"] = when(row["value_date"])
	case "number":
		a["value"] = row["value_number"]
	case "text":
		a["value"] = row["value_text"]
	case "entity":
		a["value"] = row["value_entity_id"]
	}
	if at := when(row["occurred_at"]); at != "" {
		a["as_of"] = at
	}
	if loc, ok := row["location"]; ok {
		a["location"] = loc
	}
	if ev, ok := row["evidence_id"].(string); ok && ev != "" {
		a["evidence_id"] = ev
	}
	return a
}

func event(row domain.QueryRow) map[string]any {
	e := map[string]any{"type": row["event_type"], "occurred_at": when(row["occurred_at"])}
	if roles, ok := row["roles"]; ok {
		e["participants"] = roles
	}
	if id, ok := row["event_id"].(string); ok && id != "" {
		e["event_id"] = id
	}
	return e
}

func objectView(v domain.ObjectView) map[string]any {
	props := map[string]any{}
	for _, p := range v.Properties {
		if p.Value.Type != "" {
			props[p.Property] = value(p.Value)
		}
	}
	o := map[string]any{"kind": v.Kind, "properties": props}
	if v.Lifecycle != nil && v.Lifecycle.State != "" {
		o["state"] = v.Lifecycle.State
	}
	if len(v.Links) > 0 {
		links := make([]map[string]any, 0, len(v.Links))
		for _, l := range v.Links {
			links = append(links, map[string]any{"relation": l.Label, "other": l.OtherRef, "direction": l.Direction})
		}
		o["links"] = links
	}
	return o
}

func value(v domain.StatementValue) any {
	switch v.Type {
	case "date":
		return v.Date.UTC().Format(time.RFC3339)
	case "number":
		return v.Number
	case "entity":
		return v.EntityRef
	}
	return v.Text
}

func when(v any) string {
	switch t := v.(type) {
	case time.Time:
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	case string:
		return t
	}
	return ""
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ResultSource implements domain.KernelToolSource (ENG-531): company records are
// what an ingress mapped from outside systems — ingested content.
func (t *tool) ResultSource() domain.SourceClass { return domain.SourceIngested }
