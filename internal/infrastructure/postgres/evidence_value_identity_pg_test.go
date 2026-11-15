package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// Value identity on the kernel side (ADR-0147), against a real migrated
// Postgres (PG_TEST_DSN; `orchestrator migrate up`, which applies 0037).
//
//   - D2: typed rows written under DIFFERENT refs (one per source revision)
//     at the SAME occurred_at both land, and point picks the newer one.
//   - D6: a retired ref stops counting in every read — the event store's point,
//     history and events, the plane's point and aggregate, the catalog — while
//     the row stays stored; reinstating brings it back.
//   - D8: the time basis is stored and shown.
//   - D7: LatestBySourceKey finds the newest evidence for a source key.
//
// The file name sorts before migration_guard_test.go on purpose (that test drops
// and recreates the public schema).

const viPrefix = "vi-l0"

type viFixture struct {
	pool     *pgxpool.Pool
	events   *PgEventStore
	evidence *PgEvidenceStore
	plane    *PgQueryPlane
	ctx      context.Context
	at       time.Time
}

// viScope reads through the SQL path with the owner fence (a plain Bypass),
// the way the operator plane does — not the unfiltered maintenance path.
var viScope = &domain.TagPredicate{Bypass: true}

func newValueIdentityFixture(t *testing.T) *viFixture {
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
		for _, q := range []string{
			`DELETE FROM typed_row_retirements WHERE source_ref LIKE 'vi-l0%'`,
			`DELETE FROM events WHERE source_ref LIKE 'vi-l0%'`,
			`DELETE FROM observations WHERE source_ref LIKE 'vi-l0%'`,
			`DELETE FROM evidence_outbox WHERE evidence_id IN (SELECT id FROM evidence WHERE source_id LIKE 'vi-l0%')`,
			`DELETE FROM evidence WHERE source_id LIKE 'vi-l0%'`,
		} {
			if _, err := pool.Exec(context.Background(), q); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)
	reg, err := domain.NewRelationRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	events := NewPgEventStore(pool, nil)
	ev := NewPgEvidenceStore(pool)
	return &viFixture{
		pool: pool, events: events, evidence: ev, ctx: ctx,
		plane: NewPgQueryPlane(pool, events, NewPgKnowledgeStore(pool, nil), ev,
			NewPgLinkStore(pool, reg), NewPgEntityStore(pool, nil), reg),
		at: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC),
	}
}

// evidenceRow archives one revision of one source record through the real
// store, the way the ingestor does.
func (f *viFixture) evidenceRow(t *testing.T, key, revision string) domain.EvidenceID {
	t.Helper()
	id, _, err := f.evidence.Insert(f.ctx, domain.Evidence{
		NamespaceID: "default", SourceID: viPrefix + ":erp", SourceKey: key, SourceRevision: revision,
		ContentHash: domain.CID("sha256:" + revision), Classification: []string{},
	})
	if err != nil {
		t.Fatalf("evidence %s@%s: %v", key, revision, err)
	}
	return id
}

func (f *viFixture) observe(t *testing.T, ev domain.EvidenceID, ref, entity string, v float64, basis string) {
	t.Helper()
	ins, err := f.events.RecordObservation(f.ctx, domain.Observation{
		NamespaceID: "default", EntityID: entity, Predicate: "ship_days",
		Value: domain.StatementValue{Type: "number", Number: v}, OccurredAt: f.at,
		Confidence: 1, EvidenceID: ev, SourceRef: ref, TimeBasis: basis,
	})
	if err != nil || !ins {
		t.Fatalf("observation %s: inserted=%v err=%v", ref, ins, err)
	}
}

func (f *viFixture) query(t *testing.T, q domain.KnowledgeQuery) []domain.QueryRow {
	t.Helper()
	res, err := f.plane.Query(f.ctx, q, viScope)
	if err != nil {
		t.Fatalf("query %s: %v", q.Kind, err)
	}
	return res.Rows
}

func (f *viFixture) count(t *testing.T, entity string) float64 {
	t.Helper()
	rows := f.query(t, domain.KnowledgeQuery{Kind: domain.QueryAggregate, EntityID: entity,
		Predicate: "ship_days", Aggregate: "count"})
	if len(rows) != 1 {
		t.Fatalf("aggregate rows = %d", len(rows))
	}
	v, _ := rows[0]["value"].(float64)
	return v
}

// D2: an edit of a record whose event time did not move is a new row, not a
// dropped one, and the newer row is the current answer.
func TestPg_ValueIdentity_AnEditUnderANewRevisionAppendsAndBecomesCurrent(t *testing.T) {
	f := newValueIdentityFixture(t)
	const entity = "vi-l0/order-1"
	ev1 := f.evidenceRow(t, "SO-1", "rev-a")
	ev2 := f.evidenceRow(t, "SO-1", "rev-b")
	f.observe(t, ev1, viPrefix+":SO-1:"+entity+":ship_days@r1#rev-a", entity, 3, domain.TimeBasisEvent)
	f.observe(t, ev2, viPrefix+":SO-1:"+entity+":ship_days@r1#rev-b", entity, 5, domain.TimeBasisEvent)

	hist, err := f.events.History(f.ctx, "default", entity, "ship_days", f.at.Add(-time.Hour), f.at.Add(time.Hour))
	if err != nil || len(hist) != 2 {
		t.Fatalf("history = %d rows (err %v), want both versions", len(hist), err)
	}
	if got := f.query(t, domain.KnowledgeQuery{Kind: domain.QueryPoint, EntityID: entity, Predicate: "ship_days"}); len(got) != 1 || got[0]["value_number"] != 5.0 {
		t.Fatalf("point = %v, want the edited value 5", got)
	}
	// The same ref again is a replay and still writes nothing (idempotency kept).
	ins, err := f.events.RecordObservation(f.ctx, domain.Observation{
		NamespaceID: "default", EntityID: entity, Predicate: "ship_days",
		Value: domain.StatementValue{Type: "number", Number: 5}, OccurredAt: f.at, Confidence: 1,
		EvidenceID: ev2, SourceRef: viPrefix + ":SO-1:" + entity + ":ship_days@r1#rev-b",
	})
	if err != nil || ins {
		t.Fatalf("a replay inserted=%v err=%v, want a no-op", ins, err)
	}
}

// D6: retired rows stop counting everywhere a value is read, stay stored, and
// come back when reinstated.
func TestPg_ValueIdentity_RetiredRowsStopCountingButStayStored(t *testing.T) {
	f := newValueIdentityFixture(t)
	const entity = "vi-l0/order-2"
	ev := f.evidenceRow(t, "SO-2", "rev-a")
	oldRef := viPrefix + ":SO-2:" + entity + ":ship_days@r1#rev-a"
	newRef := viPrefix + ":SO-2:" + entity + ":ship_days@r2#rev-a"
	// The retired row is written LAST, so it is the one "latest" would pick
	// (same occurred_at, higher id) unless the read honours the retirement.
	f.observe(t, ev, newRef, entity, 9, domain.TimeBasisEvent)
	f.observe(t, ev, oldRef, entity, 7, domain.TimeBasisEvent)
	evOld, _, err := f.events.RecordEvent(f.ctx, domain.Event{NamespaceID: "default", Type: "vi_l0_shipped",
		OccurredAt: f.at, EvidenceID: ev, SourceRef: viPrefix + ":SO-2@r1#rev-a",
		Roles: []domain.EventRole{{Role: "order", EntityID: entity}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.events.RecordEvent(f.ctx, domain.Event{NamespaceID: "default", Type: "vi_l0_shipped",
		OccurredAt: f.at, EvidenceID: ev, SourceRef: viPrefix + ":SO-2@r2#rev-a",
		Roles: []domain.EventRole{{Role: "order", EntityID: entity}}}); err != nil {
		t.Fatal(err)
	}
	overview := func() domain.SubstrateOverview {
		o, err := f.plane.Catalog(viScope).Overview(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	before := overview()
	if n := f.count(t, entity); n != 2 {
		t.Fatalf("count before retirement = %v, want the doubled 2 (the bug's precondition)", n)
	}

	n, err := f.events.RetireRows(f.ctx, "default",
		[]string{oldRef, viPrefix + ":SO-2@r1#rev-a"}, domain.RetireSuperseded, "mapping_revision:2")
	if err != nil || n != 2 {
		t.Fatalf("retire = %d, %v", n, err)
	}
	if n, _ := f.events.RetireRows(f.ctx, "default", []string{oldRef}, domain.RetireSuperseded, "again"); n != 0 {
		t.Errorf("re-retiring retired %d again, want an idempotent 0", n)
	}

	if c := f.count(t, entity); c != 1 {
		t.Errorf("aggregate count after retirement = %v, want 1", c)
	}
	if got := f.query(t, domain.KnowledgeQuery{Kind: domain.QueryPoint, EntityID: entity, Predicate: "ship_days"}); len(got) != 1 || got[0]["value_number"] != 9.0 {
		t.Errorf("plane point = %v, want the current revision's 9", got)
	}
	if ob, err := f.events.PointLookup(f.ctx, "default", entity, "ship_days"); err != nil || ob == nil || ob.SourceRef != newRef {
		t.Errorf("store point = %+v (%v), want %s", ob, err, newRef)
	}
	hist, _ := f.events.History(f.ctx, "default", entity, "ship_days", f.at.Add(-time.Hour), f.at.Add(time.Hour))
	if len(hist) != 1 {
		t.Errorf("history = %d rows, want 1", len(hist))
	}
	evs, _ := f.events.EventsForEntity(f.ctx, "default", entity, f.at.Add(-time.Hour), f.at.Add(time.Hour))
	if len(evs) != 1 || evs[0].ID == evOld {
		t.Errorf("events = %+v, want only the current revision's event", evs)
	}
	after := overview()
	if after.ObservationCount != before.ObservationCount-1 || after.EventCount != before.EventCount-1 {
		t.Errorf("catalog counts obs %d→%d events %d→%d, want each down by one",
			before.ObservationCount, after.ObservationCount, before.EventCount, after.EventCount)
	}
	// A record, never a delete: both rows are still in the table.
	var stored int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM observations WHERE entity_id = $1`, entity).Scan(&stored); err != nil || stored != 2 {
		t.Errorf("stored rows = %d (%v), want both kept", stored, err)
	}

	if n, err := f.events.ReinstateRows(f.ctx, "default", []string{oldRef}); err != nil || n != 1 {
		t.Fatalf("reinstate = %d, %v", n, err)
	}
	if c := f.count(t, entity); c != 2 {
		t.Errorf("count after reinstating = %v, want 2", c)
	}
	if n, err := f.events.RetireRows(f.ctx, "default", []string{oldRef}, domain.RetireSourceDeleted, "ev-tomb"); err != nil || n != 1 {
		t.Errorf("re-retiring a reinstated ref = %d, %v, want 1", n, err)
	}
	if _, err := f.events.RetireRows(f.ctx, "default", []string{oldRef}, "forgotten", ""); err == nil {
		t.Error("an unknown retirement reason was accepted")
	}
}

// D8: which clock occurred_at is travels with the row and out of the plane.
func TestPg_ValueIdentity_TheTimeBasisIsStoredAndShown(t *testing.T) {
	f := newValueIdentityFixture(t)
	const entity = "vi-l0/order-3"
	ev := f.evidenceRow(t, "SO-3", "rev-a")
	f.observe(t, ev, viPrefix+":SO-3:"+entity+":ship_days@r1#rev-a", entity, 4, domain.TimeBasisReceived)
	ob, err := f.events.PointLookup(f.ctx, "default", entity, "ship_days")
	if err != nil || ob == nil || ob.TimeBasis != domain.TimeBasisReceived {
		t.Fatalf("stored basis = %+v (%v), want received", ob, err)
	}
	if got := f.query(t, domain.KnowledgeQuery{Kind: domain.QueryPoint, EntityID: entity, Predicate: "ship_days"}); len(got) != 1 || got[0]["time_basis"] != domain.TimeBasisReceived {
		t.Errorf("plane row = %v, want time_basis=received", got)
	}
	_, err = f.events.RecordObservation(f.ctx, domain.Observation{EntityID: entity, Predicate: "ship_days",
		Value: domain.StatementValue{Type: "number", Number: 1}, OccurredAt: f.at, SourceRef: viPrefix + "-bad", TimeBasis: "guessed"})
	if err == nil || !strings.Contains(err.Error(), "time basis") {
		t.Errorf("an unknown time basis was accepted: %v", err)
	}
}

// D7: the newest evidence for a source key, by ingest order; nil for a key the
// source never sent.
func TestPg_ValueIdentity_LatestBySourceKeyFindsTheNewestRevision(t *testing.T) {
	f := newValueIdentityFixture(t)
	first := f.evidenceRow(t, "SO-4", "rev-a")
	second := f.evidenceRow(t, "SO-4", "rev-b")
	if first == second {
		t.Fatal("two revisions minted one evidence row")
	}
	got, err := f.evidence.LatestBySourceKey(f.ctx, "default", viPrefix+":erp", "SO-4")
	if err != nil || got == nil || got.ID != second || got.SourceRevision != "rev-b" {
		t.Fatalf("latest = %+v (%v), want %s", got, err, second)
	}
	none, err := f.evidence.LatestBySourceKey(f.ctx, "default", viPrefix+":erp", "SO-missing")
	if err != nil || none != nil {
		t.Errorf("unknown key = %+v, %v; want nil, nil", none, err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatal("unreachable")
	}
}

// ENG-1045: an operator asking include_retired gets the retired rows back,
// marked with why and by what they were retired; without the flag the answer is
// unchanged. (The operator-only rule lives in the authz seam.)
func TestPg_ValueIdentity_IncludeRetiredReturnsMarkedRetiredRows(t *testing.T) {
	f := newValueIdentityFixture(t)
	const entity = "vi-l0/order-r"
	ev := f.evidenceRow(t, "SO-R", "rev-a")
	oldRef := viPrefix + ":SO-R:" + entity + ":ship_days@r1#rev-a"
	newRef := viPrefix + ":SO-R:" + entity + ":ship_days@r2#rev-a"
	f.observe(t, ev, newRef, entity, 9, domain.TimeBasisEvent)
	f.observe(t, ev, oldRef, entity, 7, domain.TimeBasisEvent) // written last: "latest" if counted
	evRef := viPrefix + ":SO-R@r1#rev-a"
	if _, _, err := f.events.RecordEvent(f.ctx, domain.Event{NamespaceID: "default", Type: "vi_l0_shipped",
		OccurredAt: f.at, EvidenceID: ev, SourceRef: evRef,
		Roles: []domain.EventRole{{Role: "order", EntityID: entity}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.events.RetireRows(f.ctx, "default", []string{oldRef, evRef}, domain.RetireSourceDeleted, "ev-tomb-r"); err != nil {
		t.Fatal(err)
	}
	from, to := f.at.Add(-time.Hour), f.at.Add(time.Hour)

	// Without the flag: the retired value and event are absent, as before.
	if got := f.query(t, domain.KnowledgeQuery{Kind: domain.QueryPoint, EntityID: entity, Predicate: "ship_days"}); len(got) != 1 || got[0]["value_number"] != 9.0 || got[0]["retired"] != nil {
		t.Fatalf("point without the flag = %v", got)
	}
	if got := f.query(t, domain.KnowledgeQuery{Kind: domain.QueryEvents, EntityID: entity, From: from, To: to}); len(got) != 0 {
		t.Fatalf("events without the flag = %v, want none (the only event is retired)", got)
	}

	// With it: point answers the latest STORED row, marked.
	point := f.query(t, domain.KnowledgeQuery{Kind: domain.QueryPoint, EntityID: entity, Predicate: "ship_days", IncludeRetired: true})
	if len(point) != 1 || point[0]["value_number"] != 7.0 || point[0]["retired"] != true ||
		point[0]["retired_reason"] != domain.RetireSourceDeleted || point[0]["retired_by"] != "ev-tomb-r" || point[0]["retired_at"] == nil {
		t.Errorf("point with include_retired = %v, want the retired 7 marked source_deleted by ev-tomb-r", point)
	}
	// Under the unfiltered (maintenance) scope too — the store's own
	// PointLookup skips retired rows, so the flag must take the SQL path.
	res, err := f.plane.Query(f.ctx, domain.KnowledgeQuery{Kind: domain.QueryPoint, EntityID: entity, Predicate: "ship_days", IncludeRetired: true},
		&domain.TagPredicate{Bypass: true, AllOwners: true})
	if err != nil || len(res.Rows) != 1 || res.Rows[0]["retired"] != true {
		t.Errorf("unfiltered point with include_retired = %v", res.Rows)
	}
	hist := f.query(t, domain.KnowledgeQuery{Kind: domain.QueryHistory, EntityID: entity, Predicate: "ship_days", From: from, To: to, IncludeRetired: true})
	retired := 0
	for _, r := range hist {
		if r["retired"] == true {
			retired++
		}
	}
	if len(hist) != 2 || retired != 1 {
		t.Errorf("history with include_retired = %v, want both rows, one marked retired", hist)
	}
	evs := f.query(t, domain.KnowledgeQuery{Kind: domain.QueryEvents, EntityID: entity, From: from, To: to, IncludeRetired: true})
	if len(evs) != 1 || evs[0]["retired"] != true {
		t.Errorf("events with include_retired = %v, want the retired event marked", evs)
	}
	// A narrow scope still fences retired rows: this evidence carries no tags a
	// team:z reader holds.
	narrow, err := f.plane.Query(f.ctx, domain.KnowledgeQuery{Kind: domain.QueryHistory, EntityID: entity, Predicate: "ship_days", From: from, To: to, IncludeRetired: true},
		&domain.TagPredicate{RequiredTags: []string{"team:z"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(narrow.Rows) != 0 {
		t.Errorf("retired rows escaped the fence: %v", narrow.Rows)
	}

	// Through the kernel's principal seam (the path every plugin takes): the
	// operator plane's system principal reads the retired row, an agent is
	// refused (owner decision 2026-11-15).
	seam := authz.QueryKnowledgeFunc(domain.AllowAllAuthorizer{}, f.plane, nil)
	q := domain.KnowledgeQuery{Kind: domain.QueryPoint, EntityID: entity, Predicate: "ship_days", IncludeRetired: true}
	if res, err := seam(f.ctx, domain.SystemPrincipal, q); err != nil || len(res.Rows) != 1 || res.Rows[0]["retired"] != true {
		t.Errorf("operator plane through the seam = %v, %v", res.Rows, err)
	}
	if _, err := seam(f.ctx, domain.AgentPrincipal("agent-x"), q); !errors.Is(err, domain.ErrRetiredOperatorOnly) {
		t.Errorf("an agent read retired rows through the seam: %v", err)
	}
}
