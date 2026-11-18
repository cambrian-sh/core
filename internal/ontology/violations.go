package ontology

import (
	"context"
	"fmt"
	"sort"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz/sqlfence"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/objects"
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
//   - entities of a required verb's from_kind holding no such link;
//   - values about an object of a declared kind (its entity id stem is the
//     kind's prefix) whose type is not the property's declared type: the latest
//     such non-retired observation per entity and predicate, and current-winner
//     items carrying one. They stay in history and are not projected into the
//     object table (ADR-0149 D3);
//   - observations under the reserved "observation" kind (entities whose id
//     stem names no declared kind, ADR-0110 D2; ENG-1115): the CURRENT value
//     per (entity, predicate) — the latest non-retired observation the caller
//     can read, the value PointLookup answers — of a declared predicate whose
//     type differs, whose text is outside the enum, or whose number is outside
//     min/max. Required does not apply: the reserved kind has no population of
//     entities that must carry a predicate.
//
// Every read is fenced by scope through the one fence renderer (internal/authz/
// sqlfence): items by their own classification, links by every evidence row they
// rest on (sqlfence.LinkExists, the query plane's filterLinks rule), entities by the evidence that minted them
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
	// The id stems of every declared kind: an observation about one of these
	// validates under that kind, every other under the reserved kind — the
	// split RecordObservation makes (KindRegistry.ObjectKind).
	var prefixes []string
	for _, k := range kinds {
		if prefix := k.ObjectPrefix(); prefix != "" {
			prefixes = append(prefixes, prefix)
		}
	}
	for _, k := range kinds {
		preds := make([]string, 0, len(k.Predicates))
		for p := range k.Predicates {
			preds = append(preds, p)
		}
		sort.Strings(preds)
		for _, p := range preds {
			v := k.Predicates[p]
			subject := "kind " + k.Kind + "." + p
			if k.Kind == domain.ObservationKind {
				r.observationRules(subject, prefixes, p, v)
				continue
			}
			if prefix := k.ObjectPrefix(); prefix != "" {
				r.typeMismatch(subject, prefix, p, v.Type)
			}
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
	fence, fargs := sqlfence.LinkExists(r.scope, "l", len(args))
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
	fa, aargs := sqlfence.LinkExists(r.scope, "l", len(args))
	args = append(args, aargs...)
	fb, bargs := sqlfence.LinkExists(r.scope, "b", len(args))
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
// of the verb. The entity is fenced by its mint: the minting evidence narrowed
// by the minting row's classification (ADR-0158 D4).
func (r *report) requiredLink(subject, verb, fromKind string) {
	args := []any{r.ns, verb, fromKind, domain.RefPrefixEntity}
	fe, eargs := sqlfence.TypedRowExists(r.scope, "en.first_seen_evidence", "en.first_seen_classification", len(args))
	args = append(args, eargs...)
	fl, largs := sqlfence.LinkExists(r.scope, "l", len(args))
	args = append(args, largs...)
	r.query(domain.ViolationRequiredLink, subject, `
		SELECT en.id, ''::text FROM entities en
		WHERE en.namespace_id = $1 AND en.kind = $3`+fe+`
		  AND NOT EXISTS (SELECT 1 FROM links l
		                  WHERE `+confirmed+` AND l.from_ref = $4 || en.id`+fl+`)`, args,
		func(string) string { return fmt.Sprintf("holds no confirmed %q link", verb) })
}

// observationRules reports the reserved kind's declared predicate p against the
// current observation of every entity outside the declared kinds' stems.
func (r *report) observationRules(subject string, prefixes []string, p string, v domain.ValueSpec) {
	if v.Type != "" {
		r.observations(domain.ViolationType, subject, prefixes, p, `cur.value_type <> $4`, `cur.value_type`,
			func(found string) string { return fmt.Sprintf("a current %s value, declared %s", found, v.Type) }, v.Type)
	}
	if len(v.Enum) > 0 {
		r.observations(domain.ViolationEnum, subject, prefixes, p,
			`cur.value_text IS NOT NULL AND NOT (cur.value_text = ANY($4::text[]))`, `cur.value_text`,
			func(found string) string { return fmt.Sprintf("the current value %q is outside %v", found, v.Enum) }, v.Enum)
	}
	if v.Type == "number" && (v.Min != nil || v.Max != nil) {
		r.observations(domain.ViolationRange, subject, prefixes, p,
			`((cur.value_number < $4::float8) OR (cur.value_number > $5::float8))`, `cur.value_number::text`,
			func(found string) string { return "the current value " + found + " is outside the declared bounds" }, v.Min, v.Max)
	}
}

// observations runs one rule over the current observation per entity of
// predicate under the reserved kind ($1 namespace, $2 predicate, $3 declared
// stems, $4… extra). "Current" is the latest non-retired observation the
// caller may read (fenced by its evidence, like typeMismatch): a hidden later
// value does not exist for this reader, so the visible one is what it is told
// about. An entity id whose stem is a declared kind's is excluded — it is
// validated, and reported, under that kind.
func (r *report) observations(rule, subject string, prefixes []string, predicate, cond, other string, detail func(string) string, extra ...any) {
	args := append([]any{r.ns, predicate, prefixes}, extra...)
	fence, fargs := sqlfence.TypedRowExists(r.scope, "o.evidence_id", "o.classification", len(args))
	args = append(args, fargs...)
	r.query(rule, subject, `
		SELECT cur.entity_id, `+other+` FROM (
			SELECT DISTINCT ON (o.entity_id) o.entity_id, o.value_type, o.value_text, o.value_number
			FROM observations o
			WHERE o.namespace_id = $1 AND o.predicate = $2
			  AND NOT (o.entity_id ~ '^[^/]+/.' AND split_part(o.entity_id, '/', 1) = ANY($3::text[]))`+fence+objects.NotRetired("o")+`
			ORDER BY o.entity_id, o.occurred_at DESC, o.id DESC) cur
		WHERE `+cond, args, detail)
}

// typeMismatch reports values about objects of prefix whose predicate is
// declared with another type: the latest mismatched non-retired observation per
// (entity, predicate), fenced by its evidence, and current-winner items fenced
// by their classification ($1 namespace, $2 prefix, $3 predicate, $4 type).
func (r *report) typeMismatch(subject, prefix, predicate, valueType string) {
	detail := func(found string) string {
		return fmt.Sprintf("a %s value, declared %s (kept in history, not projected)", found, valueType)
	}
	args := []any{r.ns, prefix, predicate, valueType}
	fence, fargs := sqlfence.TypedRowExists(r.scope, "o.evidence_id", "o.classification", len(args))
	args = append(args, fargs...)
	r.query(domain.ViolationType, subject, `
		SELECT entity_id, value_type FROM (
			SELECT DISTINCT ON (o.entity_id) o.entity_id, o.value_type FROM observations o
			WHERE o.namespace_id = $1 AND split_part(o.entity_id, '/', 1) = $2 AND o.predicate = $3
			  AND o.value_type <> $4`+fence+objects.NotRetired("o")+`
			ORDER BY o.entity_id, o.occurred_at DESC, o.id DESC) mismatched`, args, detail)

	args = []any{r.ns, prefix, predicate, valueType}
	fence, fargs = sqlfence.TextArray("ki.classification").Where(r.scope, len(args))
	args = append(args, fargs...)
	r.query(domain.ViolationType, subject, `
		SELECT DISTINCT ki.id, sv.value_type FROM resolutions res
		JOIN knowledge_items ki ON ki.id = res.item_id
		JOIN statement_values sv ON sv.item_id = ki.id AND sv.predicate = $3 AND sv.value_type <> $4
		WHERE res.namespace_id = $1 AND res.system_to IS NULL AND split_part(res.entity_id, '/', 1) = $2`+fence, args, detail)
}
