package objects

import (
	"fmt"
	"sort"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// Validation of an objects request against the live declarations (ADR-0149 D6).
// Pure: the same request and snapshot always validate the same way. Every name
// the planner later puts in an identifier position is looked up here in the
// declarations and taken FROM them, never from the request.

// defaultNamespace is the substrate's default namespace (the stores' and the
// ontology's default; this package cannot import either without a cycle).
const defaultNamespace = "default"

func orDefault(ns string) string {
	if ns == "" {
		return defaultNamespace
	}
	return ns
}

// opsByType is which operators apply to which declared type.
var opsByType = map[string]map[string]bool{
	"number": set(domain.ObjectOpEq, domain.ObjectOpNe, domain.ObjectOpLt, domain.ObjectOpLe, domain.ObjectOpGt, domain.ObjectOpGe, domain.ObjectOpIn, domain.ObjectOpIsNull),
	"date":   set(domain.ObjectOpEq, domain.ObjectOpNe, domain.ObjectOpLt, domain.ObjectOpLe, domain.ObjectOpGt, domain.ObjectOpGe, domain.ObjectOpIn, domain.ObjectOpIsNull),
	"text":   set(domain.ObjectOpEq, domain.ObjectOpNe, domain.ObjectOpIn, domain.ObjectOpPrefix, domain.ObjectOpIsNull),
	"entity": set(domain.ObjectOpEq, domain.ObjectOpIn, domain.ObjectOpIsNull),
}

func set(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// prop is a declared property of a kind, resolved from the declarations.
type prop struct {
	Name string
	Type string
}

// valueCol, tagsCol, partiesCol are the property's generated columns (tables.go).
func (p prop) valueCol() string   { return ValueColumn(p.Name, p.Type) }
func (p prop) tagsCol() string    { return TagsColumn(p.Name) }
func (p prop) partiesCol() string { return PartiesColumn(p.Name) }
func (p prop) rowTagsCol() string { return RowTagsColumn(p.Name) }

// cond is a validated condition on a resolved property.
type cond struct {
	Prop   prop
	Op     string
	Values []domain.StatementValue
}

// hop is one validated link hop: the relation and the kind it reaches.
type hop struct {
	Relation string
	ToKind   domain.KindSpec
}

// linkFilter is a validated link filter: the hops and the condition on the
// object the last hop reaches.
type linkFilter struct {
	Hops []hop
	Cond cond
}

type sortKey struct {
	Prop prop
	Desc bool
}

type metric struct {
	Fn   string
	Prop *prop
}

// checked is a request the declarations admit: everything the planner needs,
// resolved. Built only by validate.
type checked struct {
	Namespace   string
	Spec        domain.KindSpec
	DeclVersion int64
	Props       []prop // every declared property of the kind, sorted by name
	Where       []cond
	Links       []linkFilter
	OrderBy     []sortKey
	GroupBy     []prop
	Metrics     []metric
	Fields      []prop
	PageSize    int
	Group       bool
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", domain.ErrObjectsInvalid, fmt.Sprintf(format, a...))
}

// validate checks q against the namespace's declarations and the bounds, and
// resolves every name. The page token is checked separately (decodeToken),
// because it is bound to this request's canonical form.
func validate(q domain.ObjectsQuery, kinds domain.KindSource, relations domain.RelationSource) (checked, error) {
	c := checked{Namespace: orDefault(q.Namespace), Group: q.IsGroup()}
	reg := kinds.KindsFor(c.Namespace)
	spec, ok := reg.Spec(q.Kind)
	if q.Kind == "" || !ok || !spec.Table {
		return c, invalid("kind %q is not a declared kind with an object table in namespace %q", q.Kind, c.Namespace)
	}
	c.Spec, c.DeclVersion = spec, reg.Version()
	c.Props = declaredProps(spec)
	lookup := func(kind domain.KindSpec, name, what string) (prop, error) {
		vs, ok := kind.Predicates[name]
		if !ok {
			return prop{}, invalid("%s: %q is not a declared property of kind %q", what, name, kind.Kind)
		}
		return prop{Name: name, Type: vs.Type}, nil
	}

	if len(q.Where) > domain.ObjectsMaxConditions {
		return c, invalid("where: %d conditions, at most %d", len(q.Where), domain.ObjectsMaxConditions)
	}
	for i, w := range q.Where {
		p, err := lookup(spec, w.Property, fmt.Sprintf("where[%d]", i))
		if err != nil {
			return c, err
		}
		cd, err := checkCond(p, w, fmt.Sprintf("where[%d]", i))
		if err != nil {
			return c, err
		}
		c.Where = append(c.Where, cd)
	}

	if len(q.Links) > domain.ObjectsMaxLinkFilters {
		return c, invalid("links: %d filters, at most %d", len(q.Links), domain.ObjectsMaxLinkFilters)
	}
	rels := relations.RelationsFor(c.Namespace)
	for i, lf := range q.Links {
		what := fmt.Sprintf("links[%d]", i)
		if len(lf.Path) == 0 || len(lf.Path) > domain.ObjectsMaxLinkHops {
			return c, invalid("%s: a link path has 1 to %d hops, got %d", what, domain.ObjectsMaxLinkHops, len(lf.Path))
		}
		from := spec
		var out linkFilter
		for h, verb := range lf.Path {
			rs, ok := rels.Spec(verb)
			if !ok {
				return c, invalid("%s: %q is not a declared relation", what, verb)
			}
			if rs.FromKind != from.ObjectPrefix() {
				return c, invalid("%s: relation %q does not start at kind %q (from_kind %q)", what, verb, from.Kind, rs.FromKind)
			}
			to, ok := reg.ObjectKind(rs.ToKind + "/x") // any local part: the stem decides
			if rs.ToKind == "" || !ok {
				return c, invalid("%s: relation %q has no declared to_kind object kind", what, verb)
			}
			if h == len(lf.Path)-1 && !to.Table {
				return c, invalid("%s: the kind %q that relation %q reaches has no object table", what, to.Kind, verb)
			}
			out.Hops = append(out.Hops, hop{Relation: verb, ToKind: to})
			from = to
		}
		p, err := lookup(from, lf.Where.Property, what+".where")
		if err != nil {
			return c, err
		}
		if out.Cond, err = checkCond(p, lf.Where, what+".where"); err != nil {
			return c, err
		}
		c.Links = append(c.Links, out)
	}

	if c.Group {
		if len(q.OrderBy) > 0 || len(q.Fields) > 0 || q.PageToken != "" || q.PageSize != 0 {
			return c, invalid("a group request (group_by or metrics) takes no order_by, fields, page_size or page_token")
		}
		if len(q.GroupBy) > domain.ObjectsMaxGroupBy {
			return c, invalid("group_by: %d properties, at most %d", len(q.GroupBy), domain.ObjectsMaxGroupBy)
		}
		seen := map[string]bool{}
		for i, g := range q.GroupBy {
			p, err := lookup(spec, g, fmt.Sprintf("group_by[%d]", i))
			if err != nil {
				return c, err
			}
			if seen[g] {
				return c, invalid("group_by: %q twice", g)
			}
			seen[g] = true
			c.GroupBy = append(c.GroupBy, p)
		}
		if len(q.Metrics) > domain.ObjectsMaxMetrics {
			return c, invalid("metrics: %d, at most %d", len(q.Metrics), domain.ObjectsMaxMetrics)
		}
		for i, m := range q.Metrics {
			what := fmt.Sprintf("metrics[%d]", i)
			switch m.Fn {
			case domain.ObjectMetricCount:
				if m.Property != "" {
					return c, invalid("%s: count takes no property", what)
				}
				c.Metrics = append(c.Metrics, metric{Fn: m.Fn})
				continue
			case domain.ObjectMetricSum, domain.ObjectMetricAvg, domain.ObjectMetricMin, domain.ObjectMetricMax:
			default:
				return c, invalid("%s: unknown metric %q (count, sum, avg, min, max)", what, m.Fn)
			}
			p, err := lookup(spec, m.Property, what)
			if err != nil {
				return c, err
			}
			numeric := m.Fn == domain.ObjectMetricSum || m.Fn == domain.ObjectMetricAvg
			if p.Type != "number" && (numeric || p.Type != "date") {
				return c, invalid("%s: %s does not apply to %s property %q", what, m.Fn, p.Type, p.Name)
			}
			c.Metrics = append(c.Metrics, metric{Fn: m.Fn, Prop: &p})
		}
		return c, nil
	}

	if len(q.OrderBy) > domain.ObjectsMaxOrderBy {
		return c, invalid("order_by: %d keys, at most %d", len(q.OrderBy), domain.ObjectsMaxOrderBy)
	}
	seen := map[string]bool{}
	for i, o := range q.OrderBy {
		p, err := lookup(spec, o.Property, fmt.Sprintf("order_by[%d]", i))
		if err != nil {
			return c, err
		}
		if seen[o.Property] {
			return c, invalid("order_by: %q twice", o.Property)
		}
		seen[o.Property] = true
		c.OrderBy = append(c.OrderBy, sortKey{Prop: p, Desc: o.Desc})
	}
	switch {
	case q.PageSize == 0:
		c.PageSize = domain.ObjectsDefaultPageSize
	case q.PageSize < 0 || q.PageSize > domain.ObjectsMaxPageSize:
		return c, invalid("page_size %d: 1 to %d (0 = %d)", q.PageSize, domain.ObjectsMaxPageSize, domain.ObjectsDefaultPageSize)
	default:
		c.PageSize = q.PageSize
	}
	if len(q.Fields) == 0 {
		c.Fields = c.Props
	} else {
		seen := map[string]bool{}
		for i, f := range q.Fields {
			p, err := lookup(spec, f, fmt.Sprintf("fields[%d]", i))
			if err != nil {
				return c, err
			}
			if !seen[f] {
				seen[f] = true
				c.Fields = append(c.Fields, p)
			}
		}
	}
	return c, nil
}

// checkCond checks one condition's operator and literals against p's type.
func checkCond(p prop, w domain.ObjectCondition, what string) (cond, error) {
	if !opsByType[p.Type][w.Op] {
		return cond{}, invalid("%s: operator %q does not apply to %s property %q", what, w.Op, p.Type, p.Name)
	}
	switch {
	case w.Op == domain.ObjectOpIsNull:
		if len(w.Values) != 0 {
			return cond{}, invalid("%s: is_null takes no value", what)
		}
	case w.Op == domain.ObjectOpIn:
		if len(w.Values) == 0 || len(w.Values) > domain.ObjectsMaxInValues {
			return cond{}, invalid("%s: in takes 1 to %d values, got %d", what, domain.ObjectsMaxInValues, len(w.Values))
		}
	case len(w.Values) != 1:
		return cond{}, invalid("%s: %s takes exactly one value, got %d", what, w.Op, len(w.Values))
	}
	for _, v := range w.Values {
		if v.Type != p.Type {
			return cond{}, invalid("%s: a %q literal for %s property %q", what, v.Type, p.Type, p.Name)
		}
		if w.Op == domain.ObjectOpPrefix && v.Text == "" {
			return cond{}, invalid("%s: prefix needs a non-empty value", what)
		}
		if p.Type == "entity" && strings.TrimSpace(v.EntityRef) == "" {
			return cond{}, invalid("%s: an empty entity ref", what)
		}
	}
	return cond{Prop: p, Op: w.Op, Values: w.Values}, nil
}

// declaredProps lists spec's properties sorted by name.
func declaredProps(spec domain.KindSpec) []prop {
	names := make([]string, 0, len(spec.Predicates))
	for n := range spec.Predicates {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]prop, len(names))
	for i, n := range names {
		out[i] = prop{Name: n, Type: spec.Predicates[n].Type}
	}
	return out
}
