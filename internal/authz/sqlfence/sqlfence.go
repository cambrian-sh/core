// Package sqlfence is the ONE rendering of the access predicate
// (domain.TagPredicate) to SQL (ADR-0149 D8; AGENTS.md §2.5: row filtering
// belongs in internal/authz and nowhere else).
//
// The in-memory form — TagPredicate.Check, and CheckRow where the row carries
// parties — is authoritative. This package is its SQL mirror, and it is the only
// one: the evidence plane (query plane, catalog), TEXT[] tag columns (knowledge
// items, the fact reader) and jsonb metadata.tags (chunks and the other metadata
// tables) all render through Shape.terms below.
//
// What a reader may see is decided ONCE, in terms:
//
//   - nothing for ScopeMaintenance (Unfiltered), the only unfiltered predicate;
//   - the owner fence (domain/owner_scope.go) on every other predicate, Bypass
//     included;
//   - for a non-Bypass predicate: required ⊆ tags, forbidden ∩ tags = ∅, every
//     any-of clause overlapping (an EMPTY clause is FALSE — it can never
//     overlap), and the party-scoped rule (ADR-0121);
//   - with a valid own-rows term (ADR-0146, TagPredicate.OwnRowTerm):
//     (main) OR (own_row AND ownrows), Check's disjunction term for term.
//
// A Shape only says how a table carries its tags. The differences between the
// shapes are deliberate and each is a parameter with its reason (see Shape):
// the table layout (TEXT[] vs jsonb), whether the row carries parties, how a nil
// predicate is treated, and two purely textual ones (array casts, term order)
// kept so the SQL each caller sends is byte-identical to what it sent before the
// consolidation (testdata/fence_render_sql.golden pins it).
package sqlfence

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/doug-martin/goqu/v9"

	"github.com/cambrian-sh/core/domain"
)

// Shape is how one table carries its tags. Build one with Evidence, TextArray or
// JSONBTags; the zero value is not usable.
type Shape struct {
	// col is the tag column as the query names it (alias-qualified or not).
	col string
	// jsonb: tags live in a jsonb document under "tags" (metadata.tags) rather than
	// a native TEXT[] column. Terms are then one containment test per tag, served
	// by the GIN (jsonb_path_ops) index, and the owner fence is a jsonpath test.
	jsonb bool
	// cast is appended to every bound tag-array placeholder (TEXT[] shapes only).
	// Textual: pgx binds []string as text[] either way; kept for byte identity.
	cast string
	// parties names the row's parties column. Non-empty: the party-scoped rule is
	// CheckRow's row-aware implication (a party-scoped row is admitted only to one
	// of its parties). Empty: the table has no parties column, and — as in Check —
	// a party-scoped tag denies the row outright ("a thing nobody can be a party to
	// is not one of the rows you are a party to").
	parties string
	// nilPanics: the evidence plane was never called with a nil predicate — the
	// query plane and catalog refuse nil with ErrScopeMissing before rendering —
	// and its renderer dereferenced it. That is kept (a nil there is a programming
	// error, and failing loudly is fail-closed). The other shapes render NOTHING for
	// nil on purpose: the fail-closed decision belongs to the chokepoint
	// (EnforcingVectorStore), which refuses the read before it reaches SQL.
	nilPanics bool
	// present: a NULL tag column means the row has NO classification to judge it
	// by — an object table property value that came from an observation without
	// evidence, or no value at all — and every filtered reader is denied it, the
	// way EvidenceExists denies a row whose evidence does not exist. Without it a
	// NULL column would pass a predicate with no tag terms (owner fence only:
	// unnest(NULL) is empty). ScopeMaintenance still sees it (no fence at all).
	present bool
}

// Evidence is the evidence table's shape: TEXT[] alias.classification plus the
// alias.parties column, so party scoping is row-aware (CheckRow's mirror).
func Evidence(alias string) Shape {
	return Shape{col: alias + ".classification", parties: alias + ".parties", nilPanics: true}
}

// TextArray is any native TEXT[] tag column with no parties column (knowledge
// items, the catalog's item read): Check's mirror, party-scoped tags deny.
func TextArray(col string) Shape {
	return Shape{col: col, cast: "::text[]"}
}

// JSONBTags is the jsonb metadata.tags shape (chunks, tools, skills,
// agent_profiles, sections). alias "" is the unqualified `metadata`, which keeps
// the single-table reads byte-identical and on the GIN index; a JOIN passes its
// table alias so `metadata` is never ambiguous.
//
// It binds ONLY on tables that carry tags in metadata. The documents table does
// not (it has a first-class tags column): there RequiredTags would match nothing
// (closed) but ForbiddenTags would match everything (OPEN). Nothing routes a
// scoped read there (TestScopePredicateNeverTargetsDocuments).
func JSONBTags(alias string) Shape {
	col := "metadata"
	if alias != "" {
		col = alias + ".metadata"
	}
	return Shape{col: col, jsonb: true}
}

// ObjectProperty is one property of an object table (ADR-0149 D3,
// internal/objects): its TEXT[] tags companion and its parties companion, the
// classification and parties of the row the property's current value came
// from. The fence applies PER PROPERTY: it is CheckRow's row-aware mirror, as
// for the evidence plane, and a NULL tags column (no classification) is denied
// to every filtered reader (present). A nil predicate panics, as on the evidence
// plane — the objects reader refuses nil with ErrQueryScopeMissing first.
//
// Rendered per fact over native TEXT[] columns, never as jsonb containment: the
// ADR-0149 P2 benchmark showed GIN containment over tags misleading the planner
// by ~3,700x on narrow lists.
func ObjectProperty(tagsCol, partiesCol string) Shape {
	return Shape{col: tagsCol, parties: partiesCol, nilPanics: true, present: true}
}

// Expr renders the fence as one boolean SQL expression with $n placeholders
// numbered from startIdx+1, and its args: "TRUE" and nil when there is no fence.
// For a reader that needs the fence as a value (a CASE WHEN, an OR of fences)
// rather than as a WHERE suffix.
func (s Shape) Expr(scope *domain.TagPredicate, startIdx int) (string, []any) {
	body, args := dollar(s.fence(scope), startIdx)
	if body == "" {
		return "TRUE", nil
	}
	return "(" + body + ")", args
}

// Where renders the fence as an " AND …" suffix with $n placeholders numbered
// from startIdx+1, and its args. "" and nil when there is no fence.
func (s Shape) Where(scope *domain.TagPredicate, startIdx int) (string, []any) {
	body, args := dollar(s.fence(scope), startIdx)
	if body == "" {
		return "", nil
	}
	return " AND " + body, args
}

// Goqu renders the fence as goqu expressions to AND into a WHERE; nil when there
// is no fence.
func (s Shape) Goqu(scope *domain.TagPredicate) []goqu.Expression {
	terms := s.fence(scope)
	if len(terms) == 0 {
		return nil
	}
	out := make([]goqu.Expression, len(terms))
	for i, t := range terms {
		out[i] = t.goqu()
	}
	return out
}

// EvidenceExists renders "may this reader see the row this evidence backs" for any
// column refCol that references evidence.id (observations, events, the outbox):
// an EXISTS against the evidence row aliased e. "" for ScopeMaintenance. The query
// plane's point, history, aggregate, events and related shapes and the catalog all
// use it, so they can never disagree.
func EvidenceExists(scope *domain.TagPredicate, refCol string, startIdx int) (string, []any) {
	conds, args := Evidence("e").Where(scope, startIdx)
	if conds == "" {
		return "", nil
	}
	return ` AND EXISTS (SELECT 1 FROM evidence e
		WHERE e.id = ` + refCol + conds + `)`, args
}

// OwnerFence renders only the owner fence over a TEXT[] column for the held owner
// tags, with its single placeholder at $n — for a listing that applies the owner
// fence without a full predicate (document_list.go).
func OwnerFence(col string, n int) string {
	sql, _ := dollar([]expr{ownerFenceTextArray(col, nil)}, n-1)
	return sql
}

// fence is the predicate as ANDed terms; nil when there is none.
func (s Shape) fence(scope *domain.TagPredicate) []expr {
	if scope == nil {
		if s.nilPanics {
			panic("sqlfence: nil predicate on the evidence plane (callers refuse nil with ErrScopeMissing first)")
		}
		return nil
	}
	if scope.Unfiltered() {
		return nil
	}
	var present []expr
	if s.present {
		present = []expr{leaf(s.col + " IS NOT NULL")}
	}
	main := s.terms(scope)
	own := scope.OwnRowTerm()
	if own == nil {
		return append(present, main...)
	}
	// own_row: the row carries owner:<the reader's single owner>; the own-rows
	// predicate's owner fence (held = that owner) supplies "and no other owner tag".
	ownRow := append([]expr{s.containsOne(domain.OwnerTag(scope.Owners[0]))}, s.terms(own)...)
	return append(present, or(and(main...), and(ownRow...)))
}

// terms renders one predicate's own terms (never its OwnRows). Never empty: the
// owner fence is always first.
func (s Shape) terms(p *domain.TagPredicate) []expr {
	var out []expr
	if s.jsonb {
		out = append(out, ownerFenceJSONB(s.col, p.HeldOwnerTags()))
	} else {
		out = append(out, ownerFenceTextArray(s.col, p.HeldOwnerTags()))
	}
	if p.Bypass {
		return out
	}
	out = append(out, s.all(p.RequiredTags)...)
	// Term order differs only textually (AND commutes); each shape keeps the order
	// its SQL had before the consolidation.
	if s.jsonb {
		out = append(out, s.anyOf(p.AnyOfClauses)...)
		out = append(out, s.none(p.ForbiddenTags)...)
	} else {
		out = append(out, s.none(p.ForbiddenTags)...)
		out = append(out, s.anyOf(p.AnyOfClauses)...)
	}
	return append(out, s.party(p)...)
}

// all: the row carries every tag.
func (s Shape) all(tags []string) []expr {
	if len(tags) == 0 {
		return nil
	}
	if !s.jsonb {
		return []expr{leaf(s.col+" @> ?"+s.cast, tags)}
	}
	out := make([]expr, 0, len(tags))
	for _, t := range tags {
		out = append(out, s.containsOne(t))
	}
	return out
}

// none: the row carries none of the tags.
func (s Shape) none(tags []string) []expr {
	if len(tags) == 0 {
		return nil
	}
	if !s.jsonb {
		return []expr{leaf("NOT ("+s.col+" && ?"+s.cast+")", tags)}
	}
	out := make([]expr, 0, len(tags))
	for _, t := range tags {
		out = append(out, leaf("NOT ("+s.col+" @> ?::jsonb)", tagDoc(t)))
	}
	return out
}

// anyOf: each clause (CNF) overlaps the row's tags.
//
// An EMPTY clause renders FALSE, for every shape: in CNF an empty disjunction is
// unsatisfiable, Check denies it (ReasonAnyOfUnsatisfied), and Postgres agrees
// (`x = ANY('{}')` is false). It used to be skipped, which made the SQL admit
// rows Check denies — wider than the authoritative form. Owner decision
// 2026-11-13: an empty clause means DENY, fail closed
// (T-20261113-sql-fence-mirrors-skip-an-empty-any-of-clause). No resolver is
// known to produce one; TagPredicate.Unsatisfiable reports it so the read that
// carries it is logged as an upstream bug.
func (s Shape) anyOf(clauses [][]string) []expr {
	var out []expr
	for _, clause := range clauses {
		if len(clause) == 0 {
			out = append(out, leaf("FALSE"))
			continue
		}
		if !s.jsonb {
			out = append(out, leaf(s.col+" && ?"+s.cast, clause))
			continue
		}
		ors := make([]expr, 0, len(clause))
		for _, t := range clause {
			ors = append(ors, s.containsOne(t))
		}
		out = append(out, or(ors...))
	}
	return out
}

// party is the party-scoping rule (ADR-0121 D1).
//
// With a parties column it is a material implication — if the row carries a
// party-scoped tag, the reader must be one of its parties. It can only REMOVE
// rows: a row without a party-scoped tag satisfies the left disjunct, and nothing
// here admits a row the tag terms rejected (INV-1: no path widens a scope). With
// no identities the right disjunct is `parties && '{}'` — always false — so every
// party-scoped row is denied (D6's fail-closed reading).
//
// Without a parties column a party-scoped tag denies, exactly like a forbidden tag.
func (s Shape) party(p *domain.TagPredicate) []expr {
	if len(p.PartyScopedTags) == 0 {
		return nil
	}
	if s.parties == "" {
		return s.none(p.PartyScopedTags)
	}
	return []expr{leaf("(NOT ("+s.col+" && ?) OR "+s.parties+" && ?)",
		p.PartyScopedTags, identityArg(p.PartyIdentities))}
}

// containsOne: the row carries this one tag (the own-row test, and the jsonb
// per-tag containment).
func (s Shape) containsOne(tag string) expr {
	if s.jsonb {
		return leaf(s.col+" @> ?::jsonb", tagDoc(tag))
	}
	return leaf(s.col+" @> ARRAY[?::text]", tag)
}

// identityArg renders a reader's identities for the SQL overlap test. A nil slice
// would be sent as NULL, and `parties && NULL` is NULL — still not-true, but by
// three-valued-logic accident. An empty array denies for the reason we mean: this
// reader is a party to nothing.
func identityArg(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

// ownerFenceTextArray is the TEXT[] owner fence: no element in the owner namespace
// outside the bound held-tags array. The held array is never NULL (HeldOwnerTags
// returns '{}' when empty) — `t <> ALL(NULL)` is NULL, which would make the NOT
// EXISTS admit every owner row. The prefix is a constant.
func ownerFenceTextArray(col string, held []string) expr {
	return leaf("NOT EXISTS (SELECT 1 FROM unnest("+col+") AS owner_tag(t) WHERE starts_with(owner_tag.t, '"+
		domain.OwnerTagPrefix+"') AND owner_tag.t <> ALL(?::text[]))", held)
}

// ownerFenceJSONB is the jsonb owner fence: no tag in the reserved owner namespace
// other than the reader's own. The jsonpath text is built from CONSTANTS and
// positional variable names only; every reader-derived value travels in the vars
// object, so nothing a principal id contains can change the path. It is bound as a
// parameter rather than inlined because jsonpath's filter syntax uses '?'. A row
// with no tags, or no owner tag, matches nothing in the path and passes.
func ownerFenceJSONB(col string, held []string) expr {
	cond := `@ starts with "` + domain.OwnerTagPrefix + `"`
	vars := make(map[string]string, len(held))
	for i, t := range held {
		name := fmt.Sprintf("o%d", i)
		cond += " && @ != $" + name
		vars[name] = t
	}
	b, _ := json.Marshal(vars) // map[string]string always marshals
	return leaf("NOT jsonb_path_exists("+col+", ?::jsonpath, ?::jsonb)",
		"lax $.tags[*] ? ("+cond+")", string(b))
}

// tagDoc is the jsonb containment document for one tag under metadata.tags.
func tagDoc(tag string) string {
	b, _ := json.Marshal(map[string][]string{"tags": {tag}}) // always marshals
	return string(b)
}

// expr is a rendered term: a leaf of SQL with one '?' per arg, or an AND / OR of
// terms. Leaf SQL never contains a literal '?' (none of the operators used here is
// jsonb's ?-family), so '?' is always a placeholder.
type expr struct {
	sql  string
	args []any
	op   string // "" leaf, "AND", "OR"
	kids []expr
}

func leaf(sql string, args ...any) expr { return expr{sql: sql, args: args} }
func and(kids ...expr) expr             { return expr{op: "AND", kids: kids} }
func or(kids ...expr) expr              { return expr{op: "OR", kids: kids} }

// dollar renders ANDed terms with $n placeholders from startIdx+1.
func dollar(terms []expr, startIdx int) (string, []any) {
	n := startIdx
	var args []any
	var render func(e expr) string
	render = func(e expr) string {
		if e.op == "" {
			var b strings.Builder
			parts := strings.Split(e.sql, "?")
			for i, part := range parts {
				b.WriteString(part)
				if i < len(parts)-1 {
					n++
					b.WriteString("$" + strconv.Itoa(n))
				}
			}
			args = append(args, e.args...)
			return b.String()
		}
		kids := make([]string, len(e.kids))
		for i, k := range e.kids {
			kids[i] = render(k)
		}
		return "(" + strings.Join(kids, " "+e.op+" ") + ")"
	}
	parts := make([]string, len(terms))
	for i, t := range terms {
		parts[i] = render(t)
	}
	return strings.Join(parts, " AND "), args
}

// goqu renders a term as a goqu expression; '?' is goqu's own placeholder.
func (e expr) goqu() goqu.Expression {
	if e.op == "" {
		return goqu.L(e.sql, e.args...)
	}
	kids := make([]goqu.Expression, len(e.kids))
	for i, k := range e.kids {
		kids[i] = k.goqu()
	}
	if e.op == "OR" {
		return goqu.Or(kids...)
	}
	return goqu.And(kids...)
}
