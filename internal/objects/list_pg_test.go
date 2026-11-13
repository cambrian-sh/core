package objects_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/objects"
	"github.com/cambrian-sh/core/internal/ontology"
)

// ADR-0149 P4 integration tests: ListObjects and GetObject over real Postgres,
// with history written through the REAL stores (so the object rows are the
// maintainer's), against a brute-force reference computed from history and the
// in-memory predicate (TagPredicate.CheckRow — authoritative; sqlfence is its
// SQL mirror). D9: "ListObjects results equal a brute-force reference computed
// from history (NoREC-style), including under narrow scopes"; keyset paging
// never skips or duplicates.

const (
	lpOrder = "lporder"
	lpCust  = "lpcust"
	lpVerb  = "lp_placed_by"
)

func lpKinds() []domain.KindSpec {
	return []domain.KindSpec{
		{Kind: lpOrder, Table: true, Open: true, Predicates: map[string]domain.ValueSpec{
			"status":   {Type: "text", Indexed: true},
			"total":    {Type: "number", Indexed: true},
			"due":      {Type: "date"},
			"customer": {Type: "entity"},
		}},
		{Kind: lpCust, Table: true, Predicates: map[string]domain.ValueSpec{"segment": {Type: "text"}}},
	}
}

func lpRelations() []domain.RelationSpec {
	return []domain.RelationSpec{{Name: lpVerb, Family: domain.LinkFamilyRelation, FromKind: lpOrder, ToKind: lpCust,
		Cardinality: domain.CardinalityMany, Inverse: "lp_has_order"}}
}

// lpEnv is one test's world with the real stores and the reader.
type lpEnv struct {
	*env
	links    *postgres.PgLinkStore
	entities *postgres.PgEntityStore
	reader   *objects.Reader
}

func newLPEnv(t *testing.T, ns string) *lpEnv {
	t.Helper()
	pool, ctx := pgPool(t)
	clean := func() {
		bg := context.Background()
		for _, q := range []string{
			`DELETE FROM event_roles WHERE event_id IN (SELECT id FROM events WHERE namespace_id = $1)`,
			`DELETE FROM events WHERE namespace_id = $1`,
			`DELETE FROM links WHERE namespace_id = $1`,
			`DELETE FROM entities WHERE namespace_id = $1`,
			`DELETE FROM typed_row_retirements WHERE namespace_id = $1`,
			`DELETE FROM observations WHERE namespace_id = $1`,
			`DELETE FROM resolutions WHERE namespace_id = $1`,
			`DELETE FROM knowledge_items WHERE namespace_id = $1`,
			`DELETE FROM evidence WHERE namespace_id = $1`,
		} {
			_, _ = pool.Exec(bg, q, ns) // best-effort cleanup of a test namespace
		}
		for _, k := range []string{lpOrder, lpCust} {
			_, _ = pool.Exec(bg, `DROP TABLE IF EXISTS `+objects.TableName(k))                                  // test-only
			_, _ = pool.Exec(bg, `DELETE FROM object_table_builds WHERE table_name = $1`, objects.TableName(k)) // test-only
		}
	}
	clean()
	t.Cleanup(clean)
	o, err := ontology.Load(ctx, pool, domain.OntologyDoc{Kinds: lpKinds(), Relations: lpRelations()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, ctx: ctx, pool: pool, ns: ns, onto: o,
		events: postgres.NewPgEventStore(pool, o), know: postgres.NewPgKnowledgeStore(pool, o)}
	return &lpEnv{env: e, links: postgres.NewPgLinkStore(pool, o), entities: postgres.NewPgEntityStore(pool),
		reader: objects.NewReader(pool, o, o)}
}

// evidenceRow is one evidence row the test writes: its tags and parties.
type evidenceRow struct {
	id, source string
	tags       []string
	parties    []string
}

var lpEvidence = []evidenceRow{
	{"south", "erp", []string{"r:south"}, nil},
	{"north", "crm", []string{"r:north"}, nil},
	{"conf", "erp2", []string{"r:south", "cls:confidential"}, []string{"am:1"}},
	{"restr", "erp3", []string{"r:south", "cls:restricted"}, nil},
	{"global", "wms", []string{"r:global"}, nil},
}

func (e *lpEnv) seedEvidence(prefix string) map[string]evidenceRow {
	e.t.Helper()
	out := map[string]evidenceRow{}
	for _, r := range lpEvidence {
		id := prefix + r.id
		parties := r.parties
		if parties == nil {
			parties = []string{}
		}
		if _, err := e.pool.Exec(e.ctx, `INSERT INTO evidence (id, namespace_id, source_id, source_key, content_hash, classification, parties)
			VALUES ($1,$2,$3,$1,'sha256:test',$4,$5) ON CONFLICT DO NOTHING`, id, e.ns, r.source, r.tags, parties); err != nil {
			e.t.Fatal(err)
		}
		r.id = id
		out[id] = r
	}
	return out
}

// lpPredicates are the callers the differential reads as.
var lpPredicates = []*domain.TagPredicate{
	domain.ScopeMaintenance,
	domain.ScopeSystem,
	{AnyOfClauses: [][]string{{"r:south", "r:global"}}, ForbiddenTags: []string{"cls:restricted"},
		PartyScopedTags: []string{"cls:confidential"}, PartyIdentities: []string{"am:1"}},
	{AnyOfClauses: [][]string{{"r:south", "r:global"}}, ForbiddenTags: []string{"cls:restricted"},
		PartyScopedTags: []string{"cls:confidential"}},
	{AnyOfClauses: [][]string{{"r:north"}}},
	{RequiredTags: []string{"r:south"}, ForbiddenTags: []string{"ops"}},
}

// ── the reference ────────────────────────────────────────────────────────────

// refCand is a history candidate with its visibility.
type refWorld struct {
	states  map[string]objects.State // entity -> state (from ALL of history)
	kinds   map[string]string        // entity -> kind
	minted  map[string]string        // entity -> first_seen evidence ("" = none)
	evRows  map[string]evidenceRow
	links   []refLink
	itemTag map[string][]string // item id -> classification
}

type refLink struct {
	from, to, relation, evidence string
	family                       string
	confirmed, retracted         bool
}

func (w refWorld) evidenceVisible(p *domain.TagPredicate, id string) bool {
	if p.Unfiltered() {
		return true
	}
	r, ok := w.evRows[id]
	if !ok {
		return false
	}
	_, allowed := p.CheckRow(r.tags, nonNilS(r.parties))
	return allowed
}

func nonNilS(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// visible is the per-property rule's reference: a value with no
// classification (Tags nil) is visible only to ScopeMaintenance.
func visible(p *domain.TagPredicate, c objects.Property) bool {
	if p.Unfiltered() {
		return true
	}
	if c.Tags == nil {
		return false
	}
	_, ok := p.CheckRow(c.Tags, nonNilS(c.Parties))
	return ok
}

func (w refWorld) value(p *domain.TagPredicate, entity, prop string) (domain.StatementValue, bool) {
	c, ok := w.states[entity].Props[prop]
	if !ok || !visible(p, c) {
		return domain.StatementValue{}, false
	}
	return c.Value, true
}

func (w refWorld) linkVisible(p *domain.TagPredicate, l refLink) bool {
	return l.confirmed && !l.retracted && w.evidenceVisible(p, l.evidence)
}

func (w refWorld) exists(p *domain.TagPredicate, kind, entity string) bool {
	if p.Unfiltered() {
		return true
	}
	if ev := w.minted[entity]; ev != "" && w.evidenceVisible(p, ev) {
		return true
	}
	for prop := range lpSpec(kind).Predicates {
		if _, ok := w.value(p, entity, prop); ok {
			return true
		}
	}
	return false
}

func lpSpec(kind string) domain.KindSpec {
	for _, k := range lpKinds() {
		if k.Kind == kind {
			return k
		}
	}
	panic(kind)
}

func compareValues(a, b domain.StatementValue) int {
	switch a.Type {
	case "number":
		switch {
		case a.Number < b.Number:
			return -1
		case a.Number > b.Number:
			return 1
		}
		return 0
	case "date":
		return a.Date.Compare(b.Date)
	case "entity":
		return strings.Compare(a.EntityRef, b.EntityRef)
	}
	return strings.Compare(a.Text, b.Text)
}

func (w refWorld) condHolds(p *domain.TagPredicate, entity string, c domain.ObjectCondition) bool {
	v, ok := w.value(p, entity, c.Property)
	if c.Op == domain.ObjectOpIsNull {
		return !ok
	}
	if !ok {
		return false
	}
	switch c.Op {
	case domain.ObjectOpIn:
		for _, x := range c.Values {
			if compareValues(v, x) == 0 {
				return true
			}
		}
		return false
	case domain.ObjectOpPrefix:
		return strings.HasPrefix(v.Text, c.Values[0].Text)
	}
	k := compareValues(v, c.Values[0])
	switch c.Op {
	case domain.ObjectOpEq:
		return k == 0
	case domain.ObjectOpNe:
		return k != 0
	case domain.ObjectOpLt:
		return k < 0
	case domain.ObjectOpLe:
		return k <= 0
	case domain.ObjectOpGt:
		return k > 0
	case domain.ObjectOpGe:
		return k >= 0
	}
	panic(c.Op)
}

func (w refWorld) linkHolds(p *domain.TagPredicate, entity string, lf domain.ObjectLinkFilter) bool {
	for _, l := range w.links {
		if l.from != domain.EntityRef(entity) || l.relation != lf.Path[0] || !w.linkVisible(p, l) {
			continue
		}
		target := strings.TrimPrefix(l.to, domain.RefPrefixEntity)
		if _, ok := w.states[target]; ok && w.kinds[target] == lpCust && w.condHolds(p, target, lf.Where) {
			return true
		}
	}
	return false
}

// matching lists the objects of kind q.Kind the request admits for p.
func (w refWorld) matching(p *domain.TagPredicate, q domain.ObjectsQuery) []string {
	var out []string
	for entity := range w.states {
		if w.kinds[entity] != q.Kind || !w.exists(p, q.Kind, entity) {
			continue
		}
		ok := true
		for _, c := range q.Where {
			ok = ok && w.condHolds(p, entity, c)
		}
		for _, lf := range q.Links {
			ok = ok && w.linkHolds(p, entity, lf)
		}
		if ok {
			out = append(out, entity)
		}
	}
	return out
}

// list is the reference answer to a list request: every row, in order.
func (w refWorld) list(p *domain.TagPredicate, q domain.ObjectsQuery) []domain.ObjectRow {
	ids := w.matching(p, q)
	sort.Slice(ids, func(i, j int) bool {
		for _, o := range q.OrderBy {
			a, aok := w.value(p, ids[i], o.Property)
			b, bok := w.value(p, ids[j], o.Property)
			switch {
			case aok && !bok:
				return true
			case !aok && bok:
				return false
			case aok && bok:
				if k := compareValues(a, b); k != 0 {
					if o.Desc {
						return k > 0
					}
					return k < 0
				}
			}
		}
		return ids[i] < ids[j]
	})
	fields := q.Fields
	if len(fields) == 0 {
		for prop := range lpSpec(q.Kind).Predicates {
			fields = append(fields, prop)
		}
		sort.Strings(fields)
	}
	out := make([]domain.ObjectRow, 0, len(ids))
	for _, id := range ids {
		row := domain.ObjectRow{EntityID: id}
		for _, f := range fields {
			if v, ok := w.value(p, id, f); ok {
				v.Predicate = f
				row.Values = append(row.Values, domain.ObjectValue{Property: f, Value: v})
			}
		}
		out = append(out, row)
	}
	return out
}

// groups is the reference answer to a group request.
func (w refWorld) groups(p *domain.TagPredicate, q domain.ObjectsQuery) []string {
	type bucket struct {
		key  []domain.StatementValue
		keyS string
		ids  []string
	}
	byKey := map[string]*bucket{}
	for _, id := range w.matching(p, q) {
		var key []domain.StatementValue
		var ks []string
		for _, g := range q.GroupBy {
			v, ok := w.value(p, id, g)
			if !ok {
				v = domain.StatementValue{}
			}
			key = append(key, v)
			ks = append(ks, renderValue(v))
		}
		k := strings.Join(ks, "|")
		if byKey[k] == nil {
			byKey[k] = &bucket{key: key, keyS: k}
		}
		byKey[k].ids = append(byKey[k].ids, id)
	}
	var bs []*bucket
	for _, b := range byKey {
		bs = append(bs, b)
	}
	sort.Slice(bs, func(i, j int) bool {
		for x := range bs[i].key {
			a, b := bs[i].key[x], bs[j].key[x]
			switch {
			case a.Type != "" && b.Type == "":
				return true
			case a.Type == "" && b.Type != "":
				return false
			case a.Type != "" && b.Type != "":
				if k := compareValues(a, b); k != 0 {
					return k < 0
				}
			}
		}
		return false
	})
	if len(q.GroupBy) == 0 && len(bs) == 0 {
		bs = []*bucket{{}} // totals over nothing: one row
	}
	var out []string
	for _, b := range bs {
		var ms []string
		for _, m := range q.Metrics {
			if m.Fn == domain.ObjectMetricCount {
				ms = append(ms, renderValue(domain.StatementValue{Type: "number", Number: float64(len(b.ids))}))
				continue
			}
			var vs []domain.StatementValue
			for _, id := range b.ids {
				if v, ok := w.value(p, id, m.Property); ok {
					vs = append(vs, v)
				}
			}
			ms = append(ms, renderValue(aggregate(m.Fn, vs)))
		}
		out = append(out, b.keyS+" => "+strings.Join(ms, ","))
	}
	return out
}

func aggregate(fn string, vs []domain.StatementValue) domain.StatementValue {
	if len(vs) == 0 {
		return domain.StatementValue{}
	}
	switch fn {
	case domain.ObjectMetricSum, domain.ObjectMetricAvg:
		s := 0.0
		for _, v := range vs {
			s += v.Number
		}
		if fn == domain.ObjectMetricAvg {
			s /= float64(len(vs))
		}
		return domain.StatementValue{Type: "number", Number: s}
	}
	best := vs[0]
	for _, v := range vs[1:] {
		k := compareValues(v, best)
		if (fn == domain.ObjectMetricMin && k < 0) || (fn == domain.ObjectMetricMax && k > 0) {
			best = v
		}
	}
	return best
}

func renderValue(v domain.StatementValue) string {
	switch v.Type {
	case "":
		return "∅"
	case "number":
		return fmt.Sprintf("n:%.6f", math.Round(v.Number*1e6)/1e6)
	case "date":
		return "d:" + v.Date.UTC().Format(time.RFC3339)
	case "entity":
		return "e:" + v.EntityRef
	}
	return "t:" + v.Text
}

func renderRows(rows []domain.ObjectRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		var vs []string
		for _, v := range r.Values {
			vs = append(vs, v.Property+"="+renderValue(v.Value))
		}
		out[i] = r.EntityID + "{" + strings.Join(vs, ",") + "}"
	}
	return out
}

func renderGroups(gs []domain.ObjectGroup) []string {
	var out []string
	for _, g := range gs {
		var ks, ms []string
		for _, k := range g.Key {
			ks = append(ks, renderValue(k.Value))
		}
		for _, m := range g.Metrics {
			ms = append(ms, renderValue(m))
		}
		out = append(out, strings.Join(ks, "|")+" => "+strings.Join(ms, ","))
	}
	return out
}

// readWorld computes the reference from ALL of history with independent reads.
func (e *lpEnv) readWorld(t *rapid.T, ev map[string]evidenceRow) refWorld {
	w := refWorld{states: map[string]objects.State{}, kinds: map[string]string{}, minted: map[string]string{},
		evRows: ev, itemTag: map[string][]string{}}
	cands := map[string][]objects.Candidate{}
	rs, err := e.pool.Query(e.ctx, `
		SELECT o.id, o.entity_id, o.predicate, o.value_type, o.value_text, o.value_number, o.value_date, o.value_entity_id,
		       o.occurred_at, COALESCE(e.source_id,''), e.classification, e.parties, COALESCE(o.evidence_id, '')
		FROM observations o LEFT JOIN evidence e ON e.id = o.evidence_id
		WHERE o.namespace_id = $1 AND NOT EXISTS (SELECT 1 FROM typed_row_retirements r
		  WHERE r.namespace_id = o.namespace_id AND r.source_ref = o.source_ref AND r.reinstated_at IS NULL)`, e.ns)
	if err != nil {
		t.Fatal(err)
	}
	for rs.Next() {
		var c objects.Candidate
		var id int64
		var entity string
		var tx, ent *string
		var nm *float64
		var dt *time.Time
		var tags, parties []string
		if err := rs.Scan(&id, &entity, &c.Predicate, &c.Value.Type, &tx, &nm, &dt, &ent, &c.At, &c.Source, &tags, &parties, &c.EvidenceID); err != nil {
			t.Fatal(err)
		}
		switch {
		case tx != nil:
			c.Value.Text = *tx
		case nm != nil:
			c.Value.Number = *nm
		case dt != nil:
			c.Value.Date = dt.UTC()
		case ent != nil:
			c.Value.EntityRef = *ent
		}
		c.Value.Predicate = c.Predicate
		c.At = c.At.UTC()
		c.ID = fmt.Sprintf("o%020d", id)
		c.Tags, c.Parties = tags, parties
		cands[entity] = append(cands[entity], c)
	}
	rs.Close()
	for _, kind := range []string{lpOrder, lpCust} {
		items, err := e.know.CurrentResolutions(e.ctx, e.ns, kind, domain.ResolutionPolicyLatestAssertion)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range items {
			if r.Item == nil {
				continue
			}
			for _, v := range r.Item.Values {
				cands[r.EntityID] = append(cands[r.EntityID], objects.Candidate{Predicate: v.Predicate, Value: v,
					At: r.Item.AssertedAt.UTC(), Source: r.Item.AssertedBy, ID: "i" + string(r.Item.ID),
					Tags: nonNilS(r.Item.Classification), Parties: []string{}, EvidenceID: string(r.Item.EvidenceID)})
			}
		}
	}
	for entity, cs := range cands {
		stem, _ := domain.EntityKindFromID(entity)
		w.kinds[entity] = stem
		w.states[entity] = objects.Current(lpSpec(stem), cs)
	}
	rs, err = e.pool.Query(e.ctx, `SELECT id, COALESCE(first_seen_evidence, '') FROM entities WHERE namespace_id = $1`, e.ns)
	if err != nil {
		t.Fatal(err)
	}
	for rs.Next() {
		var id, evid string
		if err := rs.Scan(&id, &evid); err != nil {
			t.Fatal(err)
		}
		w.minted[id] = evid
	}
	rs.Close()
	rs, err = e.pool.Query(e.ctx, `SELECT from_ref, to_ref, relation, family, COALESCE(evidence_id,''), state = 'confirmed', retracted_at IS NOT NULL
		FROM links WHERE namespace_id = $1`, e.ns)
	if err != nil {
		t.Fatal(err)
	}
	for rs.Next() {
		var l refLink
		if err := rs.Scan(&l.from, &l.to, &l.relation, &l.family, &l.evidence, &l.confirmed, &l.retracted); err != nil {
			t.Fatal(err)
		}
		w.links = append(w.links, l)
	}
	rs.Close()
	return w
}

// ── the generator ────────────────────────────────────────────────────────────

var (
	lpOrders   = []string{"lporder/o1", "lporder/o2", "lporder/o3", "lporder/o4", "lporder/o5", "lporder/o6"}
	lpCusts    = []string{"lpcust/c1", "lpcust/c2", "lpcust/c3"}
	lpStatuses = []string{"open", "picking", "shipped"}
	lpSegments = []string{"clinic", "hospital"}
)

func lpDay(n int) domain.StatementValue {
	return domain.StatementValue{Type: "date", Date: time.Date(2026, 10, 1+n, 0, 0, 0, 0, time.UTC)}
}

func entityV(ref string) domain.StatementValue {
	return domain.StatementValue{Type: "entity", EntityRef: ref}
}

// writeHistory writes a random history through the real stores.
func (e *lpEnv) writeHistory(t *rapid.T, round int, evIDs []string) {
	var refs []string
	evOrNone := append(append([]string{}, evIDs...), "")
	for i, n := 0, rapid.IntRange(4, 40).Draw(t, "ops"); i < n; i++ {
		at := time.Duration(rapid.IntRange(0, 6).Draw(t, "at")) * time.Minute
		ref := fmt.Sprintf("lp%d-%d", round, i)
		switch rapid.IntRange(0, 9).Draw(t, "op") {
		case 0, 1, 2, 3: // an order observation
			o := rapid.SampledFrom(lpOrders).Draw(t, "order")
			var pred string
			var v domain.StatementValue
			switch rapid.IntRange(0, 4).Draw(t, "pred") {
			case 0:
				pred, v = "status", txt(rapid.SampledFrom(lpStatuses).Draw(t, "status"))
			case 1:
				pred, v = "total", num(float64(rapid.IntRange(1, 5).Draw(t, "total")*10))
			case 2:
				pred, v = "due", lpDay(rapid.IntRange(0, 4).Draw(t, "due"))
			case 3:
				pred, v = "customer", entityV(rapid.SampledFrom(lpCusts).Draw(t, "cust"))
			default:
				pred, v = "colour", txt("red") // undeclared, open kind: extra only
			}
			ev := rapid.SampledFrom(evOrNone).Draw(t, "ev")
			if err := e.observe(o, pred, v, at, domain.EvidenceID(ev), ref); err != nil {
				t.Fatal(err)
			}
			refs = append(refs, ref)
		case 4: // a customer observation
			c := rapid.SampledFrom(lpCusts).Draw(t, "c")
			ev := rapid.SampledFrom(evOrNone).Draw(t, "cev")
			if err := e.observe(c, "segment", txt(rapid.SampledFrom(lpSegments).Draw(t, "seg")), at, domain.EvidenceID(ev), ref); err != nil {
				t.Fatal(err)
			}
			refs = append(refs, ref)
		case 5: // a person's item on an order, with its own classification
			s := txt(rapid.SampledFrom(lpStatuses).Draw(t, "is"))
			s.Predicate = "status"
			cls := rapid.SampledFrom([][]string{{"r:south"}, {"r:north"}, {"ops"}, {}}).Draw(t, "cls")
			if _, _, err := e.know.PutItem(e.ctx, domain.KnowledgeItem{NamespaceID: e.ns, Kind: lpOrder,
				EntityID: rapid.SampledFrom(lpOrders).Draw(t, "io"), AssertedBy: "user:alice",
				AssertedAt: base.Add(at), SourceRef: ref, Values: []domain.StatementValue{s}, Classification: cls}); err != nil {
				t.Fatal(err)
			}
		case 6: // a link order -> customer
			from := rapid.SampledFrom(lpOrders).Draw(t, "lfrom")
			to := rapid.SampledFrom(lpCusts).Draw(t, "lto")
			state := rapid.SampledFrom([]string{domain.LinkStateConfirmed, domain.LinkStateConfirmed, domain.LinkStateCandidate}).Draw(t, "lstate")
			mech := domain.LinkMechanismRecord
			if state == domain.LinkStateCandidate {
				mech = domain.LinkMechanismDerived
			}
			if _, err := e.links.PutLink(e.ctx, domain.Link{NamespaceID: e.ns, FromRef: domain.EntityRef(from), ToRef: domain.EntityRef(to),
				Relation: lpVerb, State: state, Mechanism: mech, SourceRef: ref,
				EvidenceID: domain.EvidenceID(rapid.SampledFrom(evIDs).Draw(t, "lev")), AssertedBy: "test", AssertedAt: base.Add(at)}); err != nil {
				t.Fatal(err)
			}
		case 7: // mint an entity handle with (or without) first-seen evidence
			id := rapid.SampledFrom(append(append([]string{}, lpOrders...), lpCusts...)).Draw(t, "mint")
			ev := rapid.SampledFrom(evOrNone).Draw(t, "mev")
			if _, err := e.entities.EnsureEntity(e.ctx, domain.Entity{ID: id, NamespaceID: e.ns, FirstSeenEvidence: domain.EvidenceID(ev)}); err != nil {
				t.Fatal(err)
			}
		case 8: // retire an earlier observation
			if len(refs) > 0 {
				if _, err := e.events.RetireRows(e.ctx, e.ns, []string{rapid.SampledFrom(refs).Draw(t, "retire")}, domain.RetireSuperseded, "t"); err != nil {
					t.Fatal(err)
				}
			}
		case 9: // retract a link
			if _, err := e.pool.Exec(e.ctx, `UPDATE links SET retracted_at = now() WHERE id = (SELECT id FROM links WHERE namespace_id = $1 AND retracted_at IS NULL ORDER BY id LIMIT 1)`, e.ns); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func drawCond(t *rapid.T, label string) domain.ObjectCondition {
	switch rapid.IntRange(0, 9).Draw(t, label+":kind") {
	case 0:
		return domain.ObjectCondition{Property: "status", Op: domain.ObjectOpEq, Values: []domain.StatementValue{txt(rapid.SampledFrom(lpStatuses).Draw(t, label+":s"))}}
	case 1:
		return domain.ObjectCondition{Property: "status", Op: domain.ObjectOpIn, Values: []domain.StatementValue{txt("open"), txt("picking")}}
	case 2:
		return domain.ObjectCondition{Property: "status", Op: domain.ObjectOpNe, Values: []domain.StatementValue{txt("shipped")}}
	case 3:
		return domain.ObjectCondition{Property: "status", Op: domain.ObjectOpPrefix, Values: []domain.StatementValue{txt("p")}}
	case 4:
		return domain.ObjectCondition{Property: rapid.SampledFrom([]string{"status", "total", "due", "customer"}).Draw(t, label+":np"), Op: domain.ObjectOpIsNull}
	case 5:
		op := rapid.SampledFrom([]string{domain.ObjectOpGt, domain.ObjectOpGe, domain.ObjectOpLt, domain.ObjectOpLe}).Draw(t, label+":op")
		return domain.ObjectCondition{Property: "total", Op: op, Values: []domain.StatementValue{num(float64(rapid.IntRange(1, 5).Draw(t, label+":n") * 10))}}
	case 6:
		return domain.ObjectCondition{Property: "total", Op: domain.ObjectOpIn, Values: []domain.StatementValue{num(20), num(40)}}
	case 7:
		return domain.ObjectCondition{Property: "due", Op: domain.ObjectOpLt, Values: []domain.StatementValue{lpDay(rapid.IntRange(0, 4).Draw(t, label+":d"))}}
	case 8:
		return domain.ObjectCondition{Property: "customer", Op: domain.ObjectOpEq, Values: []domain.StatementValue{entityV(rapid.SampledFrom(lpCusts).Draw(t, label+":c"))}}
	}
	return domain.ObjectCondition{Property: "total", Op: domain.ObjectOpNe, Values: []domain.StatementValue{num(30)}}
}

func drawQuery(t *rapid.T) domain.ObjectsQuery {
	q := domain.ObjectsQuery{Kind: lpOrder}
	for i, n := 0, rapid.IntRange(0, 2).Draw(t, "nwhere"); i < n; i++ {
		q.Where = append(q.Where, drawCond(t, fmt.Sprintf("w%d", i)))
	}
	if rapid.IntRange(0, 3).Draw(t, "link") == 0 {
		seg := domain.ObjectCondition{Property: "segment", Op: domain.ObjectOpEq, Values: []domain.StatementValue{txt(rapid.SampledFrom(lpSegments).Draw(t, "lseg"))}}
		if rapid.Bool().Draw(t, "lnull") {
			seg = domain.ObjectCondition{Property: "segment", Op: domain.ObjectOpIsNull}
		}
		q.Links = []domain.ObjectLinkFilter{{Path: []string{lpVerb}, Where: seg}}
	}
	if rapid.IntRange(0, 3).Draw(t, "group") == 0 {
		if rapid.Bool().Draw(t, "hasgroup") {
			q.GroupBy = []string{rapid.SampledFrom([]string{"status", "customer", "due"}).Draw(t, "gb")}
		}
		q.Metrics = []domain.ObjectMetric{{Fn: domain.ObjectMetricCount}, {Fn: domain.ObjectMetricSum, Property: "total"},
			{Fn: domain.ObjectMetricMin, Property: "due"}, {Fn: domain.ObjectMetricMax, Property: "total"}}
		return q
	}
	props := []string{"status", "total", "due", "customer"}
	used := map[string]bool{}
	for i, n := 0, rapid.IntRange(0, 2).Draw(t, "norder"); i < n; i++ {
		p := rapid.SampledFrom(props).Draw(t, fmt.Sprintf("o%d", i))
		if used[p] {
			continue
		}
		used[p] = true
		q.OrderBy = append(q.OrderBy, domain.ObjectOrder{Property: p, Desc: rapid.Bool().Draw(t, fmt.Sprintf("od%d", i))})
	}
	if rapid.Bool().Draw(t, "mask") {
		q.Fields = []string{rapid.SampledFrom(props).Draw(t, "field")}
	}
	q.PageSize = rapid.IntRange(1, 4).Draw(t, "page")
	return q
}

// listAll pages through q to the end, refusing a page that repeats an id.
func listAll(t *rapid.T, r *objects.Reader, ctx context.Context, p *domain.TagPredicate, q domain.ObjectsQuery) []domain.ObjectRow {
	var out []domain.ObjectRow
	seen := map[string]bool{}
	for page := 0; page < 100; page++ {
		res, err := r.List(ctx, p, q)
		if err != nil {
			t.Fatalf("list %+v: %v", q, err)
		}
		if len(res.Rows) > q.PageSize {
			t.Fatalf("page of %d rows for page_size %d", len(res.Rows), q.PageSize)
		}
		for _, row := range res.Rows {
			if seen[row.EntityID] {
				t.Fatalf("keyset paging returned %s twice", row.EntityID)
			}
			seen[row.EntityID] = true
		}
		out = append(out, res.Rows...)
		if res.NextPageToken == "" {
			return out
		}
		q.PageToken = res.NextPageToken
	}
	t.Fatalf("no end after 100 pages")
	return nil
}

// The D9 differential: for random histories, requests and callers, every page
// sequence and every group answer equals the brute-force reference.
func TestPg_ListObjectsEqualsBruteForceFromHistory(t *testing.T) {
	e := newLPEnv(t, "lptest-diff")
	round := 0
	rapid.Check(t, func(rt *rapid.T) {
		round++
		for _, q := range []string{
			`DELETE FROM links WHERE namespace_id = $1`, `DELETE FROM entities WHERE namespace_id = $1`,
			`DELETE FROM typed_row_retirements WHERE namespace_id = $1`, `DELETE FROM observations WHERE namespace_id = $1`,
			`DELETE FROM resolutions WHERE namespace_id = $1`, `DELETE FROM knowledge_items WHERE namespace_id = $1`,
			`DELETE FROM objects_lporder WHERE namespace_id = $1`, `DELETE FROM objects_lpcust WHERE namespace_id = $1`,
		} {
			if _, err := e.pool.Exec(e.ctx, q, e.ns); err != nil {
				rt.Fatal(err)
			}
		}
		ev := e.seedEvidence("lpdiff-")
		var evIDs []string
		for id := range ev {
			evIDs = append(evIDs, id)
		}
		sort.Strings(evIDs)
		e.writeHistory(rt, round, evIDs)
		w := e.readWorld(rt, ev)
		for qi := 0; qi < 3; qi++ {
			q := drawQuery(rt)
			q.Namespace = e.ns
			for _, p := range lpPredicates {
				if q.IsGroup() {
					res, err := e.reader.List(e.ctx, p, q)
					if err != nil {
						rt.Fatalf("group %+v: %v", q, err)
					}
					got, want := renderGroups(res.Groups), w.groups(p, q)
					if strings.Join(got, "\n") != strings.Join(want, "\n") {
						rt.Fatalf("groups differ for %+v under %+v\nlist: %v\nref:  %v", q, *p, got, want)
					}
					continue
				}
				got, want := renderRows(listAll(rt, e.reader, e.ctx, p, q)), renderRows(w.list(p, q))
				if strings.Join(got, "\n") != strings.Join(want, "\n") {
					rt.Fatalf("list differs for %+v under %+v\nlist: %v\nref:  %v", q, *p, got, want)
				}
			}
		}
		// GetObject: found exactly when the caller may know the object exists,
		// and its properties are exactly the visible ones.
		for _, entity := range append(append([]string{}, lpOrders...), lpCusts...) {
			kind, _ := domain.EntityKindFromID(entity)
			for _, p := range lpPredicates {
				view, err := e.reader.Get(e.ctx, p, e.ns, entity)
				_, hasRow := w.states[entity]
				wantFound := hasRow && (w.exists(p, kind, entity) || w.anyLinkTouching(p, entity))
				if errors.Is(err, domain.ErrObjectNotFound) {
					if wantFound {
						rt.Fatalf("get %s under %+v: not found, want found", entity, *p)
					}
					continue
				}
				if err != nil {
					rt.Fatalf("get %s: %v", entity, err)
				}
				if !wantFound {
					rt.Fatalf("get %s under %+v: found %+v, want not found", entity, *p, view)
				}
				var got, want []string
				for _, pr := range view.Properties {
					got = append(got, pr.Property+"="+renderValue(pr.Value))
				}
				props := make([]string, 0)
				for prop := range lpSpec(kind).Predicates {
					props = append(props, prop)
				}
				sort.Strings(props)
				for _, prop := range props {
					if v, ok := w.value(p, entity, prop); ok {
						want = append(want, prop+"="+renderValue(v))
					}
				}
				if strings.Join(got, ",") != strings.Join(want, ",") {
					rt.Fatalf("get %s under %+v: properties %v, want %v", entity, *p, got, want)
				}
			}
		}
	})
}

func (w refWorld) anyLinkTouching(p *domain.TagPredicate, entity string) bool {
	ref := domain.EntityRef(entity)
	for _, l := range w.links {
		if (l.from == ref || l.to == ref) && w.linkVisible(p, l) {
			return true
		}
	}
	return false
}
