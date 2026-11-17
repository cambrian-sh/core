package objects

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/cambrian-sh/core/domain"
)

// Pure-core tests of the objects query (ADR-0149 D9): validation refusals,
// golden SQL per plan, and properties — every validated request compiles, the
// fence is always present for every property a statement touches, no
// identifier comes from input, tokens round-trip and are refused under changed
// parameters.

var updatePlans = flag.Bool("update-plans", false, "rewrite testdata/plan.golden and testdata/plan_rowtags.golden")

func planKinds(t testing.TB) (*domain.KindRegistry, *domain.RelationRegistry) {
	kinds, err := domain.NewKindRegistry([]domain.KindSpec{
		{Kind: "order", Table: true, Predicates: map[string]domain.ValueSpec{
			"status": {Type: "text", Indexed: true}, "total": {Type: "number", Indexed: true},
			"due_date": {Type: "date", Indexed: true}, "customer": {Type: "entity"}, "channel": {Type: "text"},
		}},
		{Kind: "customer", Table: true, Predicates: map[string]domain.ValueSpec{"segment": {Type: "text"}, "type": {Type: "text"}}},
		{Kind: "group", Table: true, Predicates: map[string]domain.ValueSpec{"type": {Type: "text"}}},
		{Kind: "note", Predicates: map[string]domain.ValueSpec{"body": {Type: "text"}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rels, err := domain.NewRelationRegistry([]domain.RelationSpec{
		{Name: "placed_by", Family: domain.LinkFamilyRelation, FromKind: "order", ToKind: "customer"},
		{Name: "member_of", Family: domain.LinkFamilyRelation, FromKind: "customer", ToKind: "group"},
		{Name: "noted_in", Family: domain.LinkFamilyRelation, FromKind: "order", ToKind: "note"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return kinds, rels
}

func tv(s string) domain.StatementValue  { return domain.StatementValue{Type: "text", Text: s} }
func nv(n float64) domain.StatementValue { return domain.StatementValue{Type: "number", Number: n} }
func dv(y, m, d int) domain.StatementValue {
	return domain.StatementValue{Type: "date", Date: time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)}
}

func TestValidate_Refusals(t *testing.T) {
	kinds, rels := planKinds(t)
	ok := domain.ObjectsQuery{Kind: "order"}
	cases := []struct {
		name string
		mut  func(q *domain.ObjectsQuery)
		want string
	}{
		{"unknown kind", func(q *domain.ObjectsQuery) { q.Kind = "invoice" }, "not a declared kind"},
		{"kind without table", func(q *domain.ObjectsQuery) { q.Kind = "note" }, "not a declared kind"},
		{"unknown property", func(q *domain.ObjectsQuery) {
			q.Where = []domain.ObjectCondition{{Property: "colour", Op: "eq", Values: []domain.StatementValue{tv("x")}}}
		}, "not a declared property"},
		{"injected property", func(q *domain.ObjectsQuery) {
			q.Where = []domain.ObjectCondition{{Property: `status"; DROP TABLE x; --`, Op: "eq", Values: []domain.StatementValue{tv("x")}}}
		}, "not a declared property"},
		{"unknown operator", func(q *domain.ObjectsQuery) {
			q.Where = []domain.ObjectCondition{{Property: "status", Op: "like", Values: []domain.StatementValue{tv("x")}}}
		}, "operator"},
		{"range on text", func(q *domain.ObjectsQuery) {
			q.Where = []domain.ObjectCondition{{Property: "status", Op: "gt", Values: []domain.StatementValue{tv("x")}}}
		}, "operator"},
		{"prefix on number", func(q *domain.ObjectsQuery) {
			q.Where = []domain.ObjectCondition{{Property: "total", Op: "prefix", Values: []domain.StatementValue{nv(1)}}}
		}, "operator"},
		{"wrong literal type", func(q *domain.ObjectsQuery) {
			q.Where = []domain.ObjectCondition{{Property: "total", Op: "gt", Values: []domain.StatementValue{tv("50")}}}
		}, "literal"},
		{"two values for eq", func(q *domain.ObjectsQuery) {
			q.Where = []domain.ObjectCondition{{Property: "total", Op: "eq", Values: []domain.StatementValue{nv(1), nv(2)}}}
		}, "exactly one value"},
		{"empty in", func(q *domain.ObjectsQuery) {
			q.Where = []domain.ObjectCondition{{Property: "total", Op: "in"}}
		}, "in takes"},
		{"value on is_null", func(q *domain.ObjectsQuery) {
			q.Where = []domain.ObjectCondition{{Property: "total", Op: "is_null", Values: []domain.StatementValue{nv(1)}}}
		}, "no value"},
		{"too many conditions", func(q *domain.ObjectsQuery) {
			for i := 0; i <= domain.ObjectsMaxConditions; i++ {
				q.Where = append(q.Where, domain.ObjectCondition{Property: "status", Op: "is_null"})
			}
		}, "at most"},
		{"page size over", func(q *domain.ObjectsQuery) { q.PageSize = domain.ObjectsMaxPageSize + 1 }, "page_size"},
		{"negative page size", func(q *domain.ObjectsQuery) { q.PageSize = -1 }, "page_size"},
		{"unknown relation", func(q *domain.ObjectsQuery) {
			q.Links = []domain.ObjectLinkFilter{{Path: []string{"owns"}, Where: domain.ObjectCondition{Property: "segment", Op: "is_null"}}}
		}, "not a declared relation"},
		{"relation from another kind", func(q *domain.ObjectsQuery) {
			q.Links = []domain.ObjectLinkFilter{{Path: []string{"member_of"}, Where: domain.ObjectCondition{Property: "type", Op: "is_null"}}}
		}, "does not start at"},
		{"three hops", func(q *domain.ObjectsQuery) {
			q.Links = []domain.ObjectLinkFilter{{Path: []string{"placed_by", "member_of", "member_of"}, Where: domain.ObjectCondition{Property: "type", Op: "is_null"}}}
		}, "hops"},
		{"link to a kind without table", func(q *domain.ObjectsQuery) {
			q.Links = []domain.ObjectLinkFilter{{Path: []string{"noted_in"}, Where: domain.ObjectCondition{Property: "body", Op: "is_null"}}}
		}, "no object table"},
		{"link target property", func(q *domain.ObjectsQuery) {
			q.Links = []domain.ObjectLinkFilter{{Path: []string{"placed_by"}, Where: domain.ObjectCondition{Property: "status", Op: "is_null"}}}
		}, "not a declared property"},
		{"order twice", func(q *domain.ObjectsQuery) {
			q.OrderBy = []domain.ObjectOrder{{Property: "total"}, {Property: "total", Desc: true}}
		}, "twice"},
		{"too many sort keys", func(q *domain.ObjectsQuery) {
			q.OrderBy = []domain.ObjectOrder{{Property: "total"}, {Property: "status"}, {Property: "due_date"}, {Property: "channel"}}
		}, "at most"},
		{"group with page token", func(q *domain.ObjectsQuery) {
			q.GroupBy, q.PageToken = []string{"status"}, "x"
		}, "group request"},
		{"group with order", func(q *domain.ObjectsQuery) {
			q.Metrics, q.OrderBy = []domain.ObjectMetric{{Fn: "count"}}, []domain.ObjectOrder{{Property: "total"}}
		}, "group request"},
		{"sum of text", func(q *domain.ObjectsQuery) {
			q.Metrics = []domain.ObjectMetric{{Fn: "sum", Property: "status"}}
		}, "does not apply"},
		{"avg of date", func(q *domain.ObjectsQuery) {
			q.Metrics = []domain.ObjectMetric{{Fn: "avg", Property: "due_date"}}
		}, "does not apply"},
		{"count with property", func(q *domain.ObjectsQuery) {
			q.Metrics = []domain.ObjectMetric{{Fn: "count", Property: "total"}}
		}, "no property"},
		{"unknown metric", func(q *domain.ObjectsQuery) {
			q.Metrics = []domain.ObjectMetric{{Fn: "median", Property: "total"}}
		}, "unknown metric"},
		{"unknown field", func(q *domain.ObjectsQuery) { q.Fields = []string{"colour"} }, "not a declared property"},
	}
	if _, err := validate(ok, kinds, rels); err != nil {
		t.Fatalf("baseline refused: %v", err)
	}
	for _, c := range cases {
		q := ok
		c.mut(&q)
		_, err := validate(q, kinds, rels)
		if !errors.Is(err, domain.ErrObjectsInvalid) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want ErrObjectsInvalid containing %q", c.name, err, c.want)
		}
	}
}

// goldenRequests are the plans pinned by testdata/plan.golden. "s11" is the
// ADR-0149 §11 Q3 workload ("late hospital orders over 50k, totals by
// customer"); the benchmark harness executes this file's statements.
func goldenRequests() []struct {
	name string
	q    domain.ObjectsQuery
} {
	open := []domain.StatementValue{tv("confirmed"), tv("picking"), tv("packed")}
	return []struct {
		name string
		q    domain.ObjectsQuery
	}{
		{"list_default", domain.ObjectsQuery{Kind: "order"}},
		{"filter1", domain.ObjectsQuery{Kind: "order", Where: []domain.ObjectCondition{{Property: "status", Op: "eq", Values: []domain.StatementValue{tv("picking")}}}}},
		{"filter4", domain.ObjectsQuery{Kind: "order", Where: []domain.ObjectCondition{
			{Property: "status", Op: "eq", Values: []domain.StatementValue{tv("picking")}},
			{Property: "channel", Op: "eq", Values: []domain.StatementValue{tv("email")}},
			{Property: "total", Op: "gt", Values: []domain.StatementValue{nv(20000)}},
			{Property: "due_date", Op: "lt", Values: []domain.StatementValue{dv(2026, 6, 1)}},
		}, Fields: []string{"status", "total"}}},
		{"keyset_p1", domain.ObjectsQuery{Kind: "order", Where: []domain.ObjectCondition{
			{Property: "total", Op: "ge", Values: []domain.StatementValue{nv(10000)}},
			{Property: "total", Op: "le", Values: []domain.StatementValue{nv(50000)}},
		}, OrderBy: []domain.ObjectOrder{{Property: "total", Desc: true}}, Fields: []string{"total"}}},
		{"sort_unfiltered", domain.ObjectsQuery{Kind: "order", OrderBy: []domain.ObjectOrder{{Property: "due_date"}, {Property: "status", Desc: true}}, PageSize: 10}},
		{"is_null_prefix", domain.ObjectsQuery{Kind: "order", Where: []domain.ObjectCondition{
			{Property: "customer", Op: "is_null"}, {Property: "channel", Op: "prefix", Values: []domain.StatementValue{tv("e")}}}}},
		{"link2", domain.ObjectsQuery{Kind: "order", Where: []domain.ObjectCondition{{Property: "status", Op: "eq", Values: []domain.StatementValue{tv("picking")}}},
			Links: []domain.ObjectLinkFilter{{Path: []string{"placed_by", "member_of"}, Where: domain.ObjectCondition{Property: "type", Op: "eq", Values: []domain.StatementValue{tv("GPO")}}}}}},
		{"group_status", domain.ObjectsQuery{Kind: "order", GroupBy: []string{"status"},
			Metrics: []domain.ObjectMetric{{Fn: "count"}, {Fn: "sum", Property: "total"}}}},
		{"totals", domain.ObjectsQuery{Kind: "order", Metrics: []domain.ObjectMetric{{Fn: "count"}, {Fn: "avg", Property: "total"}, {Fn: "max", Property: "due_date"}}}},
		{"s11", domain.ObjectsQuery{Kind: "order",
			Where: []domain.ObjectCondition{{Property: "status", Op: "in", Values: open},
				{Property: "due_date", Op: "lt", Values: []domain.StatementValue{dv(2026, 6, 1)}},
				{Property: "total", Op: "gt", Values: []domain.StatementValue{nv(50000)}}},
			Links:   []domain.ObjectLinkFilter{{Path: []string{"placed_by"}, Where: domain.ObjectCondition{Property: "segment", Op: "eq", Values: []domain.StatementValue{tv("hospital")}}}},
			GroupBy: []string{"customer"},
			Metrics: []domain.ObjectMetric{{Fn: "count"}, {Fn: "sum", Property: "total"}}}},
	}
}

// goldenScopes: the full operator read, the system bypass and a narrow fence (a
// regional reader: south or global, never restricted, confidential only as a
// party).
func goldenScopes() []struct {
	name string
	p    *domain.TagPredicate
} {
	return []struct {
		name string
		p    *domain.TagPredicate
	}{
		{"maintenance", domain.ScopeMaintenance},
		{"system", domain.ScopeSystem},
		{"narrow", &domain.TagPredicate{AnyOfClauses: [][]string{{"r:south", "r:global"}}, ForbiddenTags: []string{"cls:restricted"},
			PartyScopedTags: []string{"cls:confidential"}, PartyIdentities: []string{"am:7"}}},
	}
}

// goldenVis stands for the caller's visible fence classes (planVisible's
// result) in the golden: a constant, as the reader binds it.
var goldenVis = []int32{3, 5, 8}

// TestPlan_Golden pins the plans for a snapshot WITHOUT row classification
// (rowTagged zero: no fence class carries row tags, no link a classification) —
// every deployment until a mapping writes one (ADR-0158). They must stay the
// statements P4 shipped, byte for byte: the benchmark harness ports this file
// (cambrian-benchmarks objstate/p4plan.py, tests/test_p4plan_parity.py).
func TestPlan_Golden(t *testing.T) {
	checkPlanGolden(t, "testdata/plan.golden", rowTagged{})
}

// TestPlan_GoldenRowTagged pins the plans for a snapshot that holds row
// classification (ADR-0158 D2, stage 4): every property fence, the visible
// classes and every link filter carry the narrow-only row-tags conjunct.
func TestPlan_GoldenRowTagged(t *testing.T) {
	checkPlanGolden(t, "testdata/plan_rowtags.golden", allRowTagged)
}

func checkPlanGolden(t *testing.T, path string, rt rowTagged) {
	kinds, rels := planKinds(t)
	var b strings.Builder
	for _, s := range goldenScopes() {
		sql, args := planVisible(s.p, rt.Values)
		fmt.Fprintf(&b, "== visible %s\n%s\n-- args: %s\n\n", s.name, sql, renderArgs(args))
	}
	for _, r := range goldenRequests() {
		c, err := validate(r.q, kinds, rels)
		if err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
		for _, s := range goldenScopes() {
			var sql string
			var args []any
			var vis []int32
			if needsExistence(c, s.p) {
				vis = goldenVis
			}
			if c.Group {
				sql, args = planGroup(c, s.p, vis, rt)
			} else {
				sql, args = planList(c, s.p, nil, vis, rt)
			}
			fmt.Fprintf(&b, "== %s %s\n%s\n-- args: %s\n\n", r.name, s.name, sql, renderArgs(args))
		}
	}
	got := b.String()
	if *updatePlans {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update-plans to create it)", err)
	}
	if string(want) != got {
		t.Fatalf("%s: plans changed; inspect the diff and rerun with -update-plans if intended", path)
	}
}

// The row-tags probe asks about links only when the request filters by one.
func TestPlan_RowTaggedProbe(t *testing.T) {
	kinds, rels := planKinds(t)
	for _, r := range goldenRequests() {
		c, err := validate(r.q, kinds, rels)
		if err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
		sql := planRowTagged(c)
		if !strings.HasPrefix(sql, "SELECT EXISTS (SELECT 1 FROM object_fence_classes WHERE cardinality(row_tags) > 0), ") {
			t.Fatalf("%s: %s", r.name, sql)
		}
		if asksLinks := strings.Contains(sql, "FROM links WHERE cardinality(classification) > 0"); asksLinks != (len(c.Links) > 0) {
			t.Fatalf("%s: probe asks about links = %v with %d link filters: %s", r.name, asksLinks, len(c.Links), sql)
		}
	}
}

// renderArgs renders bind parameters as JSON-ish text, one per placeholder.
func renderArgs(args []any) string {
	out := make([]string, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case time.Time:
			out[i] = fmt.Sprintf("%q", v.UTC().Format(time.RFC3339))
		case []time.Time:
			var s []string
			for _, x := range v {
				s = append(s, fmt.Sprintf("%q", x.UTC().Format(time.RFC3339)))
			}
			out[i] = "[" + strings.Join(s, ",") + "]"
		case []string:
			var s []string
			for _, x := range v {
				s = append(s, fmt.Sprintf("%q", x))
			}
			out[i] = "[" + strings.Join(s, ",") + "]"
		case []int32:
			var s []string
			for _, x := range v {
				s = append(s, fmt.Sprint(x))
			}
			out[i] = "[" + strings.Join(s, ",") + "]"
		case string:
			out[i] = fmt.Sprintf("%q", v)
		default:
			out[i] = fmt.Sprintf("%v", v)
		}
	}
	return "[" + strings.Join(out, ", ") + "]"
}

var (
	placeholder = regexp.MustCompile(`\$(\d+)`)
	quotedName  = regexp.MustCompile(`"([^"]+)"`)
)

// drawRequest draws a request over the plan kinds, valid or not.
func drawRequest(t *rapid.T) domain.ObjectsQuery {
	props := []string{"status", "total", "due_date", "customer", "channel", "colour", `x"y`}
	vals := map[string]domain.StatementValue{"status": tv("a"), "channel": tv("b"), "total": nv(3), "due_date": dv(2026, 1, 2),
		"customer": {Type: "entity", EntityRef: "customer/C-1"}}
	ops := []string{"eq", "ne", "lt", "le", "gt", "ge", "in", "prefix", "is_null", "bogus"}
	q := domain.ObjectsQuery{Kind: rapid.SampledFrom([]string{"order", "order", "order", "customer", "note"}).Draw(t, "kind")}
	for i, n := 0, rapid.IntRange(0, 4).Draw(t, "nw"); i < n; i++ {
		p := rapid.SampledFrom(props).Draw(t, "wp")
		op := rapid.SampledFrom(ops).Draw(t, "op")
		var vs []domain.StatementValue
		if op != "is_null" {
			vs = []domain.StatementValue{vals[p]}
		}
		q.Where = append(q.Where, domain.ObjectCondition{Property: p, Op: op, Values: vs})
	}
	if rapid.Bool().Draw(t, "link") {
		path := []string{"placed_by"}
		where := domain.ObjectCondition{Property: "segment", Op: "eq", Values: []domain.StatementValue{tv("h")}}
		if rapid.Bool().Draw(t, "hop2") {
			path, where = []string{"placed_by", "member_of"}, domain.ObjectCondition{Property: "type", Op: "is_null"}
		}
		q.Links = []domain.ObjectLinkFilter{{Path: path, Where: where}}
	}
	if rapid.Bool().Draw(t, "group") {
		q.GroupBy = []string{rapid.SampledFrom(props).Draw(t, "g")}
		q.Metrics = []domain.ObjectMetric{{Fn: "count"}, {Fn: rapid.SampledFrom([]string{"sum", "min", "max"}).Draw(t, "mfn"), Property: "total"}}
		return q
	}
	for i, n := 0, rapid.IntRange(0, 3).Draw(t, "no"); i < n; i++ {
		q.OrderBy = append(q.OrderBy, domain.ObjectOrder{Property: rapid.SampledFrom(props).Draw(t, "op"), Desc: rapid.Bool().Draw(t, "d")})
	}
	q.PageSize = rapid.IntRange(-1, 250).Draw(t, "ps")
	return q
}

func drawScope(t *rapid.T) *domain.TagPredicate {
	switch rapid.IntRange(0, 3).Draw(t, "scope") {
	case 0:
		return domain.ScopeMaintenance
	case 1:
		return domain.ScopeSystem
	case 2:
		return &domain.TagPredicate{RequiredTags: []string{"a"}}
	}
	return &domain.TagPredicate{AnyOfClauses: [][]string{{"a", "b"}}, PartyScopedTags: []string{"c"}, PartyIdentities: []string{"p"}}
}

func TestPlan_Properties(t *testing.T) {
	kinds, rels := planKinds(t)
	allowed := map[string]bool{"objects_order": true, "objects_customer": true, "objects_group": true, colVisClasses: true}
	for _, k := range []string{"order", "customer", "group"} {
		spec, _ := kinds.Spec(k)
		for p, v := range spec.Predicates {
			allowed[ValueColumn(p, v.Type)], allowed[TagsColumn(p)], allowed[PartiesColumn(p)], allowed[RowTagsColumn(p)] = true, true, true, true
		}
	}
	rapid.Check(t, func(rt *rapid.T) {
		q := drawRequest(rt)
		scope := drawScope(rt)
		tagged := rowTagged{Values: rapid.Bool().Draw(rt, "rtValues"), Links: rapid.Bool().Draw(rt, "rtLinks")}
		c, err := validate(q, kinds, rels)
		if err != nil {
			if !errors.Is(err, domain.ErrObjectsInvalid) {
				rt.Fatalf("refusal not typed: %v", err)
			}
			return
		}
		var sql string
		var args []any
		var vis []int32
		if needsExistence(c, scope) {
			vis = []int32{1}
		}
		if c.Group {
			sql, args = planGroup(c, scope, vis, tagged)
		} else {
			cur := &cursor{ID: "order/x", Keys: make([]any, len(c.OrderBy))}
			for i, o := range c.OrderBy {
				if rapid.Bool().Draw(rt, "nullkey") {
					continue
				}
				cur.Keys[i] = map[string]any{"text": "k", "entity": "e/1", "number": 1.5, "date": time.Unix(0, 0).UTC()}[o.Prop.Type]
			}
			sql, args = planList(c, scope, cur, vis, tagged)
		}
		// Every validated request compiles: placeholders are exactly $1..$len(args).
		max := 0
		for _, m := range placeholder.FindAllStringSubmatch(sql, -1) {
			var n int
			fmt.Sscan(m[1], &n)
			if n > max {
				max = n
			}
		}
		if max != len(args) {
			rt.Fatalf("placeholders up to $%d, %d args\n%s", max, len(args), sql)
		}
		// No identifier comes from input: every quoted name is a declared column or table.
		for _, m := range quotedName.FindAllStringSubmatch(sql, -1) {
			if !allowed[m[1]] {
				rt.Fatalf("identifier %q is not declared\n%s", m[1], sql)
			}
		}
		// The fence is always present: under a filtered scope, every value column
		// the statement reads is guarded by its tags companion on the same alias.
		if !scope.Unfiltered() {
			for _, m := range regexp.MustCompile(`(\w+)\."([a-z_]+)__(text|number|date|entity)"`).FindAllStringSubmatch(sql, -1) {
				if !strings.Contains(sql, m[1]+`."`+m[2]+`__tags" IS NOT NULL`) {
					rt.Fatalf("%s.%s read with no fence\n%s", m[1], m[2], sql)
				}
				// ADR-0158 D2: with row-classified values in the snapshot, every
				// value's fence carries its row-tags conjunct; without, none does.
				hasRow := strings.Contains(sql, `cardinality(`+m[1]+`."`+m[2]+`__rowtags") = 0`)
				if hasRow != tagged.Values {
					rt.Fatalf("%s.%s row-tags fence present=%v, rowTagged.Values=%v\n%s", m[1], m[2], hasRow, tagged.Values, sql)
				}
			}
			// The existence rule is present exactly when needed, as the class test.
			if hasEx := strings.Contains(sql, `unnest(o."vis_classes")`); hasEx != needsExistence(c, scope) {
				rt.Fatalf("existence rule present=%v, needed=%v\n%s", hasEx, needsExistence(c, scope), sql)
			}
			if len(c.Links) > 0 && !regexp.MustCompile(`FROM evidence e\s+WHERE e\.id = l0_0\.evidence_id`).MatchString(sql) {
				rt.Fatalf("link without its evidence fence\n%s", sql)
			}
			if hasRow := strings.Contains(sql, "cardinality(l0_0.classification) = 0"); len(c.Links) > 0 && hasRow != tagged.Links {
				rt.Fatalf("link row-tags fence present=%v, rowTagged.Links=%v\n%s", hasRow, tagged.Links, sql)
			}
		}
	})
}

func TestToken_RoundTripAndBinding(t *testing.T) {
	kinds, rels := planKinds(t)
	rapid.Check(t, func(rt *rapid.T) {
		q := domain.ObjectsQuery{Kind: "order", OrderBy: []domain.ObjectOrder{{Property: "total", Desc: true}, {Property: "due_date"}, {Property: "status"}}}
		c, err := validate(q, kinds, rels)
		if err != nil {
			rt.Fatal(err)
		}
		keys := []any{nil, nil, nil}
		if rapid.Bool().Draw(rt, "k0") {
			keys[0] = rapid.Float64Range(-1e9, 1e9).Draw(rt, "n")
		}
		if rapid.Bool().Draw(rt, "k1") {
			keys[1] = time.Unix(rapid.Int64Range(0, 4e9).Draw(rt, "s"), rapid.Int64Range(0, 999_999_999).Draw(rt, "ns")).UTC()
		}
		if rapid.Bool().Draw(rt, "k2") {
			keys[2] = rapid.String().Draw(rt, "t")
		}
		id := "order/" + rapid.StringMatching(`[A-Za-z0-9-]{1,12}`).Draw(rt, "id")
		q.PageToken = encodeToken(q, c, keys, id)
		q.PageSize = rapid.IntRange(1, 200).Draw(rt, "ps") // page_size may change
		cur, err := decodeToken(q, c)
		if err != nil {
			rt.Fatalf("round trip: %v", err)
		}
		if cur.ID != id || fmt.Sprint(cur.Keys) != fmt.Sprint(keys) {
			rt.Fatalf("round trip: %v %v, want %v %v", cur.Keys, cur.ID, keys, id)
		}
		// Any other change refuses the token.
		changed := q
		switch rapid.IntRange(0, 3).Draw(rt, "change") {
		case 0:
			changed.OrderBy = []domain.ObjectOrder{{Property: "total"}, {Property: "due_date"}, {Property: "status"}}
		case 1:
			changed.Where = []domain.ObjectCondition{{Property: "status", Op: "is_null"}}
		case 2:
			changed.Fields = []string{"status"}
		case 3:
			changed.Namespace = "other"
		}
		c2, err := validate(changed, kinds, rels)
		if err != nil {
			rt.Fatal(err)
		}
		if _, err := decodeToken(changed, c2); !errors.Is(err, domain.ErrObjectsInvalid) {
			rt.Fatalf("token accepted under changed parameters: %v", err)
		}
	})
	c, _ := validate(domain.ObjectsQuery{Kind: "order"}, kinds, rels)
	for _, bad := range []string{"!!", "e30", "eyJoIjoiYSJ9"} {
		if _, err := decodeToken(domain.ObjectsQuery{Kind: "order", PageToken: bad}, c); !errors.Is(err, domain.ErrObjectsInvalid) {
			t.Errorf("token %q: %v", bad, err)
		}
	}
}
