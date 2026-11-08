package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// The case-modelling extensions against a real, migrated Postgres (PG_TEST_DSN):
// lifecycle rules checked inside the resolution lock, entity-scoped resolution with the
// designated_actor authority, declared event types, the as_of read across a
// supersession, and a golden proving a kind that uses none of it resolves exactly as
// before (T-20261107-knowledge-substrate-extensions-for-case). Rows live in namespace
// "default" (the query plane's) under kinds prefixed "casetest_", deleted before and after.

func newCaseStores(t *testing.T, specs ...domain.KindSpec) (*PgKnowledgeStore, *PgEventStore, *PgQueryPlane, *pgxpool.Pool, context.Context) {
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
			`DELETE FROM resolutions WHERE kind LIKE 'casetest_%'`,
			`DELETE FROM knowledge_items WHERE kind LIKE 'casetest_%'`,
			`DELETE FROM event_roles WHERE event_id IN (SELECT id FROM events WHERE event_type LIKE 'casetest_%')`,
			`DELETE FROM events WHERE event_type LIKE 'casetest_%'`,
		} {
			_, _ = pool.Exec(bg, q)
		}
	}
	clean()
	t.Cleanup(clean)
	reg, err := domain.NewKindRegistry(specs, nil)
	if err != nil {
		t.Fatal(err)
	}
	ks := NewPgKnowledgeStore(pool, reg)
	evs := NewPgEventStore(pool, reg)
	rel, err := domain.NewRelationRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	plane := NewPgQueryPlane(pool, evs, ks, nil, NewPgLinkStore(pool, rel), NewPgEntityStore(pool), rel)
	return ks, evs, plane, pool, ctx
}

func caseDecisionSpec() domain.KindSpec {
	return domain.KindSpec{
		Kind:            "casetest_decision",
		Policy:          domain.ResolutionPolicyDesignatedActor,
		ResolutionScope: domain.ResolutionScopeEntity,
		Designated:      &domain.DesignatedActors{ApprovedViaCommit: true, Sources: []string{"board_minutes"}},
		Predicates: map[string]domain.ValueSpec{
			"status": {Type: "text", Enum: []string{"open", "decided", "superseded"}},
			"chosen": {Type: "text"},
		},
		Lifecycle: &domain.KindLifecycle{
			StatePredicate: "status",
			Initial:        []string{"open"},
			Transitions:    map[string][]string{"open": {"decided"}, "decided": {"superseded", "decided"}},
			Required:       map[string][]string{"decided": {"chosen"}},
		},
		EventTypes: []domain.EventTypeSpec{{Type: "casetest_decided", Roles: []string{"decision", "decider"}, RequiredRoles: []string{"decision"}}},
	}
}

var caseT0 = time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)

func caseItem(entity, by, ref string, minute int, vals ...domain.StatementValue) domain.KnowledgeItem {
	return domain.KnowledgeItem{Kind: "casetest_decision", EntityID: entity, AssertedBy: by, SourceRef: ref,
		AssertedAt: caseT0.Add(time.Duration(minute) * time.Minute), Values: vals}
}

func ctext(p, v string) domain.StatementValue {
	return domain.StatementValue{Predicate: p, Type: "text", Text: v}
}

type resRow struct {
	Entity, Actor, Policy, ItemRef, Reason string
	Current                                bool
}

func caseResolutions(t *testing.T, ctx context.Context, pool *pgxpool.Pool, kind string) []resRow {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT r.entity_id, r.actor, r.policy, COALESCE(ki.source_ref, '<nil>'), r.reason_code, r.system_to IS NULL
		FROM resolutions r LEFT JOIN knowledge_items ki ON ki.id = r.item_id
		WHERE r.kind = $1 ORDER BY r.entity_id, r.actor, r.policy, r.id`, kind)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []resRow
	for rows.Next() {
		var r resRow
		if err := rows.Scan(&r.Entity, &r.Actor, &r.Policy, &r.ItemRef, &r.Reason, &r.Current); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// GOLDEN: a kind that declares none of the extensions (latest_assertion, actor scope)
// writes exactly the resolution rows it wrote before them — per actor, versioned,
// negation as a NULL-item row, replay a no-op, erase re-deriving the survivor.
func TestPg_CaseExtensions_DefaultKindResolutionsUnchanged(t *testing.T) {
	plain := domain.KindSpec{Kind: "casetest_plain", Predicates: map[string]domain.ValueSpec{"v": {Type: "text"}}}
	ks, _, _, pool, ctx := newCaseStores(t, plain)
	put := func(entity, by, ref string, minute int, neg bool, v string) {
		t.Helper()
		it := domain.KnowledgeItem{Kind: "casetest_plain", EntityID: entity, AssertedBy: by, SourceRef: ref,
			AssertedAt: caseT0.Add(time.Duration(minute) * time.Minute), Negation: neg}
		if !neg {
			it.Values = []domain.StatementValue{ctext("v", v)}
		}
		if _, _, err := ks.PutItem(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	put("e1", "a", "a1", 1, false, "x")
	put("e1", "a", "a2", 3, false, "y")
	put("e1", "a", "a0", 0, false, "late-arriving older") // arrival order must not matter
	put("e1", "b", "b1", 2, false, "z")
	put("e1", "b", "b2", 4, true, "")
	put("e1", "a", "a2", 3, false, "y") // replay
	put("e2", "a", "c1", 1, false, "w")
	if _, err := ks.EraseItems(ctx, "default", "casetest_plain", domain.KnowledgeErasure{Entities: []string{"e2"}}); err != nil {
		t.Fatal(err)
	}
	got := caseResolutions(t, ctx, pool, "casetest_plain")
	want := []resRow{
		{"e1", "a", "latest_assertion", "a1", "latest_assertion", false},
		{"e1", "a", "latest_assertion", "a2", "latest_assertion", true},
		{"e1", "b", "latest_assertion", "b1", "latest_assertion", false},
		{"e1", "b", "latest_assertion", "<nil>", "negated", true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("default-kind resolutions changed:\n got  %+v\n want %+v", got, want)
	}
}

func TestPg_CaseExtensions_LifecycleRefusalsNameTheConstraint(t *testing.T) {
	ks, _, _, pool, ctx := newCaseStores(t, caseDecisionSpec())
	put := func(it domain.KnowledgeItem) error { _, _, err := ks.PutItem(ctx, it); return err }
	commit := func(ref string) string { return domain.KnowledgeCommitSourceRefPrefix + ref }

	if err := put(caseItem("d1", "user:alice", commit("c1"), 1, ctext("status", "decided"), ctext("chosen", "A"))); err == nil ||
		!errors.Is(err, domain.ErrKindRefused) || !strings.Contains(err.Error(), `"decided" is not an initial state`) {
		t.Fatalf("initial state: %v", err)
	}
	if err := put(caseItem("d1", "user:alice", commit("c2"), 2, ctext("status", "open"))); err != nil {
		t.Fatal(err)
	}
	if err := put(caseItem("d1", "user:alice", commit("c3"), 3, ctext("status", "decided"))); err == nil ||
		!strings.Contains(err.Error(), `requires predicate "chosen"`) {
		t.Fatalf("required: %v", err)
	}
	// A candidate's lifecycle is checked against the COMMITTED state too: proposing
	// "superseded" straight from "open" is refused.
	if err := put(caseItem("d1", "agent:scout", "p1", 4, ctext("status", "superseded"))); err == nil ||
		!strings.Contains(err.Error(), `transition status "open" -> "superseded" is not declared`) {
		t.Fatalf("transition: %v", err)
	}
	// Refused writes left nothing behind (the insert rolled back with the refusal).
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_items WHERE kind='casetest_decision'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("items after refusals = %d (%v), want 1", n, err)
	}
	// Enum on the store path.
	if err := put(caseItem("d1", "agent:scout", "p2", 5, ctext("status", "maybe"))); !errors.Is(err, domain.ErrKindRefused) {
		t.Fatalf("enum: %v", err)
	}
	// "*" is reserved in an entity-scoped kind.
	if err := put(caseItem("d1", "*", "p3", 5, ctext("chosen", "x"))); !errors.Is(err, domain.ErrKindRefused) {
		t.Fatalf("reserved actor: %v", err)
	}
}

// Concurrent writers of one entity serialize on the entity lock: of two commits racing
// from "open" with different targets, the transition check sees the winner's state.
func TestPg_CaseExtensions_LifecycleIsCheckedInsideTheLock(t *testing.T) {
	spec := caseDecisionSpec()
	spec.Lifecycle.Transitions = map[string][]string{"open": {"decided", "superseded"}} // both terminal
	ks, _, _, _, ctx := newCaseStores(t, spec)
	if _, _, err := ks.PutItem(ctx, caseItem("d1", "user:alice", domain.KnowledgeCommitSourceRefPrefix+"o", 1, ctext("status", "open"))); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, st := range []string{"decided", "superseded"} {
		wg.Add(1)
		go func(i int, st string) {
			defer wg.Done()
			_, _, errs[i] = ks.PutItem(ctx, caseItem("d1", "user:bob", domain.KnowledgeCommitSourceRefPrefix+st, 2+i,
				ctext("status", st), ctext("chosen", "A")))
		}(i, st)
	}
	wg.Wait()
	ok := 0
	for _, e := range errs {
		if e == nil {
			ok++
		} else if !strings.Contains(e.Error(), "is terminal") {
			t.Fatalf("unexpected error: %v", e)
		}
	}
	if ok != 1 {
		t.Fatalf("exactly one of two racing terminal transitions may succeed, got %d (%v)", ok, errs)
	}
}

func TestPg_CaseExtensions_DesignatedActorAcrossActors(t *testing.T) {
	ks, _, plane, pool, ctx := newCaseStores(t, caseDecisionSpec())
	put := func(it domain.KnowledgeItem) {
		t.Helper()
		if _, _, err := ks.PutItem(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	put(caseItem("d1", "agent:scout", "p-open", 1, ctext("status", "open")))
	// Nobody eligible has spoken: no cross-actor resolution at all.
	for _, r := range caseResolutions(t, ctx, pool, "casetest_decision") {
		if r.Actor == domain.ResolutionActorAll {
			t.Fatalf("a candidate produced a committed row: %+v", r)
		}
	}
	put(caseItem("d1", "board_minutes", "m-open", 2, ctext("status", "open")))
	put(caseItem("d1", "agent:scout", "p-dec", 5, ctext("status", "decided"), ctext("chosen", "B")))
	put(caseItem("d1", "user:alice", domain.KnowledgeCommitSourceRefPrefix+"ap1", 4, ctext("status", "decided"), ctext("chosen", "A")))
	put(caseItem("d1", "user:mallory", "forged-no-marker", 9, ctext("status", "decided"), ctext("chosen", "C")))

	cur, err := plane.Query(ctx, domain.KnowledgeQuery{Kind: domain.QueryCurrent, ItemKind: "casetest_decision",
		Policy: domain.ResolutionPolicyDesignatedActor, EntityID: "d1"}, &domain.TagPredicate{Bypass: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(cur.Rows) != 1 || cur.Rows[0]["actor"] != domain.ResolutionActorAll ||
		cur.Rows[0]["values"].(map[string]any)["chosen"] != "A" || cur.Rows[0]["reason_code"] != domain.ReasonDesignatedAssertion {
		t.Fatalf("committed value must be the approved commit (A), not the later agent (B) or mallory (C): %+v", cur.Rows)
	}
	// Contradictions are preserved: every speaker's current word is still readable.
	con, err := plane.Query(ctx, domain.KnowledgeQuery{Kind: domain.QueryContradictions, ItemKind: "casetest_decision",
		EntityID: "d1", Predicate: "chosen"}, &domain.TagPredicate{Bypass: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(con.Rows) != 1 {
		t.Fatalf("contradictions = %+v", con.Rows)
	}
	views := fmt.Sprint(con.Rows[0])
	for _, want := range []string{"agent:scout", "user:alice", "user:mallory", "B", "A", "C"} {
		if !strings.Contains(views, want) {
			t.Fatalf("contradiction row lacks %q: %s", want, views)
		}
	}
}

func TestPg_CaseExtensions_AsOfAcrossASupersession(t *testing.T) {
	ks, _, plane, pool, ctx := newCaseStores(t, caseDecisionSpec())
	clock := func() time.Time {
		var ts time.Time
		if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&ts); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
		return ts
	}
	put := func(it domain.KnowledgeItem) {
		t.Helper()
		if _, _, err := ks.PutItem(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	before := clock()
	put(caseItem("d1", "user:alice", domain.KnowledgeCommitSourceRefPrefix+"ap-open", 1, ctext("status", "open")))
	put(caseItem("d1", "user:alice", domain.KnowledgeCommitSourceRefPrefix+"ap-a", 2, ctext("status", "decided"), ctext("chosen", "A")))
	afterA := clock()
	put(caseItem("d1", "agent:scout", "p-b", 3, ctext("status", "decided"), ctext("chosen", "B")))
	put(caseItem("d1", "user:bob", domain.KnowledgeCommitSourceRefPrefix+"ap-b", 4, ctext("status", "decided"), ctext("chosen", "B")))
	afterB := clock()

	committedAt := func(at time.Time) []string {
		t.Helper()
		res, err := plane.Query(ctx, domain.KnowledgeQuery{Kind: domain.QueryAsOf, ItemKind: "casetest_decision",
			EntityID: "d1", AsOf: at}, &domain.TagPredicate{Bypass: true})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range res.Rows {
			if r["policy"] == domain.ResolutionPolicyDesignatedActor {
				out = append(out, fmt.Sprintf("%v/%v", r["values"].(map[string]any)["chosen"], r["asserted_by"]))
			}
		}
		return out
	}
	if got := committedAt(before); len(got) != 0 {
		t.Fatalf("before the first commit: %v", got)
	}
	if got := committedAt(afterA); !reflect.DeepEqual(got, []string{"A/user:alice"}) {
		t.Fatalf("as of after A: %v", got)
	}
	if got := committedAt(afterB); !reflect.DeepEqual(got, []string{"B/user:bob"}) {
		t.Fatalf("as of after the supersession: %v", got)
	}
}

func TestPg_CaseExtensions_DeclaredEventTypes(t *testing.T) {
	_, evs, _, _, ctx := newCaseStores(t, caseDecisionSpec())
	ev := domain.Event{Type: "casetest_decided", OccurredAt: caseT0, SourceRef: "casetest-ev-1",
		Roles: []domain.EventRole{{Role: "decision", EntityID: "d1"}, {Role: "decider", EntityID: "person/alice"}}}
	if _, _, err := evs.RecordEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	bad := ev
	bad.SourceRef, bad.Roles = "casetest-ev-2", []domain.EventRole{{Role: "decider", EntityID: "person/alice"}}
	if _, _, err := evs.RecordEvent(ctx, bad); !errors.Is(err, domain.ErrKindRefused) || !strings.Contains(err.Error(), `requires role "decision"`) {
		t.Fatalf("missing required role: %v", err)
	}
	bad.Roles = append(ev.Roles, domain.EventRole{Role: "witness", EntityID: "x"})
	if _, _, err := evs.RecordEvent(ctx, bad); !errors.Is(err, domain.ErrKindRefused) {
		t.Fatalf("undeclared role: %v", err)
	}
	free := domain.Event{Type: "casetest_free", OccurredAt: caseT0, SourceRef: "casetest-ev-3",
		Roles: []domain.EventRole{{Role: "anything", EntityID: "x"}}}
	if _, _, err := evs.RecordEvent(ctx, free); err != nil {
		t.Fatalf("an undeclared event type must pass: %v", err)
	}
}
