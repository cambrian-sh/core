package objects_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"pgregory.net/rapid"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/objects"
	"github.com/cambrian-sh/core/internal/ontology"
)

// Integration tests over real Postgres (PG_TEST_DSN, migrations through 0042).
// Every write goes through the REAL stores (PgEventStore, PgKnowledgeStore) and
// the REAL live ontology (ontology.Load / Apply), so each test exercises the
// production path from a history write to the object row.

var disposableDB = regexp.MustCompile(`(?i)test`)

func pgPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !disposableDB.MatchString(cfg.ConnConfig.Database) {
		t.Skipf("refusing to touch %q: its name does not say test", cfg.ConnConfig.Database)
	}
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// The declarations table is global; each test starts from its own seed
	// (as internal/ontology's Pg tests do — a test database only).
	if _, err := pool.Exec(ctx, `TRUNCATE ontology_declarations`); err != nil {
		t.Fatal(err)
	}
	return pool, ctx
}

// env is one test's world: a namespace, the kinds whose tables it owns, the
// live ontology and the real stores built on it.
type env struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	ns     string
	onto   *ontology.Ontology
	events *postgres.PgEventStore
	know   *postgres.PgKnowledgeStore
}

func newEnv(t *testing.T, ns string, tables []string, seed ...domain.KindSpec) *env {
	t.Helper()
	pool, ctx := pgPool(t)
	clean := func() {
		bg := context.Background()
		for _, q := range []string{
			`DELETE FROM typed_row_retirements WHERE namespace_id = $1`,
			`DELETE FROM observations WHERE namespace_id = $1`,
			`DELETE FROM resolutions WHERE namespace_id = $1`,
			`DELETE FROM knowledge_items WHERE namespace_id = $1`,
			`DELETE FROM evidence WHERE namespace_id = $1`,
		} {
			_, _ = pool.Exec(bg, q, ns)
		}
		for _, k := range tables {
			_, _ = pool.Exec(bg, `DROP TABLE IF EXISTS `+objects.TableName(k))
			_, _ = pool.Exec(bg, `DELETE FROM object_table_builds WHERE table_name = $1`, objects.TableName(k))
		}
	}
	clean()
	t.Cleanup(clean)
	e := &env{t: t, ctx: ctx, pool: pool, ns: ns}
	e.load(seed...)
	return e
}

// load (re)boots the live ontology on seed, exactly as the kernel does.
func (e *env) load(seed ...domain.KindSpec) {
	e.t.Helper()
	o, err := ontology.Load(e.ctx, e.pool, domain.OntologyDoc{Kinds: seed}, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.onto = o
	e.events = postgres.NewPgEventStore(e.pool, o)
	e.know = postgres.NewPgKnowledgeStore(e.pool, o)
}

func (e *env) evidence(id, source string, tags ...string) domain.EvidenceID {
	e.t.Helper()
	if tags == nil {
		tags = []string{}
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO evidence (id, namespace_id, source_id, source_key, content_hash, classification)
		VALUES ($1,$2,$3,$1,'sha256:test',$4) ON CONFLICT DO NOTHING`, id, e.ns, source, tags); err != nil {
		e.t.Fatal(err)
	}
	return domain.EvidenceID(id)
}

var base = time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)

func (e *env) observe(entity, pred string, v domain.StatementValue, at time.Duration, ev domain.EvidenceID, ref string) error {
	v.Predicate = pred
	_, err := e.events.RecordObservation(e.ctx, domain.Observation{NamespaceID: e.ns, EntityID: entity, Predicate: pred,
		Value: v, OccurredAt: base.Add(at), EvidenceID: ev, SourceRef: ref})
	return err
}

func (e *env) mustObserve(entity, pred string, v domain.StatementValue, at time.Duration, ev domain.EvidenceID, ref string) {
	e.t.Helper()
	if err := e.observe(entity, pred, v, at, ev, ref); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) assert(kind, entity, by string, at time.Duration, ref string, vals ...domain.StatementValue) error {
	_, _, err := e.know.PutItem(e.ctx, domain.KnowledgeItem{NamespaceID: e.ns, Kind: kind, EntityID: entity,
		AssertedBy: by, AssertedAt: base.Add(at), SourceRef: ref, Values: vals, Classification: []string{"ops"}})
	return err
}

// row reads one object row as JSON, without updated_at (the only column a
// rebuild legitimately changes). nil when there is no row.
func (e *env) row(kind, entity string) map[string]any {
	e.t.Helper()
	var raw []byte
	err := e.pool.QueryRow(e.ctx, `SELECT (to_jsonb(t) - 'updated_at')::text FROM `+objects.TableName(kind)+
		` t WHERE namespace_id = $1 AND entity_id = $2`, e.ns, entity).Scan(&raw)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return nil
		}
		e.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *env) rows(kind string) map[string]map[string]any {
	e.t.Helper()
	rs, err := e.pool.Query(e.ctx, `SELECT entity_id FROM `+objects.TableName(kind)+` WHERE namespace_id = $1`, e.ns)
	if err != nil {
		e.t.Fatal(err)
	}
	var ids []string
	for rs.Next() {
		var id string
		_ = rs.Scan(&id)
		ids = append(ids, id)
	}
	rs.Close()
	out := map[string]map[string]any{}
	for _, id := range ids {
		out[id] = e.row(kind, id)
	}
	return out
}

func txt(s string) domain.StatementValue { return domain.StatementValue{Type: "text", Text: s} }
func num(n float64) domain.StatementValue {
	return domain.StatementValue{Type: "number", Number: n}
}

func prov(m map[string]any, pred string) map[string]any {
	p, _ := m["prov"].(map[string]any)
	v, _ := p[pred].(map[string]any)
	return v
}

func p3order() domain.KindSpec {
	return domain.KindSpec{Kind: "p3order", Table: true, Open: true, Predicates: map[string]domain.ValueSpec{
		"status": {Type: "text"},
		"total":  {Type: "number", Indexed: true},
	}}
}

// The whole write path, end to end: observations and knowledge items about an
// object reach its row in the same transaction; retirement, reinstatement and
// erasure move it; open kinds keep undeclared values in extra; strict kinds and
// the object vocabulary refuse what they must.
func TestPg_E2E_HistoryWritesMaintainTheObjectRow(t *testing.T) {
	note := domain.KindSpec{Kind: "p3note", Predicates: map[string]domain.ValueSpec{"status": {Type: "text"}, "body": {Type: "text"}}}
	strict := domain.KindSpec{Kind: "p3cust", Table: true, Predicates: map[string]domain.ValueSpec{"segment": {Type: "text"}}}
	e := newEnv(t, "p3test-e2e", []string{"p3order", "p3cust"}, p3order(), note, strict)
	erp := e.evidence("p3e2e-ev-erp", "erp", "ops")
	erp2 := e.evidence("p3e2e-ev-erp2", "erp", "finance")

	e.mustObserve("p3order/O-1", "status", txt("open"), 0, erp, "erp:1")
	if r := e.row("p3order", "p3order/O-1"); r == nil || r["status__text"] != "open" ||
		!reflect.DeepEqual(r["status__tags"], []any{"ops"}) || prov(r, "status")["src"] != "erp" {
		t.Fatalf("first observation: %v", r)
	}
	e.mustObserve("p3order/O-1", "status", txt("shipped"), 2*time.Hour, erp2, "erp:2")
	e.mustObserve("p3order/O-1", "status", txt("picking"), time.Hour, erp, "erp:3") // late arrival, earlier event
	r := e.row("p3order", "p3order/O-1")
	if r["status__text"] != "shipped" || !reflect.DeepEqual(r["status__tags"], []any{"finance"}) {
		t.Fatalf("latest by EVENT time, with that value's own visibility: %v", r)
	}
	if r["decl_version"] == nil {
		t.Fatalf("the row records the declarations version: %v", r)
	}

	// An item (a person's word) about the object: a newer status from another source.
	st := txt("cancelled")
	st.Predicate = "status"
	tot := num(120)
	tot.Predicate = "total"
	if err := e.assert("p3order", "p3order/O-1", "user:alice", 3*time.Hour, "alice:1", st, tot); err != nil {
		t.Fatal(err)
	}
	r = e.row("p3order", "p3order/O-1")
	if r["status__text"] != "cancelled" || r["total__number"] != 120.0 || prov(r, "status")["conflict"] != true ||
		prov(r, "status")["src"] != "user:alice" || !reflect.DeepEqual(r["total__parties"], []any{}) {
		t.Fatalf("items and observations share one property model (D4): %v", r)
	}

	// An item of ANOTHER kind about an object speaks the object kind's
	// vocabulary: the strict customer kind refuses a predicate it does not declare.
	body := txt("hello")
	body.Predicate = "body"
	if err := e.assert("p3note", "p3cust/C-1", "user:bob", time.Hour, "bob:1", body); !errors.Is(err, domain.ErrKindRefused) {
		t.Fatalf("an item about a strict object with an undeclared property must be refused, got %v", err)
	}
	seg := txt("hospital")
	seg.Predicate = "segment"
	if err := e.assert("p3cust", "p3cust/C-1", "user:bob", time.Hour, "bob:2", seg); err != nil {
		t.Fatal(err)
	}
	if r := e.row("p3cust", "p3cust/C-1"); r["segment__text"] != "hospital" {
		t.Fatalf("customer row: %v", r)
	}

	// Open kind: an undeclared observation lands in extra.
	e.mustObserve("p3order/O-1", "colour", txt("red"), 0, erp, "erp:4")
	extra, _ := e.row("p3order", "p3order/O-1")["extra"].(map[string]any)
	if c, _ := extra["colour"].(map[string]any); c["v"] != "red" || c["t"] != "text" {
		t.Fatalf("open kind keeps an undeclared predicate in extra: %v", extra)
	}
	// Strict kind: refused, nothing written.
	if err := e.observe("p3cust/C-2", "colour", txt("red"), 0, erp, "erp:5"); !errors.Is(err, domain.ErrKindRefused) {
		t.Fatalf("strict kind refuses an undeclared predicate: %v", err)
	}
	if e.row("p3cust", "p3cust/C-2") != nil {
		t.Fatal("a refused write leaves no row")
	}

	// Retire the shipped observation: the item still wins; retire... then erase the item.
	if _, err := e.events.RetireRows(e.ctx, e.ns, []string{"erp:2"}, domain.RetireSuperseded, "test"); err != nil {
		t.Fatal(err)
	}
	if n, err := e.know.EraseItems(e.ctx, e.ns, "p3order", domain.KnowledgeErasure{Entities: []string{"p3order/O-1"}}); err != nil || n != 1 {
		t.Fatalf("erase: %d %v", n, err)
	}
	r = e.row("p3order", "p3order/O-1")
	if r["status__text"] != "picking" || r["total__number"] != nil || prov(r, "status")["conflict"] != false {
		t.Fatalf("after retiring 'shipped' and erasing the item, the next value of history is current: %v", r)
	}
	if _, err := e.events.ReinstateRows(e.ctx, e.ns, []string{"erp:2"}); err != nil {
		t.Fatal(err)
	}
	if r = e.row("p3order", "p3order/O-1"); r["status__text"] != "shipped" {
		t.Fatalf("reinstated: %v", r)
	}
	// Retire everything: no history left, no row.
	if _, err := e.events.RetireRows(e.ctx, e.ns, []string{"erp:1", "erp:2", "erp:3", "erp:4"}, domain.RetireSourceDeleted, "test"); err != nil {
		t.Fatal(err)
	}
	if r = e.row("p3order", "p3order/O-1"); r != nil {
		t.Fatalf("an object with no counted history has no row: %v", r)
	}

	// Undeclared stem: the reserved observation path, no table involved.
	if err := e.observe("sensor/S-1", "anything", txt("x"), 0, erp, "erp:6"); err != nil {
		t.Fatalf("undeclared stems keep the observation path: %v", err)
	}
}

// A live change that turns a table on creates the table and fills it from the
// history written before; a later additive property moves values out of extra
// into their new column; a value whose type mismatches stays out and is
// reported by the violations report.
func TestPg_ApplyCreatesAndRebuildsFromHistory(t *testing.T) {
	plain := domain.KindSpec{Kind: "p3ship", Open: true, Predicates: map[string]domain.ValueSpec{"carrier": {Type: "text"}}}
	e := newEnv(t, "p3test-apply", []string{"p3ship"}, plain)
	ev := e.evidence("p3apply-ev", "wms", "ops")
	e.mustObserve("p3ship/S-1", "carrier", txt("dhl"), 0, ev, "wms:1")
	e.mustObserve("p3ship/S-1", "weight", txt("heavy"), time.Hour, ev, "wms:2") // undeclared, open: kept
	e.mustObserve("p3ship/S-2", "carrier", txt("ups"), 0, ev, "wms:3")

	withTable := plain
	withTable.Table = true
	res, err := e.onto.Apply(e.ctx, e.ns, domain.OntologyDoc{Kinds: []domain.KindSpec{withTable}}, "user:ops", false)
	if err != nil || !res.Applied || res.Class != domain.ChangeAdditive {
		t.Fatalf("turning a table on is additive and applies live: %+v %v", res, err)
	}
	rows := e.rows("p3ship")
	if len(rows) != 2 || rows["p3ship/S-1"]["carrier__text"] != "dhl" || rows["p3ship/S-2"]["carrier__text"] != "ups" {
		t.Fatalf("the new table is rebuilt from history: %v", rows)
	}
	if x, _ := rows["p3ship/S-1"]["extra"].(map[string]any); x["weight"] == nil {
		t.Fatalf("open kind: weight in extra: %v", rows["p3ship/S-1"])
	}

	// Declare weight as a NUMBER: the existing text value mismatches.
	withWeight := withTable
	withWeight.Predicates = map[string]domain.ValueSpec{"carrier": {Type: "text"}, "weight": {Type: "number"}}
	if res, err = e.onto.Apply(e.ctx, e.ns, domain.OntologyDoc{Kinds: []domain.KindSpec{withWeight}}, "user:ops", false); err != nil || !res.Applied {
		t.Fatalf("new optional property: %+v %v", res, err)
	}
	r := e.row("p3ship", "p3ship/S-1")
	if _, has := r["weight__number"]; !has {
		t.Fatalf("ADD COLUMN for the new property: %v", r)
	}
	if r["weight__number"] != nil {
		t.Fatalf("a mismatched value is not projected: %v", r)
	}
	if x, _ := r["extra"].(map[string]any); x["weight"] != nil {
		t.Fatalf("a now-declared property no longer sits in extra: %v", r)
	}
	v, _, err := e.onto.Violations(e.ctx, &domain.TagPredicate{Bypass: true}, e.ns, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range v {
		if x.Rule == domain.ViolationType && x.Ref == "p3ship/S-1" && x.Subject == "kind p3ship.weight" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the mismatch is in the violations report: %+v", v)
	}
	// New writes of the wrong type are refused; the right type is projected.
	if err := e.observe("p3ship/S-1", "weight", txt("light"), 2*time.Hour, ev, "wms:4"); !errors.Is(err, domain.ErrKindRefused) {
		t.Fatalf("a mismatched NEW write is refused: %v", err)
	}
	e.mustObserve("p3ship/S-1", "weight", num(12.5), 3*time.Hour, ev, "wms:5")
	if r = e.row("p3ship", "p3ship/S-1"); r["weight__number"] != 12.5 {
		t.Fatalf("typed weight: %v", r)
	}

	// The tables survive a reboot unchanged (nothing stale, nothing rebuilt).
	var builtBefore time.Time
	if err := e.pool.QueryRow(e.ctx, `SELECT built_at FROM object_table_builds WHERE table_name = 'objects_p3ship'`).Scan(&builtBefore); err != nil {
		t.Fatal(err)
	}
	e.load(plain)
	var builtAfter time.Time
	_ = e.pool.QueryRow(e.ctx, `SELECT built_at FROM object_table_builds WHERE table_name = 'objects_p3ship'`).Scan(&builtAfter)
	if !builtAfter.Equal(builtBefore) {
		t.Fatal("a reboot with unchanged declarations must not rebuild")
	}
}

func TestPg_ApplyRefusesTwoKindsWithOnePrefixAndAuthorityChanges(t *testing.T) {
	e := newEnv(t, "p3test-refuse", []string{"p3order"}, p3order())
	dup := domain.KindSpec{Kind: "p3order_alt", IDPrefix: "p3order", Predicates: map[string]domain.ValueSpec{"x": {Type: "text"}}}
	if _, err := e.onto.Apply(e.ctx, e.ns, domain.OntologyDoc{Kinds: []domain.KindSpec{dup}}, "user:ops", false); !errors.Is(err, domain.ErrOntologyChangeRefused) ||
		!strings.Contains(err.Error(), "id prefix") {
		t.Fatalf("a second kind with the same prefix in one namespace is refused: %v", err)
	}
	// Another namespace may reuse a prefix name for a different kind (cross-namespace duplicates are fine).
	other := domain.KindSpec{Kind: "p3order_alt", IDPrefix: "p3alt", Predicates: map[string]domain.ValueSpec{"x": {Type: "text"}}}
	if _, err := e.onto.Apply(e.ctx, "p3test-refuse-2", domain.OntologyDoc{Kinds: []domain.KindSpec{other}}, "user:ops", true); err != nil {
		t.Fatalf("dry run in another namespace: %v", err)
	}
	auth := p3order()
	auth.Predicates["status"] = domain.ValueSpec{Type: "text", Authority: []string{"erp"}}
	res, err := e.onto.Apply(e.ctx, e.ns, domain.OntologyDoc{Kinds: []domain.KindSpec{auth}}, "user:ops", false)
	if !errors.Is(err, domain.ErrOntologyChangeRefused) || res.Class != domain.ChangeBreaking {
		t.Fatalf("an authority change is breaking and refused: %+v %v", res, err)
	}
}

// Per-property authority through the real stores: ERP wins for status over a
// newer person's word, and the person's value is kept and flagged.
func TestPg_PerPropertyAuthority(t *testing.T) {
	k := p3order()
	k.Predicates["status"] = domain.ValueSpec{Type: "text", Authority: []string{"erp"}}
	e := newEnv(t, "p3test-auth", []string{"p3order"}, k)
	erp := e.evidence("p3auth-ev", "erp", "ops")
	e.mustObserve("p3order/O-9", "status", txt("picking"), 0, erp, "erp:1")
	s := txt("shipped")
	s.Predicate = "status"
	if err := e.assert("p3order", "p3order/O-9", "user:alice", time.Hour, "a:1", s); err != nil {
		t.Fatal(err)
	}
	r := e.row("p3order", "p3order/O-9")
	if r["status__text"] != "picking" || prov(r, "status")["conflict"] != true || prov(r, "status")["src"] != "erp" {
		t.Fatalf("ERP wins for status, the disagreement is flagged: %v", r)
	}
}

// --- rebuild == incremental, and the table == a brute-force computation from history ---

// bruteForce computes every object's expected state from ALL of history with
// independent reads (no DISTINCT ON, no shared SQL with the maintainer) and the
// one current-value function.
func bruteForce(t *rapid.T, e *env, spec domain.KindSpec) map[string]objects.State {
	t.Helper()
	cands := map[string][]objects.Candidate{}
	rs, err := e.pool.Query(e.ctx, `
		SELECT o.id, o.entity_id, o.predicate, o.value_type, o.value_text, o.value_number, o.occurred_at, COALESCE(e.source_id,'')
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
		var tx *string
		var nm *float64
		if err := rs.Scan(&id, &entity, &c.Predicate, &c.Value.Type, &tx, &nm, &c.At, &c.Source); err != nil {
			t.Fatal(err)
		}
		if tx != nil {
			c.Value.Text = *tx
		}
		if nm != nil {
			c.Value.Number = *nm
		}
		c.ID = fmt.Sprintf("o%020d", id)
		cands[entity] = append(cands[entity], c)
	}
	rs.Close()
	items, err := e.know.CurrentResolutions(e.ctx, e.ns, spec.Kind, domain.ResolutionPolicyLatestAssertion)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range items {
		for _, v := range r.Item.Values {
			cands[r.EntityID] = append(cands[r.EntityID], objects.Candidate{Predicate: v.Predicate, Value: v,
				At: r.Item.AssertedAt, Source: r.Item.AssertedBy, ID: "i" + string(r.Item.ID)})
		}
	}
	out := map[string]objects.State{}
	for entity, cs := range cands {
		out[entity] = objects.Current(spec, cs)
	}
	return out
}

func valueOf(v domain.StatementValue) any {
	if v.Type == "number" {
		return v.Number
	}
	return v.Text
}

func TestPg_RebuildEqualsIncrementalAndTableEqualsHistory(t *testing.T) {
	spec := p3order()
	spec.Predicates["status"] = domain.ValueSpec{Type: "text", Authority: []string{"erp"}}
	e := newEnv(t, "p3test-diff", []string{"p3order"}, spec)
	e.evidence("p3diff-erp", "erp", "ops")
	e.evidence("p3diff-crm", "crm", "sales")
	e.evidence("p3diff-none", "", "ops")
	entities := []string{"p3order/A", "p3order/B", "p3order/C"}
	round := 0
	rapid.Check(t, func(rt *rapid.T) {
		round++
		// A clean slate per round, then a random history through the real stores.
		for _, q := range []string{`DELETE FROM typed_row_retirements WHERE namespace_id = $1`, `DELETE FROM observations WHERE namespace_id = $1`,
			`DELETE FROM resolutions WHERE namespace_id = $1`, `DELETE FROM knowledge_items WHERE namespace_id = $1`} {
			if _, err := e.pool.Exec(e.ctx, q, e.ns); err != nil {
				rt.Fatal(err)
			}
		}
		if _, err := e.pool.Exec(e.ctx, `DELETE FROM objects_p3order WHERE namespace_id = $1`, e.ns); err != nil {
			rt.Fatal(err)
		}
		var refs []string
		n := rapid.IntRange(1, 30).Draw(rt, "ops")
		for i := 0; i < n; i++ {
			entity := rapid.SampledFrom(entities).Draw(rt, "entity")
			at := time.Duration(rapid.IntRange(0, 5).Draw(rt, "at")) * time.Minute
			ref := fmt.Sprintf("r%d-%d", round, i)
			switch rapid.IntRange(0, 4).Draw(rt, "op") {
			case 0, 1: // observation
				pred := rapid.SampledFrom([]string{"status", "total", "colour"}).Draw(rt, "pred")
				var v domain.StatementValue
				if pred == "total" {
					v = num(float64(rapid.IntRange(1, 3).Draw(rt, "n")))
				} else {
					v = txt(rapid.SampledFrom([]string{"open", "shipped"}).Draw(rt, "s"))
				}
				ev := domain.EvidenceID(rapid.SampledFrom([]string{"p3diff-erp", "p3diff-crm", "p3diff-none"}).Draw(rt, "ev"))
				if err := e.observe(entity, pred, v, at, ev, ref); err != nil {
					rt.Fatal(err)
				}
				refs = append(refs, ref)
			case 2: // a person's or agent's item
				s := txt(rapid.SampledFrom([]string{"open", "cancelled"}).Draw(rt, "is"))
				s.Predicate = "status"
				who := rapid.SampledFrom([]string{"user:alice", "agent:x"}).Draw(rt, "who")
				if err := e.assert("p3order", entity, who, at, ref, s); err != nil {
					rt.Fatal(err)
				}
			case 3: // retire a random earlier observation
				if len(refs) > 0 {
					r := rapid.SampledFrom(refs).Draw(rt, "retire")
					if _, err := e.events.RetireRows(e.ctx, e.ns, []string{r}, domain.RetireSuperseded, "t"); err != nil {
						rt.Fatal(err)
					}
				}
			case 4: // reinstate one
				if len(refs) > 0 {
					r := rapid.SampledFrom(refs).Draw(rt, "reinstate")
					if _, err := e.events.ReinstateRows(e.ctx, e.ns, []string{r}); err != nil {
						rt.Fatal(err)
					}
				}
			}
		}
		incremental := e.rows("p3order")

		// Differential: the table equals the brute-force computation from history.
		want := bruteForce(rt, e, spec)
		if len(want) != len(incremental) {
			rt.Fatalf("objects: table %d rows, history %d", len(incremental), len(want))
		}
		for entity, st := range want {
			got := incremental[entity]
			if got == nil {
				rt.Fatalf("%s: no row, history has %+v", entity, st)
			}
			for pred, vs := range spec.Predicates {
				col := objects.ValueColumn(pred, vs.Type)
				p, ok := st.Props[pred]
				if !ok {
					if got[col] != nil {
						rt.Fatalf("%s.%s: table %v, history none", entity, pred, got[col])
					}
					continue
				}
				if got[col] != valueOf(p.Value) || prov(got, pred)["id"] != p.ID || prov(got, pred)["conflict"] != p.Conflict {
					rt.Fatalf("%s.%s: table %v %v, history %+v", entity, pred, got[col], prov(got, pred), p)
				}
			}
			extra, _ := got["extra"].(map[string]any)
			var gotX, wantX []string
			for k, v := range extra {
				gotX = append(gotX, k+"="+fmt.Sprint(v.(map[string]any)["id"]))
			}
			for k, v := range st.Extra {
				wantX = append(wantX, k+"="+v.ID)
			}
			sort.Strings(gotX)
			sort.Strings(wantX)
			if !reflect.DeepEqual(gotX, wantX) {
				rt.Fatalf("%s extra: table %v, history %v", entity, gotX, wantX)
			}
		}

		// Rebuild from scratch equals the incremental result, and is idempotent.
		if _, err := e.pool.Exec(e.ctx, `DELETE FROM objects_p3order WHERE namespace_id = $1`, e.ns); err != nil {
			rt.Fatal(err)
		}
		tbl := objects.PlanTables([]*domain.OntologySnapshot{e.onto.Current(e.ns)})[0]
		for i := 0; i < 2; i++ {
			if _, err := objects.Rebuild(e.ctx, e.pool, e.onto, tbl); err != nil {
				rt.Fatal(err)
			}
			if rebuilt := e.rows("p3order"); !reflect.DeepEqual(rebuilt, incremental) {
				rt.Fatalf("rebuild #%d != incremental:\n%v\n%v", i+1, rebuilt, incremental)
			}
		}
	})
}
