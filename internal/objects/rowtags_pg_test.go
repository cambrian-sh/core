package objects_test

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/objects"
)

// ADR-0158 stage 4 (objects, D3 sites 36-38, 40-45): a value, link, event or
// observation whose row carries its own classification R is fenced by
// fence(E) AND fence(E ∪ R) (same parties) in every objects read — ListObjects
// values, filters, sorts, groups, metrics and existence, and GetObject
// properties, extras, links, alternatives and timeline. Narrow-only: R can hide
// what E shows, never show what E hides.

// observeR writes an observation whose row carries row classification rowTags.
func (e *env) observeR(entity, pred string, v domain.StatementValue, at time.Duration, ev domain.EvidenceID, ref string, rowTags ...string) error {
	v.Predicate = pred
	_, err := e.events.RecordObservation(e.ctx, domain.Observation{NamespaceID: e.ns, EntityID: entity, Predicate: pred,
		Value: v, OccurredAt: base.Add(at), EvidenceID: ev, SourceRef: ref, Classification: rowTags})
	return err
}

func (e *env) mustObserveR(entity, pred string, v domain.StatementValue, at time.Duration, ev domain.EvidenceID, ref string, rowTags ...string) {
	e.t.Helper()
	if err := e.observeR(entity, pred, v, at, ev, ref, rowTags...); err != nil {
		e.t.Fatal(err)
	}
}

func propNames(ps []domain.ObjectProperty) string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Property)
	}
	return fmt.Sprint(out)
}

func TestPg_RowRestrictedValuesAreHiddenEverywhere(t *testing.T) {
	e := newLPEnv(t, "lptest-rowtags")
	e.seedEvidence("rt-")
	const o1, o2, c1 = "lporder/o1", "lporder/o2", "lpcust/c1"
	// o1: status on south evidence, unrestricted; total on the SAME south
	// evidence but restricted by its row (a closed tag south readers are denied).
	e.mustObserveR(o1, "status", txt("open"), 0, "rt-south", "rt:1")
	e.mustObserveR(o1, "total", num(40), 0, "rt-south", "rt:2", "cls:restricted")
	// An extra, restricted by its row; another one not.
	e.mustObserveR(o1, "colour", txt("red"), 0, "rt-south", "rt:3", "cls:restricted")
	e.mustObserveR(o1, "size", txt("xl"), 0, "rt-south", "rt:4")
	// A conflicting status from another source, restricted by its row: an
	// alternative south readers may not see.
	e.mustObserveR(o1, "status", txt("held"), -time.Minute, "rt-global", "rt:5", "cls:restricted")
	// o2 exists only through a row-restricted value.
	e.mustObserveR(o2, "total", num(10), 0, "rt-south", "rt:6", "cls:restricted")
	// A row tag that would WIDEN under a plain union: north readers (any-of
	// [[r:north]]) cannot read south evidence, and a row tag r:north must not
	// change that.
	e.mustObserveR(c1, "segment", txt("hospital"), 0, "rt-south", "rt:7", "r:north")
	// A link and an event, restricted by their rows; one link not.
	for i, l := range []struct {
		to   string
		tags []string
	}{{c1, []string{"cls:restricted"}}, {"lpcust/c2", nil}} {
		if _, err := e.links.PutLink(e.ctx, domain.Link{NamespaceID: e.ns, FromRef: domain.EntityRef(o1), ToRef: domain.EntityRef(l.to),
			Relation: lpVerb, State: domain.LinkStateConfirmed, Mechanism: domain.LinkMechanismRecord, EvidenceID: "rt-south",
			AssertedBy: "t", AssertedAt: base, SourceRef: fmt.Sprintf("rtl:%d", i), Classification: l.tags}); err != nil {
			t.Fatal(err)
		}
	}
	e.mustObserveR("lpcust/c2", "segment", txt("clinic"), 0, "rt-south", "rt:8")
	if _, _, err := e.events.RecordEvent(e.ctx, domain.Event{ID: "rt-ev1", NamespaceID: e.ns, Type: "audited", OccurredAt: base.Add(time.Hour),
		EvidenceID: "rt-south", SourceRef: "rte:1", Roles: []domain.EventRole{{Role: "order", EntityID: o1}},
		Classification: []string{"cls:restricted"}}); err != nil {
		t.Fatal(err)
	}

	// The maintainer carries R into the companion and the class triple.
	row := e.row(lpOrder, o1)
	if got := fmt.Sprint(row[objects.RowTagsColumn("total")]); got != "[cls:restricted]" {
		t.Fatalf("total__rowtags = %s", got)
	}
	if row[objects.RowTagsColumn("status")] != nil {
		t.Fatalf("an unrestricted value's row tags must be NULL (byte-identical to row format 2): %v", row[objects.RowTagsColumn("status")])
	}
	extra, _ := row["extra"].(map[string]any)
	if got := fmt.Sprint(extra["colour"].(map[string]any)["row_tags"]); got != "[cls:restricted]" {
		t.Fatalf("extra colour row_tags = %s", got)
	}
	if _, has := extra["size"].(map[string]any)["row_tags"]; has {
		t.Fatal("an unrestricted extra must carry no row_tags key (byte-identical to row format 2)")
	}
	if got := e.classesOf(t, lpOrder, o2); strings.Join(got, " ") != "[r:south]|[]|[cls:restricted]" {
		t.Fatalf("o2 classes: %v", got)
	}

	// ── south: sees status, never total; o2 does not exist for it ────────────
	if got := ids(e.list(t, southOnly, domain.ObjectsQuery{Kind: lpOrder}).Rows); fmt.Sprint(got) != "[lporder/o1]" {
		t.Fatalf("south list: %v (o2 exists only through a row-restricted value)", got)
	}
	if r := e.list(t, southOnly, domain.ObjectsQuery{Kind: lpOrder, Where: []domain.ObjectCondition{
		{Property: "total", Op: domain.ObjectOpGt, Values: []domain.StatementValue{num(0)}}}}); len(r.Rows) != 0 {
		t.Fatalf("south filter on a row-restricted value: %v", ids(r.Rows))
	}
	if r := e.list(t, southOnly, domain.ObjectsQuery{Kind: lpOrder, Where: []domain.ObjectCondition{
		{Property: "total", Op: domain.ObjectOpIsNull}}}); fmt.Sprint(ids(r.Rows)) != "[lporder/o1]" {
		t.Fatalf("south is_null on a row-restricted value (it is absent for south): %v", ids(r.Rows))
	}
	if r := e.list(t, southOnly, domain.ObjectsQuery{Kind: lpOrder, Fields: []string{"total"}}); len(r.Rows) != 1 || len(r.Rows[0].Values) != 0 {
		t.Fatalf("south fields: %+v", r.Rows)
	}
	g := e.list(t, southOnly, domain.ObjectsQuery{Kind: lpOrder, Metrics: []domain.ObjectMetric{{Fn: domain.ObjectMetricCount}, {Fn: domain.ObjectMetricSum, Property: "total"}}})
	if len(g.Groups) != 1 || g.Groups[0].Metrics[0].Number != 1 || g.Groups[0].Metrics[1].Type != "" {
		t.Fatalf("south totals: %+v", g.Groups)
	}
	if r := e.list(t, southOnly, domain.ObjectsQuery{Kind: lpOrder, Links: []domain.ObjectLinkFilter{{Path: []string{lpVerb},
		Where: domain.ObjectCondition{Property: "segment", Op: domain.ObjectOpEq, Values: []domain.StatementValue{txt("hospital")}}}}}); len(r.Rows) != 0 {
		t.Fatalf("south link filter through a row-restricted link: %v", ids(r.Rows))
	}
	if r := e.list(t, southOnly, domain.ObjectsQuery{Kind: lpOrder, Links: []domain.ObjectLinkFilter{{Path: []string{lpVerb},
		Where: domain.ObjectCondition{Property: "segment", Op: domain.ObjectOpEq, Values: []domain.StatementValue{txt("clinic")}}}}}); fmt.Sprint(ids(r.Rows)) != "[lporder/o1]" {
		t.Fatalf("south link filter through an unrestricted link: %v", ids(r.Rows))
	}
	v, err := e.reader.Get(e.ctx, southOnly, e.ns, o1)
	if err != nil {
		t.Fatal(err)
	}
	if propNames(v.Properties) != "[status]" || v.Properties[0].Conflict || len(v.Properties[0].Alternatives) != 0 {
		t.Fatalf("south properties (total and the held alternative are row-restricted): %+v", v.Properties)
	}
	if propNames(v.Extras) != "[size]" {
		t.Fatalf("south extras: %+v", v.Extras)
	}
	if len(v.Links) != 1 || v.Links[0].OtherRef != "entity:lpcust/c2" {
		t.Fatalf("south links: %+v", v.Links)
	}
	for _, en := range v.Timeline {
		if en.EventID == "rt-ev1" || en.Property == "total" || en.Property == "colour" || en.Value.Text == "held" {
			t.Fatalf("south sees a row-restricted timeline entry: %+v", en)
		}
	}
	if _, err := e.reader.Get(e.ctx, southOnly, e.ns, o2); err != domain.ErrObjectNotFound {
		t.Fatalf("south GetObject o2: %v, want not found", err)
	}

	// ── a reader allowed the restricted tag sees all of it ───────────────────
	cleared := &domain.TagPredicate{AnyOfClauses: [][]string{{"r:south", "r:global"}}}
	if got := ids(e.list(t, cleared, domain.ObjectsQuery{Kind: lpOrder}).Rows); fmt.Sprint(got) != "[lporder/o1 lporder/o2]" {
		t.Fatalf("cleared list: %v", got)
	}
	v, err = e.reader.Get(e.ctx, cleared, e.ns, o1)
	if err != nil {
		t.Fatal(err)
	}
	if propNames(v.Properties) != "[status total]" || propNames(v.Extras) != "[colour size]" || len(v.Links) != 2 {
		t.Fatalf("cleared: properties %+v extras %+v links %+v", v.Properties, v.Extras, v.Links)
	}
	if st := v.Properties[0]; !st.Conflict || len(st.Alternatives) != 1 || st.Alternatives[0].Value.Text != "held" {
		t.Fatalf("cleared status alternatives: %+v", st)
	}
	var sawEvent bool
	for _, en := range v.Timeline {
		sawEvent = sawEvent || en.EventID == "rt-ev1"
	}
	if !sawEvent {
		t.Fatalf("cleared timeline lacks the event: %+v", v.Timeline)
	}

	// ── never wider: a row tag cannot hand north the south-evidence segment ──
	if r := e.list(t, northOnly, domain.ObjectsQuery{Kind: lpCust}); len(r.Rows) != 0 {
		t.Fatalf("north list of customers (south evidence, row tag r:north): %v", ids(r.Rows))
	}
	if _, err := e.reader.Get(e.ctx, northOnly, e.ns, c1); err != domain.ErrObjectNotFound {
		t.Fatalf("north GetObject c1: %v, want not found", err)
	}
}

// The ADR-0158 stage-4 rebuild (row format 3): a table built under format 2 is
// stale and rebuilt once; rows whose values carry no row classification are
// left exactly as they were (the upsert writes nothing — same xmin), and a row
// whose value does carry one, written before format 3 (no __rowtags), gets it.
func TestPg_RowFormat3RebuildTouchesOnlyRowClassifiedRows(t *testing.T) {
	e := newLPEnv(t, "lptest-rebuild3")
	e.seedEvidence("rb-")
	e.mustObserveR("lporder/o1", "status", txt("open"), 0, "rb-south", "rb:1")
	e.mustObserveR("lporder/o1", "colour", txt("red"), 0, "rb-south", "rb:2")
	e.mustObserveR("lporder/o2", "total", num(10), 0, "rb-south", "rb:3", "cls:restricted")
	// Simulate rows written by row format 2: no __rowtags, no extra row_tags,
	// and the build recorded under another fingerprint.
	if _, err := e.pool.Exec(e.ctx, `UPDATE objects_lporder SET total__rowtags = NULL WHERE namespace_id = $1`, e.ns); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE object_table_builds SET spec_hash = 'row-format-2' WHERE table_name = 'objects_lporder'`); err != nil {
		t.Fatal(err)
	}
	xmin := func(entity string) string {
		var x string
		if err := e.pool.QueryRow(e.ctx, `SELECT xmin::text FROM objects_lporder WHERE namespace_id = $1 AND entity_id = $2`, e.ns, entity).Scan(&x); err != nil {
			t.Fatal(err)
		}
		return x
	}
	before1, row1 := xmin("lporder/o1"), e.row(lpOrder, "lporder/o1")

	// Boot: the generator reports the table stale, the ontology rebuilds it.
	snaps := []*domain.OntologySnapshot{e.onto.Current(e.ns)}
	var stale []objects.Table
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	stale, err = objects.Generate(e.ctx, tx, snaps)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}
	var tbl *objects.Table
	for i := range stale {
		if stale[i].Name == "objects_lporder" {
			tbl = &stale[i]
		}
	}
	if tbl == nil {
		t.Fatalf("objects_lporder is not stale after the row-format bump: %+v", stale)
	}
	n, err := objects.Rebuild(e.ctx, e.pool, e.onto, *tbl)
	if err != nil || n != 2 {
		t.Fatalf("rebuild: %d %v", n, err)
	}
	if after := xmin("lporder/o1"); after != before1 {
		t.Fatalf("the rebuild rewrote a row without row classification (xmin %s -> %s)", before1, after)
	}
	if !reflect.DeepEqual(e.row(lpOrder, "lporder/o1"), row1) {
		t.Fatal("an unclassified row changed in the rebuild")
	}
	if got := fmt.Sprint(e.row(lpOrder, "lporder/o2")[objects.RowTagsColumn("total")]); got != "[cls:restricted]" {
		t.Fatalf("the rebuild did not fill the row classification: %s", got)
	}
	tx, err = e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }() // read-only check
	if stale, err = objects.Generate(e.ctx, tx, snaps); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range stale {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	for _, s := range names {
		if s == "objects_lporder" {
			t.Fatal("objects_lporder still stale after its rebuild")
		}
	}
}
