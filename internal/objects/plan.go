package objects

import (
	"strconv"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz/sqlfence"
)

// Planning (ADR-0149 D6): a validated request and the caller's predicate become
// ONE parameterised statement. Pure — no I/O, same input, same SQL (pinned by
// testdata/plan.golden).
//
// Identifiers come only from the declarations (table and column names built by
// tables.go from validated names, quoted by ident); every value is a bind
// parameter.
//
// THE ACCESS RULE, applied by construction (ADR-0149 §2 principle 3, D3): for
// this caller a property value is VISIBLE when its companions pass the fence —
// sqlfence.ObjectProperty, the one rendering, per fact over the TEXT[]
// companions (never jsonb containment: the P2 finding). A value that is not
// visible is, for the caller, exactly an absent value (NULL):
//   - a filter on P is (visible(P) AND test); is_null is NOT (visible(P) AND
//     P IS NOT NULL);
//   - a returned value, sort key, group key and metric input is
//     CASE WHEN visible(P) THEN P END;
//   - an object is listed only when the caller may know it exists (ADR-0128
//     D12): it may read the evidence that minted the entity, or it can see at
//     least one of its property values. Rendered over interned fence classes:
//     the row's vis_classes (the pairs of its properties and of its minted
//     evidence, kept by the maintainer) meets the caller's visible classes V,
//     computed once per request by planVisible through the same fence
//     (sqlfence.FenceClass) and bound as a constant — evaluating one fence per
//     distinct pair instead of one per property per row
//     (T-20261113-unfiltered-listobjects-group-list-under-a).
// When a filter on P already guarantees visible(P) AND P IS NOT NULL, P's
// sort/group/metric expression is the bare column: same value, and the btree
// on (namespace_id, P) stays usable for order.
//
// ROW CLASSIFICATION (ADR-0158 D2, stage 4). A value derived from an
// observation that carries its own row classification R (<prop>__rowtags) is
// visible only when fence(tags) AND fence(tags || R) pass — narrow-only, and for
// R = ∅ exactly the fence above. A link filter's link likewise
// (sqlfence.TypedRowExists over links.classification). The planner renders the
// row-tags variants only where the request's snapshot holds row-classified data
// (rowTagged, probed once per filtered request by visibleClasses in the same
// REPEATABLE READ transaction): when no fence class carries row tags, no visible
// value does — the maintainer interns every visible value's triple in the
// transaction that writes it, and the dictionary is append-only — and when no
// link carries a classification, TypedRowExists and EvidenceExists admit the
// same links. So on data without row classification the statement is the one P4
// shipped, byte for byte (testdata/plan.golden; the benchmark harness ports it),
// and costs nothing more.

// cursor is a decoded page token: the last row's sort values (nil = absent)
// and entity id.
type cursor struct {
	Keys []any
	ID   string
}

// rowTagged records which row classifications the request's snapshot holds
// (see ROW CLASSIFICATION above): Values — some fence class carries row tags, so
// property fences and the visible classes render WithRowTags; Links — some link
// carries a row classification, so link filters render TypedRowExists. GetObject
// sets both (a point read; no golden, no probe).
type rowTagged struct {
	Values bool
	Links  bool
}

// allRowTagged renders every row-tags variant.
var allRowTagged = rowTagged{Values: true, Links: true}

// sqlb accumulates bind parameters.
type sqlb struct {
	args  []any
	scope *domain.TagPredicate
	// vis is the caller's visible fence classes (planVisible's result), read by
	// the existence rule; unused when needsExistence is false.
	vis []int32
	// rt is which row-tags variants to render.
	rt rowTagged
}

func (b *sqlb) arg(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(len(b.args))
}

// visible is P's fence on alias a as one boolean expression ("TRUE" = none).
func (b *sqlb) visible(a string, p prop) string {
	shape := sqlfence.ObjectProperty(a+"."+ident(p.tagsCol()), a+"."+ident(p.partiesCol()))
	if b.rt.Values {
		shape = shape.WithRowTags(a + "." + ident(p.rowTagsCol()))
	}
	body, args := shape.Expr(b.scope, len(b.args))
	b.args = append(b.args, args...)
	return body
}

// value is P on alias a as the caller sees it: NULL where not visible.
func (b *sqlb) value(a string, p prop) string {
	col := a + "." + ident(p.valueCol())
	if v := b.visible(a, p); v != "TRUE" {
		return "CASE WHEN " + v + " THEN " + col + " END"
	}
	return col
}

// evidenceVisible renders "the caller may read the evidence refCol names". Not
// for a typed row (events, observations, links): use typedRowVisible.
func (b *sqlb) evidenceVisible(refCol string) string {
	sql, args := sqlfence.EvidenceExists(b.scope, refCol, len(b.args))
	b.args = append(b.args, args...)
	return sql
}

// typedRowVisible renders "the caller may read the typed row whose evidence is
// refCol and whose own classification is rowCol" (sqlfence.TypedRowExists,
// ADR-0158 D2): narrow-only, and for an empty rowCol exactly evidenceVisible.
func (b *sqlb) typedRowVisible(refCol, rowCol string) string {
	sql, args := sqlfence.TypedRowExists(b.scope, refCol, rowCol, len(b.args))
	b.args = append(b.args, args...)
	return sql
}

// bindValue is a typed literal as its column's parameter.
func bindValue(v domain.StatementValue) any {
	switch v.Type {
	case "date":
		return v.Date.UTC()
	case "number":
		return v.Number
	case "entity":
		return v.EntityRef
	default:
		return v.Text
	}
}

// bindList is an `in` list as a typed array parameter.
func bindList(typ string, vs []domain.StatementValue) any {
	switch typ {
	case "date":
		out := make([]time.Time, len(vs))
		for i, v := range vs {
			out[i] = v.Date.UTC()
		}
		return out
	case "number":
		out := make([]float64, len(vs))
		for i, v := range vs {
			out[i] = v.Number
		}
		return out
	default:
		out := make([]string, len(vs))
		for i, v := range vs {
			out[i] = bindValue(v).(string)
		}
		return out
	}
}

var sqlOps = map[string]string{
	domain.ObjectOpEq: "=", domain.ObjectOpNe: "<>", domain.ObjectOpLt: "<",
	domain.ObjectOpLe: "<=", domain.ObjectOpGt: ">", domain.ObjectOpGe: ">=",
}

// condition renders one validated condition on alias a.
func (b *sqlb) condition(a string, c cond) string {
	col := a + "." + ident(c.Prop.valueCol())
	vis := b.visible(a, c.Prop)
	var test string
	switch c.Op {
	case domain.ObjectOpIsNull:
		if vis == "TRUE" {
			return col + " IS NULL"
		}
		return "NOT (" + vis + " AND " + col + " IS NOT NULL)"
	case domain.ObjectOpIn:
		test = col + " = ANY(" + b.arg(bindList(c.Prop.Type, c.Values)) + ")"
	case domain.ObjectOpPrefix:
		test = "starts_with(" + col + ", " + b.arg(c.Values[0].Text) + ")"
	default:
		test = col + " " + sqlOps[c.Op] + " " + b.arg(bindValue(c.Values[0]))
	}
	if vis == "TRUE" {
		return test
	}
	return "(" + vis + " AND " + test + ")"
}

// entityRefLen is where a links.to_ref's entity id starts (1-based substr).
var entityRefLen = strconv.Itoa(len(domain.RefPrefixEntity) + 1)

// link renders one link filter from alias o: an EXISTS over confirmed,
// unretracted links of each hop's relation, each link visible to the caller
// (its evidence), reaching an object row that satisfies the condition.
func (b *sqlb) link(o string, i int, lf linkFilter) string {
	var sb strings.Builder
	prev := ""
	var where []string
	for h, hp := range lf.Hops {
		l := "l" + strconv.Itoa(i) + "_" + strconv.Itoa(h)
		if h == 0 {
			sb.WriteString("EXISTS (SELECT 1 FROM links " + l)
			where = append(where, l+".namespace_id = "+o+".namespace_id",
				l+".from_ref = '"+domain.RefPrefixEntity+"' || "+o+".entity_id")
		} else {
			sb.WriteString(" JOIN links " + l + " ON " + l + ".namespace_id = " + prev + ".namespace_id AND " + l + ".from_ref = " + prev + ".to_ref")
		}
		where = append(where, l+".relation = "+b.arg(hp.Relation),
			l+".state = '"+domain.LinkStateConfirmed+"'", l+".retracted_at IS NULL")
		var ev string
		if b.rt.Links {
			ev = b.typedRowVisible(l+".evidence_id", l+".classification")
		} else {
			// No link in the snapshot carries a row classification (rowTagged):
			// the evidence fence admits exactly what TypedRowExists would, and
			// the statement stays the one plan.golden pins.
			ev = b.evidenceVisible(l + ".evidence_id")
		}
		if ev != "" {
			where = append(where, strings.TrimPrefix(ev, " AND "))
		}
		prev = l
	}
	t := "t" + strconv.Itoa(i)
	last := lf.Hops[len(lf.Hops)-1].ToKind
	sb.WriteString(" JOIN " + ident(TableName(last.Kind)) + " " + t + " ON " + t + ".namespace_id = " + prev +
		".namespace_id AND " + t + ".entity_id = substr(" + prev + ".to_ref, " + entityRefLen + ") AND starts_with(" +
		prev + ".to_ref, '" + domain.RefPrefixEntity + "')")
	where = append(where, b.condition(t, lf.Cond))
	sb.WriteString(" WHERE " + strings.Join(where, " AND ") + ")")
	return sb.String()
}

// filtered lists the properties a non-is_null root condition already proves
// visible and present.
func filtered(c checked) map[string]bool {
	out := map[string]bool{}
	for _, w := range c.Where {
		if w.Op != domain.ObjectOpIsNull {
			out[w.Prop.Name] = true
		}
	}
	return out
}

// keyExpr is P's sort/group/metric expression on the root alias.
func (b *sqlb) keyExpr(p prop, proven map[string]bool) string {
	if proven[p.Name] {
		return "o." + ident(p.valueCol())
	}
	return b.value("o", p)
}

// where renders the root WHERE: namespace, conditions, link filters, and the
// existence rule.
func (b *sqlb) where(c checked) string {
	parts := []string{"o.namespace_id = " + b.arg(c.Namespace)}
	for _, w := range c.Where {
		parts = append(parts, b.condition("o", w))
	}
	for i, lf := range c.Links {
		parts = append(parts, b.link("o", i, lf))
	}
	if ex := b.existence(c); ex != "" {
		parts = append(parts, ex)
	}
	return strings.Join(parts, " AND ")
}

// needsExistence: the request needs the existence rule (ADR-0128 D12) — the
// caller is filtered and no non-is_null condition already proves a visible
// value. Only then is the caller's visible class set computed.
func needsExistence(c checked, scope *domain.TagPredicate) bool {
	return !scope.Unfiltered() && len(filtered(c)) == 0
}

// planVisible renders the caller's visible fence classes V: the ids of the
// dictionary triples that pass the caller's fence, through sqlfence.FenceClass —
// the same shape as a property's fence, so a triple is in V exactly when a
// property (or the minted evidence) carrying it is visible. With rowTags
// (rowTagged.Values) it is sqlfence.FenceClassRowTags, the fence of a property
// carrying row tags; without, no triple has row tags and the two admit the same
// ids. Run once per request, in the request's read transaction, only when
// needsExistence.
func planVisible(scope *domain.TagPredicate, rowTags bool) (string, []any) {
	shape := sqlfence.FenceClass("c")
	if rowTags {
		shape = sqlfence.FenceClassRowTags("c")
	}
	body, args := shape.Expr(scope, 0)
	return "SELECT c.id FROM object_fence_classes c WHERE " + body, args
}

// planRowTagged renders the probe for rowTagged in the request's snapshot: one
// row, (some fence class carries row tags, some link carries a row
// classification — FALSE when the request has no link filter). Each EXISTS is
// served by a partial index holding only row-classified rows
// (object_fence_classes_row_tagged, migration 0055; idx_links_row_classified,
// internal/migrate concurrentIndexes), empty until a mapping writes row
// classification. Run only for a filtered caller.
func planRowTagged(c checked) string {
	links := "FALSE"
	if len(c.Links) > 0 {
		links = "EXISTS (SELECT 1 FROM links WHERE cardinality(classification) > 0)"
	}
	return "SELECT EXISTS (SELECT 1 FROM object_fence_classes WHERE cardinality(row_tags) > 0), " + links
}

// existence is ADR-0128 D12 on the root alias: the caller may read the evidence
// that minted the entity, or sees at least one property value — i.e. one of the
// row's fence classes is in V (see the access rule above). "" when nothing
// needs proving: ScopeMaintenance sees everything, and a non-is_null condition
// already proves a visible value.
func (b *sqlb) existence(c checked) string {
	if !needsExistence(c, b.scope) {
		return ""
	}
	return "EXISTS (SELECT 1 FROM unnest(o." + ident(colVisClasses) + ") AS vc(id) WHERE vc.id = ANY(" + b.arg(b.vis) + "::integer[]))"
}

// planList renders a list request: the page plus one row (to know whether a
// next page exists). Columns: entity_id, one per sort key, one per field.
//
// vis is planVisible's result (nil when needsExistence is false); rt is the
// snapshot's rowTagged.
func planList(c checked, scope *domain.TagPredicate, cur *cursor, vis []int32, rt rowTagged) (string, []any) {
	b := &sqlb{scope: scope, vis: vis, rt: rt}
	proven := filtered(c)
	keys := make([]string, len(c.OrderBy))
	for i, o := range c.OrderBy {
		keys[i] = b.keyExpr(o.Prop, proven)
	}
	cols := append([]string{"o.entity_id"}, keys...)
	for _, f := range c.Fields {
		cols = append(cols, b.value("o", f))
	}
	where := b.where(c)
	if cur != nil {
		where += " AND " + b.after(c, keys, cur)
	}
	order := make([]string, 0, len(keys)+1)
	for i, k := range keys {
		dir := " ASC"
		if c.OrderBy[i].Desc {
			dir = " DESC"
		}
		// A key a filter proves present has no NULLs: the NULLS clause is then
		// left at Postgres's default so a DESC sort can walk the btree on
		// (namespace_id, P) backwards (DESC NULLS LAST cannot; the P4 benchmark
		// at 10^6 found keyset page 1 sorting ~10^5 rows instead; objects-query-p4-1e6).
		if !proven[c.OrderBy[i].Prop.Name] {
			dir += " NULLS LAST"
		}
		order = append(order, k+dir)
	}
	order = append(order, "o.entity_id ASC")
	sql := "SELECT " + strings.Join(cols, ", ") + " FROM " + ident(TableName(c.Spec.Kind)) + " o WHERE " + where +
		" ORDER BY " + strings.Join(order, ", ") + " LIMIT " + strconv.Itoa(c.PageSize+1)
	return sql, b.args
}

// after renders the keyset predicate "strictly after the cursor" for keys
// sorted NULLS LAST with entity_id ASC as the final key:
//
//	OR over i: (k_1 = v_1 AND … AND k_{i-1} = v_{i-1} AND k_i after v_i)
//	OR (every k = v AND entity_id > id)
//
// where "k = v" is null-safe and "after v" is (k > v OR k IS NULL) ascending,
// (k < v OR k IS NULL) descending, and FALSE when v is NULL (nothing sorts
// after an absent value but other absent values, which the later keys order).
func (b *sqlb) after(c checked, keys []string, cur *cursor) string {
	var ors []string
	var eqs []string
	for i, k := range keys {
		v := cur.Keys[i]
		if v != nil {
			op := ">"
			if c.OrderBy[i].Desc {
				op = "<"
			}
			p := b.arg(v)
			ors = append(ors, "("+strings.Join(append(append([]string{}, eqs...), "("+k+" "+op+" "+p+" OR "+k+" IS NULL)"), " AND ")+")")
			eqs = append(eqs, k+" = "+b.arg(v))
		} else {
			eqs = append(eqs, k+" IS NULL")
		}
	}
	ors = append(ors, "("+strings.Join(append(eqs, "o.entity_id > "+b.arg(cur.ID)), " AND ")+")")
	return "(" + strings.Join(ors, " OR ") + ")"
}

// planGroup renders a group request: one row per bucket (keys then metrics),
// ordered by the keys, at most ObjectsMaxGroups+1 (to report truncation).
// vis and rt as for planList.
func planGroup(c checked, scope *domain.TagPredicate, vis []int32, rt rowTagged) (string, []any) {
	b := &sqlb{scope: scope, vis: vis, rt: rt}
	proven := filtered(c)
	var cols, pos []string
	for i, g := range c.GroupBy {
		cols = append(cols, b.keyExpr(g, proven))
		pos = append(pos, strconv.Itoa(i+1))
	}
	for _, m := range c.Metrics {
		if m.Prop == nil {
			cols = append(cols, "count(*)")
			continue
		}
		cols = append(cols, m.Fn+"("+b.keyExpr(*m.Prop, proven)+")")
	}
	sql := "SELECT " + strings.Join(cols, ", ") + " FROM " + ident(TableName(c.Spec.Kind)) + " o WHERE " + b.where(c)
	if len(pos) > 0 {
		order := make([]string, len(pos))
		for i, p := range pos {
			order[i] = p + " NULLS LAST"
		}
		sql += " GROUP BY " + strings.Join(pos, ", ") + " ORDER BY " + strings.Join(order, ", ") +
			" LIMIT " + strconv.Itoa(domain.ObjectsMaxGroups+1)
	}
	return sql, b.args
}
