package objects

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/cambrian-sh/core/domain"
)

func snap(t testing.TB, ns string, kinds ...domain.KindSpec) *domain.OntologySnapshot {
	t.Helper()
	s, err := domain.NewOntologySnapshot(ns, 1, domain.OntologyDoc{Kinds: kinds}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func order(preds map[string]domain.ValueSpec) domain.KindSpec {
	return domain.KindSpec{Kind: "order", Table: true, Predicates: preds}
}

func TestPlanTables_OnlyTableKindsWithTypedColumnsAndCompanions(t *testing.T) {
	tables := PlanTables([]*domain.OntologySnapshot{snap(t, "*",
		order(map[string]domain.ValueSpec{"status": {Type: "text"}, "total": {Type: "number", Indexed: true},
			"due": {Type: "date"}, "customer": {Type: "entity"}}),
		domain.KindSpec{Kind: "memory_fact", Predicates: map[string]domain.ValueSpec{"v": {Type: "text"}}},
	)})
	if len(tables) != 1 || tables[0].Name != "objects_order" {
		t.Fatalf("only the table kind gets a table: %+v", tables)
	}
	got := map[string]string{}
	for _, c := range tables[0].Columns {
		got[c.Name] = c.SQLType
	}
	want := map[string]string{
		"status__text": "text", "status__tags": "text[]", "status__parties": "text[]", "status__rowtags": "text[]",
		"total__number": "double precision", "total__tags": "text[]", "total__parties": "text[]", "total__rowtags": "text[]",
		"due__date": "timestamptz", "due__tags": "text[]", "due__parties": "text[]", "due__rowtags": "text[]",
		"customer__entity": "text", "customer__tags": "text[]", "customer__parties": "text[]", "customer__rowtags": "text[]",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("columns:\n got %v\nwant %v", got, want)
	}
	if !reflect.DeepEqual(tables[0].Indexed, []string{"total__number"}) || !reflect.DeepEqual(tables[0].Prefixes, []string{"order"}) {
		t.Fatalf("indexed %v prefixes %v", tables[0].Indexed, tables[0].Prefixes)
	}
}

func TestPlanTables_NamespacesShareOneTableWithoutTypeConflicts(t *testing.T) {
	a := snap(t, "a", order(map[string]domain.ValueSpec{"ref": {Type: "text"}}))
	b := snap(t, "b", domain.KindSpec{Kind: "order", Table: true, IDPrefix: "so", Predicates: map[string]domain.ValueSpec{"ref": {Type: "number"}}})
	tables := PlanTables([]*domain.OntologySnapshot{a, b})
	if len(tables) != 1 {
		t.Fatalf("one table per kind: %+v", tables)
	}
	var names []string
	for _, c := range tables[0].Columns {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "ref__number,ref__parties,ref__rowtags,ref__tags,ref__text" {
		t.Fatalf("one value column per (property, type), shared companions: %v", names)
	}
	if !reflect.DeepEqual(tables[0].Prefixes, []string{"order", "so"}) {
		t.Fatalf("prefixes: %v", tables[0].Prefixes)
	}
}

func TestPlanTables_HashFollowsTheDeclarationsOnly(t *testing.T) {
	base := order(map[string]domain.ValueSpec{"status": {Type: "text"}})
	h := func(snaps ...*domain.OntologySnapshot) string { return PlanTables(snaps)[0].Hash }
	h1 := h(snap(t, "*", base))
	if h1 != h(snap(t, "*", base)) || h1 != h(snap(t, "*", base), snap(t, "x", base)) {
		t.Fatal("the same declarations in more namespaces must not change the fingerprint (no needless rebuild)")
	}
	added := order(map[string]domain.ValueSpec{"status": {Type: "text"}, "total": {Type: "number"}})
	if h1 == h(snap(t, "*", added)) || h1 == h(snap(t, "*", base), snap(t, "x", added)) {
		t.Fatal("a new property anywhere must change the fingerprint (it triggers the rebuild)")
	}
}

var quotedIdent = regexp.MustCompile(`"[a-z0-9_]+"`)

func TestDDL_IsIdempotentAdditiveAndQuoted(t *testing.T) {
	tbl := PlanTables([]*domain.OntologySnapshot{snap(t, "*", order(map[string]domain.ValueSpec{
		"status": {Type: "text"}, "total": {Type: "number", Indexed: true}}))})[0]
	ddl := tbl.DDL()
	if !strings.HasPrefix(ddl[0], `CREATE TABLE IF NOT EXISTS "objects_order" (`) ||
		!strings.Contains(ddl[0], "PRIMARY KEY (namespace_id, entity_id)") {
		t.Fatalf("create: %s", ddl[0])
	}
	for _, s := range ddl[1:] {
		if !strings.Contains(s, "IF NOT EXISTS") {
			t.Fatalf("every statement must be idempotent: %s", s)
		}
		up := strings.ToUpper(s)
		for _, bad := range []string{"DROP", "ALTER COLUMN", "TYPE ", "RENAME"} {
			if strings.Contains(up, bad) {
				t.Fatalf("the generator only adds: %s", s)
			}
		}
	}
	last := ddl[len(ddl)-1]
	if !strings.HasPrefix(last, `CREATE INDEX IF NOT EXISTS "ox_`) || !strings.Contains(last, `(namespace_id, "total__number")`) {
		t.Fatalf("index on the declared indexed property: %s", last)
	}
}

// Property: for any valid table declaration the generated names are quoted,
// at most 63 bytes, and the plan is deterministic.
func TestPlanTables_Property_NamesFitAndPlanIsDeterministic(t *testing.T) {
	ident := rapid.StringMatching(`[a-z][a-z0-9]{0,8}(_[a-z0-9]{1,8}){0,4}`)
	rapid.Check(t, func(t *rapid.T) {
		kind := ident.Filter(func(s string) bool { return s != domain.ObservationKind }).Draw(t, "kind")
		preds := map[string]domain.ValueSpec{}
		for _, p := range rapid.SliceOfNDistinct(ident, 1, 6, func(s string) string { return s }).Draw(t, "preds") {
			preds[p] = domain.ValueSpec{Type: rapid.SampledFrom([]string{"text", "number", "date", "entity"}).Draw(t, "type"),
				Indexed: rapid.Bool().Draw(t, "idx")}
		}
		s, err := domain.NewOntologySnapshot("*", 1, domain.OntologyDoc{Kinds: []domain.KindSpec{{Kind: kind, Table: true, Predicates: preds}}}, nil)
		if err != nil {
			t.Skip(err.Error())
		}
		a, b := PlanTables([]*domain.OntologySnapshot{s}), PlanTables([]*domain.OntologySnapshot{s})
		if !reflect.DeepEqual(a, b) {
			t.Fatal("plan is not deterministic")
		}
		for _, stmt := range a[0].DDL() {
			for _, q := range quotedIdent.FindAllString(stmt, -1) {
				if len(q)-2 > 63 {
					t.Fatalf("identifier %s exceeds 63 bytes", q)
				}
			}
		}
		if len(a[0].Name) > 63 {
			t.Fatalf("table name %s too long", a[0].Name)
		}
	})
}

// ADR-0158 stage 4 bumps the row format to 3 (the <prop>__rowtags companions,
// extra row_tags, fence-class triples): every table built under format 2 must be
// stale, so the next Generate rebuilds it once.
func TestPlanTables_RowFormat3MarksFormat2TablesStale(t *testing.T) {
	spec := order(map[string]domain.ValueSpec{"status": {Type: "text"}})
	tbl := PlanTables([]*domain.OntologySnapshot{snap(t, "*", spec)})[0]
	var specJSON string
	{
		s, ok := snap(t, "*", spec).Kinds.Spec("order")
		if !ok {
			t.Fatal("order not declared")
		}
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		specJSON = string(raw)
	}
	hashUnder := func(format string) string {
		h := sha256.New()
		h.Write([]byte(format))
		h.Write([]byte{0})
		h.Write([]byte(specJSON))
		h.Write([]byte{0})
		return hex.EncodeToString(h.Sum(nil))
	}
	if tbl.Hash != hashUnder("row-format:3") {
		t.Fatalf("Table.Hash is not the row-format:3 fingerprint of the declarations")
	}
	if tbl.Hash == hashUnder("row-format:2") {
		t.Fatal("a table built under row format 2 would not be rebuilt")
	}
}
