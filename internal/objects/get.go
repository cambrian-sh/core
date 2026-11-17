package objects

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz/sqlfence"
)

// GetObject (ADR-0149 D7): one call for an object page, built only from
// existing pieces — the object row (current values and their provenance), the
// entity handle, confirmed links, events, observations, and the kind's
// lifecycle declaration. Every read is fenced through the one fence renderer
// (sqlfence): values per property (ObjectProperty with the value's row tags)
// and undeclared values per entry (ObjectExtraRowTags); links, events and
// observations by their own evidence AND their own row classification
// (TypedRowExists, ADR-0158 D2: narrow-only, identical for an empty one);
// alternative item values by their classification (TextArray). Nothing the
// caller cannot see is present — omitted, never redacted.

// Bounds of an object page.
const (
	getMaxLinks        = 200
	getTimelineEvents  = 25
	getTimelineChanges = 25
)

// Get answers GetObject for entityID as the holder of scope. An id that is not
// an object of a table kind, an object the caller may not know exists
// (ADR-0128 D12: it can read neither the evidence that minted it, nor any of its
// values, nor any link touching it) and an unknown id are all
// domain.ErrObjectNotFound.
func (r *Reader) Get(ctx context.Context, scope *domain.TagPredicate, namespace, entityID string) (domain.ObjectView, error) {
	if scope == nil {
		return domain.ObjectView{}, domain.ErrQueryScopeMissing
	}
	ns := orDefault(namespace)
	reg := r.kinds.KindsFor(ns)
	spec, ok := reg.ObjectKind(entityID)
	if !ok || !spec.Table {
		return domain.ObjectView{}, domain.ErrObjectNotFound
	}
	view := domain.ObjectView{EntityID: entityID, Kind: spec.Kind, Namespace: ns}
	props := declaredProps(spec)
	err := r.read(ctx, func(tx pgx.Tx) error {
		known, conflicted, err := readObjectRow(ctx, tx, scope, spec, props, ns, entityID, &view)
		if err != nil {
			return err
		}
		if err := readLinks(ctx, tx, scope, reg, r.relations.RelationsFor(ns), ns, entityID, &view); err != nil {
			return err
		}
		if !known && len(view.Properties) == 0 && len(view.Links) == 0 && len(view.Aliases) == 0 {
			return domain.ErrObjectNotFound
		}
		if len(conflicted) > 0 {
			if err := readAlternatives(ctx, tx, scope, reg, spec, ns, entityID, conflicted, &view); err != nil {
				return err
			}
		}
		// After the existence decision on purpose: extras never make an object
		// visible (D12 stays narrower, fail-closed).
		if err := readExtras(ctx, tx, scope, spec, ns, entityID, &view); err != nil {
			return err
		}
		return readTimeline(ctx, tx, scope, ns, entityID, &view)
	})
	if errors.Is(err, domain.ErrObjectNotFound) {
		return domain.ObjectView{}, domain.ErrObjectNotFound
	}
	if err != nil {
		return domain.ObjectView{}, err
	}
	view.Lifecycle = lifecycleOf(spec, view.Properties)
	view.Evidence = citedEvidence(view)
	return view, nil
}

// readObjectRow reads the object row: each declared property's value and
// provenance where visible, the handle's meta where the mint is readable.
// known: the caller may read the mint — its evidence narrowed by the minting
// row's classification (the typed-row fence, ADR-0158 D4). conflicted: the
// visible properties whose provenance records a conflict.
func readObjectRow(ctx context.Context, tx pgx.Tx, scope *domain.TagPredicate, spec domain.KindSpec, props []prop,
	ns, entityID string, view *domain.ObjectView) (known bool, conflicted map[string]int, err error) {
	b := &sqlb{scope: scope, rt: allRowTagged}
	cols := []string{"o.decl_version", "o.updated_at",
		"(TRUE" + b.typedRowVisible("en.first_seen_evidence", "en.first_seen_classification") + ")"}
	for _, p := range props {
		vis := b.visible("o", p)
		cols = append(cols, "CASE WHEN "+vis+" THEN o."+ident(p.valueCol())+" END",
			"CASE WHEN "+vis+" THEN o.prov -> "+b.arg(p.Name)+" END")
	}
	cols = append(cols, "en.created_at", "en.first_seen_evidence")
	sql := "SELECT " + strings.Join(cols, ", ") + " FROM " + ident(TableName(spec.Kind)) +
		" o LEFT JOIN entities en ON en.id = o.entity_id AND en.namespace_id = o.namespace_id" +
		" WHERE o.namespace_id = " + b.arg(ns) + " AND o.entity_id = " + b.arg(entityID)
	rows, err := tx.Query(ctx, sql, b.args...)
	if err != nil {
		return false, nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return false, nil, err
		}
		return false, nil, domain.ErrObjectNotFound
	}
	vals, err := rows.Values()
	if err != nil {
		return false, nil, err
	}
	if dv, ok := vals[0].(int64); ok {
		view.DeclVersion = dv
	}
	if t, ok := vals[1].(time.Time); ok {
		view.UpdatedAt = t.UTC()
	}
	known, _ = vals[2].(bool)
	conflicted = map[string]int{}
	for i, p := range props {
		v, ok := typedValue(p, vals[3+2*i])
		if !ok {
			continue
		}
		op := domain.ObjectProperty{Property: p.Name, Value: v}
		if raw, ok := vals[4+2*i].(map[string]any); ok {
			pv := decodeProv(raw)
			op.Source, op.At, op.EvidenceID = pv.Source, pv.at(), pv.Evidence
			if pv.Conflict {
				conflicted[p.Name] = len(view.Properties)
			}
		}
		view.Properties = append(view.Properties, op)
	}
	if known {
		n := len(vals)
		if t, ok := vals[n-2].(time.Time); ok {
			t = t.UTC()
			view.CreatedAt = &t
		}
		view.FirstSeenEvidence, _ = vals[n-1].(string)
	}
	return known, conflicted, rows.Err()
}

// readExtras reads an open kind's undeclared properties from the row's extra
// jsonb, each entry fenced by its own tags/parties/row_tags
// (sqlfence.ObjectExtraRowTags), in name order. A key the kind now declares is skipped: the declared column is the value
// of record, and a row written before the declaration may still carry it in extra.
func readExtras(ctx context.Context, tx pgx.Tx, scope *domain.TagPredicate, spec domain.KindSpec,
	ns, entityID string, view *domain.ObjectView) error {
	b := &sqlb{scope: scope}
	sql := "SELECT x.key, x.value FROM " + ident(TableName(spec.Kind)) + " o, jsonb_each(o." + colExtra + ") AS x(key, value)" +
		" WHERE o.namespace_id = " + b.arg(ns) + " AND o.entity_id = " + b.arg(entityID)
	fence, args := sqlfence.ObjectExtraRowTags("x.value").Expr(scope, len(b.args))
	b.args = append(b.args, args...)
	if fence != "TRUE" {
		sql += " AND " + fence
	}
	sql += " ORDER BY x.key"
	rows, err := tx.Query(ctx, sql, b.args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var raw map[string]any
		if err := rows.Scan(&key, &raw); err != nil {
			return err
		}
		if _, declared := spec.Predicates[key]; declared {
			continue
		}
		pv := decodeProv(raw)
		v, ok := extraValue(key, pv)
		if !ok {
			continue // a malformed entry yields nothing, never a wrong value
		}
		view.Extras = append(view.Extras, domain.ObjectProperty{
			Property: key, Value: v, Source: pv.Source, At: pv.at(), EvidenceID: pv.Evidence, Conflict: pv.Conflict,
		})
	}
	return rows.Err()
}

// extraValue is an extra entry's typed value (written by jsonValue).
func extraValue(key string, pv provenance) (domain.StatementValue, bool) {
	v := domain.StatementValue{Predicate: key, Type: pv.Type}
	switch pv.Type {
	case "number":
		f, ok := pv.Value.(float64)
		if !ok {
			return domain.StatementValue{}, false
		}
		v.Number = f
	case "date":
		s, _ := pv.Value.(string)
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return domain.StatementValue{}, false
		}
		v.Date = t.UTC()
	case "entity":
		s, ok := pv.Value.(string)
		if !ok {
			return domain.StatementValue{}, false
		}
		v.EntityRef = s
	default:
		s, ok := pv.Value.(string)
		if !ok {
			return domain.StatementValue{}, false
		}
		v.Text = s
	}
	return v, true
}

func decodeProv(raw map[string]any) provenance {
	var pv provenance
	b, _ := json.Marshal(raw)  // it came from jsonb
	_ = json.Unmarshal(b, &pv) // best-effort: a malformed prov entry yields empty provenance, never a wrong value
	return pv
}

func (p provenance) at() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, p.At) // written by provOf; zero on a malformed entry
	return t.UTC()
}

// readLinks reads the confirmed, unretracted links touching the object that the
// caller may read (their own evidence and row classification), newest first: identity links become
// aliases, every other link a typed link in its direction.
func readLinks(ctx context.Context, tx pgx.Tx, scope *domain.TagPredicate, kinds *domain.KindRegistry,
	rels *domain.RelationRegistry, ns, entityID string, view *domain.ObjectView) error {
	b := &sqlb{scope: scope}
	ref := domain.EntityRef(entityID)
	sql := `SELECT l.id, l.relation, l.family, l.from_ref, l.to_ref, COALESCE(l.evidence_id, ''), l.asserted_at, l.valid_from, l.valid_to
		FROM links l WHERE l.namespace_id = ` + b.arg(ns) + ` AND (l.from_ref = ` + b.arg(ref) + ` OR l.to_ref = $2)
		  AND l.state = '` + domain.LinkStateConfirmed + `' AND l.retracted_at IS NULL` + b.typedRowVisible("l.evidence_id", "l.classification") + `
		ORDER BY l.asserted_at DESC, l.id LIMIT ` + strconv.Itoa(getMaxLinks)
	rows, err := tx.Query(ctx, sql, b.args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var l domain.ObjectLink
		var from, to string
		if err := rows.Scan(&l.LinkID, &l.Relation, &l.Family, &from, &to, &l.EvidenceID, &l.AssertedAt, &l.ValidFrom, &l.ValidTo); err != nil {
			return err
		}
		l.AssertedAt = l.AssertedAt.UTC()
		l.Direction, l.OtherRef, l.Label = domain.ObjectLinkOut, to, l.Relation
		if from != ref {
			l.Direction, l.OtherRef = domain.ObjectLinkIn, from
			if rs, ok := rels.Spec(l.Relation); ok && rs.Inverse != "" {
				l.Label = rs.Inverse
			}
		}
		if id, ok := strings.CutPrefix(l.OtherRef, domain.RefPrefixEntity); ok {
			if k, ok := kinds.ObjectKind(id); ok {
				l.OtherKind = k.Kind
			} else if stem, ok := domain.EntityKindFromID(id); ok {
				l.OtherKind = stem
			}
		}
		if l.Family == domain.LinkFamilyIdentity {
			view.Aliases = append(view.Aliases, l)
		} else {
			view.Links = append(view.Links, l)
		}
	}
	return rows.Err()
}

// readAlternatives fills each conflicted property's alternatives: every other
// source's best current value (BestBySource over the same history the
// maintainer reads) that differs from the shown value AND whose row the caller
// may read. A disagreement the caller cannot see is not reported.
func readAlternatives(ctx context.Context, tx pgx.Tx, scope *domain.TagPredicate, reg *domain.KindRegistry,
	spec domain.KindSpec, ns, entityID string, conflicted map[string]int, view *domain.ObjectView) error {
	cands, err := readCandidates(ctx, tx, reg, ns, entityID)
	if err != nil {
		return err
	}
	byPred := map[string][]Candidate{}
	for _, c := range cands {
		if vs, ok := spec.Predicates[c.Predicate]; ok && c.Value.Type == vs.Type {
			byPred[c.Predicate] = append(byPred[c.Predicate], c)
		}
	}
	var alts []Candidate
	for pred, i := range conflicted {
		shown := view.Properties[i]
		for _, c := range BestBySource(byPred[pred], spec.Predicates[pred].Authority) {
			if c.Source != shown.Source && !SameValue(c.Value, shown.Value) {
				alts = append(alts, c)
			}
		}
	}
	readable, err := readableRows(ctx, tx, scope, alts)
	if err != nil {
		return err
	}
	sort.Slice(alts, func(i, j int) bool {
		if !alts[i].At.Equal(alts[j].At) {
			return alts[i].At.After(alts[j].At)
		}
		return alts[i].ID > alts[j].ID
	})
	for _, c := range alts {
		if !readable[c.ID] {
			continue
		}
		i := conflicted[c.Predicate]
		p := &view.Properties[i]
		p.Alternatives = append(p.Alternatives, domain.ObjectAlternative{
			Value: c.Value, At: c.At, Source: c.Source, EvidenceID: c.EvidenceID})
		p.Conflict = true
	}
	return nil
}

// readableRows reports which candidates' source rows the caller may read:
// observations by their evidence and row classification, items by their
// classification.
func readableRows(ctx context.Context, tx pgx.Tx, scope *domain.TagPredicate, cs []Candidate) (map[string]bool, error) {
	var obs []int64
	var items []string
	for _, c := range cs {
		if id, ok := strings.CutPrefix(c.ID, "o"); ok {
			n, err := strconv.ParseInt(id, 10, 64)
			if err == nil {
				obs = append(obs, n)
			}
		} else if id, ok := strings.CutPrefix(c.ID, "i"); ok {
			items = append(items, id)
		}
	}
	out := map[string]bool{}
	if len(obs) > 0 {
		fence, fargs := sqlfence.TypedRowExists(scope, "o.evidence_id", "o.classification", 1)
		rows, err := tx.Query(ctx, `SELECT o.id FROM observations o WHERE o.id = ANY($1)`+fence, append([]any{obs}, fargs...)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			out["o"+leftPad(id)] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	if len(items) > 0 {
		fence, fargs := sqlfence.TextArray("ki.classification").Where(scope, 1)
		rows, err := tx.Query(ctx, `SELECT ki.id FROM knowledge_items ki WHERE ki.id = ANY($1)`+fence, append([]any{items}, fargs...)...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			out["i"+id] = true
		}
		return out, rows.Err()
	}
	return out, nil
}

// leftPad renders an observation id the way readCandidates keys it.
func leftPad(id int64) string {
	s := strconv.FormatInt(id, 10)
	return strings.Repeat("0", max(0, 20-len(s))) + s
}

// readTimeline reads the object's recent timeline: the latest events it took
// part in and the latest recorded values about it, each visible by its own
// evidence and row classification and not retired, merged newest first.
func readTimeline(ctx context.Context, tx pgx.Tx, scope *domain.TagPredicate, ns, entityID string, view *domain.ObjectView) error {
	b := &sqlb{scope: scope}
	sql := `SELECT ev.id, ev.event_type, ev.occurred_at, COALESCE(ev.evidence_id, '') FROM events ev
		WHERE ev.namespace_id = ` + b.arg(ns) + ` AND EXISTS (SELECT 1 FROM event_roles r WHERE r.event_id = ev.id AND r.entity_id = ` + b.arg(entityID) + `)` +
		NotRetired("ev") + b.typedRowVisible("ev.evidence_id", "ev.classification") + `
		ORDER BY ev.occurred_at DESC, ev.id DESC LIMIT ` + strconv.Itoa(getTimelineEvents)
	rows, err := tx.Query(ctx, sql, b.args...)
	if err != nil {
		return err
	}
	var entries []domain.ObjectTimelineEntry
	var eventIDs []string
	for rows.Next() {
		e := domain.ObjectTimelineEntry{Kind: domain.ObjectTimelineEvent}
		if err := rows.Scan(&e.EventID, &e.EventType, &e.At, &e.EvidenceID); err != nil {
			rows.Close()
			return err
		}
		e.At = e.At.UTC()
		entries = append(entries, e)
		eventIDs = append(eventIDs, e.EventID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(eventIDs) > 0 {
		roles := map[string]map[string]string{}
		rows, err := tx.Query(ctx, `SELECT event_id, role, entity_id FROM event_roles WHERE event_id = ANY($1) ORDER BY event_id, role, entity_id`, eventIDs)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, role, ent string
			if err := rows.Scan(&id, &role, &ent); err != nil {
				rows.Close()
				return err
			}
			if roles[id] == nil {
				roles[id] = map[string]string{}
			}
			roles[id][role] = ent
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range entries {
			entries[i].Roles = roles[entries[i].EventID]
		}
	}

	b = &sqlb{scope: scope}
	sql = `SELECT o.predicate, o.value_type, o.value_date, o.value_number, o.value_text, o.value_entity_id, o.occurred_at, COALESCE(o.evidence_id, '')
		FROM observations o WHERE o.namespace_id = ` + b.arg(ns) + ` AND o.entity_id = ` + b.arg(entityID) +
		NotRetired("o") + b.typedRowVisible("o.evidence_id", "o.classification") + `
		ORDER BY o.occurred_at DESC, o.id DESC LIMIT ` + strconv.Itoa(getTimelineChanges)
	rows, err = tx.Query(ctx, sql, b.args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var c Candidate
		if err := scanValue(rows, &c, func(v ...any) []any { return append(v, &c.At, &c.EvidenceID) }); err != nil {
			return err
		}
		entries = append(entries, domain.ObjectTimelineEntry{Kind: domain.ObjectTimelineProperty, At: c.At,
			Property: c.Predicate, Value: c.Value, EvidenceID: c.EvidenceID})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].At.After(entries[j].At) })
	view.Timeline = entries
	return nil
}

// lifecycleOf is the object's lifecycle section: its visible state and the
// states the declaration allows next (the initial states when it has none).
func lifecycleOf(spec domain.KindSpec, props []domain.ObjectProperty) *domain.ObjectLifecycle {
	lc := spec.Lifecycle
	if lc == nil {
		return nil
	}
	out := &domain.ObjectLifecycle{StatePredicate: lc.StatePredicate}
	for _, p := range props {
		if p.Property == lc.StatePredicate {
			out.State = p.Value.Text
		}
	}
	if out.State == "" {
		out.Allowed = append([]string{}, lc.Initial...)
	} else {
		out.Allowed = append([]string{}, lc.Transitions[out.State]...)
	}
	return out
}

// citedEvidence lists, once each in order of first citation, the evidence the
// page cites.
func citedEvidence(v domain.ObjectView) []string {
	var out []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	add(v.FirstSeenEvidence)
	for _, p := range v.Properties {
		add(p.EvidenceID)
		for _, a := range p.Alternatives {
			add(a.EvidenceID)
		}
	}
	for _, p := range v.Extras {
		add(p.EvidenceID)
	}
	for _, l := range append(append([]domain.ObjectLink{}, v.Aliases...), v.Links...) {
		add(l.EvidenceID)
	}
	for _, e := range v.Timeline {
		add(e.EvidenceID)
	}
	return out
}
