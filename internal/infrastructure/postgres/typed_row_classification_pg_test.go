package postgres

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// Row classification on typed rows, stage 1 (ADR-0158 D1; ENG-1974), against a
// real migrated Postgres (PG_TEST_DSN; migration 0054):
//
//   - events, observations, links and the entity mint persist the row
//     classification and read it back (canonical order, nil when empty);
//   - it is write-once: a replay of the same natural key with a different
//     classification keeps the first (the conflict clauses stay DO NOTHING);
//   - a link confirmation inherits the candidate's classification;
//   - an unclassified write stores '{}' and reads nil;
//   - the store refuses a malformed or kernel-stamped tag before writing.
//
// No read is FENCED by the column yet — that is stages 2-5.

const rowClassNS = "test-rowclass"

type rowClassFixture struct {
	pool     *pgxpool.Pool
	events   *PgEventStore
	entities *PgEntityStore
	links    *PgLinkStore
	ctx      context.Context
	ev       domain.EvidenceID
	at       time.Time
}

func newRowClassFixture(t *testing.T) *rowClassFixture {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	clean := func() {
		bg := context.Background()
		for _, q := range []string{
			`DELETE FROM links WHERE namespace_id = $1`,
			`DELETE FROM entities WHERE namespace_id = $1`,
			`DELETE FROM events WHERE namespace_id = $1`,
			`DELETE FROM observations WHERE namespace_id = $1`,
			`DELETE FROM evidence WHERE namespace_id = $1`,
		} {
			if _, err := pool.Exec(bg, q, rowClassNS); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)
	if _, err := pool.Exec(ctx, `
		INSERT INTO evidence (id, namespace_id, source_id, source_key, content_hash)
		VALUES ('ev-rowclass', $1, 'test-source', 'ev-rowclass', 'sha256:test')`, rowClassNS); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}
	reg, err := domain.NewRelationRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &rowClassFixture{
		pool: pool, ctx: ctx, ev: "ev-rowclass",
		events:   NewPgEventStore(pool, nil),
		entities: NewPgEntityStore(pool, nil),
		links:    NewPgLinkStore(pool, reg),
		at:       time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC),
	}
}

func (f *rowClassFixture) stored(t *testing.T, q string, args ...any) []string {
	t.Helper()
	var got []string
	if err := f.pool.QueryRow(f.ctx, q, args...).Scan(&got); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return got
}

func TestPgRowClassification_EventsRoundTripWriteOnce(t *testing.T) {
	f := newRowClassFixture(t)
	ev := domain.Event{
		NamespaceID: rowClassNS, Type: "salary_change", OccurredAt: f.at, EvidenceID: f.ev,
		SourceRef: "payroll/1@r1#1", Roles: []domain.EventRole{{Role: "employee", EntityID: "employee/E-17"}},
		Classification: []string{"restricted", "hr"},
	}
	id, created, err := f.events.RecordEvent(f.ctx, ev)
	if err != nil || !created {
		t.Fatalf("record: created=%v err=%v", created, err)
	}
	want := []string{"hr", "restricted"}
	got, err := f.events.EventsForEntity(f.ctx, rowClassNS, "employee/E-17", f.at.Add(-time.Hour), f.at.Add(time.Hour))
	if err != nil || len(got) != 1 {
		t.Fatalf("read back: %v, %v", got, err)
	}
	if !reflect.DeepEqual(got[0].Classification, want) {
		t.Fatalf("classification = %#v, want %#v (canonical order)", got[0].Classification, want)
	}
	// Replay under the same source_ref with another classification: DO NOTHING keeps
	// the first row and its classification.
	ev.Classification = []string{"finance"}
	again, created, err := f.events.RecordEvent(f.ctx, ev)
	if err != nil || created || again != id {
		t.Fatalf("replay = %v, %v, %v; want the first id, not created", again, created, err)
	}
	if s := f.stored(t, `SELECT classification FROM events WHERE id = $1`, string(id)); !reflect.DeepEqual(s, want) {
		t.Fatalf("after replay stored %v, want %v (write-once)", s, want)
	}

	// Unclassified: stored '{}', read as nil.
	plain := domain.Event{NamespaceID: rowClassNS, Type: "salary_change", OccurredAt: f.at.Add(time.Minute),
		EvidenceID: f.ev, SourceRef: "payroll/2@r1#1", Roles: []domain.EventRole{{Role: "employee", EntityID: "employee/E-18"}}}
	pid, _, err := f.events.RecordEvent(f.ctx, plain)
	if err != nil {
		t.Fatal(err)
	}
	if s := f.stored(t, `SELECT classification FROM events WHERE id = $1`, string(pid)); s == nil || len(s) != 0 {
		t.Fatalf("unclassified event stored %#v, want '{}'", s)
	}
	got, err = f.events.EventsForEntity(f.ctx, rowClassNS, "employee/E-18", f.at.Add(-time.Hour), f.at.Add(time.Hour))
	if err != nil || len(got) != 1 || got[0].Classification != nil {
		t.Fatalf("unclassified read = %#v, %v; want nil classification", got, err)
	}
}

func TestPgRowClassification_ObservationsRoundTripWriteOnce(t *testing.T) {
	f := newRowClassFixture(t)
	ob := domain.Observation{
		NamespaceID: rowClassNS, EntityID: "employee/E-17", Predicate: "salary",
		Value: domain.StatementValue{Type: "number", Number: 5000}, OccurredAt: f.at,
		EvidenceID: f.ev, SourceRef: "payroll/1@r1#1", Classification: []string{"hr"},
	}
	if ins, err := f.events.RecordObservation(f.ctx, ob); err != nil || !ins {
		t.Fatalf("record: %v, %v", ins, err)
	}
	ob.Classification = nil
	ob.Value.Number = 1
	if ins, err := f.events.RecordObservation(f.ctx, ob); err != nil || ins {
		t.Fatalf("replay: inserted=%v err=%v; want a no-op", ins, err)
	}
	hist, err := f.events.History(f.ctx, rowClassNS, "employee/E-17", "salary", f.at.Add(-time.Hour), f.at.Add(time.Hour))
	if err != nil || len(hist) != 1 {
		t.Fatalf("history: %v, %v", hist, err)
	}
	if !reflect.DeepEqual(hist[0].Classification, []string{"hr"}) || hist[0].Value.Number != 5000 {
		t.Fatalf("read back %#v; want the first row with [hr]", hist[0])
	}
	point, err := f.events.PointLookup(f.ctx, rowClassNS, "employee/E-17", "salary")
	if err != nil || point == nil || !reflect.DeepEqual(point.Classification, []string{"hr"}) {
		t.Fatalf("point = %#v, %v", point, err)
	}
}

func TestPgRowClassification_LinksAndConfirmationInherit(t *testing.T) {
	f := newRowClassFixture(t)
	l := domain.Link{
		NamespaceID: rowClassNS, FromRef: domain.EntityRef("customer/C-1"), ToRef: domain.EntityRef("customer/A-9"),
		Relation: domain.RelationSameAs, Mechanism: domain.LinkMechanismScored, Producer: "p@v1",
		EvidenceID: f.ev, AssertedBy: "producer:p", SourceRef: "orders/row-1",
		Classification: []string{"restricted"},
	}
	if created, err := f.links.PutLink(f.ctx, l); err != nil || !created {
		t.Fatalf("put: %v, %v", created, err)
	}
	l.Classification = []string{"other"}
	if created, err := f.links.PutLink(f.ctx, l); err != nil || created {
		t.Fatalf("replay: %v, %v; want a no-op", created, err)
	}
	got, err := f.links.LinksFor(f.ctx, rowClassNS, domain.EntityRef("customer/C-1"), domain.LinkQuery{})
	if err != nil || len(got) != 1 {
		t.Fatalf("links: %v, %v", got, err)
	}
	if !reflect.DeepEqual(got[0].Classification, []string{"restricted"}) {
		t.Fatalf("link classification = %#v, want [restricted] (write-once)", got[0].Classification)
	}
	confirmed, err := f.links.ConfirmLink(f.ctx, rowClassNS, got[0].ID, "operator:ada")
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if !reflect.DeepEqual(confirmed.Classification, []string{"restricted"}) {
		t.Fatalf("confirmation classification = %#v; it must inherit the candidate's", confirmed.Classification)
	}
	if s := f.stored(t, `SELECT classification FROM links WHERE id = $1`, confirmed.ID); !reflect.DeepEqual(s, []string{"restricted"}) {
		t.Fatalf("stored confirmation classification = %v", s)
	}
}

func TestPgRowClassification_EntityMintWriteOnce(t *testing.T) {
	f := newRowClassFixture(t)
	if created, err := f.entities.EnsureEntity(f.ctx, domain.Entity{
		ID: "employee/E-17", NamespaceID: rowClassNS, FirstSeenEvidence: f.ev,
		FirstSeenClassification: []string{"hr"},
	}); err != nil || !created {
		t.Fatalf("mint: %v, %v", created, err)
	}
	if created, err := f.entities.EnsureEntity(f.ctx, domain.Entity{
		ID: "employee/E-17", NamespaceID: rowClassNS, FirstSeenEvidence: f.ev,
	}); err != nil || created {
		t.Fatalf("re-mint: %v, %v; want a no-op", created, err)
	}
	e, ok, err := f.entities.GetEntity(f.ctx, rowClassNS, "employee/E-17")
	if err != nil || !ok || !reflect.DeepEqual(e.FirstSeenClassification, []string{"hr"}) {
		t.Fatalf("entity = %#v ok=%v err=%v; want first_seen_classification [hr]", e, ok, err)
	}
	if _, err := f.entities.EnsureEntity(f.ctx, domain.Entity{ID: "employee/E-18", NamespaceID: rowClassNS}); err != nil {
		t.Fatal(err)
	}
	if s := f.stored(t, `SELECT first_seen_classification FROM entities WHERE namespace_id = $1 AND id = 'employee/E-18'`, rowClassNS); s == nil || len(s) != 0 {
		t.Fatalf("unclassified mint stored %#v, want '{}'", s)
	}
}

func TestPgRowClassification_RefusedBeforeWrite(t *testing.T) {
	f := newRowClassFixture(t)
	bad := []string{"owner:alice"}
	if _, _, err := f.events.RecordEvent(f.ctx, domain.Event{NamespaceID: rowClassNS, Type: "x", OccurredAt: f.at,
		EvidenceID: f.ev, SourceRef: "bad/1", Classification: bad}); !errors.Is(err, domain.ErrRowClassificationRefused) {
		t.Errorf("event: %v", err)
	}
	if _, err := f.events.RecordObservation(f.ctx, domain.Observation{NamespaceID: rowClassNS, EntityID: "employee/E-1",
		Predicate: "p", Value: domain.StatementValue{Type: "text", Text: "v"}, OccurredAt: f.at, EvidenceID: f.ev,
		SourceRef: "bad/1", Classification: []string{"hr", "hr"}}); !errors.Is(err, domain.ErrRowClassificationRefused) {
		t.Errorf("observation: %v", err)
	}
	if _, err := f.links.PutLink(f.ctx, domain.Link{NamespaceID: rowClassNS, FromRef: domain.EntityRef("customer/C-1"),
		ToRef: domain.EntityRef("customer/A-9"), Relation: domain.RelationSameAs, Mechanism: domain.LinkMechanismScored,
		EvidenceID: f.ev, AssertedBy: "producer:p", SourceRef: "bad/1", Classification: []string{" hr"}}); !errors.Is(err, domain.ErrRowClassificationRefused) {
		t.Errorf("link: %v", err)
	}
	if _, err := f.entities.EnsureEntity(f.ctx, domain.Entity{ID: "employee/E-9", NamespaceID: rowClassNS,
		FirstSeenClassification: []string{"provenance:source=x"}}); !errors.Is(err, domain.ErrRowClassificationRefused) {
		t.Errorf("entity: %v", err)
	}
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT
		(SELECT count(*) FROM events WHERE namespace_id = $1) + (SELECT count(*) FROM observations WHERE namespace_id = $1) +
		(SELECT count(*) FROM links WHERE namespace_id = $1) + (SELECT count(*) FROM entities WHERE namespace_id = $1)`,
		rowClassNS).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d rows written despite the refusals", n)
	}
}
