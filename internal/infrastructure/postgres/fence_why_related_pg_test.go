package postgres

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// ADR-0148 (accepted 2026-11-12): `why` returns stored lineage by default;
// related rows (shared participant, inside a window, hub-damped, ranked, capped
// per event, optionally role-filtered) only under include_correlation; an empty
// flag-off answer carries a fenced hint
// (T-20261112-implement-adr-0148-why-stored-lineage-default).
//
// Fixture around one anchoring event A (t0), customer c, shipment s, hub h:
//
//	near    t0-1h   c (customer) + s (shipment)      → strongest
//	role    t0-2h   c, but as "assignee"             → dropped by a role filter
//	mid     t0-10h  c (customer)
//	far     t0-80h  c (customer)                     → outside the 72 h window
//	secret  t0-30m  c (customer), k-secret evidence  → hidden from the narrow caller
//	alice   t0-20m  c (customer), owner:alice        → hidden from bob
//	hub     70 events t0-1m … t0-70m sharing only h; 10 of them k-secret
//
// For the operator, h is in 71 events in the window (> 64): a hub, flagged and
// not followed. For the narrow caller h is in 61 visible events: not a hub —
// df counts only what the caller may see, so the flag never discloses hidden
// rows. The file name sorts before migration_guard_test.go on purpose.

type relFixture struct {
	pool   *pgxpool.Pool
	plane  *PgQueryPlane
	ctx    context.Context
	t0     time.Time
	ev     map[string]string // name -> event ref
	narrow *domain.TagPredicate
}

func newRelFixture(t *testing.T) *relFixture {
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
			`DELETE FROM typed_row_retirements WHERE source_ref LIKE 'rel148-%'`,
			`DELETE FROM event_roles WHERE entity_id LIKE 'rel148/%'`,
			`DELETE FROM events WHERE source_ref LIKE 'rel148-%'`,
			`DELETE FROM evidence_outbox WHERE evidence_id IN (SELECT id FROM evidence WHERE source_id = 'rel148-src')`,
			`DELETE FROM evidence WHERE source_id = 'rel148-src'`,
		} {
			if _, err := pool.Exec(bg, q); err != nil {
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
	f := &relFixture{
		pool: pool, ctx: ctx, t0: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC), ev: map[string]string{},
		plane:  NewPgQueryPlane(pool, events, nil, nil, NewPgLinkStore(pool, reg), NewPgEntityStore(pool), reg),
		narrow: &domain.TagPredicate{RequiredTags: []string{"mail"}, ForbiddenTags: []string{"k-secret"}},
	}
	evs := NewPgEvidenceStore(pool)
	evidence := func(name string, tags []string) domain.EvidenceID {
		id, _, err := evs.Insert(ctx, domain.Evidence{NamespaceID: "default", SourceID: "rel148-src",
			SourceKey: name, SourceTime: f.t0, IngestedAt: f.t0,
			ContentHash: domain.CID("sha256:rel148-" + name), Classification: tags})
		if err != nil {
			t.Fatalf("evidence %s: %v", name, err)
		}
		return id
	}
	pub := evidence("pub", []string{"mail"})
	sec := evidence("secret", []string{"mail", "k-secret"})
	ali := evidence("alice", []string{"mail", "owner:alice"})
	event := func(name string, ev domain.EvidenceID, ago time.Duration, roles ...string) {
		e := domain.Event{ID: domain.EventID("rel148-" + name), NamespaceID: "default", Type: "rel148_" + name,
			OccurredAt: f.t0.Add(-ago), EvidenceID: ev, SourceRef: "rel148-" + name}
		for i := 0; i+1 < len(roles); i += 2 {
			e.Roles = append(e.Roles, domain.EventRole{Role: roles[i], EntityID: roles[i+1]})
		}
		if _, _, err := events.RecordEvent(ctx, e); err != nil {
			t.Fatalf("event %s: %v", name, err)
		}
		f.ev[name] = domain.RefPrefixEvent + "rel148-" + name
	}
	event("A", pub, 0, "customer", "rel148/c", "shipment", "rel148/s", "site", "rel148/h")
	event("near", pub, time.Hour, "customer", "rel148/c", "shipment", "rel148/s")
	event("role", pub, 2*time.Hour, "assignee", "rel148/c")
	event("mid", pub, 10*time.Hour, "customer", "rel148/c")
	event("far", pub, 80*time.Hour, "customer", "rel148/c")
	event("secret", sec, 30*time.Minute, "customer", "rel148/c")
	event("alice", ali, 20*time.Minute, "customer", "rel148/c")
	for i := 1; i <= 70; i++ {
		ev := pub
		if i > 60 {
			ev = sec
		}
		event(fmt.Sprintf("hub%02d", i), ev, time.Duration(i)*time.Minute, "site", "rel148/h")
	}
	return f
}

func (f *relFixture) why(t *testing.T, q domain.KnowledgeQuery, scope *domain.TagPredicate) []domain.QueryRow {
	t.Helper()
	q.Kind = domain.QueryWhy
	res, err := f.plane.Query(f.ctx, q, scope)
	if err != nil {
		t.Fatalf("why %+v: %v", q, err)
	}
	return res.Rows
}

func relatedTo(rows []domain.QueryRow, from string) []string {
	var out []string
	for _, r := range rows {
		if r["row"] == "related" && r["from_ref"] == from {
			out = append(out, fmt.Sprint(r["to_ref"]))
		}
	}
	return out
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// D1/D3: flag off is stored lineage only (A has none), and the empty answer
// carries the hint with exactly the number of related rows the flag would add.
func TestPg_WhyRelated_FlagOffAnswersStoredOnlyWithAHint(t *testing.T) {
	f := newRelFixture(t)
	off := f.why(t, domain.KnowledgeQuery{EntityID: f.ev["A"], Hops: 1}, f.narrow)
	if len(off) != 1 || off[0]["row"] != "hint" {
		t.Fatalf("flag-off why(A) = %v, want exactly one hint row", off)
	}
	on := relatedTo(f.why(t, domain.KnowledgeQuery{EntityID: f.ev["A"], Hops: 1, IncludeCorrelation: true}, f.narrow), f.ev["A"])
	if off[0]["related_count"] != len(on) || len(on) == 0 {
		t.Fatalf("hint related_count = %v, flag-on related rows = %d", off[0]["related_count"], len(on))
	}
	if want := fmt.Sprintf("no stored lineage; %d related events available with include_correlation", len(on)); off[0]["text"] != want {
		t.Fatalf("hint text = %q, want %q", off[0]["text"], want)
	}
	if off[0]["flag"] != "include_correlation" || off[0]["hint"] != "related_available" {
		t.Fatalf("hint row shape: %v", off[0])
	}
}

// D4: the window.
func TestPg_WhyRelated_WindowBoundsRelatedEvents(t *testing.T) {
	f := newRelFixture(t)
	def := relatedTo(f.why(t, domain.KnowledgeQuery{EntityID: f.ev["A"], Hops: 1, IncludeCorrelation: true,
		CorrelationRoles: []string{"customer"}}, domain.ScopeMaintenance), f.ev["A"])
	if has(def, f.ev["far"]) || !has(def, f.ev["mid"]) {
		t.Fatalf("72 h default window: got %v (want mid in, far out)", def)
	}
	wide := relatedTo(f.why(t, domain.KnowledgeQuery{EntityID: f.ev["A"], Hops: 1, IncludeCorrelation: true,
		CorrelationRoles: []string{"customer"}, CorrelationWindow: 100 * time.Hour}, domain.ScopeMaintenance), f.ev["A"])
	if !has(wide, f.ev["far"]) {
		t.Fatalf("100 h window should admit far: %v", wide)
	}
	short := relatedTo(f.why(t, domain.KnowledgeQuery{EntityID: f.ev["A"], Hops: 1, IncludeCorrelation: true,
		CorrelationRoles: []string{"customer"}, CorrelationWindow: 5 * time.Hour}, domain.ScopeMaintenance), f.ev["A"])
	if has(short, f.ev["mid"]) || !has(short, f.ev["near"]) {
		t.Fatalf("5 h window: got %v (want near in, mid out)", short)
	}
}

// D7: the role must match in BOTH events.
func TestPg_WhyRelated_RoleFilterMatchesBothEvents(t *testing.T) {
	f := newRelFixture(t)
	q := domain.KnowledgeQuery{EntityID: f.ev["A"], Hops: 1, IncludeCorrelation: true}
	all := relatedTo(f.why(t, q, f.narrow), f.ev["A"])
	if !has(all, f.ev["role"]) {
		t.Fatalf("no filter: role event missing from %v", all)
	}
	q.CorrelationRoles = []string{"customer"}
	cust := relatedTo(f.why(t, q, f.narrow), f.ev["A"])
	if has(cust, f.ev["role"]) || !has(cust, f.ev["near"]) {
		t.Fatalf("customer filter: got %v (role is customer in A but assignee there — must drop)", cust)
	}
	q.CorrelationRoles = []string{"assignee"}
	if got := relatedTo(f.why(t, q, f.narrow), f.ev["A"]); len(got) != 0 {
		t.Fatalf("assignee filter: c is not an assignee in A, got %v", got)
	}
}

// D5 + D6 + the cap: hub damping on caller-visible counts, ordering by score,
// at most 8 per event.
func TestPg_WhyRelated_HubsRankingAndCap(t *testing.T) {
	f := newRelFixture(t)
	q := domain.KnowledgeQuery{EntityID: f.ev["A"], Hops: 1, IncludeCorrelation: true}

	op := f.why(t, q, domain.ScopeMaintenance)
	var flagged []domain.QueryRow
	for _, r := range op {
		if r["row"] == "flagged" {
			flagged = append(flagged, r)
		}
	}
	if len(flagged) != 1 || flagged[0]["ref"] != "entity:rel148/h" || flagged[0]["events"] != 71 ||
		flagged[0]["guard"] != "why_hub_participant" || flagged[0]["limit"] != domain.CorrelationHubThreshold {
		t.Fatalf("operator: want one flagged hub row for h with 71 events, got %v", flagged)
	}
	for _, to := range relatedTo(op, f.ev["A"]) {
		if strings.Contains(to, "rel148-hub") {
			t.Fatalf("operator: a hub-only event was followed: %s", to)
		}
	}

	nar := f.why(t, q, f.narrow)
	for _, r := range nar {
		if r["row"] == "flagged" {
			t.Fatalf("narrow: h has 61 VISIBLE events, so it is no hub for this caller (and a flag would count hidden rows): %v", r)
		}
	}
	rel := relatedTo(nar, f.ev["A"])
	if len(rel) != domain.CorrelationMaxPerEvent {
		t.Fatalf("narrow: %d related rows from A, want the cap %d (%v)", len(rel), domain.CorrelationMaxPerEvent, rel)
	}
	if rel[0] != f.ev["near"] || rel[1] != f.ev["role"] || rel[2] != f.ev["mid"] {
		t.Fatalf("narrow: ranking %v, want near, role, mid first", rel[:3])
	}
	last := 1e9
	for _, r := range nar {
		if r["row"] != "related" {
			continue
		}
		s, ok := r["score"].(float64)
		if !ok || s > last || s <= 0 {
			t.Fatalf("scores must be positive and non-increasing: %v", nar)
		}
		last = s
		if r["mechanism"] != domain.LinkMechanismSharedObject || r["stored"] != false {
			t.Fatalf("related row mislabelled: %v", r)
		}
	}
	for _, to := range rel {
		if to == f.ev["secret"] {
			t.Fatal("narrow: a k-secret event was related")
		}
	}
}

// The principal path: bob (OSS authorizer, Bypass + owner:bob) and alice ask
// the same flag-off question about entity c. Each gets a hint; alice's counts
// her own event, bob's does not — and an entity bob can see nothing of answers
// exactly as an entity that never existed.
func TestPg_WhyRelated_HintIsFencedThroughTheSeam(t *testing.T) {
	f := newRelFixture(t)
	query := authz.QueryKnowledgeFunc(domain.AllowAllAuthorizer{}, f.plane, nil)
	ask := func(who, ref string, on bool) (domain.QueryResult, error) {
		return query(f.ctx, domain.PrincipalRef{Kind: domain.PrincipalUser, ID: who},
			domain.KnowledgeQuery{Kind: domain.QueryWhy, EntityID: ref, Hops: 1, IncludeCorrelation: on})
	}
	count := func(res domain.QueryResult) int {
		if len(res.Rows) != 1 || res.Rows[0]["row"] != "hint" {
			t.Fatalf("want one hint row, got %v", res.Rows)
		}
		return res.Rows[0]["related_count"].(int)
	}
	bob, err := ask("bob", "rel148/c", false)
	if err != nil {
		t.Fatal(err)
	}
	alice, err := ask("alice", "rel148/c", false)
	if err != nil {
		t.Fatal(err)
	}
	if count(alice) != count(bob)+1 {
		t.Fatalf("alice's hint should count exactly her one extra event: alice=%d bob=%d", count(alice), count(bob))
	}
	on, err := ask("bob", "rel148/c", true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprint(on.Rows), "rel148-alice") {
		t.Fatalf("alice's private event reached bob as a related row: %v", on.Rows)
	}

	blind := &domain.TagPredicate{RequiredTags: []string{"rel148-nothing-has-this"}}
	hidden, hErr := f.plane.Query(f.ctx, domain.KnowledgeQuery{Kind: domain.QueryWhy, EntityID: "rel148/c"}, blind)
	absent, aErr := f.plane.Query(f.ctx, domain.KnowledgeQuery{Kind: domain.QueryWhy, EntityID: "rel148/never-minted"}, blind)
	if fmt.Sprintf("%#v|%v", hidden, hErr) != fmt.Sprintf("%#v|%v", absent, aErr) {
		t.Fatalf("a caller who sees nothing of c can tell it exists: hidden=%v absent=%v", hidden.Rows, absent.Rows)
	}
}

// ADR-0147 retirement holds in the related lane (T-20261112-shared-object-hop-
// sql-in-why-does-not-apply-the): a retired event is neither related, nor
// counted toward a hub, nor stood on as a root.
func TestPg_WhyRelated_SkipsRetiredEvents(t *testing.T) {
	f := newRelFixture(t)
	q := domain.KnowledgeQuery{EntityID: f.ev["A"], Hops: 1, IncludeCorrelation: true, CorrelationRoles: []string{"customer"}}
	if !has(relatedTo(f.why(t, q, domain.ScopeMaintenance), f.ev["A"]), f.ev["near"]) {
		t.Fatal("control: near should be related before retirement")
	}
	if _, err := NewPgEventStore(f.pool, nil).RetireRows(f.ctx, "default", []string{"rel148-near"}, "superseded", "test"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if has(relatedTo(f.why(t, q, f.narrow), f.ev["A"]), f.ev["near"]) {
		t.Fatal("a retired event is still related")
	}
	root := f.why(t, domain.KnowledgeQuery{EntityID: f.ev["near"], Hops: 1, IncludeCorrelation: true}, f.narrow)
	absent := f.why(t, domain.KnowledgeQuery{EntityID: "event:rel148-never", Hops: 1, IncludeCorrelation: true}, f.narrow)
	if fmt.Sprint(root) != fmt.Sprint(absent) {
		t.Fatalf("a retired root is still stood on: %v", root)
	}
}
