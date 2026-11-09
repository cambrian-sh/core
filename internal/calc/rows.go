package calc

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/ledger"
)

// readRef reads a ledger entry's JSON body through the ENFORCED reader (the
// caller's predicate is on ctx: the tool executor's runKernelTool put it there)
// and selects path in it. Absent and forbidden read the same.
func (c *core) readRef(ctx context.Context, tool, ref, path string) (any, error) {
	ref = strings.TrimSpace(ref)
	if !domain.IsLedgerID(ref) {
		return nil, fmt.Errorf("%s: ref %q is not a ledger id (led_...)", tool, ref)
	}
	if c.d.Reader == nil {
		return nil, fmt.Errorf("%s: references need the run ledger (execution.ledger.enabled); pass the values instead", tool)
	}
	e, err := c.d.Reader.Get(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("%s: reading %s: %v", tool, ref, err)
	}
	if e == nil {
		return nil, fmt.Errorf("%s: ledger entry %s not found", tool, ref)
	}
	if e.Truncated {
		return nil, fmt.Errorf("%s: ledger entry %s was stored truncated (%d bytes kept of %d); it cannot be computed on whole", tool, ref, len(e.Body), e.SizeBytes)
	}
	var v any
	if err := json.Unmarshal(e.Body, &v); err != nil {
		return nil, fmt.Errorf("%s: ledger entry %s is not JSON (%s)", tool, ref, e.ContentType)
	}
	if strings.TrimSpace(path) == "" {
		return v, nil
	}
	sel, err := ledger.SelectPath(v, path)
	if err != nil {
		return nil, fmt.Errorf("%s: path %q in %s: %v", tool, path, ref, err)
	}
	return sel, nil
}

// asList is v as a list: a JSON list, or a string holding one.
func asList(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case string:
		var inner []any
		if json.Unmarshal([]byte(t), &inner) == nil {
			return inner, true
		}
	}
	return nil, false
}

// field reads a row's field (a JSON path relative to the row); "" is the row.
func field(row any, path string) (any, bool) {
	if strings.TrimSpace(path) == "" {
		return row, true
	}
	v, err := ledger.SelectPath(row, path)
	return v, err == nil
}

// ─── calc_aggregate ──────────────────────────────────────────────────────────

type aggregateTool struct{ c *core }

var aggregateOps = []string{"sum", "count", "min", "max", "avg", "list"}

func (aggregateTool) Spec() domain.SystemTool {
	return domain.SystemTool{
		Name: ToolAggregate,
		Description: "Compute over a list of records held in the ledger, by reference, so you never add, count or " +
			"compare numbers yourself. ref is the ledger id of a tool result or step output, path the JSON path to " +
			"the list inside it (a.b[0].c; a JSON string is walked into), field the JSON path of the number inside " +
			"each record (money strings like \"1.234,50 TL\" are read). op: sum, count, min, max, avg, or list (the " +
			"matching records). where filters records first: each {field, op, value} with op eq, ne, gt, gte, lt, " +
			"lte, contains, in, before, after (dates). group_by gives one result per distinct value of that field. " +
			"Instead of ref you may pass values (a short list you were given). Returns the result, the record counts " +
			"and ledger_ref (cite it for the figures).",
		Schema: []byte(`{"type":"object","properties":{` +
			`"ref":{"type":"string","description":"ledger id, led_..."},` +
			`"path":{"type":"string","description":"JSON path to the list inside the entry"},` +
			`"values":{"type":"array","description":"the list itself, instead of ref"},` +
			`"op":{"type":"string","enum":["sum","count","min","max","avg","list"]},` +
			`"field":{"type":"string","description":"JSON path of the value in each record"},` +
			`"group_by":{"type":"string","description":"JSON path of the grouping value in each record"},` +
			`"where":{"type":"array","items":{"type":"object","properties":{` +
			`"field":{"type":"string"},"op":{"type":"string","enum":["eq","ne","gt","gte","lt","lte","contains","in","before","after"]},` +
			`"value":{}},"required":["field","op","value"],"additionalProperties":false}}},` +
			`"required":["op"],"additionalProperties":false}`),
		Effects: []domain.ToolEffect{domain.EffectRead},
	}
}

type condition struct {
	Field string          `json:"field"`
	Op    string          `json:"op"`
	Value json.RawMessage `json:"value"`
}

func (t aggregateTool) Call(ctx context.Context, call domain.KernelToolCall) ([]byte, error) {
	var a struct {
		Ref     string      `json:"ref"`
		Path    string      `json:"path"`
		Values  []any       `json:"values"`
		Op      string      `json:"op"`
		Field   string      `json:"field"`
		GroupBy string      `json:"group_by"`
		Where   []condition `json:"where"`
	}
	if err := decodeArgs(ToolAggregate, call.ArgsJSON, &a); err != nil {
		return nil, err
	}
	op := strings.ToLower(strings.TrimSpace(a.Op))
	if !containsStr(aggregateOps, op) {
		return nil, fmt.Errorf("%s: op must be one of %s", ToolAggregate, strings.Join(aggregateOps, ", "))
	}
	var rows []any
	var refs []string
	switch {
	case strings.TrimSpace(a.Ref) != "" && a.Values != nil:
		return nil, fmt.Errorf("%s: pass ref or values, not both", ToolAggregate)
	case strings.TrimSpace(a.Ref) != "":
		v, err := t.c.readRef(ctx, ToolAggregate, a.Ref, a.Path)
		if err != nil {
			return nil, err
		}
		list, ok := asList(v)
		if !ok {
			return nil, fmt.Errorf("%s: the value at path %q in %s is not a list; give the path to the list", ToolAggregate, a.Path, a.Ref)
		}
		rows, refs = list, []string{strings.TrimSpace(a.Ref)}
	case a.Values != nil:
		rows = a.Values
	default:
		return nil, fmt.Errorf("%s: ref (a ledger id) or values is required", ToolAggregate)
	}
	if len(rows) > t.c.d.MaxRows {
		return nil, fmt.Errorf("%s: %d records exceed the limit of %d", ToolAggregate, len(rows), t.c.d.MaxRows)
	}
	conds := make([]compiledCond, 0, len(a.Where))
	for _, w := range a.Where {
		cc, err := compileCond(w)
		if err != nil {
			return nil, fmt.Errorf("%s: where: %v", ToolAggregate, err)
		}
		conds = append(conds, cc)
	}
	var matched []any
	var matchedIdx []int
	for i, r := range rows {
		ok := true
		for _, cc := range conds {
			if !cc.holds(r) {
				ok = false
				break
			}
		}
		if ok {
			matched = append(matched, r)
			matchedIdx = append(matchedIdx, i)
		}
	}
	res := map[string]any{"op": op, "records": len(rows), "matched": len(matched)}
	if a.Field != "" {
		res["field"] = a.Field
	}
	if len(a.Where) > 0 {
		res["where"] = a.Where
	}
	if op == "list" {
		listed := matched
		if len(listed) > t.c.d.MaxListed {
			listed = listed[:t.c.d.MaxListed]
			res["truncated"] = true
		}
		res["result"] = listed
		res["indices"] = matchedIdx[:len(listed)]
		return t.c.finish(ctx, call, ToolAggregate, refs, res)
	}
	if a.GroupBy == "" {
		r, used, skipped := reduce(op, a.Field, matched)
		res["result"], res["used"] = r, used
		if len(skipped) > 0 {
			res["skipped"] = skipped
		}
		return t.c.finish(ctx, call, ToolAggregate, refs, res)
	}
	res["group_by"] = a.GroupBy
	groups := map[string][]any{}
	var keys []string
	for _, r := range matched {
		v, ok := field(r, a.GroupBy)
		k := "(missing)"
		if ok && v != nil {
			k = text(v)
		}
		if _, seen := groups[k]; !seen {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], r)
	}
	sort.Strings(keys)
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		r, used, skipped := reduce(op, a.Field, groups[k])
		g := map[string]any{"key": k, "result": r, "records": len(groups[k]), "used": used}
		if len(skipped) > 0 {
			g["skipped"] = skipped
		}
		out = append(out, g)
	}
	res["groups"] = out
	return t.c.finish(ctx, call, ToolAggregate, refs, res)
}

// skippedNote is one record left out of a numeric reduction, and why.
type skippedNote struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

// reduce applies op to field over rows. count counts the rows (a field, when
// given, must be present). Records whose field is missing or not a number are
// skipped and listed (up to 20), never silently dropped.
func reduce(op, path string, rows []any) (any, int, []skippedNote) {
	var skipped []skippedNote
	note := func(i int, why string) {
		if len(skipped) < 20 {
			skipped = append(skipped, skippedNote{Index: i, Reason: why})
		}
	}
	if op == "count" {
		n := 0
		for i, r := range rows {
			if path == "" {
				n++
				continue
			}
			if v, ok := field(r, path); ok && v != nil {
				n++
			} else {
				note(i, "no "+path)
			}
		}
		return n, n, skipped
	}
	var nums []float64
	for i, r := range rows {
		v, ok := field(r, path)
		if !ok || v == nil {
			note(i, "no "+firstNonEmpty(path, "value"))
			continue
		}
		f, ok := number(v)
		if !ok {
			note(i, fmt.Sprintf("%q is not a number", text(v)))
			continue
		}
		nums = append(nums, f)
	}
	if len(nums) == 0 {
		return nil, 0, skipped
	}
	var r float64
	switch op {
	case "sum", "avg":
		for _, f := range nums {
			r += f
		}
		if op == "avg" {
			r /= float64(len(nums))
		}
	case "min":
		r = nums[0]
		for _, f := range nums[1:] {
			r = min(r, f)
		}
	case "max":
		r = nums[0]
		for _, f := range nums[1:] {
			r = max(r, f)
		}
	}
	return round(r), len(nums), skipped
}

// ─── conditions (where, calc_compare) ────────────────────────────────────────

var condOps = []string{"eq", "ne", "gt", "gte", "lt", "lte", "contains", "in", "before", "after"}

type compiledCond struct {
	field string
	op    string
	value any
}

func compileCond(c condition) (compiledCond, error) {
	op := strings.ToLower(strings.TrimSpace(c.Op))
	if !containsStr(condOps, op) {
		return compiledCond{}, fmt.Errorf("op %q must be one of %s", c.Op, strings.Join(condOps, ", "))
	}
	var v any
	if len(c.Value) > 0 {
		if err := json.Unmarshal(c.Value, &v); err != nil {
			return compiledCond{}, fmt.Errorf("value: %v", err)
		}
	}
	if op == "in" {
		if _, ok := v.([]any); !ok {
			return compiledCond{}, fmt.Errorf("op in needs a list value")
		}
	}
	return compiledCond{field: c.Field, op: op, value: v}, nil
}

func (cc compiledCond) holds(row any) bool {
	v, ok := field(row, cc.field)
	if !ok {
		return cc.op == "ne"
	}
	ok, _ = compare(v, cc.op, cc.value, nil)
	return ok
}

// compare evaluates left op right. Numbers compare by value (money strings
// included), dates by calendar day, anything else as text (eq/ne/contains
// case-insensitively). kind names how the two were compared.
func compare(left any, op string, right any, right2 any) (bool, string) {
	switch op {
	case "in":
		list, _ := right.([]any)
		for _, x := range list {
			if ok, _ := compare(left, "eq", x, nil); ok {
				return true, "in"
			}
		}
		return false, "in"
	case "contains":
		return strings.Contains(strings.ToLower(text(left)), strings.ToLower(text(right))), "text"
	case "between":
		a, _ := compare(left, "gte", right, nil)
		b, k := compare(left, "lte", right2, nil)
		return a && b, k
	}
	if op == "before" || op == "after" {
		l, ok1 := ParseDate(text(left))
		r, ok2 := ParseDate(text(right))
		if !ok1 || !ok2 {
			return false, "date"
		}
		if op == "before" {
			return l.Before(r), "date"
		}
		return l.After(r), "date"
	}
	// Dates first when both sides are dates: "2026-11-08" also parses as a number.
	if ls, lok := left.(string); lok {
		if rs, rok := right.(string); rok {
			l, ok1 := ParseDate(ls)
			r, ok2 := ParseDate(rs)
			if ok1 && ok2 {
				return order(op, l.Compare(r)), "date"
			}
		}
	}
	if l, ok1 := number(left); ok1 {
		if r, ok2 := number(right); ok2 {
			d := l - r
			c := 0
			switch {
			case d > 1e-9:
				c = 1
			case d < -1e-9:
				c = -1
			}
			return order(op, c), "number"
		}
	}
	c := strings.Compare(strings.ToLower(strings.TrimSpace(text(left))), strings.ToLower(strings.TrimSpace(text(right))))
	return order(op, c), "text"
}

func order(op string, c int) bool {
	switch op {
	case "eq":
		return c == 0
	case "ne":
		return c != 0
	case "gt":
		return c > 0
	case "gte":
		return c >= 0
	case "lt":
		return c < 0
	case "lte":
		return c <= 0
	}
	return false
}

// ─── calc_compare ────────────────────────────────────────────────────────────

type compareTool struct{ c *core }

func (compareTool) Spec() domain.SystemTool {
	return domain.SystemTool{
		Name: ToolCompare,
		Description: "Check thresholds and rules exactly, never by eye: each comparison is left op right, where a side " +
			"is a literal value or {\"ref\": ledger id, \"path\": JSON path} read from the ledger. op: eq, ne, gt, gte, " +
			"lt, lte, between (right is the minimum, right_max the maximum, both included), contains, in, before, " +
			"after. Numbers compare by value (money strings are read), dates by day. Returns each comparison's " +
			"values and whether it holds, all_hold, any_hold, and ledger_ref (cite it for the result).",
		Schema: []byte(`{"type":"object","properties":{"comparisons":{"type":"array","minItems":1,"items":{"type":"object","properties":{` +
			`"name":{"type":"string"},"left":{},"op":{"type":"string","enum":["eq","ne","gt","gte","lt","lte","between","contains","in","before","after"]},` +
			`"right":{},"right_max":{}},"required":["left","op","right"],"additionalProperties":false}}},` +
			`"required":["comparisons"],"additionalProperties":false}`),
		Effects: []domain.ToolEffect{domain.EffectRead},
	}
}

func (t compareTool) Call(ctx context.Context, call domain.KernelToolCall) ([]byte, error) {
	var a struct {
		Comparisons []struct {
			Name     string          `json:"name"`
			Left     json.RawMessage `json:"left"`
			Op       string          `json:"op"`
			Right    json.RawMessage `json:"right"`
			RightMax json.RawMessage `json:"right_max"`
		} `json:"comparisons"`
	}
	if err := decodeArgs(ToolCompare, call.ArgsJSON, &a); err != nil {
		return nil, err
	}
	if len(a.Comparisons) == 0 {
		return nil, fmt.Errorf("%s: comparisons is required", ToolCompare)
	}
	var refs []string
	resolve := func(raw json.RawMessage, side string, i int) (any, error) {
		if len(raw) == 0 {
			return nil, fmt.Errorf("%s: comparison %d: %s is required", ToolCompare, i, side)
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("%s: comparison %d: %s: %v", ToolCompare, i, side, err)
		}
		m, isObj := v.(map[string]any)
		if !isObj {
			return v, nil
		}
		if ref, ok := m["ref"].(string); ok {
			path, _ := m["path"].(string)
			got, err := t.c.readRef(ctx, ToolCompare, ref, path)
			if err != nil {
				return nil, err
			}
			refs = appendUnique(refs, strings.TrimSpace(ref))
			return got, nil
		}
		if lit, ok := m["value"]; ok {
			return lit, nil
		}
		return v, nil
	}
	results := make([]map[string]any, 0, len(a.Comparisons))
	all, anyHold := true, false
	for i, c := range a.Comparisons {
		op := strings.ToLower(strings.TrimSpace(c.Op))
		if op != "between" && !containsStr(condOps, op) {
			return nil, fmt.Errorf("%s: comparison %d: op %q is not one of eq, ne, gt, gte, lt, lte, between, contains, in, before, after", ToolCompare, i, c.Op)
		}
		l, err := resolve(c.Left, "left", i)
		if err != nil {
			return nil, err
		}
		r, err := resolve(c.Right, "right", i)
		if err != nil {
			return nil, err
		}
		var r2 any
		if op == "between" {
			if r2, err = resolve(c.RightMax, "right_max", i); err != nil {
				return nil, err
			}
		}
		holds, kind := compare(l, op, r, r2)
		all = all && holds
		anyHold = anyHold || holds
		res := map[string]any{"left": l, "op": op, "right": r, "holds": holds, "compared_as": kind}
		if c.Name != "" {
			res["name"] = c.Name
		}
		if op == "between" {
			res["right_max"] = r2
		}
		if kind == "number" {
			lf, _ := number(l)
			rf, _ := number(r)
			res["difference"] = round(lf - rf)
		}
		if kind == "date" {
			ld, ok1 := ParseDate(text(l))
			rd, ok2 := ParseDate(text(r))
			if ok1 && ok2 {
				res["days_difference"] = int(ld.Sub(rd) / (24 * time.Hour))
			}
		}
		results = append(results, res)
	}
	return t.c.finish(ctx, call, ToolCompare, refs, map[string]any{"comparisons": results, "all_hold": all, "any_hold": anyHold})
}

func containsStr(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func appendUnique(xs []string, x string) []string {
	if x == "" || containsStr(xs, x) {
		return xs
	}
	return append(xs, x)
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}
