package postgres

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// Policy and Actor on the belief shapes (current, as_of, contradictions) against
// a real, migrated Postgres (PG_TEST_DSN). Owner decision 2026-11-12 (ADR-0111
// amendment; T-20261112-knowledgequery-actor-is-accepted-and-silently):
//   - as_of defaults Policy exactly as current does (latest_assertion) — the '*'
//     cross-actor rows and the per-actor rows of an entity-scoped kind are never
//     mixed in one answer, and as_of(now) == current;
//   - Actor narrows to that actor's own rows before resolution (its per-actor
//     latest) on current and as_of; on contradictions only the disagreements it
//     is part of;
//   - a shape that cannot honour Actor/Policy refuses (ErrCannotExpress).

const selEntKind = "casetest_selent" // entity-scoped, designated_actor ("erp" wins)
const selActKind = "casetest_selact" // per-actor, latest_assertion

func selSpecs() []domain.KindSpec {
	return []domain.KindSpec{
		{
			Kind: selEntKind, Policy: domain.ResolutionPolicyDesignatedActor,
			ResolutionScope: domain.ResolutionScopeEntity,
			Designated:      &domain.DesignatedActors{Sources: []string{"erp"}},
			Predicates:      map[string]domain.ValueSpec{"status": {Type: "text"}},
		},
		{Kind: selActKind, Predicates: map[string]domain.ValueSpec{"status": {Type: "text"}}},
	}
}

type selFixture struct {
	plane *PgQueryPlane
	ctx   context.Context
	mid   time.Time // after the first round of claims, before the second
}

// newSelFixture writes, for both kinds and entity o1:
//
//	round 1: erp "open",  crm "open",   web "draft"
//	round 2: erp "shipped", crm "open" (unchanged), web "cancelled"
//
// and for o2 one agreeing round (erp "open", crm "open") — no disagreement.
func newSelFixture(t *testing.T) selFixture {
	t.Helper()
	ks, _, plane, pool, ctx := newCaseStores(t, selSpecs()...)
	clock := func() time.Time {
		var ts time.Time
		if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&ts); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
		return ts
	}
	put := func(kind, entity, by, ref string, minute int, status string) {
		t.Helper()
		if _, _, err := ks.PutItem(ctx, domain.KnowledgeItem{Kind: kind, EntityID: entity, AssertedBy: by,
			SourceRef: ref, AssertedAt: caseT0.Add(time.Duration(minute) * time.Minute),
			Values: []domain.StatementValue{ctext("status", status)}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{selEntKind, selActKind} {
		put(k, "o1", "erp", "erp-1", 1, "open")
		put(k, "o1", "crm", "crm-1", 2, "open")
		put(k, "o1", "web", "web-1", 3, "draft")
		put(k, "o2", "erp", "erp-o2", 1, "open")
		put(k, "o2", "crm", "crm-o2", 2, "open")
	}
	mid := clock()
	for _, k := range []string{selEntKind, selActKind} {
		put(k, "o1", "erp", "erp-2", 10, "shipped")
		put(k, "o1", "web", "web-2", 11, "cancelled")
	}
	clock()
	return selFixture{plane: plane, ctx: ctx, mid: mid}
}

// beliefRows renders rows as sorted "entity/actor/policy=status" strings.
func beliefRows(t *testing.T, f selFixture, q domain.KnowledgeQuery) []string {
	t.Helper()
	res, err := f.plane.Query(f.ctx, q, &domain.TagPredicate{Bypass: true})
	if err != nil {
		t.Fatalf("%s %+v: %v", q.Kind, q, err)
	}
	var out []string
	for _, r := range res.Rows {
		if q.Kind == domain.QueryContradictions {
			out = append(out, fmt.Sprintf("%v/%v %v", r["entity_id"], r["predicate"], r["actors"]))
			continue
		}
		vals, _ := r["values"].(map[string]any)
		out = append(out, fmt.Sprintf("%v/%v/%v=%v", r["entity_id"], r["actor"], r["policy"], vals["status"]))
	}
	sort.Strings(out)
	return out
}

func TestPg_BeliefSelectors_AsOfDefaultsPolicyLikeCurrent(t *testing.T) {
	f := newSelFixture(t)

	// Entity-scoped kind, neither field set: each actor's own latest, NO '*' row.
	// Before the amendment as_of returned the '*' designated_actor row as well.
	wantLatest := []string{
		"o1/crm/latest_assertion=open", "o1/erp/latest_assertion=shipped", "o1/web/latest_assertion=cancelled",
		"o2/crm/latest_assertion=open", "o2/erp/latest_assertion=open",
	}
	now := time.Now().Add(time.Hour)
	for _, kind := range []string{selEntKind, selActKind} {
		got := beliefRows(t, f, domain.KnowledgeQuery{Kind: domain.QueryAsOf, ItemKind: kind, AsOf: now})
		if !reflect.DeepEqual(got, wantLatest) {
			t.Fatalf("%s as_of(now), no policy:\n got  %v\n want %v", kind, got, wantLatest)
		}
		// as_of(now) == current, field for field.
		cur := beliefRows(t, f, domain.KnowledgeQuery{Kind: domain.QueryCurrent, ItemKind: kind})
		if !reflect.DeepEqual(got, cur) {
			t.Fatalf("%s as_of(now) != current:\n as_of   %v\n current %v", kind, got, cur)
		}
	}

	// Policy = the kind's cross-actor policy: the '*' winner only, as current.
	designated := domain.KnowledgeQuery{Kind: domain.QueryAsOf, ItemKind: selEntKind, AsOf: now,
		Policy: domain.ResolutionPolicyDesignatedActor}
	wantDesignated := []string{"o1/*/designated_actor=shipped", "o2/*/designated_actor=open"}
	if got := beliefRows(t, f, designated); !reflect.DeepEqual(got, wantDesignated) {
		t.Fatalf("as_of designated_actor:\n got  %v\n want %v", got, wantDesignated)
	}
	designated.Kind = domain.QueryCurrent
	if got := beliefRows(t, f, designated); !reflect.DeepEqual(got, wantDesignated) {
		t.Fatalf("current designated_actor:\n got  %v\n want %v", got, wantDesignated)
	}

	// as_of in the past replays what we recorded then, per policy.
	past := beliefRows(t, f, domain.KnowledgeQuery{Kind: domain.QueryAsOf, ItemKind: selEntKind, AsOf: f.mid,
		Policy: domain.ResolutionPolicyDesignatedActor})
	if want := []string{"o1/*/designated_actor=open", "o2/*/designated_actor=open"}; !reflect.DeepEqual(past, want) {
		t.Fatalf("as_of(mid) designated_actor:\n got  %v\n want %v", past, want)
	}
}

func TestPg_BeliefSelectors_ActorFiltersToItsOwnClaims(t *testing.T) {
	f := newSelFixture(t)
	now := time.Now().Add(time.Hour)
	for _, kind := range []string{selEntKind, selActKind} {
		for _, shape := range []string{domain.QueryCurrent, domain.QueryAsOf} {
			q := domain.KnowledgeQuery{Kind: shape, ItemKind: kind, Actor: "web"}
			if shape == domain.QueryAsOf {
				q.AsOf = now
			}
			if got, want := beliefRows(t, f, q), []string{"o1/web/latest_assertion=cancelled"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("%s %s actor=web:\n got  %v\n want %v", kind, shape, got, want)
			}
		}
		// The past: the actor's latest as we recorded it then.
		got := beliefRows(t, f, domain.KnowledgeQuery{Kind: domain.QueryAsOf, ItemKind: kind, Actor: "erp", AsOf: f.mid})
		if want := []string{"o1/erp/latest_assertion=open", "o2/erp/latest_assertion=open"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s as_of(mid) actor=erp:\n got  %v\n want %v", kind, got, want)
		}
		// An actor that never spoke gets nothing — not everyone.
		if got := beliefRows(t, f, domain.KnowledgeQuery{Kind: domain.QueryCurrent, ItemKind: kind, Actor: "nobody"}); len(got) != 0 {
			t.Fatalf("%s current actor=nobody returned %v", kind, got)
		}
	}
	// Per-actor kind with its own (latest_assertion) policy named explicitly: same answer.
	got := beliefRows(t, f, domain.KnowledgeQuery{Kind: domain.QueryCurrent, ItemKind: selActKind, Actor: "crm",
		Policy: domain.ResolutionPolicyLatestAssertion})
	if want := []string{"o1/crm/latest_assertion=open", "o2/crm/latest_assertion=open"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("per-actor kind actor=crm policy=latest_assertion:\n got  %v\n want %v", got, want)
	}
}

func TestPg_BeliefSelectors_ContradictionsKeepTheActorsDisagreements(t *testing.T) {
	f := newSelFixture(t)
	for _, kind := range []string{selEntKind, selActKind} {
		all := beliefRows(t, f, domain.KnowledgeQuery{Kind: domain.QueryContradictions, ItemKind: kind})
		want := []string{"o1/status map[crm:open erp:shipped web:cancelled]"}
		if !reflect.DeepEqual(all, want) {
			t.Fatalf("%s contradictions:\n got  %v\n want %v", kind, all, want)
		}
		// crm is a side of o1's disagreement: the row comes back with every side.
		if got := beliefRows(t, f, domain.KnowledgeQuery{Kind: domain.QueryContradictions, ItemKind: kind, Actor: "crm"}); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s contradictions actor=crm:\n got  %v\n want %v", kind, got, want)
		}
		// An actor in no disagreement: nothing.
		if got := beliefRows(t, f, domain.KnowledgeQuery{Kind: domain.QueryContradictions, ItemKind: kind, Actor: "nobody"}); len(got) != 0 {
			t.Fatalf("%s contradictions actor=nobody returned %v", kind, got)
		}
	}
}

func TestPg_BeliefSelectors_RefuseWhatCannotBeHonoured(t *testing.T) {
	f := newSelFixture(t)
	now := time.Now().Add(time.Hour)
	// Actor on the entity-scoped kind's cross-actor policy: not stored, so refused.
	for _, shape := range []string{domain.QueryCurrent, domain.QueryAsOf, domain.QueryContradictions} {
		q := domain.KnowledgeQuery{Kind: shape, ItemKind: selEntKind, Actor: "erp",
			Policy: domain.ResolutionPolicyDesignatedActor, AsOf: now}
		if shape != domain.QueryAsOf {
			q.AsOf = time.Time{}
		}
		_, err := f.plane.Query(f.ctx, q, &domain.TagPredicate{Bypass: true})
		if !errors.Is(err, domain.ErrCannotExpress) {
			t.Fatalf("%s actor+cross-actor policy: err = %v, want ErrCannotExpress", shape, err)
		}
	}
	// Shapes that read no resolution rows refuse both fields; '*' is not an actor.
	for name, q := range map[string]domain.KnowledgeQuery{
		"point actor":  {Kind: domain.QueryPoint, EntityID: "o1", Predicate: "status", Actor: "erp"},
		"point policy": {Kind: domain.QueryPoint, EntityID: "o1", Predicate: "status", Policy: domain.ResolutionPolicyLatestAssertion},
		"events actor": {Kind: domain.QueryEvents, EntityID: "o1", From: caseT0, To: now, Actor: "erp"},
		"current *":    {Kind: domain.QueryCurrent, ItemKind: selEntKind, Actor: domain.ResolutionActorAll},
	} {
		if _, err := f.plane.Query(f.ctx, q, &domain.TagPredicate{Bypass: true}); !errors.Is(err, domain.ErrCannotExpress) {
			t.Fatalf("%s: err = %v, want ErrCannotExpress", name, err)
		}
	}
}

// Through the principal seam an agent's or operator's query takes
// (authz.QueryKnowledgeFunc): Actor narrows, as_of defaults policy, and a
// refusal reaches the caller as ErrCannotExpress.
func TestPg_BeliefSelectors_ThroughTheQuerySeam(t *testing.T) {
	f := newSelFixture(t)
	query := authz.QueryKnowledgeFunc(domain.AllowAllAuthorizer{}, f.plane, nil)
	bob := domain.PrincipalRef{Kind: domain.PrincipalUser, ID: "bob"}
	res, err := query(f.ctx, bob, domain.KnowledgeQuery{Kind: domain.QueryAsOf, ItemKind: selEntKind,
		AsOf: time.Now().Add(time.Hour), Actor: "web"})
	if err != nil {
		t.Fatalf("seam as_of actor=web: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0]["actor"] != "web" || res.Rows[0]["policy"] != domain.ResolutionPolicyLatestAssertion {
		t.Fatalf("seam as_of actor=web = %+v", res.Rows)
	}
	res, err = query(f.ctx, bob, domain.KnowledgeQuery{Kind: domain.QueryAsOf, ItemKind: selEntKind, AsOf: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("seam as_of: %v", err)
	}
	for _, r := range res.Rows {
		if r["actor"] == domain.ResolutionActorAll {
			t.Fatalf("seam as_of with no policy mixed in the cross-actor row: %+v", res.Rows)
		}
	}
	if _, err := query(f.ctx, bob, domain.KnowledgeQuery{Kind: domain.QueryCurrent, ItemKind: selEntKind,
		Actor: "erp", Policy: domain.ResolutionPolicyDesignatedActor}); !errors.Is(err, domain.ErrCannotExpress) {
		t.Fatalf("seam actor+cross-actor policy: err = %v, want ErrCannotExpress", err)
	}
}
