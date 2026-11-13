package ontology

import (
	"context"
	"fmt"
	"sort"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz/sqlfence"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// DefaultViolationsLimit caps a report that names no limit; MaxViolationsLimit
// caps any report.
const (
	DefaultViolationsLimit = 200
	MaxViolationsLimit     = 1000
)

// Violations lists existing rows that namespace's CURRENT declarations would
// refuse if they were written now (ADR-0149 D2: tightened rules apply to new
// writes; old rows are reported, never retro-rejected). It is a query, not a
// table: every call reads the rows as they are.
//
// What it covers:
//   - knowledge items that are a CURRENT resolution winner (a superseded item is
//     history, not a violation): a Required predicate missing, a text value
//     outside its enum, a number outside min/max;
//   - CONFIRMED, unretracted links (candidates are proposals, ADR-0128): an
//     endpoint of the wrong entity kind, and overlapping links of a
//     cardinality-one verb;
//   - entities of a required verb's from_kind holding no such link.
//
// Every read is fenced by scope through the one fence renderer (internal/authz/
// sqlfence): items by their own classification, links by their own evidence (the
// query plane's filterLinks rule), entities by the evidence that minted them
// (mayKnowEntityExists). A link the caller cannot read does not satisfy a
// required verb either — hidden and absent read the same (ADR-0128 D12).
//
// truncated is true when more violations exist than limit.
func (o *Ontology) Violations(ctx context.Context, scope *domain.TagPredicate, namespace string, limit int) (out []domain.OntologyViolation, truncated bool, err error) {
	if scope == nil {
		return nil, false, errNilScope
	}
	switch {
	case limit <= 0:
		limit = DefaultViolationsLimit
	case limit > MaxViolationsLimit:
		limit = MaxViolationsLimit
	}
	ns := orDefault(namespace)
	snap := o.Current(ns)
	r := &report{o: o, ctx: ctx, scope: scope, ns: ns, limit: limit + 1}

	kinds := append([]domain.KindSpec(nil), snap.Doc.Kinds...)
	sort.Slice(kinds, func(i, j int) bool { return kinds[i].Kind < kinds[j].Kind })
	for _, k := range kinds {
		preds := make([]string, 0, len(k.Predicates))
		for p := range k.Predicates {
			preds = append(preds, p)
		}
		sort.Strings(preds)
		for _, p := range preds {
			v := k.Predicates[p]
			subject := "kind " + k.Kind + "." + p
			if v.Required {
				r.items(domain.ViolationRequired, subject, k.Kind, p, `
					AND NOT EXISTS (SELECT 1 FROM statement_values sv WHERE sv.item_id = ki.id AND sv.predicate = $3)`,
					"no value for the required predicate")
			}
			if len(v.Enum) > 0 {
				r.items(domain.ViolationEnum, subject, k.Kind, p, `
					AND EXISTS (SELECT 1 FROM statement_values sv WHERE sv.item_id = ki.id AND sv.predicate = $3
					            AND sv.value_text IS NOT NULL AND NOT (sv.value_text = ANY($4::text[])))`,
					fmt.Sprintf("a value outside %v", v.Enum), v.Enum)
			}
			if v.Type == "number" && (v.Min != nil || v.Max != nil) {
				r.items(domain.ViolationRange, subject, k.Kind, p, `
					AND EXISTS (SELECT 1 FROM statement_values sv WHERE sv.item_id = ki.id AND sv.predicate = $3
					            AND ((sv.value_number < $4::float8) OR (sv.value_number > $5::float8)))`,
					"a number outside the declared bounds", v.Min, v.Max)
			}
		}
	}

	rels := append([]domain.RelationSpec(nil), snap.Doc.Relations...)
	sort.Slice(rels, func(i, j int) bool { return rels[i].Name < rels[j].Name })
	for _, rel := range rels {
		subject := "relation " + rel.Name
		if rel.FromKind != "" {
			r.endpoint(domain.ViolationFromKind, subject, rel.Name, "from_ref", rel.FromKind)
		}
		if rel.ToKind != "" {
			r.endpoint(domain.ViolationToKind, subject, rel.Name, "to_ref", rel.ToKind)
		}
		if rel.SingleValued() {
			r.cardinality(subject, rel.Name)
		}
		if rel.Required {
			r.requiredLink(subject, rel.Name, rel.FromKind)
		}
	}
	if r.err != nil {
		return nil, false, r.err
	}
	if len(r.out) > limit {
		return r.out[:limit], true, nil
	}
	return r.out, false, nil
}

// report accumulates violations, stopping once one more than the limit is held
// (that extra row is what tells the caller the report was truncated).
type report struct {
	o     *Ontology
	ctx   context.Context
	scope *domain.TagPredicate
	ns    string
	limit int
	out   []domain.OntologyViolation
	err   error
}

func (r *report) room() int { return r.limit - len(r.out) }

// query runs one rule's SELECT, which returns (ref, other) text rows; detail
// renders the row's message from other. The fence is already part of sql, so
// args include its parameters. Only this package's constants and $n
// placeholders reach SQL text — names and values travel as parameters.
func (r *report) query(rule, subject, sql string, args []any, detail func(other string) string) {
	if r.err != nil || r.room() <= 0 {
		return
	}
	args = append(args, r.room())
	rows, err := r.o.pool.Query(r.ctx, sql+fmt.Sprintf(" ORDER BY 1 LIMIT $%d", len(args)), args...)
	if err != nil {
		r.err = fmt.Errorf("ontology violations (%s %s): %w", rule, subject, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var ref, other string
		if err := rows.Scan(&ref, &other); err != nil {
			r.err = err
			return
		}
		r.out = append(r.out, domain.OntologyViolation{Rule: rule, Subject: subject, Ref: ref, Detail: detail(other)})
	}
	if err := rows.Err(); err != nil {
		r.err = err
	}
}

// items reports current-winner items of kind that match cond ($1 namespace, $2
// kind, $3 predicate, $4… extra).
func (r *report) items(rule, subject, kind, predicate, cond, detail string, extra ...any) {
	args := append([]any{r.ns, kind, predicate}, extra...)
	fence, fargs := sqlfence.TextArray("ki.classification").Where(r.scope, len(args))
	args = append(args, fargs...)
	r.query(rule, subject, `
		SELECT DISTINCT ki.id, ''::text FROM resolutions res
		JOIN knowledge_items ki ON ki.id = res.item_id
		WHERE res.namespace_id = $1 AND res.kind = $2 AND res.system_to IS NULL`+cond+fence, args,
		func(string) string { return detail })
}

// confirmed is the shape every typed-link rule reads: a confirmed, unretracted
// link of the verb, in the namespace.
const confirmed = `l.namespace_id = $1 AND l.relation = $2 AND l.state = 'confirmed' AND l.retracted_at IS NULL`

// endpoint reports confirmed links whose col is not an entity ref of kind.
func (r *report) endpoint(rule, subject, verb, col, kind string) {
	args := []any{r.ns, verb, domain.RefPrefixEntity + kind + "/"}
	fence, fargs := sqlfence.EvidenceExists(r.scope, "l.evidence_id", len(args))
	args = append(args, fargs...)
	r.query(rule, subject, `
		SELECT l.id, l.`+col+` FROM links l
		WHERE `+confirmed+`
		  AND NOT (starts_with(l.`+col+`, $3) AND length(l.`+col+`) > length($3))`+fence, args,
		func(ref string) string { return fmt.Sprintf("%s is not a %q entity", ref, kind) })
}

// cardinality reports each pair of overlapping confirmed links of a
// cardinality-one verb from the same endpoint (once per pair).
func (r *report) cardinality(subject, verb string) {
	args := []any{r.ns, verb}
	fa, aargs := sqlfence.EvidenceExists(r.scope, "l.evidence_id", len(args))
	args = append(args, aargs...)
	fb, bargs := sqlfence.EvidenceExists(r.scope, "b.evidence_id", len(args))
	args = append(args, bargs...)
	r.query(domain.ViolationCardinality, subject, `
		SELECT l.id, b.id FROM links l
		JOIN links b ON b.namespace_id = l.namespace_id AND b.relation = l.relation
		  AND b.from_ref = l.from_ref AND b.to_ref <> l.to_ref AND b.id > l.id
		  AND b.state = 'confirmed' AND b.retracted_at IS NULL
		  AND `+postgres.LinkValidityOverlaps("b", "l.valid_from", "l.valid_to")+`
		WHERE `+confirmed+fa+fb, args,
		func(other string) string { return "overlaps confirmed link " + other + " from the same endpoint" })
}

// requiredLink reports entities of fromKind holding no readable confirmed link
// of the verb.
func (r *report) requiredLink(subject, verb, fromKind string) {
	args := []any{r.ns, verb, fromKind, domain.RefPrefixEntity}
	fe, eargs := sqlfence.EvidenceExists(r.scope, "en.first_seen_evidence", len(args))
	args = append(args, eargs...)
	fl, largs := sqlfence.EvidenceExists(r.scope, "l.evidence_id", len(args))
	args = append(args, largs...)
	r.query(domain.ViolationRequiredLink, subject, `
		SELECT en.id, ''::text FROM entities en
		WHERE en.namespace_id = $1 AND en.kind = $3`+fe+`
		  AND NOT EXISTS (SELECT 1 FROM links l
		                  WHERE `+confirmed+` AND l.from_ref = $4 || en.id`+fl+`)`, args,
		func(string) string { return fmt.Sprintf("holds no confirmed %q link", verb) })
}
